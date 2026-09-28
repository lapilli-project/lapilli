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
    /// Most captures one payload may create. `0` = unlimited. See [`handle`].
    pub max_captures_per_payload: usize,
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

/// Webhook body limit.
///
/// "Alertmanager payloads are a few KiB" — what this comment used to say — is true of a single
/// alert and false of the case that matters. A real `KubePodCrashLooping` alert as
/// kube-prometheus-stack POSTs it (labels, annotations, `generatorURL`, `fingerprint`,
/// timestamps) measures **1,059 bytes**, so the old 256 KiB limit rejected the whole payload at
/// about **247 alerts** — and rejected it *entirely*, not the excess, with a 413 that none of
/// the four `webhook_requests_total` outcomes could see. A 250-node zone failure therefore
/// produced **zero captures and zero telemetry**: the recorder blind at exactly the scale that
/// matters most, silently.
///
/// 1 MiB is about 990 realistic alerts. It is a limit, not a capacity: past it the answer is a
/// per-payload cap that records what it turned away, which is a design decision and not this
/// constant's job. What this constant now guarantees is that exceeding it is **counted**.
const MAX_BODY: usize = 1 << 20;
/// How many firing alerts the cap turns away, given how many became captures and how many were
/// already dropped for another reason before the cap engaged.
///
/// Pulled out of [`handle`] so it can be tested: the handler needs a live `kube::Client`, and the
/// one thing in it that can be wrong by one is this. Saturating, because a count that went
/// negative would underflow into a colossal drop total and make the metric worse than absent.
fn alerts_past_cap(firing_total: usize, captured: usize, already_dropped: usize) -> usize {
    firing_total
        .saturating_sub(captured)
        .saturating_sub(already_dropped)
}

/// Is this alert's `startsAt` a claim nobody can read?
///
/// The asymmetry is the whole decision: an **absent** `startsAt` is a gap Lapilli fills with its
/// own clock, as it always has (`create_capture`), while a **present** one that is not an instant
/// is refused. Pulled out of [`handle`] for the same reason as `alerts_past_cap`: the handler needs
/// a live `kube::Client`, and this is the rule that has to be right.
fn refuse_firing_ts(starts_at: &str) -> bool {
    !starts_at.is_empty() && !crate::crd::firing_ts_ok(starts_at)
}

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
        // OUTSIDE the body limit, so it observes the 413 that layer produces. Inside it, the
        // handler never runs and the rejection is invisible.
        .layer(middleware::from_fn(count_oversize))
        .layer(tower::limit::ConcurrencyLimitLayer::new(MAX_CONCURRENT))
        .with_state(state)
}

/// Counts a payload the body limit turned away. A dropped payload is dropped alerts, and the
/// product's whole claim is that it does not quietly lose evidence.
async fn count_oversize(
    req: axum::extract::Request,
    next: middleware::Next,
) -> axum::response::Response {
    let res = next.run(req).await;
    if res.status() == StatusCode::PAYLOAD_TOO_LARGE {
        crate::telemetry::metrics().payload_dropped("too-large");
        tracing::warn!(
            limit_bytes = MAX_BODY,
            "a webhook payload exceeded the body limit and was rejected whole; \
             every alert in it was lost"
        );
    }
    res
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
/// `{"captures":["ic-1a2b..."]}` (Alertmanager ignores the body; `lapilli demo` reads it).
///
/// The body is parsed as JSON whatever its `Content-Type`: Alertmanager always sends JSON,
/// and `kubectl create --raw` (how `lapilli demo` posts through the API-server service proxy)
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
    let mut dropped = 0usize;
    for alert in payload.alerts.iter().filter(|a| a.status != "resolved") {
        // The per-payload cap. Raising `MAX_BODY` to 1 MiB admitted payloads of about 990 alerts,
        // and the measured envelope stops far short of that: 20 alerts peaked at 124 MiB of the
        // chart's 256 MiB limit and 50 alerts at 147 MiB, so the cost grows with the size of the
        // storm and nothing bounded it. Letting a payload past the measured envelope is not
        // neutral — an OOM mid-storm loses the captures already in flight as well as the ones
        // refused. A bound that is known is better than a bound that is discovered.
        //
        // This is the shape the design review demanded: a cap that RECORDS what it turned away.
        // Silently dropping alerts is the failure this product exists to remove, so the excess is
        // counted, logged with the knob that raises it, and returned in `dropped` — where the
        // harness's `captured + dropped = sent` identity checks it.
        if state.max_captures_per_payload > 0 && captures.len() >= state.max_captures_per_payload {
            let firing = payload
                .alerts
                .iter()
                .filter(|a| a.status != "resolved")
                .count();
            let remaining = alerts_past_cap(firing, captures.len(), dropped);
            for _ in 0..remaining {
                crate::telemetry::metrics().alert_dropped("payload-cap");
            }
            dropped += remaining;
            tracing::warn!(
                cap = state.max_captures_per_payload,
                turned_away = remaining,
                "one payload asked for more captures than the cap allows; the rest were not \
                 captured. Raise LAPILLI_MAX_CAPTURES_PER_PAYLOAD (chart: \
                 webhook.maxCapturesPerPayload) only after measuring your own cluster — the \
                 default is the largest storm this project has measured"
            );
            break;
        }
        // An alert with no `pod` label has no target. This used to fall through with `pod` = ""
        // and `namespace` = the controller's own, and the result was **worse than a refusal**:
        // the capture reached `Exported`, incremented `captures_total{result="sealed"}`, and
        // produced a signed 1.7 KB bundle whose `events.json` and `timeline.json` were both `[]`
        // and whose every PromQL result was empty — a success report for nothing, from a tool
        // whose whole job is not claiming evidence it does not have. Measured on kind, not
        // theorised: a `NodeNotReady` alert produced exactly that.
        //
        // Node- and cluster-level alerts are the common case for this, and the honest answer is
        // that Lapilli records a *pod's* incident window and has nothing to record here. It is
        // counted rather than swallowed so an operator can see the gap.
        if alert.labels.get("pod").is_none_or(|p| p.is_empty()) {
            crate::telemetry::metrics().alert_dropped("no-pod");
            tracing::info!(
                rule = alert
                    .labels
                    .get("alertname")
                    .map(String::as_str)
                    .unwrap_or("unknown"),
                "alert has no pod label; no capture (lapilli records a pod's incident window)"
            );
            dropped += 1;
            continue;
        }
        // An alert that names a firing time nobody can read. The CRD shapes `firingTs` now, so
        // sending it on would be a 422 from the API server — which surfaces as a 500 to
        // Alertmanager, a retry loop, and (because `handle` returns on the first create error)
        // the rest of this payload's alerts never attempted. Refused here instead, where it
        // costs one alert and is counted.
        //
        // Refused rather than rewritten to `now()`. An *absent* `startsAt` is a gap Lapilli
        // fills, and has always filled; a present one that does not parse is a claim, and
        // replacing a claim with our own clock inside a **signed** manifest would publish a
        // firing time the alert never asserted, in the document a reviewer trusts most. It would
        // also cost the dedup that is the point of the deterministic name: the bucket is a
        // minute of the firing time, so a bucket taken from `now()` puts every resend of the
        // same alert in a new capture. Every bucket in the system stays a real timestamp prefix.
        if refuse_firing_ts(&alert.starts_at) {
            crate::telemetry::metrics().alert_dropped("bad-firing-ts");
            tracing::warn!(
                rule = alert
                    .labels
                    .get("alertname")
                    .map(String::as_str)
                    .unwrap_or("unknown"),
                // Bounded and flattened: this is a string the sender chose, and it is going into
                // a log an operator reads. `notify.rs`'s escape is the one that already does it.
                starts_at = %crate::notify::escape(&alert.starts_at, 64),
                expected = crate::crd::FIRING_TS_EXAMPLE,
                "alert's startsAt is not an RFC 3339 instant; no capture (the capture window, the \
                 dedup bucket and the sealed manifest are all derived from it). Send an RFC 3339 \
                 timestamp, or omit startsAt to have the receive time used"
            );
            dropped += 1;
            continue;
        }
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
    (
        StatusCode::OK,
        Json(json!({ "captures": captures, "dropped": dropped })),
    )
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
    // `handle` has already refused a `startsAt` that is present and unreadable, so this is either
    // an instant or a gap Lapilli fills with its own clock. Both are values the CRD's pattern
    // accepts, which is what keeps `create` below off the 422 path.
    let firing_ts = if alert.starts_at.is_empty() {
        chrono::Utc::now().to_rfc3339()
    } else {
        alert.starts_at.clone()
    };

    // Dedup: bucket the firing time to the minute so resends collapse onto one capture.
    let bucket = firing_ts.get(0..16).unwrap_or(&firing_ts); // "YYYY-MM-DDTHH:MM"
                                                             // The export mode is part of the identity: a forged "local" alert must not be able to
                                                             // claim the real alert's capture (which then 409s) and keep it off remote storage.
    let local = alert.labels.get("lapilli.dev/export").map(String::as_str) == Some("local");
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
        // Counted, not only returned. An authenticated alert that the API server refuses to turn
        // into a capture — a missing `create` permission is the realistic cause — used to land in
        // no bucket at all: the caller got a 500 and every series in /metrics stayed flat while
        // capture was impossible. `rejected` is deliberately not reused; that means a bad token.
        Err(e) => {
            crate::telemetry::metrics().webhook_error();
            Err(e.into())
        }
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
    use super::{alerts_past_cap, constant_time_eq, deterministic_name, refuse_firing_ts};

    /// The cap turns away exactly the alerts it never looked at — no more, and never fewer.
    #[test]
    fn the_cap_accounts_for_every_alert_it_turned_away() {
        // 100 firing, cap reached at 50 captures, nothing dropped before: 50 turned away, and
        // 50 + 0 + 50 = 100, which is the identity the storm harness asserts.
        assert_eq!(alerts_past_cap(100, 50, 0), 50);
        // Some were already dropped for having no pod. Those must not be counted twice.
        assert_eq!(alerts_past_cap(100, 50, 10), 40);
        // Exactly at the cap with nothing left over: turning anything away would be a lie.
        assert_eq!(alerts_past_cap(50, 50, 0), 0);
    }

    /// A count that went negative would underflow to ~1.8e19 and make the counter worse than
    /// having none. This cannot happen through `handle`, which is why it is asserted here.
    #[test]
    fn the_cap_never_underflows() {
        assert_eq!(alerts_past_cap(10, 50, 0), 0);
        assert_eq!(alerts_past_cap(10, 5, 50), 0);
    }

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

    /// An absent firing time is a gap Lapilli fills; a present one that cannot be read is a
    /// claim, and a claim Lapilli cannot use is refused rather than replaced. Round 30: `firingTs`
    /// had no schema constraint, the postmortem interpolated it into a table row and — when it did
    /// not parse — into `window.start`/`end` as well, so one forged alert could end the row and
    /// write its own `## Root cause` into a permanent document.
    #[test]
    fn an_unreadable_starts_at_is_refused_and_an_absent_one_is_not() {
        // Absent: the receive time stands in, and always has.
        assert!(!refuse_firing_ts(""));
        // What Alertmanager sends.
        assert!(!refuse_firing_ts("2026-09-17T02:14:33.123456789+00:00"));
        assert!(!refuse_firing_ts("2026-09-17T02:14:33Z"));
        // A claim nobody can read — including the one that ends a Markdown table row.
        assert!(refuse_firing_ts("now"));
        assert!(refuse_firing_ts(
            "2026-09-17T02:14:33Z |\n\n## Root cause\n\nthe database"
        ));
        assert!(refuse_firing_ts("2026-09-17 02:14:33"));
    }

    #[test]
    fn same_rule_different_pods_same_minute_are_distinct() {
        let a = deterministic_name("R", "c", "ns/pod-a", "2026-09-17T02:14");
        let b = deterministic_name("R", "c", "ns/pod-b", "2026-09-17T02:14");
        assert_ne!(a, b);
    }
}
