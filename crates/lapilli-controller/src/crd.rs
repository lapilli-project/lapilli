//! Lapilli Custom Resource Definitions.
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
    group = "lapilli.dev",
    version = "v1alpha1",
    kind = "CaptureProfile",
    namespaced,
    doc = "Reusable Lapilli capture policy: window, collectors, export target, signing mode."
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
    /// Collectors this profile deliberately does not run because the data is kept elsewhere — a
    /// log shipper for `logs`, an event exporter for `events`, Prometheus for `metrics`. Written
    /// into every bundle's `coverage.deferred` (IEB rule 6), so 100% coverage of a short list
    /// never reads as a full capture. Must not overlap `collectors`; the API server refuses the
    /// overlap. Not a way to skip a collector quietly: a name here is a claim that the data
    /// exists somewhere else.
    #[serde(default)]
    #[schemars(schema_with = "string_set")]
    pub deferred: Vec<String>,
    /// Where to write the sealed bundle.
    #[serde(default)]
    pub export: ExportSpec,
    /// Signing mode. v0.1: "none" (default) or "static".
    #[serde(default)]
    pub signing: SigningSpec,
    /// What `diffs/` may read beyond the owner chain.
    #[serde(default)]
    pub diffs: DiffsSpec,
    /// Credential redaction applied to captured objects before they are written. Best-effort in
    /// every mode; `strict` widens the candidate set, it does not promise a bundle holds no
    /// secret. `docs/data-handling.md` lists what no mode touches.
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

/// Redaction policy v1 (see `spec/IEB-SPEC.md`). Best-effort in every mode: `strict` widens the
/// candidate set, it does not promise a bundle holds no secret. No mode redacts container logs,
/// labels, image references, IP addresses, `nodeName`, `serviceAccountName` or `managedFields`.
#[derive(Clone, Debug, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct RedactionSpec {
    /// "default" (name + value rules), "strict" (every candidate value except `plaintext`
    /// names), or "off" (recorded in the bundle and flagged by `lapilli verify`).
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
    pub fn policy(&self) -> lapilli_bundle::redact::Policy {
        use lapilli_bundle::redact::Mode;
        lapilli_bundle::redact::Policy {
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
    /// capture target's values (escaped for a PromQL string). Empty = Lapilli's built-in set.
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
    /// Path Lapilli writes bundles to (a mounted PVC in-cluster). Default `/var/lib/lapilli/bundles`.
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
    group = "lapilli.dev",
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
    /// Name of the `CaptureProfile` (same namespace) governing this capture, so an RFC 1123
    /// subdomain: a value that cannot be an object name can only fail to resolve. The chart
    /// mirrors this on `profile.name`, with a negative case in `scripts/helm-renders.sh`, because
    /// `values.schema.json` used to allow names the API server accepts and this field would not.
    #[schemars(regex(
        pattern = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
    ))]
    #[schemars(length(max = 253))]
    pub profile: String,
    /// Unique, non-reusable incident id. Bound into the signed manifest (replay defense).
    ///
    /// Deliberately **not** constrained here. `export::path_safe` already bounds it before it is
    /// ever echoed, and it must stay anyway for bytes that arrived from a bucket. A schema
    /// pattern would move the refusal to admission, where `lapilli_captures_total{result="refused"}`
    /// is never incremented and no status exists for the alert to point at
    /// (`docs/design-review-round19.md` R4).
    pub incident_id: String,
    /// Stable cluster identifier, recorded in the bundle. Same pattern as the chart's
    /// `clusterId` and as `main.rs`'s check on the controller's own id: 83 because the webhook
    /// composes `<cluster>-<16 hex>` and `path_safe` caps the result at 100.
    #[schemars(regex(pattern = r"^[A-Za-z0-9._-]{1,83}$"))]
    pub cluster_id: String,
    /// The trigger that fired.
    pub trigger: TriggerSpec,
    /// Namespace/name of the primary workload involved (owner-chain root for collectors).
    pub target: TargetRef,
    /// Keep this capture local: skip the profile's object-store destinations (set for
    /// alerts labeled `lapilli.dev/export: local`, e.g. `lapilli demo`).
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub skip_remote_export: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct TriggerSpec {
    /// The alert rule / alertname that fired. Whoever wrote the `PrometheusRule` chose this
    /// string, and it is rendered into a chat message and into a permanent Markdown document, so
    /// the API refuses the characters a renderer could mistake for structure: `<`, `>` and `&`,
    /// which Slack treats as control syntax; `|`, which splits a Markdown table cell; and every
    /// control character, the newline that would end a table row and the ESC that starts an ANSI
    /// sequence among them. Renderers escape it as well — they have to, because a bundle can be
    /// read on a laptop that never saw an API server.
    #[schemars(regex(pattern = r"^[^<>&|\x00-\x1F\x7F]{1,200}$"))]
    pub rule: String,
    /// RFC 3339 firing timestamp (self-asserted upstream; see DESIGN §5).
    ///
    /// Shaped, because it had no constraint at all and three surfaces interpolate it: the
    /// postmortem's header table, the chat summary, and — when it does not parse —
    /// `window.start` and `window.end`, which the reconciler fills with this string verbatim. A
    /// value with a newline in it therefore reached a Markdown table as three rows. The webhook
    /// refuses the same shape before it creates a capture, so a real alert never meets this check.
    ///
    /// A shape, not a calendar: `2026-13-45T99:99:99Z` matches, and the parsers then fall back to
    /// the capture time exactly as they did before.
    #[schemars(regex(
        pattern = r"^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d{1,9})?([Zz]|[+-]\d{2}:\d{2})$"
    ))]
    #[schemars(length(max = 64))]
    pub firing_ts: String,
}

/// An instant in the shape the schema's `firingTs` pattern accepts, for the message the webhook
/// logs when it refuses one. A pattern is not an error message.
pub const FIRING_TS_EXAMPLE: &str = "2026-09-17T02:14:33Z (or 2026-09-17T02:14:33.123+09:00)";

/// Would the API server accept `s` as a [`TriggerSpec::firing_ts`], **and** can it be read as an
/// instant?
///
/// Both halves matter and neither implies the other. The pattern is what admission checks, so the
/// webhook must not send anything that fails it — a rejected create is a 500 to Alertmanager, a
/// retry loop, and the rest of that payload's alerts never attempted. The parse is what every
/// consumer downstream needs; a string that matches the shape but names the 45th of December is
/// not a firing time.
///
/// Hand-rolled rather than compiled: the controller links no regex engine, and one pattern does
/// not justify adding one. `the_schema_carries_the_patterns_the_webhook_checks_against` reads the
/// pattern back out of the generated schema, which is what keeps the two in step.
pub fn firing_ts_ok(s: &str) -> bool {
    firing_ts_shape(s) && chrono::DateTime::parse_from_rfc3339(s).is_ok()
}

/// The schema's `firingTs` pattern —
/// `^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d{1,9})?([Zz]|[+-]\d{2}:\d{2})$` — by hand.
fn firing_ts_shape(s: &str) -> bool {
    let b = s.as_bytes();
    // `[+-]\d{2}:\d{2}` is the longest tail, so the shortest match is 20 bytes and the longest
    // 35; the schema's own cap is 64. Non-ASCII cannot match, and rejecting it up front keeps
    // every index below on a character boundary.
    if !s.is_ascii() || b.len() < 20 || b.len() > 64 {
        return false;
    }
    let digits = |r: &[u8]| r.iter().all(u8::is_ascii_digit);
    let ok = digits(&b[0..4])
        && b[4] == b'-'
        && digits(&b[5..7])
        && b[7] == b'-'
        && digits(&b[8..10])
        && (b[10] == b'T' || b[10] == b't')
        && digits(&b[11..13])
        && b[13] == b':'
        && digits(&b[14..16])
        && b[16] == b':'
        && digits(&b[17..19]);
    if !ok {
        return false;
    }
    let mut rest = &b[19..];
    if rest.first() == Some(&b'.') {
        let n = rest[1..].iter().take_while(|c| c.is_ascii_digit()).count();
        if n == 0 || n > 9 {
            return false;
        }
        rest = &rest[1 + n..];
    }
    match rest {
        [b'Z' | b'z'] => true,
        [b'+' | b'-', h1, h2, b':', m1, m2] => digits(&[*h1, *h2, *m1, *m2]),
        _ => false,
    }
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
    ///
    /// Capped by the schema at `MESSAGE_MAX` because a schema is the only guard that binds a
    /// writer which is not this controller — four call sites reach `patch_status` directly, and
    /// a holder of `incidentcaptures/status` runs none of this code at all. Writers inside the
    /// controller go through `StatusMessage`, which truncates, so the controller never has its
    /// own patch rejected and loses the whole status on the day it matters. The two guards cover
    /// different populations; neither replaces the other (`docs/design-status-message.md`).
    #[schemars(length(max = 1024))]
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
    /// What happened to the **local** copy. **Reporting only**, like `notification`: the durable
    /// record of a reclaim is the journal at the bundle root, because this field dies with the CR
    /// and a Kubernetes Event expires within the hour (docs/design-retention.md).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub local: Option<LocalStatus>,
}

/// The local bundle, once retention has reclaimed it.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct LocalStatus {
    /// `reclaimed` is the only value today. Absent means the bundle is still where
    /// `bundlePath` says it is.
    pub state: String,
    /// RFC 3339 instant the local copy was reclaimed.
    pub at: String,
    /// A fixed code, never transport text: `max-bytes`, `age`, `orphan`.
    pub reason: String,
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
    /// The `lapilli.dev/retry-seal` annotation value last acted on.
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
    /// SHA-256 of the uploaded object (`lapilli verify s3://… --expect-sha256`).
    // Serialized as null when unset, so a merge patch clears stale values.
    #[serde(default)]
    pub sha256: Option<String>,
    /// The store's version id of the uploaded object, on a versioned bucket
    /// (`lapilli verify s3://… --version-id`).
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
/// The same default the chart writes (`values.yaml` `profile.collectors`). It used to be
/// `["logs"]` — the thinnest capture there is, and two defaults for one thing that disagree
/// meant a hand-written profile that omitted `collectors` got something no chart install ever
/// produced (docs/design-permissions-by-profile.md, rule 6).
fn default_collectors() -> Vec<String> {
    ["logs", "resources", "events", "changes"]
        .into_iter()
        .map(String::from)
        .collect()
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
    "/var/lib/lapilli/bundles".to_string()
}

// ------------------------------------------------------------------ bounded status text -----

/// The cap on `status.message`, and on every Event note built from one.
///
/// Not a round number. Measured against what the code can actually produce: the give-up
/// message's static skeleton is 108 bytes, a realistic AWS `AccessDeniedException` carrying an
/// IRSA role ARN and a key ARN makes it 480, the GCP equivalent 561, and `staging-mismatch` with
/// a maxed cluster and incident id 411. 1024 is ~1.8x that — and it is also exactly the limit
/// `events.k8s.io/v1` puts on `note`.
pub const MESSAGE_MAX: usize = 1024;

/// How much of an error detail may ride inside a message. Half the budget, so the sentence that
/// tells an operator what to do still fits after it.
pub const DETAIL_MAX: usize = 512;

const _: () = assert!(
    MESSAGE_MAX <= 1024,
    "events.k8s.io/v1 caps `note` at 1024 bytes and the API server rejects a longer one. \
     reconcile.rs discards the publish result, so an over-long note would drop the SealFailed \
     Event an operator alerts on, silently. Raising this needs the Event path to stop sharing \
     the string."
);

/// Truncate to at most `max` bytes, on a character boundary, keeping the **front** and saying how
/// much went.
///
/// The front is the load-bearing end: every message in this controller starts with its reason
/// code (`reconcile.rs`'s convention), that is what an operator greps, and `test/e2e/kms.sh`
/// asserts on the prefix. Cutting from the front to keep a "more recent" tail would break all
/// three.
pub fn bounded(s: &str, max: usize) -> String {
    if s.len() <= max {
        return s.to_string();
    }
    // The marker's own length depends on how much is dropped, which depends on where the cut
    // lands. Reserve a fixed budget instead of solving that: `…` is 3 bytes and the count cannot
    // reach 11 digits, so the marker never exceeds this.
    const RESERVE: usize = 24;
    let mut end = max.saturating_sub(RESERVE);
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…(+{} bytes)", &s[..end], s.len() - end)
}

/// A status message that cannot exceed [`MESSAGE_MAX`], because the only constructor bounds it.
///
/// This binds writers inside the controller. It cannot bind a writer that never runs this code —
/// that is what the schema cap on `IncidentCaptureStatus::message` is for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StatusMessage(String);

impl StatusMessage {
    pub fn new(s: impl AsRef<str>) -> Self {
        Self(bounded(s.as_ref(), MESSAGE_MAX))
    }

    pub fn into_string(self) -> String {
        self.0
    }
}

/// `.into()` is allowed because it routes through [`StatusMessage::new`]: there is no way to
/// build one of these that is not bounded, whichever syntax a call site uses. The tuple field is
/// private, so this module is the only place that could bypass it.
impl From<&str> for StatusMessage {
    fn from(s: &str) -> Self {
        Self::new(s)
    }
}

impl From<String> for StatusMessage {
    fn from(s: String) -> Self {
        Self::new(s)
    }
}

impl std::fmt::Display for StatusMessage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

#[cfg(test)]
mod firing_ts_tests {
    use super::*;

    /// Everything Alertmanager and every hand-written CR in this repo actually sends.
    #[test]
    fn a_real_firing_timestamp_is_accepted() {
        for s in [
            // kube-prometheus-stack's Alertmanager: nanoseconds and a numeric offset.
            "2026-09-17T02:14:33.123456789+00:00",
            "2026-09-17T02:14:33.123456789Z",
            // `date -u +%Y-%m-%dT%H:%M:%SZ`, which is what test/e2e/*.sh writes.
            "2026-09-17T02:14:33Z",
            "2026-09-17T02:14:33+09:00",
            "2026-09-17T02:14:33-05:00",
            "2026-09-17T02:14:33.5Z",
            // RFC 3339 §5.6 NOTE: lowercase is allowed and chrono reads it.
            "2026-09-17t02:14:33z",
        ] {
            assert!(firing_ts_ok(s), "refused a valid firing time: {s:?}");
        }
        // And what the webhook itself writes when `startsAt` is absent must pass its own check,
        // or the gap it fills would be refused by admission.
        assert!(firing_ts_ok(&chrono::Utc::now().to_rfc3339()));
    }

    /// The finding: `firingTs` had no constraint, the postmortem interpolated it into a table row
    /// and into `window.start`/`end`, and one forged alert could therefore write its own heading.
    #[test]
    fn a_firing_timestamp_that_could_break_a_markdown_row_is_refused() {
        for s in [
            "2026-09-17T02:14:33Z |\n\n## Root cause\n\nthe database",
            "2026-09-17T02:14:33Z\r\n",
            "now",
            "",
            " 2026-09-17T02:14:33Z",
            "2026-09-17T02:14:33Z ",
            "2026-09-17 02:14:33Z",
            "2026-09-17T02:14:33",
            "2026-09-17T02:14:33.Z",
            "2026-09-17T02:14:33.1234567890Z",
            "2026-09-17T02:14:33+0900",
            "2026-09-17T02:14:33\u{2028}Z",
            // Matches the shape, is not a date: refused by the parse half, not the pattern half.
            "2026-13-45T99:99:99Z",
            // Longer than the schema's maxLength, so admission would refuse it too.
            "2026-09-17T02:14:33.123456789+00:00aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        ] {
            assert!(!firing_ts_ok(s), "accepted an unusable firing time: {s:?}");
        }
    }

    /// The rule the API server enforces and the rule the webhook runs are the same rule written
    /// twice — once as a pattern in the schema, once by hand in [`firing_ts_shape`], because the
    /// controller links no regex engine. This reads the pattern back **out of the generated
    /// schema**, so the two cannot drift silently: the day they do, the webhook starts handing the
    /// API server values admission refuses, which is a 500 to Alertmanager and a retry loop.
    #[test]
    fn the_schema_carries_the_patterns_the_webhook_checks_against() {
        let schema = serde_json::to_value(schemars::schema_for!(TriggerSpec)).unwrap();
        let props = &schema["properties"];
        assert_eq!(
            props["firingTs"]["pattern"],
            serde_json::json!(
                r"^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d{1,9})?([Zz]|[+-]\d{2}:\d{2})$"
            ),
            "the schema's firingTs pattern changed: `firing_ts_shape` spells the same rule out by \
             hand and must change with it, or the webhook will start handing the API server \
             values admission refuses — a 500 to Alertmanager and a retry loop"
        );
        assert_eq!(props["firingTs"]["maxLength"], serde_json::json!(64));
        assert_eq!(
            props["rule"]["pattern"],
            serde_json::json!(r"^[^<>&|\x00-\x1F\x7F]{1,200}$"),
            "the schema's rule pattern changed: it refuses `<`, `>`, `&`, `|` and every C0 \
             control character plus DEL — the newline that ends a Markdown table row and the ESC \
             that starts an ANSI sequence among them — and the renderers' escaping is the other \
             half of it"
        );
    }
}

#[cfg(test)]
mod bounded_tests {
    use super::*;

    #[test]
    fn a_message_within_the_cap_is_untouched() {
        let m = StatusMessage::new("cluster-mismatch: this controller records cluster \"a\"");
        assert_eq!(
            m.to_string(),
            "cluster-mismatch: this controller records cluster \"a\""
        );
    }

    #[test]
    fn a_long_message_is_cut_to_the_cap_and_says_how_much_went() {
        let long = format!("staging-mismatch: {}", "x".repeat(10_000));
        let m = StatusMessage::new(&long).to_string();
        assert!(m.len() <= MESSAGE_MAX, "{} > {MESSAGE_MAX}", m.len());
        assert!(
            m.starts_with("staging-mismatch: "),
            "the reason code must survive: {m:.40}"
        );
        assert!(
            m.ends_with(" bytes)"),
            "the cut must be visible: {}",
            &m[m.len() - 20..]
        );
    }

    /// The front is what an operator greps and what `test/e2e/kms.sh` asserts on, so a cut may
    /// never eat it. Mutating `bounded` to keep the TAIL instead fails here.
    #[test]
    fn the_reason_code_survives_a_cut_that_drops_almost_everything() {
        let long = format!("kms-unavailable: {}", "y".repeat(100_000));
        assert!(StatusMessage::new(&long)
            .to_string()
            .starts_with("kms-unavailable: "));
    }

    /// A byte-wise cut would split a multi-byte character and produce invalid UTF-8 — or, in
    /// Rust, panic on the slice. Hangul is three bytes per syllable, so a cap that is not a
    /// multiple of three lands mid-character unless the boundary is respected.
    #[test]
    fn a_cut_never_splits_a_character() {
        for cap in 64..200 {
            let s = "가".repeat(1000);
            let out = bounded(&s, cap);
            assert!(out.len() <= cap, "cap {cap}: {} bytes", out.len());
            assert!(std::str::from_utf8(out.as_bytes()).is_ok());
        }
    }

    /// The case that makes component bounding load-bearing rather than decorative: a message
    /// with TWO variable halves, where the attacker controls the first. `staging-mismatch` reads
    /// its left-hand ids from the seal file on the volume; without a per-component cap the whole
    /// string is truncated and the EXPECTED ids — the half an operator needs — are gone.
    /// Removing the `bounded()` calls in `sealing.rs` makes this fail.
    #[test]
    fn a_huge_component_cannot_crowd_out_the_one_that_follows_it() {
        const ID: usize = DETAIL_MAX / 4;
        let attacker = "A".repeat(50_000);
        let msg = StatusMessage::new(format!(
            "staging-mismatch: staged data is for {}/{}, not {}/{}",
            bounded(&attacker, ID),
            bounded(&attacker, ID),
            "kind-lapilli",
            "expected-incident-id",
        ))
        .to_string();
        assert!(msg.len() <= MESSAGE_MAX);
        assert!(
            msg.contains("kind-lapilli/expected-incident-id"),
            "the expected ids were crowded out: {msg}"
        );
    }

    /// The detail budget has to leave room for the sentence that follows it, which is the whole
    /// reason the give-up message puts the instruction first.
    #[test]
    fn a_bounded_detail_leaves_room_for_the_instruction() {
        let detail = "z".repeat(50_000);
        let msg = StatusMessage::new(format!(
            "kms-unavailable: gave up after 5 attempts; the collected data is kept: set the \
             lapilli.dev/retry-seal annotation to retry ({})",
            bounded(&detail, DETAIL_MAX)
        ))
        .to_string();
        assert!(msg.len() <= MESSAGE_MAX);
        assert!(
            msg.contains("annotation to retry"),
            "the instruction was cut: {msg}"
        );
    }

    #[test]
    fn into_is_bounded_too() {
        let long: StatusMessage = "w".repeat(9_999).as_str().into();
        assert!(long.to_string().len() <= MESSAGE_MAX);
    }
}
