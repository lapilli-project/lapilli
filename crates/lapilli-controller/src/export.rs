//! Object-store export ([`docs/design-export.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/design-export.md)): copy each sealed `.ieb` to admin-defined
//! destinations, once, never overwriting.
//!
//! - Destinations come only from the controller's configuration (the chart); a
//!   `CaptureProfile` can reference one by name and alter nothing about it.
//! - An S3-compatible `endpoint` is parsed by [`lapilli_net`], like every other endpoint the
//!   controller talks to, and a destination that fails it is refused permanently
//!   (`destination-misconfigured`): see [`check_endpoint`], and [`build`] for the two of that
//!   crate's rules an `object_store` client cannot carry.
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
    /// that the object they verify is the one Lapilli wrote.
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

/// A 64-character lowercase hex key id, which is what `archive_public_key` names its files.
fn is_key_id(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
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
    /// `(destination, key_id)` pairs already archived there, so this costs one PUT per key and
    /// nothing afterwards. In memory only: a restart re-checks, which a create makes cheap.
    keys_copied: tokio::sync::Mutex<std::collections::HashSet<(String, String)>>,
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
            keys_copied: Default::default(),
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
        // The cluster id is a key segment, and `lapilli verify s3://…` reads the expected
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
        // another file. Read the bytes **once**, off the async runtime, and verify those very
        // bytes: verifying the path and then re-reading it would leave a window in which a
        // writer on the shared volume swaps the file between the two reads, and the thing that
        // left the cluster would not be the thing that was checked.
        let (bundle_path, cluster, incident) = (
            bundle.to_path_buf(),
            self.cluster_id.clone(),
            incident_id.to_string(),
        );
        let checked = tokio::task::spawn_blocking(move || -> Result<Vec<u8>, Outcome> {
            use std::io::Read as _;
            let unreadable = || Outcome::Retry {
                reason: "local-bundle-unreadable",
            };
            let too_large = || Outcome::Refused {
                reason: "too-large",
            };
            let file = std::fs::File::open(&bundle_path).map_err(|_| unreadable())?;
            let len = file.metadata().map_err(|_| unreadable())?.len();
            if len > MAX_UPLOAD_BYTES {
                return Err(too_large());
            }
            // `take` and not the stat alone: the file may grow between the two, and `too-large`
            // has to stay a bound on how much of it is in memory. One byte over the limit is
            // enough to tell that it is over.
            let mut bytes = Vec::with_capacity(len as usize);
            file.take(MAX_UPLOAD_BYTES + 1)
                .read_to_end(&mut bytes)
                .map_err(|_| unreadable())?;
            if bytes.len() as u64 > MAX_UPLOAD_BYTES {
                return Err(too_large());
            }
            let opts = lapilli_bundle::VerifyOptions {
                expected_cluster: Some(cluster),
                expected_incident: Some(incident),
                trusted_key_pem: None,
            };
            // A cursor over the buffer, so the verdict is about the bytes being uploaded. An
            // `.ieb` is a stream to `verify_reader`; the export path only ever has the sealed
            // file, never an unpacked directory (reading one fails above, as it did before).
            let report = lapilli_bundle::verify_reader(std::io::Cursor::new(&bytes), &opts);
            if !matches!(
                report.verdict,
                lapilli_bundle::Verdict::Ok | lapilli_bundle::Verdict::Partial
            ) {
                tracing::warn!(bundle = %bundle_path.display(), verdict = ?report.verdict, problems = ?report.problems, "refusing to export a file that is not this capture's verified bundle");
                return Err(Outcome::Refused {
                    reason: "not-a-verified-bundle",
                });
            }
            Ok(bytes)
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
            Attribute::Metadata("lapilli-sha256".into()),
            digest.clone().into(),
        );
        attributes.insert(
            Attribute::Metadata("lapilli-incident".into()),
            incident_id.to_string().into(),
        );
        attributes.insert(
            Attribute::Metadata("lapilli-format".into()),
            "ieb/v1".into(),
        );
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

    /// Copy the locally archived signing keys to a destination, beside the bundles they signed.
    ///
    /// The PVC dies with the cluster; the bucket is what outlives it, so an archive that only
    /// exists locally does not keep the promise that evidence outlives its cluster. Each object is
    /// named by its own key id, so the name and the content check each other, and a create that
    /// finds the same bytes already there is success.
    ///
    /// Idempotent and remembered per destination, so this costs one PUT the first time a key is
    /// seen and nothing afterwards. A failure is logged and dropped: it must never fail a bundle's
    /// export, which is the thing that actually matters.
    pub async fn copy_archived_keys(&self, name: &str, bundle_root: &Path) {
        let dir = bundle_root.join("keys");
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return; // nothing signed yet
        };
        for entry in entries.flatten() {
            let path = entry.path();
            let Some(id) = path
                .file_stem()
                .and_then(|s| s.to_str())
                .filter(|s| is_key_id(s))
            else {
                continue; // not one of ours
            };
            if path.extension().and_then(|e| e.to_str()) != Some("pub") {
                continue;
            }
            {
                let done = self.keys_copied.lock().await;
                if done.contains(&(name.to_string(), id.to_string())) {
                    continue;
                }
            }
            let Ok(pem) = std::fs::read(&path) else {
                continue;
            };
            match self.put_key(name, id, pem).await {
                Ok(url) => {
                    self.keys_copied
                        .lock()
                        .await
                        .insert((name.to_string(), id.to_string()));
                    tracing::info!(destination = %name, key_id = %id, %url,
                                   "archived the signing key beside the exported bundles");
                }
                Err(why) => tracing::warn!(destination = %name, key_id = %id, %why,
                    "could not archive the signing key at the destination; bundles are unaffected"),
            }
        }
    }

    /// One conditional create of `<prefix>/<cluster>/keys/<key_id>.pub`.
    async fn put_key(&self, name: &str, key_id: &str, pem: Vec<u8>) -> Result<String, String> {
        let _permit = self.gate.acquire().await;
        let d = self
            .destination(name)
            .await
            .map_err(|o| format!("destination unavailable: {o:?}"))?;
        let key = format!(
            "{}{}/keys/{key_id}.pub",
            if d.prefix.is_empty() {
                String::new()
            } else {
                format!("{}/", d.prefix)
            },
            self.cluster_id
        );
        let path = ObjectPath::from(key.clone());
        let url = format!("{}/{key}", d.bucket_url);
        let digest = hex(&Sha256::digest(&pem));
        let size = pem.len() as u64;
        let mut attributes = Attributes::new();
        attributes.insert(Attribute::Metadata("lapilli-sha256".into()), digest.into());
        attributes.insert(
            Attribute::Metadata("lapilli-key-id".into()),
            key_id.to_string().into(),
        );
        let opts = PutOptions {
            mode: PutMode::Create,
            attributes,
            ..Default::default()
        };
        match d.store.put_opts(&path, PutPayload::from(pem), opts).await {
            Ok(_) => Ok(url),
            // The name is the content's hash, so an object of the same size at this key is the
            // same key. A different size is somebody else's object and worth saying so.
            Err(object_store::Error::AlreadyExists { .. })
            | Err(object_store::Error::Precondition { .. }) => match d.store.head(&path).await {
                Ok(meta) if meta.size == size => Ok(url),
                Ok(_) => Err(format!("{url} already holds different bytes")),
                Err(e) => Err(e.to_string()),
            },
            Err(e) => Err(e.to_string()),
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
            format!("{}/.lapilli-probe", self.cluster_id)
        } else {
            format!("{}/{}/.lapilli-probe", d.prefix, self.cluster_id)
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
                .put_opts(&path, PutPayload::from_static(b"lapilli"), create())
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
    check_endpoint(spec)?;
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

/// An S3-compatible `endpoint` must pass the same parser every other configured endpoint does.
///
/// This is the destination the sealed bundle itself goes to — and, with `credentialsSecret`, the
/// host that sees those static keys. Before round 30 it went straight into
/// `AmazonS3Builder::with_endpoint` with nothing but `with_allow_http`, so [`docs/egress.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/egress.md)'s
/// claim that Lapilli "refuses link-local addresses … for every endpoint it parses" was true of
/// the notify and metrics paths and false of the one path that carries evidence out of the
/// cluster. [`lapilli_net::parse`] is that check: strict syntax (no whitespace, user info,
/// escapes, brackets or non-ASCII, so the host printed is the host contacted) and plain HTTP
/// only to a loopback or cluster-local name, and only when this destination asked for it with
/// `allowHttp`. Note that this is stricter than `allowHttp` alone was: `http://minio.minio`
/// (two labels, indistinguishable from a public name) is refused — write `minio.minio.svc`. A
/// link-local address written out as the endpoint is refused here too.
///
/// Only the **validating** half of `lapilli-net` is used. Its `client`/`connect` hand back a
/// `reqwest::Client`, and `object_store` builds its own client from [`ClientOptions`] and uses
/// it for the credential chain too, so there is no client here to replace: see the note in
/// [`build`] for what that costs and why the alternative is worse.
fn check_endpoint(spec: &DestinationSpec) -> anyhow::Result<()> {
    if spec.endpoint.is_empty() {
        return Ok(()); // the cloud's own endpoint, built by `object_store` and always HTTPS
    }
    // A path is left alone: `object_store` treats the endpoint as a base URL, so a gateway
    // published under `https://gw.example/s3` is a legitimate destination. The host is what
    // needed vetting.
    let endpoint = lapilli_net::parse(&spec.endpoint, spec.allow_http)
        .map_err(|e| anyhow::anyhow!("endpoint: {e}"))?;
    // `parse` judges a name; link-local is refused when something *resolves* the name
    // (`lapilli_net::resolve`), and nothing resolves here — `object_store`'s client connects, and
    // it cannot be given that check (see [`build`]). A literal address is the one case that can be
    // judged with no DNS, so judge it: an endpoint written as `https://169.254.169.254` or
    // `http://169.254.170.23` is a cloud metadata or credential agent, never an object store, and
    // a signed `PutObject` aimed at one is an exfil attempt or a serious mistake. A *name* that
    // resolves into that range is not covered.
    if let Ok(ip) = endpoint.host.parse::<std::net::IpAddr>() {
        let ip = match ip {
            std::net::IpAddr::V6(v6) => v6.to_ipv4_mapped().map_or(ip, std::net::IpAddr::V4),
            v4 => v4,
        };
        let link_local = match ip {
            std::net::IpAddr::V4(v4) => v4.is_link_local(),
            std::net::IpAddr::V6(v6) => (v6.segments()[0] & 0xffc0) == 0xfe80,
        };
        anyhow::ensure!(
            !link_local,
            "endpoint is a link-local address (cloud metadata and credential agents live there)"
        );
    }
    Ok(())
}

async fn build(client: &Client, ns: &str, spec: &DestinationSpec) -> anyhow::Result<Destination> {
    let (scheme, bucket, prefix) = parse_url(&spec.url)?;
    // The endpoint reached here has already been through [`check_endpoint`] (only validated
    // specs are kept in `Exporter::specs`), so the host is one `lapilli_net::parse` accepted and
    // plain HTTP is only in play for a loopback or cluster-local name.
    //
    // Two of `lapilli-net`'s rules cannot be carried over, and it is worth being exact about
    // which:
    //
    // - **Redirects.** `ClientOptions` (object_store 0.14) has no redirect knob — the only way
    //   to get `redirect::Policy::none()` is `with_http_connector`, i.e. building the
    //   `reqwest::Client` here instead. That client is also what the AWS credential chain uses,
    //   and `lapilli_net`'s client sets `https_only` from *our* endpoint's scheme, which would
    //   break EKS Pod Identity and GKE Workload Identity: their credential endpoints are plain
    //   HTTP on link-local (`docs/egress.md`). Reproducing `ClientOptions` by hand to avoid that
    //   (compression off, so `Content-Length` stays meaningful; timeouts; TLS) is a bigger and
    //   more fragile change than the exposure justifies, and object_store follows a 3xx on
    //   purpose for S3-compatible gateways. What limits it: `https_only` is on unless this
    //   destination allowed HTTP, so no redirect can downgrade to plaintext, and reqwest strips
    //   `Authorization` (the SigV4 signature) on a cross-host hop.
    // - **Address pinning / link-local refusal** (`lapilli_net::connect`). The available hook is
    //   `ClientOptions::with_dns_resolver`, but the credential chain shares this client and
    //   `metadata.google.internal` resolves to 169.254.169.254: refusing link-local here would
    //   break the credential modes `docs/kms.md` recommends.
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
        assert!(path_safe("kind-lapilli-ed39c3c612dea604"));
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

    fn spec(endpoint: &str, allow_http: bool) -> DestinationSpec {
        DestinationSpec {
            name: "evidence".into(),
            url: "s3://evidence/prod".into(),
            region: "us-east-1".into(),
            endpoint: endpoint.into(),
            allow_http,
            credentials_secret: String::new(),
        }
    }

    /// The destination the bundle (and any static credentials) actually goes to is parsed by
    /// `lapilli_net`, like every other configured endpoint.
    #[test]
    fn endpoints_go_through_lapilli_net() {
        // No endpoint: the cloud's own, HTTPS, built by object_store.
        assert!(validate(&spec("", false)).is_ok());
        assert!(validate(&spec("https://s3.example.com", false)).is_ok());
        // A path is a legitimate gateway prefix.
        assert!(validate(&spec("https://gw.example.com/s3", false)).is_ok());
        // Plaintext: only to a cluster-local or loopback name, and only when asked for.
        for local in [
            "http://localstack.s3.svc:4566",
            "http://minio.minio.svc.cluster.local:9000",
            "http://127.0.0.1:4566",
            "http://localhost:4566",
        ] {
            assert!(validate(&spec(local, true)).is_ok(), "refused {local}");
            assert!(
                validate(&spec(local, false)).is_err(),
                "accepted {local} without allowHttp"
            );
        }
        for bad in [
            // plaintext to something that is not cluster-local, even with allowHttp: this is the
            // exfil shape — a sealed bundle in the clear to a host a chart value named
            "http://evidence.attacker.example",
            // two labels are not a cluster-local name (`localstack.s3` could be public DNS)
            "http://localstack.s3:4566",
            // the shapes that fool a naive host check
            "https://127.0.0.1@attacker.example/",
            "https://user:pw@s3.example.com/",
            "https://s3.example.com\\@attacker.example/",
            "https://s3.example.com\t/",
            "https://[::1]:9000/",
            "https://s3.example.com:99999/",
            // not an http(s) endpoint at all
            "s3.example.com:9000",
            "file:///etc/passwd",
            // a literal metadata / credential-agent address is not an object store
            "https://169.254.169.254",
            "http://169.254.170.23/",
            "https://169.254.0.1:9000",
        ] {
            assert!(
                validate(&spec(bad, true)).is_err(),
                "accepted endpoint {bad}"
            );
        }
    }

    /// …and a destination that fails it is refused permanently, with the same closed-set reason
    /// the other configuration errors use.
    #[tokio::test]
    async fn a_misconfigured_endpoint_is_a_permanent_refusal() {
        let mut ex = Exporter::empty("cluster-a".into());
        // What `Exporter::load` records for a spec `validate` rejected.
        ex.config_errors
            .insert("evidence".into(), "destination-misconfigured");
        let bundle = tempfile::NamedTempFile::new().unwrap();
        assert_eq!(
            ex.upload("evidence", "cluster-a", "inc-1", bundle.path())
                .await,
            Outcome::Refused {
                reason: "destination-misconfigured"
            }
        );
    }

    /// The bytes that are verified are the bytes that would be uploaded: `upload` reads the file
    /// once and verifies that buffer, so nothing can swap the file in between. A file that is not
    /// this capture's sealed bundle is refused before any destination is touched.
    #[tokio::test]
    async fn only_bytes_that_verify_are_uploaded() {
        let mut ex = Exporter::empty("cluster-a".into());
        ex.specs.insert("evidence".into(), spec("", false));
        let mut bundle = tempfile::NamedTempFile::new().unwrap();
        std::io::Write::write_all(&mut bundle, b"not an ieb").unwrap();
        assert_eq!(
            ex.upload("evidence", "cluster-a", "inc-1", bundle.path())
                .await,
            Outcome::Refused {
                reason: "not-a-verified-bundle"
            }
        );
        // The size bound still settles before anything is read into memory.
        let big = tempfile::NamedTempFile::new().unwrap();
        big.as_file().set_len(MAX_UPLOAD_BYTES + 1).unwrap();
        assert_eq!(
            ex.upload("evidence", "cluster-a", "inc-1", big.path())
                .await,
            Outcome::Refused {
                reason: "too-large"
            }
        );
        // A path that is not there at all is transient, not a verdict about the bundle.
        assert_eq!(
            ex.upload(
                "evidence",
                "cluster-a",
                "inc-1",
                Path::new("/nonexistent/lapilli-test.ieb")
            )
            .await,
            Outcome::Retry {
                reason: "local-bundle-unreadable"
            }
        );
    }
}
