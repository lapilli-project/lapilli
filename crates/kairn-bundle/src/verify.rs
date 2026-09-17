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

/// What the caller asserts the bundle should be, plus optional signature checking.
#[derive(Debug, Default, Clone)]
pub struct VerifyOptions {
    /// If set, must equal `manifest.incident.cluster_id` (fail-closed on mismatch).
    pub expected_cluster: Option<String>,
    /// If set, must equal `manifest.incident.id` (fail-closed on mismatch).
    pub expected_incident: Option<String>,
    /// If true and a `signature/` is present, verify it; if a signature is required but
    /// absent/invalid, that is a failure.
    pub require_signature: bool,
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
    Absent,
    Valid,
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

/// Verify an unpacked bundle directory. (Unpacking a `.ieb` tar+zstd is the caller's job.)
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

    // 4) Optional signature over the literal manifest bytes.
    let sigdir = dir.join(SIGNATURE_DIR);
    let sig_path = sigdir.join(SIG_FILE);
    let pub_path = sigdir.join(PUBKEY_FILE);
    let signature = if sig_path.exists() && pub_path.exists() {
        let sig_b64 = std::fs::read_to_string(&sig_path)?;
        let pub_pem = std::fs::read_to_string(&pub_path)?;
        match verify_b64(&pub_pem, &manifest_bytes, &sig_b64) {
            Ok(()) => SignatureStatus::Valid,
            Err(e) => {
                problems.push(format!("signature invalid: {e}"));
                SignatureStatus::Invalid
            }
        }
    } else {
        if opts.require_signature {
            problems.push("signature required but absent".to_string());
        }
        SignatureStatus::Absent
    };

    // Decide the verdict. Any hard failure dominates a PARTIAL.
    let sig_failed = matches!(signature, SignatureStatus::Invalid)
        || (opts.require_signature && matches!(signature, SignatureStatus::Absent));
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
