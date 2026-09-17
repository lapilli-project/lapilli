//! Sealing: turn a directory of collected files into a manifested (and optionally signed)
//! Incident Evidence Bundle. Called by the controller's sealer stage.

use std::path::Path;

use crate::hashtree::{HashTree, MANIFEST_FILE, SIGNATURE_DIR};
use crate::manifest::{Coverage, IncidentIdentity, Manifest, Producer, Timing, SCHEMA_VERSION};
use crate::sign::Signer;
use crate::BundleError;

/// Everything needed to seal a bundle except the hash tree (computed here).
pub struct SealInput {
    pub incident: IncidentIdentity,
    pub producer: Producer,
    pub coverage: Coverage,
    pub timing: Timing,
}

/// File names written under `signature/` when signing is enabled.
pub const SIG_FILE: &str = "manifest.sig";
pub const PUBKEY_FILE: &str = "cosign.pub";

/// Seal the bundle rooted at `dir`: compute the content hash tree, write `manifest.json`,
/// and — if a signer is provided — write `signature/manifest.sig` (base64 DER) and
/// `signature/cosign.pub` (SPKI PEM). Returns the manifest.
pub fn seal_dir(
    dir: &Path,
    input: SealInput,
    signer: Option<&dyn Signer>,
) -> Result<Manifest, BundleError> {
    let hash_tree = HashTree::from_dir(dir)?;
    let manifest = Manifest {
        schema_version: SCHEMA_VERSION.to_string(),
        incident: input.incident,
        producer: input.producer,
        hash_tree,
        coverage: input.coverage,
        timing: input.timing,
    };

    // The literal bytes we write are also the bytes we sign (cosign blob model).
    let bytes = manifest.to_signing_bytes()?;
    std::fs::write(dir.join(MANIFEST_FILE), &bytes)?;

    if let Some(signer) = signer {
        let sig = signer.sign_b64(&bytes)?;
        let pubkey = signer.public_key_pem()?;
        let sigdir = dir.join(SIGNATURE_DIR);
        std::fs::create_dir_all(&sigdir)?;
        std::fs::write(sigdir.join(SIG_FILE), sig)?;
        std::fs::write(sigdir.join(PUBKEY_FILE), pubkey)?;
    }

    Ok(manifest)
}
