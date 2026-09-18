//! Alertmanager webhook receiver: turns firing alerts into `IncidentCapture` CRs.
//!
//! The CR name is a deterministic hash of `{rule, cluster, target, firing-bucket}` so
//! Alertmanager resends and grouping collapse onto the same capture (natural dedup), while
//! the same rule firing for two different pods in the same minute stays two captures —
//! DESIGN §6.1.

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::StatusCode;
use axum::routing::{get, post};
use axum::{Json, Router};
use kube::api::PostParams;
use kube::{Api, Client, ResourceExt};
use serde::Deserialize;
use serde_json::json;
use sha2::{Digest, Sha256};

use crate::crd::{IncidentCapture, IncidentCaptureSpec, TargetRef, TriggerSpec};

#[derive(Clone)]
pub struct WebhookState {
    pub client: Client,
    /// Namespace to create IncidentCapture CRs in.
    pub namespace: String,
    /// Cluster id recorded in every capture.
    pub cluster_id: String,
    /// CaptureProfile name to attach.
    pub profile: String,
}

/// Minimal subset of the Alertmanager webhook payload.
#[derive(Debug, Deserialize)]
struct AmPayload {
    #[serde(default)]
    alerts: Vec<AmAlert>,
}

#[derive(Debug, Deserialize)]
struct AmAlert {
    #[serde(default)]
    status: String,
    #[serde(default)]
    labels: std::collections::BTreeMap<String, String>,
    #[serde(rename = "startsAt", default)]
    starts_at: String,
}

pub fn router(state: WebhookState) -> Router {
    Router::new()
        .route("/healthz", get(|| async { "ok" }))
        .route("/webhook", post(handle))
        .with_state(Arc::new(state))
}

/// Responds with the names of the `IncidentCapture`s ensured for the firing alerts, e.g.
/// `{"captures":["ic-1a2b..."]}` (Alertmanager ignores the body; `kairn demo` reads it).
///
/// The body is parsed as JSON whatever its `Content-Type`: Alertmanager always sends JSON,
/// and `kubectl create --raw` (how `kairn demo` posts through the API-server service proxy)
/// sends none. A body that isn't a valid payload is still rejected with 400.
async fn handle(
    State(state): State<Arc<WebhookState>>,
    body: Bytes,
) -> (StatusCode, Json<serde_json::Value>) {
    let payload: AmPayload = match serde_json::from_slice(&body) {
        Ok(p) => p,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({ "error": format!("invalid Alertmanager payload: {e}") })),
            )
        }
    };
    let mut captures = Vec::new();
    for alert in payload.alerts.iter().filter(|a| a.status != "resolved") {
        match create_capture(&state, alert).await {
            Ok(name) => {
                tracing::info!(%name, "IncidentCapture ensured");
                captures.push(name);
            }
            Err(e) => {
                tracing::warn!(error = %e, "failed to create IncidentCapture");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({ "error": e.to_string() })),
                );
            }
        }
    }
    (StatusCode::OK, Json(json!({ "captures": captures })))
}

async fn create_capture(state: &WebhookState, alert: &AmAlert) -> anyhow::Result<String> {
    let rule = alert
        .labels
        .get("alertname")
        .cloned()
        .unwrap_or_else(|| "unknown".to_string());
    let namespace = alert
        .labels
        .get("namespace")
        .cloned()
        .unwrap_or_else(|| state.namespace.clone());
    let pod = alert.labels.get("pod").cloned().unwrap_or_default();
    let firing_ts = if alert.starts_at.is_empty() {
        chrono::Utc::now().to_rfc3339()
    } else {
        alert.starts_at.clone()
    };

    // Dedup: bucket the firing time to the minute so resends collapse onto one capture.
    let bucket = firing_ts.get(0..16).unwrap_or(&firing_ts); // "YYYY-MM-DDTHH:MM"
    let target = format!("{namespace}/{pod}");
    let name = deterministic_name(&rule, &state.cluster_id, &target, bucket);
    let incident_id = format!("{}-{}", state.cluster_id, &name["ic-".len()..]);

    let ic = IncidentCapture::new(
        &name,
        IncidentCaptureSpec {
            profile: state.profile.clone(),
            incident_id,
            cluster_id: state.cluster_id.clone(),
            trigger: TriggerSpec { rule, firing_ts },
            target: TargetRef {
                namespace,
                pod,
                container: None,
            },
        },
    );

    let api: Api<IncidentCapture> = Api::namespaced(state.client.clone(), &state.namespace);
    // Deterministic name = natural dedup: a resend of the same alert hits 409, which we
    // treat as success (the capture already exists — no double capture). DESIGN §6.1.
    match api.create(&PostParams::default(), &ic).await {
        Ok(o) => Ok(o.name_any()),
        Err(kube::Error::Api(ae)) if ae.code == 409 => Ok(name),
        Err(e) => Err(e.into()),
    }
}

fn deterministic_name(rule: &str, cluster: &str, target: &str, bucket: &str) -> String {
    let mut h = Sha256::new();
    for (i, part) in [rule, cluster, target, bucket].iter().enumerate() {
        if i > 0 {
            h.update(b"|");
        }
        h.update(part.as_bytes());
    }
    let digest = h.finalize();
    let hex: String = digest.iter().take(8).map(|b| format!("{b:02x}")).collect();
    format!("ic-{hex}")
}

#[cfg(test)]
mod tests {
    use super::deterministic_name;

    #[test]
    fn resend_of_same_alert_dedups() {
        let a = deterministic_name("R", "c", "ns/p", "2026-09-17T02:14");
        let b = deterministic_name("R", "c", "ns/p", "2026-09-17T02:14");
        assert_eq!(a, b);
    }

    #[test]
    fn same_rule_different_pods_same_minute_are_distinct() {
        let a = deterministic_name("R", "c", "ns/pod-a", "2026-09-17T02:14");
        let b = deterministic_name("R", "c", "ns/pod-b", "2026-09-17T02:14");
        assert_ne!(a, b);
    }
}
