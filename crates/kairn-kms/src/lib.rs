//! KMS-backed signing of Kairn bundles: AWS KMS and GCP Cloud KMS (docs/design-kms.md).
//!
//! The signature is exactly what `ieb/v1` already specifies (ECDSA P-256 SHA-256, DER,
//! canonical low-S, `key_id` = SHA-256 of the SPKI DER), so verifiers need nothing new.
//! No cloud SDKs: AWS requests are signed with `object_store`'s SigV4 signer and credential
//! chain (IRSA, EKS Pod Identity, env); GCP uses `object_store`'s token provider (Workload
//! Identity, service-account files) or `GOOGLE_OAUTH_ACCESS_TOKEN`.
//!
//! The binary must install a rustls crypto provider (ring) before use.

use std::time::Duration;

use base64::{engine::general_purpose::STANDARD, Engine as _};
use object_store::aws::{AmazonS3Builder, AwsAuthorizer, AwsCredentialProvider};
use object_store::client::HttpRequestBody;
use object_store::gcp::{GcpCredentialProvider, GoogleCloudStorageBuilder};
use p256::pkcs8::{DecodePublicKey, EncodePublicKey, LineEnding};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};

/// A configured KMS key. Only exact, immutable key identities are accepted: an AWS key ARN
/// (never an alias, which `UpdateAlias` can repoint) or a GCP key *version*.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum KmsKey {
    Aws {
        arn: String,
        partition: String,
        region: String,
    },
    Gcp {
        version: String,
    },
}

impl KmsKey {
    pub fn parse(s: &str) -> Result<Self, KmsError> {
        let s = s.trim();
        let word = |w: &str| {
            !w.is_empty()
                && w.bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        };
        if s.starts_with("arn:") {
            let parts: Vec<&str> = s.splitn(6, ':').collect();
            let [_, partition, service, region, account, resource] = parts[..] else {
                return Err(KmsError::config(format!("{s}: not an ARN")));
            };
            if service != "kms" {
                return Err(KmsError::config(format!("{s}: not a KMS ARN")));
            }
            if resource.starts_with("alias/") {
                return Err(KmsError::new(
                    ErrorKind::KeyUnsupported,
                    format!(
                        "{s}: alias ARNs are refused (an alias can be repointed to another key); \
                         use the key ARN"
                    ),
                ));
            }
            let key_ok = resource.strip_prefix("key/").is_some_and(word);
            let region_ok = word(region) && region.bytes().all(|b| b != b'_');
            let account_ok = account.len() == 12 && account.bytes().all(|b| b.is_ascii_digit());
            if !matches!(partition, "aws" | "aws-cn" | "aws-us-gov")
                || !region_ok
                || !account_ok
                || !key_ok
            {
                return Err(KmsError::config(format!(
                    "{s}: expected arn:<aws|aws-cn|aws-us-gov>:kms:<region>:<account>:key/<id>"
                )));
            }
            return Ok(KmsKey::Aws {
                arn: s.to_string(),
                partition: partition.to_string(),
                region: region.to_string(),
            });
        }
        let seg: Vec<&str> = s.split('/').collect();
        let gcp_ok = seg.len() == 10
            && seg[0] == "projects"
            && seg[2] == "locations"
            && seg[4] == "keyRings"
            && seg[6] == "cryptoKeys"
            && seg[8] == "cryptoKeyVersions"
            && [1, 3, 5, 7].iter().all(|&i| word(seg[i]))
            && !seg[9].is_empty()
            && seg[9].bytes().all(|b| b.is_ascii_digit());
        if gcp_ok {
            return Ok(KmsKey::Gcp {
                version: s.to_string(),
            });
        }
        Err(KmsError::config(format!(
            "{s}: expected an AWS key ARN or a GCP key version \
             (projects/…/cryptoKeys/<k>/cryptoKeyVersions/<n>)"
        )))
    }

    pub fn name(&self) -> &str {
        match self {
            KmsKey::Aws { arn, .. } => arn,
            KmsKey::Gcp { version } => version,
        }
    }
}

/// Why signing failed. `kind` is a fixed reason code for status and Events.
#[derive(Debug, Clone, thiserror::Error)]
#[error("{}: {message}", kind.reason())]
pub struct KmsError {
    pub kind: ErrorKind,
    pub message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    /// The configuration itself is wrong (a malformed key name, a bad endpoint override).
    Config,
    /// Transient: network, throttling, the service's own errors.
    Unavailable,
    /// Credentials or permissions.
    Denied,
    /// The key is not a usable P-256 signing key (spec, usage, state).
    KeyUnsupported,
    /// The service answered for another key, or a signature doesn't verify against the
    /// pinned public key.
    KeyChanged,
}

impl ErrorKind {
    pub fn reason(self) -> &'static str {
        match self {
            ErrorKind::Config => "signing-misconfigured",
            ErrorKind::Unavailable => "signing-unavailable",
            ErrorKind::Denied => "signing-denied",
            ErrorKind::KeyUnsupported => "signing-key-unsupported",
            ErrorKind::KeyChanged => "signing-key-changed",
        }
    }
}

impl KmsError {
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Self {
            kind,
            message: message.into(),
        }
    }
    fn config(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Config, message)
    }
    fn unavailable(message: impl Into<String>) -> Self {
        Self::new(ErrorKind::Unavailable, message)
    }
}

/// A signature from the KMS, ready to store: canonical low-S DER, base64; verified locally.
#[derive(Debug, Clone)]
pub struct Signed {
    pub signature_b64: String,
    /// SHA-256 (hex) of the signed manifest bytes: what the cloud audit log saw.
    pub digest_hex: String,
    /// The cloud's request id, to match the audit log entry (AWS; GCP REST returns none).
    pub request_id: Option<String>,
}

enum Auth {
    Aws(AwsCredentialProvider),
    Gcp(GcpCredentialProvider),
    Bearer(String),
}

/// A connected KMS signer: the key's public key is fetched, checked and pinned.
pub struct KmsSigner {
    key: KmsKey,
    /// The endpoint as [`kairn_net`] accepted it; every client is built from it.
    endpoint: kairn_net::Endpoint,
    /// The same endpoint as a base URL (no trailing slash), which is what is printed.
    base_url: String,
    auth: Auth,
    public_key_pem: String,
    key_id: String,
}

impl std::fmt::Debug for KmsSigner {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("KmsSigner")
            .field("key", &self.key)
            .field("endpoint", &self.base_url)
            .field("key_id", &self.key_id)
            .finish()
    }
}

/// The environment variable that overrides this key's endpoint.
fn override_var(key: &KmsKey) -> &'static str {
    match key {
        KmsKey::Aws { .. } => "AWS_ENDPOINT_URL_KMS",
        KmsKey::Gcp { .. } => "KAIRN_GCP_KMS_ENDPOINT",
    }
}

/// Test and private-endpoint overrides (never set by the chart): `AWS_ENDPOINT_URL_KMS`,
/// `KAIRN_GCP_KMS_ENDPOINT`.
pub fn endpoint_override(key: &KmsKey) -> Option<String> {
    std::env::var(override_var(key))
        .ok()
        .filter(|v| !v.is_empty())
}

/// The endpoint to talk to: the override if one is set, otherwise the service's own.
///
/// [`kairn_net::parse`] owns the syntax, with `allow_http_local` for an override only: a KMS
/// emulator in a test runs on loopback or a cluster-local name with no TLS, and nothing else
/// may be plain HTTP. That is also what refuses `http://127.0.0.1@evil.example/` — user info
/// is the shape that defeats a check of the host's first bytes, which is how this used to be
/// done here.
fn endpoint_for(key: &KmsKey, override_url: Option<&str>) -> Result<kairn_net::Endpoint, KmsError> {
    match override_url {
        Some(url) => kairn_net::parse(url, true)
            .map_err(|e| KmsError::config(format!("{}: {e}", override_var(key)))),
        None => {
            let url = match key {
                KmsKey::Aws {
                    partition, region, ..
                } if partition == "aws-cn" => format!("https://kms.{region}.amazonaws.com.cn"),
                KmsKey::Aws { region, .. } => format!("https://kms.{region}.amazonaws.com"),
                KmsKey::Gcp { .. } => "https://cloudkms.googleapis.com".to_string(),
            };
            kairn_net::parse(&url, false).map_err(KmsError::config)
        }
    }
}

impl KmsSigner {
    /// Connect and preflight: fetch the public key, check its spec and that the service
    /// answers for exactly the configured key, and pin it.
    pub async fn connect(key: KmsKey) -> Result<Self, KmsError> {
        let endpoint = endpoint_for(&key, endpoint_override(&key).as_deref())?;
        let base_url = endpoint.url().trim_end_matches('/').to_string();
        // The object-store builders are used only for their credential chains; the bucket
        // name is a placeholder and no storage request is ever made.
        let auth = match &key {
            KmsKey::Aws {
                region, partition, ..
            } => {
                let mut builder = AmazonS3Builder::from_env()
                    .with_bucket_name("kairn-kms-credentials")
                    .with_region(region);
                // Web identity (IRSA) exchanges its token at STS, whose default host
                // (sts.<region>.amazonaws.com) doesn't exist in China.
                if partition == "aws-cn" && std::env::var_os("AWS_ENDPOINT_URL_STS").is_none() {
                    builder = builder.with_config(
                        object_store::aws::AmazonS3ConfigKey::StsEndpoint,
                        format!("https://sts.{region}.amazonaws.com.cn"),
                    );
                }
                let store = builder
                    .build()
                    .map_err(|e| KmsError::config(format!("AWS credentials: {e}")))?;
                Auth::Aws(store.credentials().clone())
            }
            KmsKey::Gcp { .. } => match std::env::var("GOOGLE_OAUTH_ACCESS_TOKEN") {
                Ok(t) if !t.is_empty() => Auth::Bearer(t),
                _ => {
                    let store = GoogleCloudStorageBuilder::from_env()
                        .with_bucket_name("kairn-kms-credentials")
                        .build()
                        .map_err(|e| KmsError::config(format!("GCP credentials: {e}")))?;
                    Auth::Gcp(store.credentials().clone())
                }
            },
        };
        let mut signer = KmsSigner {
            key,
            endpoint,
            base_url,
            auth,
            public_key_pem: String::new(),
            key_id: String::new(),
        };
        signer.public_key_pem = signer.fetch_public_key().await?;
        signer.key_id = kairn_bundle::sign::key_id(&signer.public_key_pem)
            .map_err(|e| KmsError::new(ErrorKind::KeyUnsupported, e.to_string()))?;
        Ok(signer)
    }

    pub fn key(&self) -> &KmsKey {
        &self.key
    }
    pub fn endpoint(&self) -> &str {
        &self.base_url
    }

    /// The HTTP client for one KMS call, from [`kairn_net::connect`]: no redirects (a 3xx
    /// from a KMS is an error, not a hop), no referer, HTTPS enforced unless the endpoint is
    /// a local plain-HTTP override, the host resolved once and the vetted addresses pinned
    /// into the client, so the connection cannot land on an address that was not checked.
    ///
    /// [`kairn_net::Reach::Cluster`], not `Internet`: an override points at an emulator on
    /// loopback or at a private endpoint inside the cluster or VPC, which `Internet` would
    /// refuse. Link-local is refused for every reach, and that is the one that matters here:
    /// it is where the metadata endpoint that hands out the credentials this signer is about
    /// to use lives.
    ///
    /// Built per call, like the controller's notifier. A client cached for the life of the
    /// process would hold pinned addresses that were vetted once and can go stale (a cloud
    /// KMS endpoint's addresses change), and the controller keeps one signer for the life of
    /// the process.
    async fn client(&self) -> Result<reqwest::Client, KmsError> {
        kairn_net::connect(
            &self.endpoint,
            kairn_net::Reach::Cluster,
            Some(Duration::from_secs(30)),
        )
        .await
        .map_err(KmsError::unavailable)
    }
    /// The pinned public key (SPKI PEM).
    pub fn public_key_pem(&self) -> &str {
        &self.public_key_pem
    }
    /// The `ieb/v1` key id of the pinned public key.
    pub fn key_id(&self) -> &str {
        &self.key_id
    }

    /// Sign `manifest` (the exact `manifest.json` bytes): the KMS signs its SHA-256; the
    /// result is normalized to low-S and verified against the pinned key before it is
    /// returned. A signature that doesn't verify is an error, never a bundle.
    pub async fn sign_manifest(&self, manifest: &[u8]) -> Result<Signed, KmsError> {
        let digest = Sha256::digest(manifest);
        let (der, request_id) = match &self.key {
            KmsKey::Aws { arn, .. } => {
                let (v, request_id) = self
                    .aws(
                        "Sign",
                        json!({
                            "KeyId": arn,
                            "Message": STANDARD.encode(digest),
                            "MessageType": "DIGEST",
                            "SigningAlgorithm": "ECDSA_SHA_256",
                        }),
                    )
                    .await?;
                if v["KeyId"].as_str() != Some(arn) {
                    return Err(KmsError::new(
                        ErrorKind::KeyChanged,
                        format!("Sign answered for key {}", v["KeyId"]),
                    ));
                }
                (b64_field(&v, "Signature")?, request_id)
            }
            KmsKey::Gcp { version } => {
                let v = self
                    .gcp(
                        reqwest::Method::POST,
                        &format!("{version}:asymmetricSign"),
                        Some(json!({
                            "digest": { "sha256": STANDARD.encode(digest) },
                            "digestCrc32c": crc32c(&digest).to_string(),
                        })),
                    )
                    .await?;
                if v["name"].as_str() != Some(version) {
                    return Err(KmsError::new(
                        ErrorKind::KeyChanged,
                        format!("asymmetricSign answered for {}", v["name"]),
                    ));
                }
                if v["verifiedDigestCrc32c"] != json!(true) {
                    return Err(KmsError::unavailable(
                        "the service did not confirm the digest's CRC32C",
                    ));
                }
                let der = b64_field(&v, "signature")?;
                if !crc_matches(&v["signatureCrc32c"], &der) {
                    return Err(KmsError::unavailable(
                        "signature CRC32C mismatch in transit",
                    ));
                }
                (der, None)
            }
        };
        let signature_b64 = kairn_bundle::sign::canonical_signature_b64(&der).map_err(|e| {
            KmsError::new(ErrorKind::KeyChanged, format!("not a P-256 signature: {e}"))
        })?;
        kairn_bundle::sign::verify_b64(&self.public_key_pem, manifest, &signature_b64).map_err(
            |e| {
                KmsError::new(
                    ErrorKind::KeyChanged,
                    format!("the signature does not verify against the pinned key: {e}"),
                )
            },
        )?;
        Ok(Signed {
            signature_b64,
            digest_hex: digest.iter().map(|b| format!("{b:02x}")).collect(),
            request_id,
        })
    }

    async fn fetch_public_key(&self) -> Result<String, KmsError> {
        let unsupported = |m: String| KmsError::new(ErrorKind::KeyUnsupported, m);
        let der_to_pem = |der: &[u8]| -> Result<String, KmsError> {
            p256::ecdsa::VerifyingKey::from_public_key_der(der)
                .and_then(|k| k.to_public_key_pem(LineEnding::LF))
                .map_err(|e| unsupported(format!("not a P-256 public key: {e}")))
        };
        match &self.key {
            KmsKey::Aws { arn, .. } => {
                let (v, _) = self.aws("GetPublicKey", json!({ "KeyId": arn })).await?;
                if v["KeyId"].as_str() != Some(arn) {
                    return Err(KmsError::new(
                        ErrorKind::KeyChanged,
                        format!("GetPublicKey answered for key {}", v["KeyId"]),
                    ));
                }
                let spec = v["KeySpec"].as_str().unwrap_or("?");
                let usage = v["KeyUsage"].as_str().unwrap_or("?");
                if spec != "ECC_NIST_P256" || usage != "SIGN_VERIFY" {
                    return Err(unsupported(format!(
                        "key spec {spec} / usage {usage}; Kairn needs ECC_NIST_P256 / SIGN_VERIFY"
                    )));
                }
                der_to_pem(&b64_field(&v, "PublicKey")?)
            }
            KmsKey::Gcp { version } => {
                let v = self
                    .gcp(reqwest::Method::GET, &format!("{version}/publicKey"), None)
                    .await?;
                if v["name"].as_str() != Some(version) {
                    return Err(KmsError::new(
                        ErrorKind::KeyChanged,
                        format!("getPublicKey answered for {}", v["name"]),
                    ));
                }
                let algorithm = v["algorithm"].as_str().unwrap_or("?");
                if algorithm != "EC_SIGN_P256_SHA256" {
                    return Err(unsupported(format!(
                        "algorithm {algorithm}; Kairn needs EC_SIGN_P256_SHA256"
                    )));
                }
                let pem = v["pem"]
                    .as_str()
                    .ok_or_else(|| KmsError::unavailable("getPublicKey: no pem"))?;
                if !crc_matches(&v["pemCrc32c"], pem.as_bytes()) {
                    return Err(KmsError::unavailable(
                        "public key CRC32C mismatch in transit",
                    ));
                }
                let key = p256::ecdsa::VerifyingKey::from_public_key_pem(pem)
                    .map_err(|e| unsupported(format!("not a P-256 public key: {e}")))?;
                let der = key
                    .to_public_key_der()
                    .map_err(|e| unsupported(e.to_string()))?;
                der_to_pem(der.as_bytes())
            }
        }
    }

    /// One AWS KMS JSON call, SigV4-signed. Returns the body and the request id.
    async fn aws(&self, target: &str, body: Value) -> Result<(Value, Option<String>), KmsError> {
        let (Auth::Aws(provider), KmsKey::Aws { region, .. }) = (&self.auth, &self.key) else {
            unreachable!("AWS call with non-AWS auth");
        };
        let credential = provider
            .get_credential()
            .await
            .map_err(|e| KmsError::new(ErrorKind::Denied, format!("AWS credentials: {e}")))?;
        let bytes = serde_json::to_vec(&body).expect("JSON");
        let url = format!("{}/", self.base_url);
        let mut request = http::Request::builder()
            .method("POST")
            .uri(&url)
            .header("content-type", "application/x-amz-json-1.1")
            .header("x-amz-target", format!("TrentService.{target}"))
            .body(HttpRequestBody::from(bytes.clone()))
            .map_err(|e| KmsError::config(e.to_string()))?;
        AwsAuthorizer::new(&credential, "kms", region)
            .try_authorize(&mut request, None)
            .map_err(|e| KmsError::config(format!("signing the request: {e}")))?;
        let resp = self
            .client()
            .await?
            .post(&url)
            .headers(request.headers().clone())
            .body(bytes)
            .send()
            .await
            .map_err(|e| KmsError::unavailable(format!("{target}: {}", e.without_url())))?;
        let status = resp.status();
        let request_id = resp
            .headers()
            .get("x-amzn-requestid")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let text = resp
            .text()
            .await
            .map_err(|e| KmsError::unavailable(format!("{target}: {}", e.without_url())))?;
        let v: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        if status.is_success() {
            return Ok((v, request_id));
        }
        let kind_name = v["__type"]
            .as_str()
            .unwrap_or("")
            .rsplit('#')
            .next()
            .unwrap_or("")
            .to_string();
        let message = v["message"]
            .as_str()
            .or(v["Message"].as_str())
            .unwrap_or("")
            .to_string();
        let kind = if status.is_server_error()
            || kind_name.contains("Throttl")
            || kind_name.contains("Internal")
            || kind_name.contains("DependencyTimeout")
        {
            ErrorKind::Unavailable
        } else if kind_name.contains("AccessDenied")
            || kind_name.contains("NotAuthorized")
            || kind_name.contains("UnrecognizedClient")
            || kind_name.contains("InvalidSignature")
            || kind_name.contains("ExpiredToken")
            || status.as_u16() == 403
        {
            ErrorKind::Denied
        } else {
            ErrorKind::KeyUnsupported
        };
        Err(KmsError::new(
            kind,
            format!("{target}: {status} {kind_name} {message}")
                .trim()
                .to_string(),
        ))
    }

    /// One Cloud KMS REST call with a bearer token.
    async fn gcp(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<Value, KmsError> {
        let token = match &self.auth {
            Auth::Bearer(t) => t.clone(),
            Auth::Gcp(provider) => provider
                .get_credential()
                .await
                .map_err(|e| KmsError::new(ErrorKind::Denied, format!("GCP credentials: {e}")))?
                .bearer
                .clone(),
            Auth::Aws(_) => unreachable!("GCP call with AWS auth"),
        };
        let url = format!("{}/v1/{path}", self.base_url);
        let mut req = self
            .client()
            .await?
            .request(method, &url)
            .bearer_auth(token);
        if let Some(b) = body {
            req = req
                .header("content-type", "application/json")
                .body(serde_json::to_vec(&b).expect("JSON"));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| KmsError::unavailable(format!("{path}: {}", e.without_url())))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| KmsError::unavailable(format!("{path}: {}", e.without_url())))?;
        let v: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        if status.is_success() {
            return Ok(v);
        }
        let message = v["error"]["message"].as_str().unwrap_or("").to_string();
        let kind = match status.as_u16() {
            401 | 403 => ErrorKind::Denied,
            429 | 500..=599 => ErrorKind::Unavailable,
            _ => ErrorKind::KeyUnsupported,
        };
        Err(KmsError::new(
            kind,
            format!("{path}: {status} {message}").trim().to_string(),
        ))
    }
}

fn b64_field(v: &Value, field: &str) -> Result<Vec<u8>, KmsError> {
    v[field]
        .as_str()
        .and_then(|s| STANDARD.decode(s).ok())
        .ok_or_else(|| KmsError::unavailable(format!("response without a valid {field}")))
}

/// CRC32C (Castagnoli), as Cloud KMS uses for its integrity fields.
pub fn crc32c(data: &[u8]) -> u32 {
    let mut crc = !0u32;
    for &b in data {
        crc ^= u32::from(b);
        for _ in 0..8 {
            crc = if crc & 1 != 0 {
                (crc >> 1) ^ 0x82F6_3B78
            } else {
                crc >> 1
            };
        }
    }
    !crc
}

/// Cloud KMS sends int64 CRCs as JSON strings.
fn crc_matches(field: &Value, data: &[u8]) -> bool {
    let want = crc32c(data).to_string();
    field.as_str() == Some(want.as_str()) || field.as_u64() == Some(u64::from(crc32c(data)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_names() {
        let aws = KmsKey::parse(
            "arn:aws:kms:ap-northeast-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
        )
        .unwrap();
        assert!(matches!(aws, KmsKey::Aws { ref region, .. } if region == "ap-northeast-2"));
        assert!(KmsKey::parse("arn:aws-cn:kms:cn-north-1:123456789012:key/mrk-1a2b").is_ok());
        let alias = KmsKey::parse("arn:aws:kms:us-east-1:123456789012:alias/kairn").unwrap_err();
        assert_eq!(alias.kind, ErrorKind::KeyUnsupported);
        for bad in [
            "alias/kairn",
            "1234abcd-12ab-34cd-56ef-1234567890ab",
            "arn:aws:s3:us-east-1:123456789012:key/x",
            "arn:aws:kms:us-east-1:1234:key/x",
            "arn:evil:kms:us-east-1:123456789012:key/x",
            "arn:aws:kms:us-east-1.evil.example:123456789012:key/x",
            "projects/p/locations/l/keyRings/r/cryptoKeys/k",
            "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/latest",
            "projects/p/../l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
        ] {
            assert!(KmsKey::parse(bad).is_err(), "{bad}");
        }
        assert!(matches!(
            KmsKey::parse(
                "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/3"
            ),
            Ok(KmsKey::Gcp { .. })
        ));
    }

    fn aws() -> KmsKey {
        KmsKey::parse("arn:aws:kms:us-east-1:123456789012:key/k1").unwrap()
    }

    #[test]
    fn plain_http_only_locally() {
        let key = aws();
        let ep = |u: &str| endpoint_for(&key, Some(u));
        // The forms the emulators use: `test/kms/emulators.sh` (localhost) and
        // `test/e2e/kms.sh` (a LocalStack Service in the cluster).
        for local in [
            "http://localhost:4566",
            "http://127.0.0.1:8080",
            "http://kms.test.svc.cluster.local:8080",
            "http://localstack.localstack.svc.cluster.local:4566",
        ] {
            assert_eq!(ep(local).unwrap().scheme, "http", "refused {local}");
        }
        assert_eq!(ep("https://kms.example").unwrap().scheme, "https");
        for bad in [
            "http://kms.example",
            "http://localhost.evil.example",
            "ftp://x",
            // User info is what defeated the old check: it read the host as `127.0.0.1`
            // and allowed plain HTTP to evil.example.
            "http://127.0.0.1@evil.example/",
            "http://127.0.0.1@evil.example:8080/hook",
            "http://localhost@evil.example/",
        ] {
            let e = ep(bad).unwrap_err();
            assert_eq!(e.kind, ErrorKind::Config, "{bad}");
        }
        // The message names the variable to fix, not just the URL.
        assert!(ep("http://kms.example")
            .unwrap_err()
            .message
            .contains("AWS_ENDPOINT_URL_KMS"));
    }

    #[test]
    fn the_services_own_endpoints_are_https() {
        assert_eq!(
            endpoint_for(&aws(), None).unwrap().url(),
            "https://kms.us-east-1.amazonaws.com/"
        );
        let cn = KmsKey::parse("arn:aws-cn:kms:cn-north-1:123456789012:key/k").unwrap();
        assert_eq!(
            endpoint_for(&cn, None).unwrap().host,
            "kms.cn-north-1.amazonaws.com.cn"
        );
        let gcp = KmsKey::parse(
            "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
        )
        .unwrap();
        let gcp = endpoint_for(&gcp, None).unwrap();
        assert_eq!(
            (gcp.scheme.as_str(), gcp.host.as_str()),
            ("https", "cloudkms.googleapis.com")
        );
    }

    #[test]
    fn crc32c_known_answer() {
        assert_eq!(crc32c(b"123456789"), 0xE306_9283);
    }
}
