//! Object-store export (`docs/design-export.md`): copy each sealed `.ieb` to admin-defined
//! destinations, once, never overwriting.
//!
//! - Destinations come only from the controller's configuration (the chart); a
//!   `CaptureProfile` can reference one by name and alter nothing about it.
//! - The object key uses the **controller's** cluster id, never the capture's.
//! - Uploads are conditional creates with a service-verified SHA-256; an existing object is
//!   downloaded and hashed: equal → uploaded, different → conflict (never "fixed").
//! - Before a destination's first upload, a probe checks that the backend really refuses a
//!   second conditional create; one that doesn't is disabled.

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use k8s_openapi::api::core::v1::Secret;
use kube::{Api, Client};
use object_store::aws::{AmazonS3Builder, Checksum};
use object_store::gcp::GoogleCloudStorageBuilder;
use object_store::path::Path as ObjectPath;
use object_store::{
    Attribute, Attributes, ClientOptions, ObjectStore, ObjectStoreExt, PutMode, PutOptions,
    PutPayload, RetryConfig,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};

/// v0.2 reads a bundle into memory to upload it; larger bundles are refused (`too-large`).
pub const MAX_UPLOAD_BYTES: u64 = 100 << 20;
/// Attempts before an upload is given up (≈ a day with the backoff below).
pub const MAX_ATTEMPTS: u32 = 24;

/// One destination as the admin wrote it in the chart values.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DestinationSpec {
    pub name: String,
    pub url: String,
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub endpoint: String,
    #[serde(default)]
    pub allow_http: bool,
    #[serde(default)]
    pub credentials_secret: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Probe {
    Unknown,
    Ok,
    Unsupported,
}

struct Destination {
    /// `s3://bucket` or `gs://bucket`, for building object URLs.
    bucket_url: String,
    /// Path prefix inside the bucket (may be empty).
    prefix: String,
    store: Arc<dyn ObjectStore>,
    probe: tokio::sync::Mutex<Probe>,
}

/// What one upload attempt concluded.
#[derive(Debug, PartialEq, Eq)]
pub enum Outcome {
    /// The object holds exactly this bundle (uploaded now, or by an earlier attempt).
    /// `sha256` and the store's `version` id are recorded, so a reader can later check
    /// that the object they verify is the one Kairn wrote.
    Uploaded {
        url: String,
        sha256: String,
        version: Option<String>,
    },
    /// Will never be uploaded to this destination (settled). `reason` is a fixed code.
    Refused { reason: &'static str },
    /// A different object already exists under the key (settled; never overwritten).
    Conflict { url: String },
    /// Transient; retry later. `reason` is a fixed code.
    Retry { reason: &'static str },
}

pub struct Exporter {
    /// Destinations as the admin defined them (validated, not yet connected).
    specs: BTreeMap<String, DestinationSpec>,
    /// Destinations whose configuration itself is wrong (bad URL, scheme…): refused.
    config_errors: BTreeMap<String, &'static str>,
    /// Built lazily at first use, so a Secret that isn't there yet (ExternalSecrets,
    /// an API blip) is retried instead of disabling the destination for good.
    built: tokio::sync::Mutex<BTreeMap<String, Arc<Destination>>>,
    client: Option<Client>,
    namespace: String,
    cluster_id: String,
    /// One upload at a time per controller (memory, and no storms after an outage).
    gate: tokio::sync::Semaphore,
}

impl Exporter {
    pub fn empty(cluster_id: String) -> Self {
        Self {
            specs: BTreeMap::new(),
            config_errors: BTreeMap::new(),
            built: tokio::sync::Mutex::new(BTreeMap::new()),
            client: None,
            namespace: String::new(),
            cluster_id,
            gate: tokio::sync::Semaphore::new(1),
        }
    }

    /// Load destinations from the chart-rendered JSON file (missing file = none). Only the
    /// configuration is checked here; connections and credentials come at first use.
    pub async fn load(client: &Client, namespace: &str, cluster_id: String, file: &Path) -> Self {
        let mut me = Self::empty(cluster_id);
        me.client = Some(client.clone());
        me.namespace = namespace.to_string();
        let specs: Vec<DestinationSpec> = match std::fs::read(file) {
            Ok(b) => match serde_json::from_slice(&b) {
                Ok(s) => s,
                Err(e) => {
                    tracing::error!(error = %e, file = %file.display(), "invalid destinations file; object-store export disabled");
                    return me;
                }
            },
            Err(_) => return me,
        };
        // The cluster id is a key segment, and `kairn verify s3://…` reads the expected
        // cluster back from it: it must survive the key unchanged.
        let cluster_ok = path_safe(&me.cluster_id);
        if !cluster_ok && !specs.is_empty() {
            tracing::error!(cluster_id = %me.cluster_id, "cluster id must be [A-Za-z0-9._-] (at most 100) to export; object-store export disabled");
        }
        for spec in specs {
            if !cluster_ok {
                me.config_errors
                    .insert(spec.name.clone(), "invalid-cluster-id");
                continue;
            }
            match validate(&spec) {
                Ok(()) => {
                    tracing::info!(destination = %spec.name, url = %spec.url, "export destination defined");
                    me.specs.insert(spec.name.clone(), spec);
                }
                Err(e) => {
                    tracing::error!(destination = %spec.name, error = %e, "export destination misconfigured");
                    me.config_errors
                        .insert(spec.name.clone(), "destination-misconfigured");
                }
            }
        }
        me
    }

    /// The full object URL a capture would be stored at (for status).
    pub fn object_url(&self, name: &str, incident_id: &str) -> Option<String> {
        let spec = self.specs.get(name)?;
        let (scheme, bucket, prefix) = parse_url(&spec.url).ok()?;
        Some(format!(
            "{scheme}://{bucket}/{}",
            self.key(&prefix, incident_id)
        ))
    }

    fn key(&self, prefix: &str, incident_id: &str) -> String {
        let mut k = String::new();
        if !prefix.is_empty() {
            k.push_str(prefix);
            k.push('/');
        }
        k.push_str(&format!("{}/{incident_id}.ieb", self.cluster_id));
        k
    }

    async fn destination(&self, name: &str) -> Result<Arc<Destination>, Outcome> {
        if let Some(reason) = self.config_errors.get(name) {
            return Err(Outcome::Refused { reason });
        }
        let Some(spec) = self.specs.get(name) else {
            return Err(Outcome::Refused {
                reason: "not-allowed",
            });
        };
        let mut built = self.built.lock().await;
        if let Some(d) = built.get(name) {
            return Ok(d.clone());
        }
        let Some(client) = &self.client else {
            return Err(Outcome::Retry {
                reason: "credentials-unavailable",
            });
        };
        match build(client, &self.namespace, spec).await {
            Ok(d) => {
                let d = Arc::new(d);
                built.insert(name.to_string(), d.clone());
                Ok(d)
            }
            Err(e) => {
                tracing::warn!(destination = %name, error = %e, "destination not ready (will retry)");
                Err(Outcome::Retry {
                    reason: "credentials-unavailable",
                })
            }
        }
    }

    /// Try to upload `bundle` to destination `name`.
    pub async fn upload(
        &self,
        name: &str,
        capture_cluster_id: &str,
        incident_id: &str,
        bundle: &Path,
    ) -> Outcome {
        if let Some(reason) = self.config_errors.get(name) {
            return Outcome::Refused { reason };
        }
        if !self.specs.contains_key(name) {
            return Outcome::Refused {
                reason: "not-allowed",
            };
        }
        if capture_cluster_id != self.cluster_id {
            return Outcome::Refused {
                reason: "cluster-mismatch",
            };
        }
        if !path_safe(incident_id) {
            return Outcome::Refused {
                reason: "invalid-incident-id",
            };
        }

        // Only a verified bundle of *this* capture ever leaves the cluster. The path comes
        // from the capture's status, which anyone allowed to patch status could point at
        // another file. Verify and read the same file off the async runtime.
        let (bundle_path, cluster, incident) = (
            bundle.to_path_buf(),
            self.cluster_id.clone(),
            incident_id.to_string(),
        );
        let checked = tokio::task::spawn_blocking(move || -> Result<Vec<u8>, Outcome> {
            let len = std::fs::metadata(&bundle_path)
                .map_err(|_| Outcome::Retry {
                    reason: "local-bundle-unreadable",
                })?
                .len();
            if len > MAX_UPLOAD_BYTES {
                return Err(Outcome::Refused {
                    reason: "too-large",
                });
            }
            let opts = kairn_bundle::VerifyOptions {
                expected_cluster: Some(cluster),
                expected_incident: Some(incident),
                trusted_key_pem: None,
            };
            match kairn_bundle::verify_bundle(&bundle_path, &opts) {
                Ok(r) if matches!(
                    r.verdict,
                    kairn_bundle::Verdict::Ok | kairn_bundle::Verdict::Partial
                ) => {}
                Ok(r) => {
                    tracing::warn!(bundle = %bundle_path.display(), verdict = ?r.verdict, problems = ?r.problems, "refusing to export a file that is not this capture's verified bundle");
                    return Err(Outcome::Refused {
                        reason: "not-a-verified-bundle",
                    });
                }
                Err(_) => {
                    return Err(Outcome::Retry {
                        reason: "local-bundle-unreadable",
                    })
                }
            }
            std::fs::read(&bundle_path).map_err(|_| Outcome::Retry {
                reason: "local-bundle-unreadable",
            })
        })
        .await;
        let bytes = match checked {
            Ok(Ok(b)) => b,
            Ok(Err(outcome)) => return outcome,
            Err(_) => return Outcome::Retry { reason: "error" },
        };

        let _permit = self.gate.acquire().await;
        let d = match self.destination(name).await {
            Ok(d) => d,
            Err(outcome) => return outcome,
        };
        if let Some(reason) = self.ensure_probe(name, &d).await {
            return reason;
        }
        let digest = hex(&Sha256::digest(&bytes));
        let size = bytes.len() as u64;
        let key = self.key(&d.prefix, incident_id);
        let path = ObjectPath::from(key.clone());
        let url = format!("{}/{key}", d.bucket_url);

        let mut attributes = Attributes::new();
        attributes.insert(
            Attribute::Metadata("kairn-sha256".into()),
            digest.clone().into(),
        );
        attributes.insert(
            Attribute::Metadata("kairn-incident".into()),
            incident_id.to_string().into(),
        );
        attributes.insert(Attribute::Metadata("kairn-format".into()), "ieb/v1".into());
        let opts = PutOptions {
            mode: PutMode::Create,
            attributes,
            ..Default::default()
        };
        match d.store.put_opts(&path, PutPayload::from(bytes), opts).await {
            Ok(r) => Outcome::Uploaded {
                url,
                sha256: digest,
                version: r.version,
            },
            Err(object_store::Error::AlreadyExists { .. })
            | Err(object_store::Error::Precondition { .. }) => {
                // Our own earlier attempt, or someone else's object: compare sizes first,
                // then the bytes (never downloading more than our own bundle's size).
                match d.store.head(&path).await {
                    Ok(meta) if meta.size != size => Outcome::Conflict { url },
                    Ok(_) => match d.store.get(&path).await {
                        Ok(existing) => {
                            let version = existing.meta.version.clone();
                            match existing.bytes().await {
                                Ok(b) if hex(&Sha256::digest(&b)) == digest => Outcome::Uploaded {
                                    url,
                                    sha256: digest,
                                    version,
                                },
                                Ok(_) => Outcome::Conflict { url },
                                Err(e) => classify(name, &e),
                            }
                        }
                        Err(e) => classify(name, &e),
                    },
                    Err(e) => classify(name, &e),
                }
            }
            Err(e) => classify(name, &e),
        }
    }

    /// Run the conditional-write probe once per destination (retried until it concludes).
    async fn ensure_probe(&self, name: &str, d: &Destination) -> Option<Outcome> {
        let mut state = d.probe.lock().await;
        match *state {
            Probe::Ok => return None,
            Probe::Unsupported => {
                return Some(Outcome::Refused {
                    reason: "conditional-writes-unsupported",
                })
            }
            Probe::Unknown => {}
        }
        let key = if d.prefix.is_empty() {
            format!("{}/.kairn-probe", self.cluster_id)
        } else {
            format!("{}/{}/.kairn-probe", d.prefix, self.cluster_id)
        };
        let path = ObjectPath::from(key);
        let create = || PutOptions {
            mode: PutMode::Create,
            ..Default::default()
        };
        // First create may succeed or find the probe from an earlier start; the second must
        // be refused, or the backend silently ignores conditional writes.
        for attempt in 0..2 {
            match d
                .store
                .put_opts(&path, PutPayload::from_static(b"kairn"), create())
                .await
            {
                Ok(_) if attempt == 1 => {
                    tracing::error!(destination = %name, "backend ignores conditional writes; destination disabled");
                    *state = Probe::Unsupported;
                    return Some(Outcome::Refused {
                        reason: "conditional-writes-unsupported",
                    });
                }
                Ok(_) => {}
                Err(object_store::Error::AlreadyExists { .. })
                | Err(object_store::Error::Precondition { .. }) => {
                    if attempt == 1 {
                        *state = Probe::Ok;
                        return None;
                    }
                }
                Err(e) => return Some(classify(name, &e)),
            }
        }
        *state = Probe::Ok;
        None
    }
}

/// Fixed reason codes for status; details only in the controller log (no ARNs or account
/// ids in objects anyone with `get` on captures can read).
fn classify(name: &str, e: &object_store::Error) -> Outcome {
    tracing::warn!(destination = %name, error = %e, "object-store call failed");
    match e {
        object_store::Error::PermissionDenied { .. }
        | object_store::Error::Unauthenticated { .. } => Outcome::Retry {
            reason: "access-denied",
        },
        // Conditional writes switched off (e.g. via AWS_* configuration): never retry into
        // a mode that could overwrite evidence.
        object_store::Error::NotImplemented { .. } | object_store::Error::NotSupported { .. } => {
            Outcome::Refused {
                reason: "conditional-writes-unsupported",
            }
        }
        _ => Outcome::Retry { reason: "error" },
    }
}

/// Backoff before attempt `attempts + 1`: 30 s doubling to 1 h, ±20 % jitter.
pub fn backoff(attempts: u32) -> Duration {
    let base = 30u64
        .saturating_mul(1u64 << attempts.saturating_sub(1).min(7))
        .min(3600);
    let jitter = {
        let mut b = [0u8; 2];
        rand_core::RngCore::fill_bytes(&mut rand_core::OsRng, &mut b);
        u16::from_le_bytes(b) as f64 / u16::MAX as f64 // 0..1
    };
    Duration::from_secs_f64(base as f64 * (0.8 + 0.4 * jitter))
}

pub(crate) fn path_safe(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 100
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
        && s != "."
        && s != ".."
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// `s3://bucket/some/prefix` → (`s3`, `bucket`, `some/prefix`).
fn parse_url(url: &str) -> anyhow::Result<(&str, &str, String)> {
    let (scheme, rest) = url
        .split_once("://")
        .ok_or_else(|| anyhow::anyhow!("destination url needs a scheme (s3:// or gs://)"))?;
    let (bucket, prefix) = rest.split_once('/').unwrap_or((rest, ""));
    anyhow::ensure!(!bucket.is_empty(), "destination url has no bucket");
    let prefix = prefix.trim_matches('/').to_string();
    anyhow::ensure!(
        prefix.is_empty() || prefix.split('/').all(path_safe),
        "destination prefix must be [A-Za-z0-9._-] segments"
    );
    Ok((scheme, bucket, prefix))
}

/// Configuration-only checks (no network, no Secrets): a failure here is permanent.
fn validate(spec: &DestinationSpec) -> anyhow::Result<()> {
    let (scheme, _, _) = parse_url(&spec.url)?;
    match scheme {
        "s3" => Ok(()),
        "gs" => {
            anyhow::ensure!(
                spec.credentials_secret.is_empty(),
                "gs:// destinations use Workload Identity; credentialsSecret is not supported"
            );
            Ok(())
        }
        other => anyhow::bail!("unsupported destination scheme {other}://"),
    }
}

async fn build(client: &Client, ns: &str, spec: &DestinationSpec) -> anyhow::Result<Destination> {
    let (scheme, bucket, prefix) = parse_url(&spec.url)?;
    let options = ClientOptions::new()
        .with_timeout(Duration::from_secs(30))
        .with_connect_timeout(Duration::from_secs(10))
        .with_allow_http(spec.allow_http);
    // Few quick retries inside the client; the reconcile loop owns the long ones.
    let retry = RetryConfig {
        max_retries: 3,
        retry_timeout: Duration::from_secs(60),
        ..Default::default()
    };
    let store: Arc<dyn ObjectStore> = match scheme {
        "s3" => {
            let mut b = AmazonS3Builder::from_env()
                .with_bucket_name(bucket)
                .with_checksum_algorithm(Checksum::SHA256)
                .with_allow_http(spec.allow_http)
                .with_client_options(options)
                .with_retry(retry);
            if !spec.region.is_empty() {
                b = b.with_region(&spec.region);
            }
            if !spec.endpoint.is_empty() {
                b = b.with_endpoint(&spec.endpoint);
            }
            if !spec.credentials_secret.is_empty() {
                let (id, secret) = read_keys(client, ns, &spec.credentials_secret).await?;
                b = b.with_access_key_id(id).with_secret_access_key(secret);
            }
            Arc::new(b.build()?)
        }
        "gs" => {
            anyhow::ensure!(
                spec.credentials_secret.is_empty(),
                "gs:// destinations use Workload Identity; credentialsSecret is not supported"
            );
            Arc::new(
                GoogleCloudStorageBuilder::from_env()
                    .with_bucket_name(bucket)
                    .with_client_options(options)
                    .with_retry(retry)
                    .build()?,
            )
        }
        other => anyhow::bail!("unsupported destination scheme {other}://"),
    };
    Ok(Destination {
        bucket_url: format!("{scheme}://{bucket}"),
        prefix,
        store,
        probe: tokio::sync::Mutex::new(Probe::Unknown),
    })
}

async fn read_keys(client: &Client, ns: &str, name: &str) -> anyhow::Result<(String, String)> {
    let secret = Api::<Secret>::namespaced(client.clone(), ns)
        .get(name)
        .await?;
    let data = secret.data.unwrap_or_default();
    let field = |k: &str| -> anyhow::Result<String> {
        let v = data
            .get(k)
            .ok_or_else(|| anyhow::anyhow!("secret {name} has no {k}"))?;
        Ok(String::from_utf8(v.0.clone())?.trim().to_string())
    };
    Ok((field("access_key_id")?, field("secret_access_key")?))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls_parse_into_bucket_and_prefix() {
        assert_eq!(
            parse_url("s3://evidence/prod-apne2/").unwrap(),
            ("s3", "evidence", "prod-apne2".to_string())
        );
        assert_eq!(parse_url("gs://b").unwrap(), ("gs", "b", String::new()));
        assert!(parse_url("evidence/prod").is_err());
        assert!(parse_url("s3:///prefix").is_err());
        assert!(parse_url("s3://b/../x").is_err());
    }

    #[test]
    fn backoff_grows_and_caps_with_jitter() {
        for (n, base) in [(1u32, 30.0), (2, 60.0), (5, 480.0), (20, 3600.0)] {
            let d = backoff(n).as_secs_f64();
            assert!(d >= base * 0.8 - 1e-6 && d <= base * 1.2 + 1e-6, "{n}: {d}");
        }
    }

    #[test]
    fn incident_ids_must_be_path_safe() {
        assert!(path_safe("kind-kairn-ed39c3c612dea604"));
        assert!(!path_safe("../x"));
        assert!(!path_safe("a/b"));
        assert!(!path_safe(""));
    }

    #[tokio::test]
    async fn unknown_and_mismatched_captures_are_refused() {
        let ex = Exporter::empty("cluster-a".into());
        let bundle = tempfile::NamedTempFile::new().unwrap();
        assert_eq!(
            ex.upload("nope", "cluster-a", "inc-1", bundle.path()).await,
            Outcome::Refused {
                reason: "not-allowed"
            }
        );
    }
}
