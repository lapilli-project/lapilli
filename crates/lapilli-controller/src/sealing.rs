//! KMS signing as its own phase (docs/design-kms.md): collect once, then sign and seal with
//! retries, across restarts, without collecting again. Fail closed: with KMS configured,
//! no bundle is ever written unsigned or signed by anything but the pinned key.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use lapilli_bundle::manifest::{Manifest, SigningDecl, ALG_ECDSA_P256_SHA256};
use lapilli_bundle::{attach_signature, prepare_seal, HashTree, SealInput};
use lapilli_kms::{KmsError, KmsKey, KmsSigner};
use serde::{Deserialize, Serialize};

use crate::crd::IncidentCapture;

/// The controller's KMS key, connected (and its public key pinned) at first success.
pub struct Kms {
    pub key: KmsKey,
    signer: tokio::sync::Mutex<Option<Arc<KmsSigner>>>,
}

impl Kms {
    pub fn new(key: KmsKey) -> Self {
        Self {
            key,
            signer: tokio::sync::Mutex::new(None),
        }
    }

    /// The pinned signer; connects (preflight) on first use. Once pinned, the public key
    /// never changes for the life of the process.
    pub async fn signer(&self) -> Result<Arc<KmsSigner>, KmsError> {
        let mut pinned = self.signer.lock().await;
        if let Some(s) = pinned.as_ref() {
            return Ok(s.clone());
        }
        let s = Arc::new(KmsSigner::connect(self.key.clone()).await?);
        tracing::info!(key = %self.key.name(), key_id = %s.key_id(), endpoint = %s.endpoint(),
            "KMS signing key pinned (give auditors this key_id and `lapilli key fetch --kms`)");
        crate::telemetry::metrics().signing_key_pinned(s.key_id());
        *pinned = Some(s.clone());
        Ok(s)
    }

    /// Preflight at startup, retried until it succeeds. Never gates readiness: captures
    /// keep being collected and wait in `Sealing` meanwhile.
    pub fn spawn_preflight(self: &Arc<Self>) {
        if let Some(e) = lapilli_kms::endpoint_override(&self.key) {
            tracing::warn!(endpoint = %e, "KMS endpoint override in use (test or private endpoint)");
        }
        let kms = self.clone();
        tokio::spawn(async move {
            let mut attempt = 0u32;
            loop {
                match kms.signer().await {
                    Ok(_) => return,
                    Err(e) => {
                        attempt += 1;
                        tracing::error!(key = %kms.key.name(), reason = e.kind.reason(), error = %e,
                            "KMS preflight failed; captures will wait in Sealing until it succeeds");
                        tokio::time::sleep(crate::export::backoff(attempt)).await;
                    }
                }
            }
        });
    }
}

/// What collection recorded for sealing later: the manifest input and the hash tree of the
/// collected files. Kept next to the staging directory (outside the hash tree).
#[derive(Serialize, Deserialize)]
pub struct SavedSeal {
    pub input: SealInput,
    pub tree: HashTree,
    pub destinations: Vec<String>,
}

pub fn staging_dir(root: &Path, ic: &IncidentCapture, uid: &str) -> PathBuf {
    root.join(format!(".staging-{}-{uid}", ic.spec.incident_id))
}

pub fn seal_file(stage: &Path) -> PathBuf {
    let mut name = stage.as_os_str().to_owned();
    name.push(".seal.json");
    PathBuf::from(name)
}

/// The destinations recorded at collection, if the seal file is still there.
pub fn saved_destinations(stage: &Path) -> Option<Vec<String>> {
    let bytes = std::fs::read(seal_file(stage)).ok()?;
    serde_json::from_slice::<SavedSeal>(&bytes)
        .ok()
        .map(|s| s.destinations)
}

pub fn save(stage: &Path, saved: &SavedSeal) -> Result<(), String> {
    let bytes = serde_json::to_vec(saved).map_err(|e| e.to_string())?;
    std::fs::write(seal_file(stage), bytes).map_err(|e| e.to_string())
}

/// Why an attempt did not produce a bundle.
pub enum SealError {
    /// Retry later (the KMS or the network); counts against the attempt budget.
    Kms(KmsError),
    /// Retry later for a local reason (packing): a fixed reason code and details.
    Retry(&'static str, String),
    /// Final: the staged data is gone or inconsistent, or it isn't this capture's.
    Fatal(String),
}

/// A signed bundle and what matches it to the cloud's audit log.
pub struct Sealed {
    pub destinations: Vec<String>,
    /// From collection to this seal, and whether some intended collector didn't run.
    pub capture_seconds: f64,
    pub partial: bool,
    pub key_id: String,
    pub manifest_sha256: String,
    pub request_id: Option<String>,
}

/// One signing attempt: verify the staged data is what was collected and is this
/// capture's, sign the manifest with KMS, check the signature, attach it. Packing is the
/// caller's.
pub async fn attempt(
    kms: &Kms,
    stage: &Path,
    ic: &IncidentCapture,
    cluster_id: &str,
) -> Result<Sealed, SealError> {
    let fatal = |m: String| SealError::Fatal(m);
    if !stage.is_dir() {
        return Err(fatal(format!("staging-lost: {} is gone", stage.display())));
    }
    let saved: SavedSeal = std::fs::read(seal_file(stage))
        .map_err(|e| fatal(format!("staging-lost: {e}")))
        .and_then(|b| {
            serde_json::from_slice(&b).map_err(|e| fatal(format!("staging-lost: {e}")))
        })?;
    // Only this controller's cluster and this capture's incident, whatever the file says.
    let incident = &saved.input.incident;
    if incident.cluster_id != cluster_id || incident.id != ic.spec.incident_id {
        // The two values on the left come from the seal FILE, so they are whatever wrote the
        // volume — and this check runs before the hash-tree check below, so a tamperer reaches
        // it. Bound each component, not the assembled string: the expected ids on the right are
        // the useful half, and a single huge left-hand value would crowd them out of a message
        // that is then truncated as a whole (`docs/design-status-message.md`).
        const ID: usize = crate::crd::DETAIL_MAX / 4;
        return Err(fatal(format!(
            "staging-mismatch: staged data is for {}/{}, not {cluster_id}/{}",
            crate::crd::bounded(&incident.cluster_id, ID),
            crate::crd::bounded(&incident.id, ID),
            ic.spec.incident_id
        )));
    }
    // The files must be exactly what was collected.
    let now = HashTree::from_dir(stage).map_err(|e| fatal(format!("staging-lost: {e}")))?;
    if now.root != saved.tree.root || now.files != saved.tree.files {
        return Err(fatal(
            "staging-modified: the staged files changed after collection".into(),
        ));
    }
    let signer = kms.signer().await.map_err(SealError::Kms)?;
    // Deterministic: the same input and tree give the same manifest bytes on every attempt
    // (timing was fixed at collection), declaring the pinned key.
    let (manifest, bytes): (Manifest, Vec<u8>) = prepare_seal(
        stage,
        saved.input,
        Some(SigningDecl {
            alg: ALG_ECDSA_P256_SHA256.to_string(),
            key_id: signer.key_id().to_string(),
        }),
    )
    .map_err(|e| fatal(format!("staging-lost: {e}")))?;
    if manifest.hash_tree.root != saved.tree.root {
        return Err(fatal(
            "staging-modified: the staged files changed after collection".into(),
        ));
    }
    let capture_seconds = manifest.timing.capture_to_seal_ms as f64 / 1000.0;
    let partial = manifest.coverage.is_partial();
    // Count the KMS call itself, so the series can be reconciled against the cloud's
    // audit log (packing failures are a separate counter).
    let signed = signer.sign_manifest(&bytes).await;
    crate::telemetry::metrics().seal_attempt(signed.is_ok());
    let signed = signed.map_err(SealError::Kms)?;
    attach_signature(stage, &signed.signature_b64, signer.public_key_pem())
        .map_err(|e| fatal(format!("staging-lost: {e}")))?;
    Ok(Sealed {
        destinations: saved.destinations,
        capture_seconds,
        partial,
        key_id: signer.key_id().to_string(),
        manifest_sha256: signed.digest_hex,
        request_id: signed.request_id,
    })
}
