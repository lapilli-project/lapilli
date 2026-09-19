//! Sealing: turn a directory of collected files into a manifested (and optionally signed)
//! Incident Evidence Bundle. Called by the controller's sealer stage.

use std::path::Path;

use crate::hashtree::{HashTree, MANIFEST_FILE, SIGNATURE_DIR};
use crate::manifest::{
    Coverage, IncidentIdentity, Manifest, Producer, SigningDecl, Timing, ALG_ECDSA_P256_SHA256,
    SCHEMA_VERSION,
};
use crate::sign::Signer;
use crate::BundleError;

/// Everything needed to seal a bundle except the hash tree (computed here). Serializable so
/// a seal can be prepared after a restart from what was recorded at collection time.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
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
    let signing = match signer {
        Some(s) => Some(SigningDecl {
            alg: ALG_ECDSA_P256_SHA256.to_string(),
            key_id: s.key_id()?,
        }),
        None => None,
    };
    let (manifest, bytes) = prepare_seal(dir, input, signing)?;
    if let Some(signer) = signer {
        attach_signature(dir, &signer.sign_b64(&bytes)?, &signer.public_key_pem()?)?;
    }
    Ok(manifest)
}

/// First half of sealing: compute the hash tree and write `manifest.json` (declaring
/// `signing`, if any). Returns the manifest and its exact bytes, which are what a signer
/// must sign. For signers that can't sign synchronously (a KMS), followed later by
/// [`attach_signature`].
pub fn prepare_seal(
    dir: &Path,
    input: SealInput,
    signing: Option<SigningDecl>,
) -> Result<(Manifest, Vec<u8>), BundleError> {
    let hash_tree = HashTree::from_dir(dir)?;
    let manifest = Manifest {
        schema_version: SCHEMA_VERSION.to_string(),
        incident: input.incident,
        producer: input.producer,
        signing,
        hash_tree,
        coverage: input.coverage,
        timing: input.timing,
    };
    // The literal bytes we write are also the bytes we sign (cosign blob model).
    let bytes = manifest.to_signing_bytes()?;
    std::fs::write(dir.join(MANIFEST_FILE), &bytes)?;
    Ok((manifest, bytes))
}

/// Second half of sealing: write `signature/manifest.sig` (base64 DER) and
/// `signature/cosign.pub` (SPKI PEM).
pub fn attach_signature(
    dir: &Path,
    sig_b64: &str,
    public_key_pem: &str,
) -> Result<(), BundleError> {
    let sigdir = dir.join(SIGNATURE_DIR);
    std::fs::create_dir_all(&sigdir)?;
    std::fs::write(sigdir.join(SIG_FILE), sig_b64)?;
    std::fs::write(sigdir.join(PUBKEY_FILE), public_key_pem)?;
    Ok(())
}
