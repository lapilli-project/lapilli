//! The `manifest.json` schema — the signed heart of an Incident Evidence Bundle.
//!
//! The manifest is written last, contains the hash tree over every other file, and its
//! literal bytes are the payload that gets signed (see `spec/IEB-SPEC.md`).

use serde::{Deserialize, Serialize};

use crate::hashtree::HashTree;

/// Current IEB manifest schema version.
pub const SCHEMA_VERSION: &str = "lapilli.dev/ieb/v1";
/// Prefix of every schema version; the suffix is the format major (`v1`, …).
pub const SCHEMA_PREFIX: &str = "lapilli.dev/ieb/";
/// The only signing algorithm of `ieb/v1`.
pub const ALG_ECDSA_P256_SHA256: &str = "ecdsa-p256-sha256";

/// Files each collector must have written when it is listed in `coverage.collectors_run`
/// (frozen for `ieb/v1`). Unknown collector names have no requirements (additive).
pub fn required_files(collector: &str) -> &'static [&'static str] {
    match collector {
        "logs" => &["logs/index.json"],
        "resources" => &["resources/pod.json"],
        "events" => &["events.json", "timeline.json"],
        "changes" => &["changes.json", "diffs/index.json"],
        "metrics" => &["metrics/index.json"],
        _ => &[],
    }
}

/// The signing declaration, part of the signed manifest. `key_id` is the SHA-256 (hex) of
/// the public key's SPKI DER.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SigningDecl {
    pub alg: String,
    pub key_id: String,
}

/// The bound incident identity. This tuple is part of the signed manifest and is checked by
/// `lapilli verify` (fail-closed) to defeat replay / bundle substitution.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct IncidentIdentity {
    /// Unique, non-reusable incident id (e.g. a UUID or `{cluster}-{rule}-{ts}` hash).
    pub id: String,
    /// Stable identifier of the cluster the bundle was captured in.
    pub cluster_id: String,
    pub trigger: Trigger,
    pub window: Window,
    /// The pod the capture was about. Optional and additive: absent from every bundle sealed
    /// before it existed, omitted from the bytes when `None`, so those bundles and the
    /// fixtures built from the spec alone are unchanged. It exists so a lookup by
    /// `{namespace, pod, time}` — what an alert carries — never has to unpack a bundle.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<Target>,
}

/// The pod a bundle is about (`incident.target`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Target {
    pub namespace: String,
    pub pod: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Trigger {
    /// The alert rule / source that fired (e.g. Alertmanager alertname).
    pub rule: String,
    /// RFC 3339 timestamp when the trigger fired (self-asserted; see DESIGN §5).
    pub firing_ts: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Window {
    /// RFC 3339 start of the capture window.
    pub start: String,
    /// RFC 3339 end of the capture window.
    pub end: String,
}

/// Who produced the bundle. `image_digest` is self-reported (unverified) in v0.1; signed
/// SLSA provenance binding it is a v0.2 item.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Producer {
    pub version: String,
    pub image_digest: String,
}

/// Which collectors ran vs. were intended. Drives the coverage score and the `PARTIAL`
/// verdict in `lapilli verify`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Coverage {
    pub collectors_run: Vec<String>,
    pub collectors_intended: Vec<String>,
    /// Collectors the producer chose not to intend because the data is kept elsewhere
    /// (spec rule 6). Disjoint from `collectors_intended`; never changes the verdict; always
    /// reported. Omitted from the bytes when empty, so a capture that defers nothing seals
    /// to exactly the manifest it did before this field existed. `null` reads as empty: a
    /// Go producer's nil slice serializes that way, and a reader that FAILED it would turn a
    /// bundle the previous reader accepted (as an unknown member) into a broken one.
    #[serde(
        default,
        deserialize_with = "null_as_empty",
        skip_serializing_if = "Vec::is_empty"
    )]
    pub deferred: Vec<String>,
}

/// `null` → `[]`. Serde's `default` covers only a *missing* key.
fn null_as_empty<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Vec<String>, D::Error> {
    Ok(Option::<Vec<String>>::deserialize(d)?.unwrap_or_default())
}

impl Coverage {
    /// Fraction in [0.0, 1.0] of intended collectors that ran.
    /// Fraction in [0.0, 1.0] of intended collectors that ran, compared as **sets** (a
    /// duplicated or unknown name in `collectors_run` can't make up for a missing one).
    pub fn score(&self) -> f64 {
        let intended: std::collections::BTreeSet<&String> =
            self.collectors_intended.iter().collect();
        if intended.is_empty() {
            return 1.0;
        }
        let ran = intended
            .iter()
            .filter(|c| self.collectors_run.contains(c))
            .count();
        ran as f64 / intended.len() as f64
    }

    /// True if any intended collector is not among those that ran.
    pub fn is_partial(&self) -> bool {
        self.collectors_intended
            .iter()
            .any(|c| !self.collectors_run.contains(c))
    }
}

#[cfg(test)]
mod coverage_tests {
    use super::Coverage;

    #[test]
    fn coverage_is_a_set_comparison() {
        let c = Coverage {
            collectors_run: vec!["logs".into(), "logs".into()],
            collectors_intended: vec!["logs".into(), "metrics".into()],
            deferred: vec![],
        };
        assert!(
            c.is_partial(),
            "a duplicate must not stand in for a missing collector"
        );
        assert_eq!(c.score(), 0.5);
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Timing {
    pub capture_started: String,
    pub sealed_at: String,
    /// Milliseconds between capture start and seal, for transparency (see DESIGN §5).
    pub capture_to_seal_ms: u64,
}

/// The IEB manifest. Field order is fixed by this struct; `serde_json::to_vec` therefore
/// produces stable bytes without any canonicalization step.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Manifest {
    pub schema_version: String,
    pub incident: IncidentIdentity,
    pub producer: Producer,
    /// `null` when unsigned. Declared inside the signed bytes, so a signature lost in transit
    /// is detected (it does not stop an attacker who can re-seal an unsigned bundle).
    pub signing: Option<SigningDecl>,
    pub hash_tree: HashTree,
    pub coverage: Coverage,
    pub timing: Timing,
}

impl Manifest {
    /// Serialize to the exact bytes that get written to `manifest.json` **and** signed.
    /// Uses compact (non-pretty) JSON so the signed payload is deterministic.
    pub fn to_signing_bytes(&self) -> Result<Vec<u8>, serde_json::Error> {
        serde_json::to_vec(self)
    }
}
