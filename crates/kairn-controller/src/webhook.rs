//! Alertmanager webhook receiver: turns firing alerts into `IncidentCapture` CRs.
//!
//! The CR name is a deterministic hash of `{rule, cluster, target, firing-bucket}` so
//! Alertmanager resends and grouping collapse onto the same capture (natural dedup), while
//! the same rule firing for two different pods in the same minute stays two captures —
//! DESIGN §6.1.

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::DefaultBodyLimit;
use axum::extract::State;
use axum::http::header::AUTHORIZATION;
use axum::http::StatusCode;
use axum::middleware;
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
    /// Bearer token callers must present (`Authorization: Bearer …`), re-read from its file
    /// every few seconds so rotating the Secret needs no restart. `None` = unauthenticated
    /// (logged loudly at start).
    pub token: Option<Arc<TokenCache>>,
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

/// Webhook body limit: Alertmanager payloads are a few KiB.
const MAX_BODY: usize = 256 << 10;
/// Concurrent webhook requests; more wait (Alertmanager retries).
const MAX_CONCURRENT: usize = 16;
/// Minimum token length accepted at start.
pub const MIN_TOKEN_LEN: usize = 32;

/// `/webhook`, authenticated **before** the body is read, size- and concurrency-limited.
pub fn router(state: WebhookState) -> Router {
    let state = Arc::new(state);
    Router::new()
        .route("/webhook", post(handle))
        .route_layer(middleware::from_fn_with_state(state.clone(), authenticate))
        .layer(DefaultBodyLimit::max(MAX_BODY))
        .layer(tower::limit::ConcurrencyLimitLayer::new(MAX_CONCURRENT))
        .with_state(state)
}

/// `/healthz` on its own port, so a NetworkPolicy on the webhook port never blocks probes.
pub fn health_router() -> Router {
    Router::new()
        .route("/healthz", get(|| async { "ok" }))
        // Prometheus metrics (docs/COMPATIBILITY.md §2). No authentication: the series carry
        // no incident content, only counts and the signing key id.
        .route(
            "/metrics",
            get(|| async {
                (
                    [(
                        axum::http::header::CONTENT_TYPE,
                        "text/plain; version=0.0.4; charset=utf-8",
                    )],
                    crate::telemetry::metrics().render(),
                )
            }),
        )
}

/// Cached token (re-read at most every few seconds, so rotation still applies quickly and
/// unauthenticated floods don't turn into file reads).
pub struct TokenCache {
    file: std::path::PathBuf,
    cached: std::sync::Mutex<(std::time::Instant, Option<String>)>,
    rejected: std::sync::atomic::AtomicU64,
    last_log: std::sync::Mutex<std::time::Instant>,
}

impl TokenCache {
    pub fn new(file: std::path::PathBuf) -> Self {
        let past = std::time::Instant::now() - std::time::Duration::from_secs(3600);
        Self {
            file,
            cached: std::sync::Mutex::new((past, None)),
            rejected: std::sync::atomic::AtomicU64::new(0),
            last_log: std::sync::Mutex::new(past),
        }
    }

    fn token(&self) -> Option<String> {
        let mut c = self.cached.lock().unwrap();
        if c.0.elapsed() > std::time::Duration::from_secs(5) {
            let t = std::fs::read_to_string(&self.file)
                .ok()
                .map(|t| t.trim().to_string())
                .filter(|t| t.len() >= MIN_TOKEN_LEN);
            *c = (std::time::Instant::now(), t);
        }
        c.1.clone()
    }

    /// Count a rejection; log at most every 10 s (a stale token in Alertmanager shows up as
    /// a steady stream of these, which is what to alert on).
    fn rejected(&self) {
        let n = self
            .rejected
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            + 1;
        let mut last = self.last_log.lock().unwrap();
        if last.elapsed() > std::time::Duration::from_secs(10) {
            *last = std::time::Instant::now();
            tracing::warn!(rejected_total = n, "webhook requests rejected: missing or wrong bearer token (is Alertmanager's copy of the token current?)");
        }
    }
}

async fn authenticate(
    State(state): State<Arc<WebhookState>>,
    req: axum::extract::Request,
    next: middleware::Next,
) -> axum::response::Response {
    use axum::response::IntoResponse;
    let Some(cache) = &state.token else {
        return next.run(req).await;
    };
    let Some(expected) = cache.token() else {
        // Fail closed: a missing, empty or short token never means "no auth".
        tracing::error!(file = %cache.file.display(), "webhook token unreadable or too short; rejecting");
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({ "error": "webhook authentication is misconfigured" })),
        )
            .into_response();
    };
    let presented = req
        .headers()
        .get(AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    if constant_time_eq(presented.as_bytes(), expected.as_bytes()) {
        next.run(req).await
    } else {
        crate::telemetry::metrics().webhook_rejected();
        cache.rejected();
        (
            StatusCode::UNAUTHORIZED,
            Json(json!({ "error": "unauthorized" })),
        )
            .into_response()
    }
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
                                                             // The export mode is part of the identity: a forged "local" alert must not be able to
                                                             // claim the real alert's capture (which then 409s) and keep it off remote storage.
    let local = alert.labels.get("kairn.dev/export").map(String::as_str) == Some("local");
    let target = if local {
        format!("{namespace}/{pod}#local")
    } else {
        format!("{namespace}/{pod}")
    };
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
            skip_remote_export: local,
        },
    );

    let api: Api<IncidentCapture> = Api::namespaced(state.client.clone(), &state.namespace);
    // Deterministic name = natural dedup: a resend of the same alert hits 409, which we
    // treat as success (the capture already exists — no double capture). DESIGN §6.1.
    match api.create(&PostParams::default(), &ic).await {
        Ok(o) => {
            crate::telemetry::metrics().webhook_accepted();
            Ok(o.name_any())
        }
        // A resend of the same alert: the capture already exists, no double capture.
        Err(kube::Error::Api(ae)) if ae.code == 409 => {
            crate::telemetry::metrics().webhook_duplicate();
            Ok(name)
        }
        Err(e) => Err(e.into()),
    }
}

/// Compare without an early exit, so response timing doesn't reveal how much of a guessed
/// token matched. (The length is not secret.)
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
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
    use super::{constant_time_eq, deterministic_name};

    #[test]
    fn token_comparison() {
        assert!(constant_time_eq(b"s3cret-token", b"s3cret-token"));
        assert!(!constant_time_eq(b"s3cret-token", b"s3cret-tokeN"));
        assert!(!constant_time_eq(b"", b"s3cret-token"));
        assert!(!constant_time_eq(b"s3cret", b"s3cret-token"));
    }

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
