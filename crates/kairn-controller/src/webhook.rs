//! Alertmanager webhook receiver: turns firing alerts into `IncidentCapture` CRs.
//!
//! The CR name is a deterministic hash of `{rule, cluster, firing-bucket}` so Alertmanager
//! resends and grouping collapse onto the same capture (natural dedup) — DESIGN §6.1.

use std::sync::Arc;

use axum::extract::State;
use axum::http::StatusCode;
use axum::routing::{get, post};
use axum::{Json, Router};
use kube::api::PostParams;
use kube::{Api, Client, ResourceExt};
use serde::Deserialize;
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

async fn handle(
    State(state): State<Arc<WebhookState>>,
    Json(payload): Json<AmPayload>,
) -> (StatusCode, String) {
    let mut created = 0usize;
    for alert in payload.alerts.iter().filter(|a| a.status != "resolved") {
        match create_capture(&state, alert).await {
            Ok(name) => {
                created += 1;
                tracing::info!(%name, "IncidentCapture ensured");
            }
            Err(e) => {
                tracing::warn!(error = %e, "failed to create IncidentCapture");
                return (StatusCode::INTERNAL_SERVER_ERROR, format!("error: {e}"));
            }
        }
    }
    (StatusCode::OK, format!("ensured {created} capture(s)"))
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
    let name = deterministic_name(&rule, &state.cluster_id, bucket);
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

fn deterministic_name(rule: &str, cluster: &str, bucket: &str) -> String {
    let mut h = Sha256::new();
    h.update(rule.as_bytes());
    h.update(b"|");
    h.update(cluster.as_bytes());
    h.update(b"|");
    h.update(bucket.as_bytes());
    let digest = h.finalize();
    let hex: String = digest.iter().take(8).map(|b| format!("{b:02x}")).collect();
    format!("ic-{hex}")
}
