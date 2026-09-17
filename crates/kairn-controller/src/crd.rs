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
    /// Collectors to run. v0.1 supports "logs" (previous-container log tails).
    #[serde(default = "default_collectors")]
    pub collectors: Vec<String>,
    /// Where to write the sealed bundle.
    #[serde(default)]
    pub export: ExportSpec,
    /// Signing mode. v0.1: "none" (default) or "static".
    #[serde(default)]
    pub signing: SigningSpec,
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct ExportSpec {
    /// Path Kairn writes bundles to (a mounted PVC in-cluster). Default `/var/lib/kairn/bundles`.
    #[serde(default = "default_export_path")]
    pub path: String,
}

impl Default for ExportSpec {
    fn default() -> Self {
        Self {
            path: default_export_path(),
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
}

#[derive(Clone, Debug, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "camelCase")]
pub struct TriggerSpec {
    /// The alert rule / alertname that fired.
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
fn default_export_path() -> String {
    "/var/lib/kairn/bundles".to_string()
}
