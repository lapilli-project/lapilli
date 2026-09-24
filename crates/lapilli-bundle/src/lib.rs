//! # lapilli-bundle
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
                version: "0.1.0".into(),
                image_digest: "sha256:deadbeef".into(),
            },
            coverage: Coverage {
                collectors_run: if run_all {
                    vec!["alpha".into(), "beta".into()]
                } else {
                    vec!["alpha".into()]
                },
                collectors_intended: vec!["alpha".into(), "beta".into()],
                deferred: vec![],
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

    /// Write a bundle whose PromQL result files hold `series` series each.
    fn bundle_with_metrics(series: usize) -> tempfile::TempDir {
        let dir = bundle_dir();
        fs::create_dir_all(dir.path().join("metrics")).unwrap();
        let result: String = (0..series)
            .map(|i| format!(r#"{{"metric":{{"pod":"p{i}"}},"values":[[1,"1"]]}}"#))
            .collect::<Vec<_>>()
            .join(",");
        for name in ["cpu_usage_cores", "memory_working_set_bytes"] {
            fs::write(
                dir.path().join(format!("metrics/{name}.json")),
                format!(
                    r#"{{"status":"success","data":{{"resultType":"matrix","result":[{result}]}}}}"#
                ),
            )
            .unwrap();
        }
        // index.json is not a result file and must not be counted as one.
        fs::write(
            dir.path().join("metrics/index.json"),
            br#"{"queries":[{"name":"cpu_usage_cores","file":"metrics/cpu_usage_cores.json"}]}"#,
        )
        .unwrap();
        dir
    }

    fn notices(report: &VerifyReport) -> Vec<String> {
        report
            .problems
            .iter()
            .filter(|p| p.code == ProblemCode::Notice)
            .map(|p| p.message.clone())
            .collect()
    }

    /// A bundle whose every PromQL query came back empty still verifies, and says so. Coverage
    /// reports that the metrics collector RAN; it cannot report that it brought anything back,
    /// and a reader who sees `coverage=100%` will not infer the difference on their own. Measured
    /// on kind: a capture of a crash-looping pod produced exactly this bundle.
    #[test]
    fn empty_promql_results_are_noticed_without_changing_the_verdict() {
        let dir = bundle_with_metrics(0);
        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
        assert_eq!(report.coverage_score, 1.0);
        let n = notices(&report);
        assert!(
            n.iter().any(|m| m.contains("all 2 PromQL queries")),
            "expected the empty-metrics notice, got {n:?}"
        );
    }

    /// The other direction, which is what keeps the notice worth reading: a bundle that DID come
    /// back with series must not carry it. Without this the notice could be unconditional and
    /// every test above would still pass.
    #[test]
    fn a_bundle_with_metric_series_gets_no_empty_notice() {
        let dir = bundle_with_metrics(3);
        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
        let n = notices(&report);
        assert!(
            !n.iter().any(|m| m.contains("PromQL")),
            "a bundle with series must not be called empty: {n:?}"
        );
    }

    /// Mixed is not empty. One query with data means the bundle has metrics in it, and claiming
    /// otherwise would be the same false report in the opposite direction.
    #[test]
    fn one_query_with_data_is_enough_to_suppress_the_notice() {
        let dir = bundle_with_metrics(0);
        fs::write(
            dir.path().join("metrics/cpu_usage_cores.json"),
            br#"{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"1"]]}]}}"#,
        )
        .unwrap();
        seal_dir(dir.path(), sample_input(true), None).unwrap();

        let report = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        let n = notices(&report);
        assert!(
            !n.iter().any(|m| m.contains("PromQL")),
            "one non-empty query must suppress it: {n:?}"
        );
    }

    /// The same bundle read as a `.ieb` stream, because the tar path and the directory path read
    /// files differently and only one of them was written first.
    #[test]
    fn the_empty_metrics_notice_survives_the_tar_path() {
        let dir = bundle_with_metrics(0);
        seal_dir(dir.path(), sample_input(true), None).unwrap();
        let out = tempfile::tempdir().unwrap();
        let ieb = out.path().join("b.ieb");
        pack(dir.path(), &ieb).unwrap();

        let report = verify_bundle(&ieb, &VerifyOptions::default()).unwrap();
        assert_eq!(report.verdict, Verdict::Ok, "{:?}", report.problems);
        let n = notices(&report);
        assert!(
            n.iter().any(|m| m.contains("all 2 PromQL queries")),
            "expected the notice from the tar path, got {n:?}"
        );
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

/// `coverage.deferred` (spec rule 6): collectors the producer chose not to intend. Every rule
/// is enforced and the notice is never silent — the alternative, a declaration nothing
/// checks, is the "legal but inert" shape that let an empty collector read as coverage
/// (docs/design-review-round24.md).
#[cfg(test)]
mod deferred_tests {
    use super::*;
    use manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
    use std::fs;

    fn input(intended: &[&str], run: &[&str], deferred: &[&str]) -> SealInput {
        let s = |v: &[&str]| v.iter().map(|x| x.to_string()).collect::<Vec<_>>();
        SealInput {
            incident: IncidentIdentity {
                id: "inc-1".into(),
                cluster_id: "c".into(),
                trigger: Trigger {
                    rule: "R".into(),
                    firing_ts: "2026-09-24T00:00:00Z".into(),
                },
                window: Window {
                    start: "2026-09-23T23:55:00Z".into(),
                    end: "2026-09-24T00:05:00Z".into(),
                },
            },
            producer: Producer {
                version: "0.1.0".into(),
                image_digest: "sha256:t".into(),
            },
            coverage: Coverage {
                collectors_run: s(run),
                collectors_intended: s(intended),
                deferred: s(deferred),
            },
            timing: Timing {
                capture_started: "2026-09-24T00:00:01Z".into(),
                sealed_at: "2026-09-24T00:00:02Z".into(),
                capture_to_seal_ms: 1000,
            },
        }
    }

    /// A perishable capture: `resources` ran, the rest is declared kept elsewhere.
    fn perishable_dir() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("redaction.json"), br#"{"mode":"default"}"#).unwrap();
        fs::create_dir(dir.path().join("resources")).unwrap();
        fs::write(dir.path().join("resources/pod.json"), b"{}").unwrap();
        dir
    }

    fn codes(r: &VerifyReport) -> Vec<&'static str> {
        r.problems.iter().map(|p| p.code.as_str()).collect()
    }

    #[test]
    fn a_deferred_bundle_is_ok_and_says_so_in_three_places() {
        let dir = perishable_dir();
        seal_dir(
            dir.path(),
            input(
                &["resources"],
                &["resources"],
                &["logs", "events", "metrics"],
            ),
            None,
        )
        .unwrap();
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        // 1) the verdict is unchanged: deferral is a decision, not a defect
        assert_eq!(r.verdict, Verdict::Ok, "{:?}", r.problems);
        assert_eq!(r.coverage_score, 1.0);
        // 2) but it is never silent
        assert_eq!(codes(&r), vec!["notice"]);
        assert!(
            r.problems[0].message.contains("logs, events, metrics"),
            "{}",
            r.problems[0].message
        );
        // 3) and the set itself is machine-readable, not only prose
        assert_eq!(r.deferred, vec!["logs", "events", "metrics"]);
        assert_eq!(r.collectors_intended, vec!["resources"]);
    }

    #[test]
    fn deferred_does_not_mask_a_collector_that_failed() {
        let dir = perishable_dir();
        seal_dir(
            dir.path(),
            input(&["resources", "changes"], &["resources"], &["logs"]),
            None,
        )
        .unwrap();
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(r.verdict, Verdict::Partial, "{:?}", r.problems);
        assert!(codes(&r).contains(&"partial"));
    }

    #[test]
    fn a_collector_cannot_be_both_deferred_and_intended() {
        let dir = perishable_dir();
        seal_dir(
            dir.path(),
            input(&["resources"], &["resources"], &["resources"]),
            None,
        )
        .unwrap();
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(r.verdict, Verdict::Failed, "{:?}", r.problems);
        assert_eq!(codes(&r), vec!["manifest"]);
        assert!(r.problems[0].message.contains("both deferred and intended"));
    }

    #[test]
    fn deferring_a_name_the_format_does_not_define_is_malformed() {
        let dir = perishable_dir();
        seal_dir(
            dir.path(),
            input(&["resources"], &["resources"], &["traces"]),
            None,
        )
        .unwrap();
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(r.verdict, Verdict::Failed, "{:?}", r.problems);
        assert_eq!(codes(&r), vec!["manifest"]);
    }

    #[test]
    fn duplicate_deferred_names_are_malformed() {
        let dir = perishable_dir();
        seal_dir(
            dir.path(),
            input(&["resources"], &["resources"], &["logs", "logs"]),
            None,
        )
        .unwrap();
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(r.verdict, Verdict::Failed, "{:?}", r.problems);
        assert_eq!(codes(&r), vec!["manifest"]);
    }

    /// The field is additive: a capture that defers nothing must seal to exactly the bytes
    /// it sealed to before the field existed, or every released fixture's hash would move.
    #[test]
    fn deferring_nothing_leaves_the_manifest_bytes_untouched() {
        let dir = perishable_dir();
        seal_dir(dir.path(), input(&["resources"], &["resources"], &[]), None).unwrap();
        let bytes = fs::read(dir.path().join("manifest.json")).unwrap();
        assert!(
            !String::from_utf8_lossy(&bytes).contains("deferred"),
            "an empty deferred set must be omitted, not serialized as []"
        );
        let r = verify_bundle_dir(dir.path(), &VerifyOptions::default()).unwrap();
        assert_eq!(r.verdict, Verdict::Ok);
        assert!(r.deferred.is_empty());
        assert!(codes(&r).is_empty(), "no notice when nothing was deferred");
    }
}
