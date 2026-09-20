//! Kairn Custom Resource Definitions.
//!
//! `CaptureProfile` holds reusable policy (window, collectors, export target, signing mode).
//! `IncidentCapture` is one capture instance created by a trigger; the controller drives it
//! through a phase machine (see [`Phase`]). See `DESIGN.md` §6.1.

use kube::CustomResource;
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

/// Reusable capture policy. Referenced by `IncidentCapture.spec.profile`.
#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kairn.dev",
    version = "v1alpha1",
    kind = "CaptureProfile",
    namespaced,
    doc = "Reusable Kairn capture policy: window, collectors, export target, signing mode."
)]
#[serde(rename_all = "camelCase")]
pub struct CaptureProfileSpec {
    /// Seconds of window before the trigger firing time (best-effort; see DESIGN §3).
    #[serde(default = "default_pre_seconds")]
    pub pre_seconds: u32,
    /// Seconds of window after the trigger firing time.
    #[serde(default = "default_post_seconds")]
    pub post_seconds: u32,
    /// Collectors to run: "logs", "resources", "events", "changes", "metrics".
    #[serde(default = "default_collectors")]
    #[schemars(schema_with = "string_set")]
    pub collectors: Vec<String>,
    /// Where to write the sealed bundle.
    #[serde(default)]
    pub export: ExportSpec,
    /// Signing mode. v0.1: "none" (default) or "static".
    #[serde(default)]
    pub signing: SigningSpec,
    /// What `diffs/` may read beyond the owner chain.
    #[serde(default)]
    pub diffs: DiffsSpec,
    /// Credential redaction applied to captured objects before they are written.
    #[serde(default)]
    pub redaction: RedactionSpec,
    /// Prometheus range queries around the window, used by the "metrics" collector.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub metrics: Option<MetricsSpec>,
    /// Where a one-screen summary is posted when a capture is sealed.
    #[serde(default)]
    pub notify: NotifySpec,
}

/// A profile may only **name** a destination the admin defined in the chart. It cannot define
/// one, point it elsewhere, or raise its detail level: editing a profile must not be a way to
/// redirect other teams' evidence summaries, or to widen what they carry.
#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct NotifySpec {
    /// The name of a route from the chart's `notify.routes`. Empty (the default) means this
    /// profile's captures are not announced anywhere.
    #[serde(default)]
    #[schemars(regex(pattern = r"^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$|^$"))]
    pub route: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct DiffsSpec {
    /// When a rollout changed which ConfigMap the pod template references (kustomize hash
    /// suffixes, immutable ConfigMaps), GET both and diff them key by key. Needs `get
    /// configmaps` in the watched namespaces (the chart only allows it with watchNamespaces).
    #[serde(default)]
    pub config_maps: bool,
}

/// Redaction policy v1 (see `spec/IEB-SPEC.md`). Best-effort; `strict` for a guarantee.
#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct RedactionSpec {
    /// "default" (name + value rules), "strict" (every candidate value except `plaintext`
    /// names), or "off" (recorded in the bundle and flagged by `kairn verify`).
    #[serde(default)]
    pub mode: RedactionMode,
    /// Env/header/annotation names exempt from `strict`.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub plaintext: Vec<String>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "lowercase")]
pub enum RedactionMode {
    #[default]
    Default,
    Strict,
    Off,
}

impl RedactionSpec {
    pub fn policy(&self) -> kairn_bundle::redact::Policy {
        use kairn_bundle::redact::Mode;
        kairn_bundle::redact::Policy {
            mode: match self.mode {
                RedactionMode::Default => Mode::Default,
                RedactionMode::Strict => Mode::Strict,
                RedactionMode::Off => Mode::Off,
            },
            plaintext: self.plaintext.clone(),
        }
    }
}

/// Where and what to query for the `metrics/` part of a bundle.
#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct MetricsSpec {
    /// Base URL of a Prometheus-compatible HTTP API, e.g. `http://prometheus.monitoring:9090`.
    pub prometheus_url: String,
    /// Range queries to snapshot. `$namespace`, `$pod` and `$container` are replaced with the
    /// capture target's values (escaped for a PromQL string). Empty = Kairn's built-in set.
    #[serde(default)]
    pub queries: Vec<MetricQuery>,
    /// Minimum resolution in seconds (default 15). Widened automatically so no series exceeds
    /// 1000 points.
    #[serde(default = "default_step_seconds")]
    pub step_seconds: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub struct MetricQuery {
    /// File-safe name: the result lands in `metrics/<name>.json`. `[a-z0-9_-]`, max 64 chars.
    pub name: String,
    /// PromQL expression.
    pub query: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ExportSpec {
    /// Path Kairn writes bundles to (a mounted PVC in-cluster). Default `/var/lib/kairn/bundles`.
    #[serde(default = "default_export_path")]
    pub path: String,
    /// Names of object-store destinations to copy each bundle to. Destinations themselves
    /// (URL, endpoint, credentials) are defined by the admin in the controller's
    /// configuration and can't be changed here; unknown names are refused.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    #[schemars(schema_with = "string_set")]
    pub destinations: Vec<String>,
}

impl Default for ExportSpec {
    fn default() -> Self {
        Self {
            path: default_export_path(),
            destinations: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct SigningSpec {
    /// "none" (default) or "static". KMS/keyless are later versions.
    #[serde(default)]
    pub mode: SigningMode,
    /// For mode=static: name of a Secret holding a PKCS#8 PEM private key under `key.pem`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub key_secret: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub enum SigningMode {
    #[default]
    None,
    Static,
}

/// One incident capture, created by a trigger. The controller reconciles its status.
#[derive(CustomResource, Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[kube(
    group = "kairn.dev",
    version = "v1alpha1",
    kind = "IncidentCapture",
    namespaced,
    status = "IncidentCaptureStatus",
    doc = "One Kubernetes incident capture: trigger metadata + capture window, driven through a phase machine.",
    printcolumn = r#"{"name":"Phase","type":"string","jsonPath":".status.phase"}"#,
    printcolumn = r#"{"name":"Bundle","type":"string","jsonPath":".status.bundlePath"}"#,
    printcolumn = r#"{"name":"Export","type":"string","jsonPath":".status.exportSummary"}"#,
    printcolumn = r#"{"name":"Notify","type":"string","jsonPath":".status.notification.state"}"#,
    printcolumn = r#"{"name":"Age","type":"date","jsonPath":".metadata.creationTimestamp"}"#
)]
#[serde(rename_all = "camelCase")]
pub struct IncidentCaptureSpec {
    /// Name of the `CaptureProfile` (same namespace) governing this capture.
    pub profile: String,
    /// Unique, non-reusable incident id. Bound into the signed manifest (replay defense).
    pub incident_id: String,
    /// Stable cluster identifier, recorded in the bundle.
    pub cluster_id: String,
    /// The trigger that fired.
    pub trigger: TriggerSpec,
    /// Namespace/name of the primary workload involved (owner-chain root for collectors).
    pub target: TargetRef,
    /// Keep this capture local: skip the profile's object-store destinations (set for
    /// alerts labeled `kairn.dev/export: local`, e.g. `kairn demo`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub skip_remote_export: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct TriggerSpec {
    /// The alert rule / alertname that fired. Whoever wrote the `PrometheusRule` chose this
    /// string, and it is rendered into a chat message, so the API refuses the three
    /// characters a renderer could mistake for markup. Renderers escape it as well: this is
    /// the boundary check, not the only one.
    #[schemars(regex(pattern = r"^[^<>&]{1,200}$"))]
    pub rule: String,
    /// RFC 3339 firing timestamp (self-asserted upstream; see DESIGN §5).
    pub firing_ts: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct TargetRef {
    pub namespace: String,
    /// Pod name to collect logs from (v0.1 collects previous-container tails from this pod).
    pub pod: String,
    /// Optional specific container; defaults to all containers in the pod.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub container: Option<String>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct IncidentCaptureStatus {
    /// Current phase of the capture.
    #[serde(default)]
    pub phase: Phase,
    /// Path of the sealed bundle once exported.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bundle_path: Option<String>,
    /// Human-readable detail (error message on Failed, progress otherwise).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    /// The `.metadata.generation` this status was computed for (idempotency).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_generation: Option<i64>,
    /// Remote copies, keyed by destination name (a map, so each entry is patched alone).
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    pub exports: std::collections::BTreeMap<String, ExportStatus>,
    /// One-line summary for `kubectl get`, e.g. `evidence=uploaded`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub export_summary: Option<String>,
    /// KMS signing (docs/design-kms.md): attempts while `Sealing`, and once signed, what
    /// matches the cloud's audit log. Informational: never trusted as input.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seal: Option<SealStatus>,
    /// What happened to this incident's summary message. **Reporting only**: the dispatcher's
    /// once-only guard is the `<incident>.notified` file, so patching this changes nothing.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub notification: Option<NotificationStatus>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct NotificationStatus {
    /// `sent`, `repeat`, `failed`, `suppressed`, `dropped` or `already-notified`.
    pub state: String,
    /// When the dispatcher settled it (RFC 3339).
    pub at: String,
    /// A fixed code on anything but `sent`, never transport text and never a webhook response
    /// body (a 4xx body can quote the request that was sent): one of `unreachable`, `timeout`,
    /// `endpoint-refused`, `endpoint-error`, `rate-limited-by-endpoint`, `endpoint-redirected`,
    /// `route-unusable`, `rate-capped`, `claim-failed`, `already-notified`, `in-cooldown`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    /// The route it went to, so a team can tell which channel has it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub route: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct SealStatus {
    /// Signing attempts so far in this round.
    #[serde(default)]
    pub attempts: u32,
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub next_attempt_at: Option<String>,
    /// Fixed reason code of the last failure (`signing-unavailable`, `signing-denied`, …).
    #[serde(default)]
    pub reason: Option<String>,
    /// The KMS key (ARN or version) and the `ieb/v1` key id it signed with.
    #[serde(default)]
    pub key: Option<String>,
    #[serde(default)]
    pub key_id: Option<String>,
    /// SHA-256 of the signed manifest and the cloud request id, to match audit log entries.
    #[serde(default)]
    pub manifest_sha256: Option<String>,
    #[serde(default)]
    pub request_id: Option<String>,
    /// The `kairn.dev/retry-seal` annotation value last acted on.
    #[serde(default)]
    pub retry_token: Option<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ExportStatus {
    /// Full object URL (e.g. `s3://bucket/prefix/cluster/incident.ieb`).
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub url: Option<String>,
    pub state: ExportState,
    #[serde(default)]
    pub attempts: u32,
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub last_attempt_at: Option<String>,
    /// Fixed reason code; details are in the controller log only.
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub reason: Option<String>,
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub uploaded_at: Option<String>,
    /// SHA-256 of the uploaded object (`kairn verify s3://… --expect-sha256`).
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub sha256: Option<String>,
    /// The store's version id of the uploaded object, on a versioned bucket
    /// (`kairn verify s3://… --version-id`).
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub version_id: Option<String>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub enum ExportState {
    #[default]
    Pending,
    Uploaded,
    Refused,
    Conflict,
    /// Attempts exhausted.
    Failed,
}

impl ExportState {
    pub fn settled(self) -> bool {
        !matches!(self, ExportState::Pending)
    }
}

/// The capture phase machine: Pending → Capturing → Sealing → Exported | Failed.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize, JsonSchema)]
pub enum Phase {
    #[default]
    Pending,
    Capturing,
    Sealing,
    Exported,
    Failed,
}

fn default_pre_seconds() -> u32 {
    300
}
fn default_post_seconds() -> u32 {
    300
}
fn default_collectors() -> Vec<String> {
    vec!["logs".to_string()]
}
/// A list of unique strings (`x-kubernetes-list-type: set`): the API server rejects
/// duplicates, so a bundle can't be sealed with malformed coverage.
fn string_set(gen: &mut schemars::gen::SchemaGenerator) -> schemars::schema::Schema {
    let mut schema = gen.subschema_for::<Vec<String>>().into_object();
    schema
        .extensions
        .insert("x-kubernetes-list-type".into(), serde_json::json!("set"));
    schema.into()
}

fn default_step_seconds() -> u32 {
    15
}
fn default_export_path() -> String {
    "/var/lib/kairn/bundles".to_string()
}
