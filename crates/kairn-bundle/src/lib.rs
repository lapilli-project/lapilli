//! # kairn-bundle
//!
//! The Incident Evidence Bundle (IEB): manifest schema, content hash tree, sealing, signing,
//! and verification. This crate has **no Kubernetes dependencies** — it is the single source
//! of truth shared by the controller (which seals) and the CLI (which verifies), so the two
//! can never disagree on bytes. See `spec/IEB-SPEC.md` and `DESIGN.md`.

pub mod hashtree;
pub mod manifest;
pub mod pack;
pub mod redact;
pub mod seal;
pub mod sign;
pub mod summary;
pub mod verify;

pub use hashtree::HashTree;
pub use manifest::{Coverage, IncidentIdentity, Manifest, Producer, Timing, Trigger, Window};
pub use pack::{pack, unpack};
pub use seal::{attach_signature, prepare_seal, seal_dir, SealInput};
pub use sign::{generate_key_pair, Signer, StaticKeySigner};
pub use summary::Summary;
pub use verify::{
    verify_bundle, verify_bundle_dir, verify_reader, Problem, ProblemCode, SignatureStatus,
    Verdict, VerifyOptions, VerifyReport, VERIFY_MAX_BYTES,
};

/// Errors produced across the bundle library.
#[derive(Debug, thiserror::Error)]
pub enum BundleError {
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("json error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("path error: {0}")]
    Path(String),
    #[error("key error: {0}")]
    Key(String),
    #[error("signature error: {0}")]
    Signature(String),
}

#[cfg(test)]
mod tests {
    use super::*;
    use manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
    use std::fs;

    /// A staging dir with the file every v1 bundle carries.
    fn bundle_dir() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("redaction.json"), br#"{"mode":"default"}"#).unwrap();
        dir
    }

    fn sample_input(run_all: bool) -> SealInput {
        SealInput {
            incident: IncidentIdentity {
                id: "inc-123".into(),
                cluster_id: "cluster-a".into(),
                trigger: Trigger {
                    rule: "KubePodCrashLooping".into(),
                    firing_ts: "2026-09-11T02:14:33Z".into(),
                },
                window: Window {
                    start: "2026-09-11T02:09:33Z".into(),
                    end: "2026-09-11T02:19:33Z".into(),
                },
            },
            producer: Producer {
                kairn_version: "0.1.0".into(),
                image_digest: "sha256:deadbeef".into(),
            },
            coverage: Coverage {
                collectors_run: if run_all {
                    vec!["alpha".into(), "beta".into()]
                } else {
                    vec!["alpha".into()]
                },
                collectors_intended: vec!["alpha".into(), "beta".into()],
            },
            timing: Timing {
                capture_started: "2026-09-11T02:14:34Z".into(),
                sealed_at: "2026-09-11T02:14:36Z".into(),
                capture_to_seal_ms: 2000,
            },
        }
    }

    #[test]
    fn seal_then_verify_ok() {
        let dir = bundle_dir();
        fs::create_dir(dir.path().join("logs")).unwrap();
        fs::write(dir.path().join("logs/app-previous.log"), b"panic: boom").unwrap();

        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let opts = VerifyOptions {
            expected_cluster: Some("cluster-a".into()),
            expected_incident: Some("inc-123".into()),
            trusted_key_pem: None,
        };
        let report = verify_bundle_dir(dir.path(), &opts).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
    }

    #[test]
    fn tamper_fails_closed() {
        let dir = bundle_dir();
        fs::write(dir.path().join("resources.yaml"), b"kind: Pod").unwrap();
        seal_dir(dir.path(), sample_input(true), None).unwrap();

        // Edit a captured file after sealing.
        fs::write(dir.path().join("resources.yaml"), b"kind: Deployment").unwrap();
        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Failed);
        assert!(!report.hash_ok);
    }

    #[test]
    fn wrong_context_fails_closed() {
        let dir = bundle_dir();
        fs::write(dir.path().join("x"), b"y").unwrap();
        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let opts = VerifyOptions {
            expected_incident: Some("WRONG".into()),
            ..Default::default()
        };
        let report = verify_bundle_dir(dir.path(), &opts).unwrap();
        assert_eq!(report.verdict, Verdict::Failed);
        assert!(!report.context_ok);
    }

    #[test]
    fn missing_collector_is_partial() {
        let dir = bundle_dir();
        fs::write(dir.path().join("x"), b"y").unwrap();
        seal_dir(dir.path(), sample_input(false), None).unwrap();

        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Partial);
        assert!(report.partial);
    }

    fn signed_bundle(key_pem: &str) -> tempfile::TempDir {
        let dir = bundle_dir();
        fs::write(dir.path().join("x.log"), b"FATAL: boom").unwrap();
        let signer = StaticKeySigner::from_pkcs8_pem(key_pem).unwrap();
        seal_dir(dir.path(), sample_input(true), Some(&signer)).unwrap();
        dir
    }

    fn trusting(public: &str) -> VerifyOptions {
        VerifyOptions {
            trusted_key_pem: Some(public.to_string()),
            ..Default::default()
        }
    }

    #[test]
    fn signed_with_trusted_key_is_ok() {
        let (private, public) = generate_key_pair().unwrap();
        let dir = signed_bundle(&private);
        let report = verify_bundle_dir(dir.path(), &trusting(&public)).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
        assert_eq!(report.signature, SignatureStatus::Trusted);
    }

    #[test]
    fn resealed_by_attacker_fails_against_trusted_key() {
        // The attack the embedded key cannot stop: rewrite a file, re-seal with your own key
        // (which also replaces signature/cosign.pub).
        let (private, public) = generate_key_pair().unwrap();
        let (attacker, _) = generate_key_pair().unwrap();
        let dir = signed_bundle(&private);
        fs::write(dir.path().join("x.log"), b"all good, nothing to see").unwrap();
        let forged = StaticKeySigner::from_pkcs8_pem(&attacker).unwrap();
        seal_dir(dir.path(), sample_input(true), Some(&forged)).unwrap();

        // Self-consistent, so without a trusted key it can only be "unpinned"…
        let unpinned = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(unpinned.signature, SignatureStatus::Unpinned);
        // …and with the real producer's key it is rejected.
        let report = verify_bundle_dir(dir.path(), &trusting(&public)).unwrap();
        assert_eq!(report.verdict, Verdict::Failed);
        assert_eq!(report.signature, SignatureStatus::Invalid);
    }

    #[test]
    fn trusted_key_on_unsigned_bundle_fails() {
        let (_, public) = generate_key_pair().unwrap();
        let dir = bundle_dir();
        fs::write(dir.path().join("x"), b"y").unwrap();
        seal_dir(dir.path(), sample_input(true), None).unwrap();
        let report = verify_bundle_dir(dir.path(), &trusting(&public)).unwrap();
        assert_eq!(report.verdict, Verdict::Failed);
        assert_eq!(report.signature, SignatureStatus::Absent);
    }
}
