//! # kairn-bundle
//!
//! The Incident Evidence Bundle (IEB): manifest schema, content hash tree, sealing, signing,
//! and verification. This crate has **no Kubernetes dependencies** — it is the single source
//! of truth shared by the controller (which seals) and the CLI (which verifies), so the two
//! can never disagree on bytes. See `spec/IEB-SPEC.md` and `DESIGN.md`.

pub mod hashtree;
pub mod manifest;
pub mod pack;
pub mod seal;
pub mod sign;
pub mod verify;

pub use hashtree::HashTree;
pub use manifest::{Coverage, IncidentIdentity, Manifest, Producer, Timing, Trigger, Window};
pub use pack::{pack, unpack};
pub use seal::{seal_dir, SealInput};
pub use sign::{Signer, StaticKeySigner};
pub use verify::{
    verify_bundle, verify_bundle_dir, SignatureStatus, Verdict, VerifyOptions, VerifyReport,
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
                    vec!["logs".into(), "events".into()]
                } else {
                    vec!["logs".into()]
                },
                collectors_intended: vec!["logs".into(), "events".into()],
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
        let dir = tempfile::tempdir().unwrap();
        fs::create_dir(dir.path().join("logs")).unwrap();
        fs::write(dir.path().join("logs/app-previous.log"), b"panic: boom").unwrap();

        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let opts = VerifyOptions {
            expected_cluster: Some("cluster-a".into()),
            expected_incident: Some("inc-123".into()),
            require_signature: false,
        };
        let report = verify_bundle_dir(dir.path(), &opts).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
    }

    #[test]
    fn tamper_fails_closed() {
        let dir = tempfile::tempdir().unwrap();
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
        let dir = tempfile::tempdir().unwrap();
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
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("x"), b"y").unwrap();
        seal_dir(dir.path(), sample_input(false), None).unwrap();

        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Partial);
        assert!(report.partial);
    }
}
