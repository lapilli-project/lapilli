//! Verification — the load-bearing integrity feature (works with or without a signature).
//!
//! `kairn verify` maps the [`Verdict`] to a process exit code: `Ok` → 0, `Partial` → non-zero,
//! `Failed` → non-zero. Context mismatch and hash-tree tampering are **fail-closed**.

use std::path::Path;

use crate::hashtree::MANIFEST_FILE;
use crate::hashtree::SIGNATURE_DIR;
use crate::manifest::Manifest;
use crate::seal::{PUBKEY_FILE, SIG_FILE};
use crate::sign::verify_b64;
use crate::BundleError;

/// What the caller asserts the bundle should be, plus the key it trusts.
#[derive(Debug, Default, Clone)]
pub struct VerifyOptions {
    /// If set, must equal `manifest.incident.cluster_id` (fail-closed on mismatch).
    pub expected_cluster: Option<String>,
    /// If set, must equal `manifest.incident.id` (fail-closed on mismatch).
    pub expected_incident: Option<String>,
    /// SPKI PEM public key the verifier obtained **out of band**. When set, the bundle must
    /// carry a signature that verifies against it. The public key embedded in the bundle is
    /// never trusted for authenticity: whoever can rewrite a bundle can also re-sign it and
    /// swap that key.
    pub trusted_key_pem: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    Ok,
    Partial,
    Failed,
}

impl Verdict {
    pub fn exit_code(self) -> i32 {
        match self {
            Verdict::Ok => 0,
            Verdict::Partial => 2,
            Verdict::Failed => 1,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SignatureStatus {
    /// No signature in the bundle.
    Absent,
    /// Signature verifies against the caller's trusted key: integrity + producer authenticity.
    Trusted,
    /// Signature present and self-consistent (checked against the embedded key, if any), but
    /// no trusted key was supplied — says nothing about *who* sealed the bundle.
    Unpinned,
    /// Signature does not verify.
    Invalid,
}

#[derive(Debug, Clone)]
pub struct VerifyReport {
    pub verdict: Verdict,
    pub hash_ok: bool,
    pub context_ok: bool,
    pub coverage_score: f64,
    pub partial: bool,
    pub signature: SignatureStatus,
    /// Human-readable problems (tampered/missing/extra files, context mismatch, ...).
    pub problems: Vec<String>,
}

/// Verify a bundle given a path that is either a sealed directory or a `.ieb` file.
/// A `.ieb` file is unpacked into a temp dir first. This is what `kairn verify` calls.
pub fn verify_bundle(path: &Path, opts: &VerifyOptions) -> Result<VerifyReport, BundleError> {
    if path.is_dir() {
        return verify_bundle_dir(path, opts);
    }
    // Treat as a packed .ieb: unpack into a temp dir, then verify.
    let tmp = tempdir()?;
    crate::pack::unpack(path, tmp.path())?;
    verify_bundle_dir(tmp.path(), opts)
}

/// Create a private temp directory for unpacking. Kept dependency-light (no `tempfile` in
/// the non-dev build): uses the OS temp dir with a pid/nanos-free unique-ish name derived
/// from the input, cleaned on drop.
fn tempdir() -> Result<TempDir, BundleError> {
    TempDir::new()
}

/// Minimal self-cleaning temp directory.
struct TempDir {
    path: std::path::PathBuf,
}

impl TempDir {
    fn new() -> Result<Self, BundleError> {
        let base = std::env::temp_dir();
        // Unique-ish name from a monotonic counter + address entropy; avoids extra deps.
        use std::sync::atomic::{AtomicU64, Ordering};
        static CTR: AtomicU64 = AtomicU64::new(0);
        let n = CTR.fetch_add(1, Ordering::Relaxed);
        let salt = &n as *const u64 as usize;
        let dir = base.join(format!("kairn-verify-{n}-{salt:x}"));
        std::fs::create_dir_all(&dir)?;
        Ok(Self { path: dir })
    }
    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// Verify an unpacked bundle directory.
pub fn verify_bundle_dir(dir: &Path, opts: &VerifyOptions) -> Result<VerifyReport, BundleError> {
    let mut problems = Vec::new();

    // Read the literal manifest bytes: used both for parsing and as the signature payload.
    let manifest_bytes = std::fs::read(dir.join(MANIFEST_FILE))?;
    let manifest: Manifest = serde_json::from_slice(&manifest_bytes)?;

    // 1) Hash tree — recompute over the directory and compare.
    let tree_problems = manifest.hash_tree.verify_against_dir(dir)?;
    let hash_ok = tree_problems.is_empty();
    problems.extend(tree_problems);

    // 2) Bound context — fail-closed on mismatch.
    let mut context_ok = true;
    if let Some(exp) = &opts.expected_cluster {
        if exp != &manifest.incident.cluster_id {
            context_ok = false;
            problems.push(format!(
                "cluster mismatch: expected {exp}, bundle {}",
                manifest.incident.cluster_id
            ));
        }
    }
    if let Some(exp) = &opts.expected_incident {
        if exp != &manifest.incident.id {
            context_ok = false;
            problems.push(format!(
                "incident mismatch: expected {exp}, bundle {}",
                manifest.incident.id
            ));
        }
    }

    // 3) Coverage.
    let coverage_score = manifest.coverage.score();
    let partial = manifest.coverage.is_partial();
    if partial {
        problems.push(format!(
            "PARTIAL capture: {}/{} collectors ran",
            manifest.coverage.collectors_run.len(),
            manifest.coverage.collectors_intended.len()
        ));
    }

    // 4) Signature over the literal manifest bytes. Authenticity comes only from a key the
    //    caller trusts; the embedded key is used at most as a corruption check.
    let sigdir = dir.join(SIGNATURE_DIR);
    let sig_path = sigdir.join(SIG_FILE);
    let pub_path = sigdir.join(PUBKEY_FILE);
    let sig_b64 = if sig_path.exists() {
        Some(std::fs::read_to_string(&sig_path)?)
    } else {
        None
    };
    let signature = match (&opts.trusted_key_pem, &sig_b64) {
        (Some(key), Some(sig)) => match verify_b64(key, &manifest_bytes, sig) {
            Ok(()) => SignatureStatus::Trusted,
            Err(e) => {
                problems.push(format!("not signed by the trusted key ({e})"));
                SignatureStatus::Invalid
            }
        },
        (Some(_), None) => {
            problems.push("a trusted key was given but the bundle is unsigned".to_string());
            SignatureStatus::Absent
        }
        (None, Some(sig)) if pub_path.exists() => {
            let embedded = std::fs::read_to_string(&pub_path)?;
            match verify_b64(&embedded, &manifest_bytes, sig) {
                Ok(()) => SignatureStatus::Unpinned,
                Err(e) => {
                    problems.push(format!("signature invalid: {e}"));
                    SignatureStatus::Invalid
                }
            }
        }
        (None, Some(_)) => SignatureStatus::Unpinned,
        (None, None) => SignatureStatus::Absent,
    };

    // Decide the verdict. Any hard failure dominates a PARTIAL.
    let sig_failed = matches!(signature, SignatureStatus::Invalid)
        || (opts.trusted_key_pem.is_some() && signature != SignatureStatus::Trusted);
    let verdict = if !hash_ok || !context_ok || sig_failed {
        Verdict::Failed
    } else if partial {
        Verdict::Partial
    } else {
        Verdict::Ok
    };

    Ok(VerifyReport {
        verdict,
        hash_ok,
        context_ok,
        coverage_score,
        partial,
        signature,
        problems,
    })
}
