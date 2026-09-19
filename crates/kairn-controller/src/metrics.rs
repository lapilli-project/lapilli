//! The `metrics` collector: Prometheus range queries around the incident into `metrics/`.
//!
//! Metrics are the one source that can honestly cover the *pre*-incident side of the window:
//! events and logs can only be read from alert time on, but Prometheus has been keeping
//! history all along. The queried range is `[firing - pre, min(firing + post, now)]` — the
//! post side is capped at capture time, and `metrics/index.json` records the exact range.
//!
//! Each query's raw `query_range` response is kept verbatim in `metrics/<name>.json`;
//! `metrics/index.json` records the rendered query, range, step, series count, or the error.
//! Any failed query fails the collector (→ PARTIAL), with the reason in the index.

use std::path::Path;
use std::time::Duration;

use chrono::{DateTime, Utc};
use serde_json::{json, Value};

use crate::crd::{MetricQuery, MetricsSpec, TargetRef};

/// Per-query HTTP timeout. A capture must not hang on a slow Prometheus.
const QUERY_TIMEOUT: Duration = Duration::from_secs(10);
/// Largest response kept per query; bigger means a query far too broad for evidence.
const MAX_RESPONSE_BYTES: usize = 8 << 20;
/// Resolution cap: the step is widened so a series never exceeds this many points.
const MAX_POINTS: i64 = 1000;

/// Built-in queries: container memory/CPU from cAdvisor, limits and restarts from
/// kube-state-metrics. The limit is what makes an OOM's memory curve readable.
pub fn builtin_queries() -> Vec<MetricQuery> {
    let sel = r#"namespace="$namespace",pod="$pod",container!="",container!="POD""#;
    vec![
        MetricQuery {
            name: "memory_working_set_bytes".into(),
            query: format!("container_memory_working_set_bytes{{{sel}}}"),
        },
        MetricQuery {
            name: "cpu_usage_cores".into(),
            query: format!("rate(container_cpu_usage_seconds_total{{{sel}}}[1m])"),
        },
        MetricQuery {
            name: "memory_limit_bytes".into(),
            query: r#"kube_pod_container_resource_limits{namespace="$namespace",pod="$pod",resource="memory"}"#.into(),
        },
        MetricQuery {
            name: "restarts_total".into(),
            query: r#"kube_pod_container_status_restarts_total{namespace="$namespace",pod="$pod"}"#.into(),
        },
    ]
}

pub async fn collect_metrics(
    spec: &MetricsSpec,
    target: &TargetRef,
    firing_ts: &str,
    pre: u32,
    post: u32,
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let (start, end, capped) = query_range(firing_ts, pre, post, Utc::now());
    let step = step_seconds(start, end, spec.step_seconds);
    let queries = if spec.queries.is_empty() {
        builtin_queries()
    } else {
        spec.queries.clone()
    };

    let dir = stage_dir.join("metrics");
    std::fs::create_dir_all(&dir)?;
    let client = reqwest::Client::builder().timeout(QUERY_TIMEOUT).build()?;
    let url = format!(
        "{}/api/v1/query_range",
        spec.prometheus_url.trim_end_matches('/')
    );

    let mut entries = Vec::new();
    let mut failed = 0;
    for q in &queries {
        let rendered = render(&q.query, target);
        let mut entry = json!({ "name": q.name, "query": rendered });
        let result = if valid_name(&q.name) {
            fetch(&client, &url, &rendered, start, end, step).await
        } else {
            Err(anyhow::anyhow!(
                "invalid query name (want [a-z0-9_-], max 64)"
            ))
        };
        match result {
            Ok((body, series)) => {
                let file = format!("metrics/{}.json", q.name);
                std::fs::write(stage_dir.join(&file), body)?;
                entry["status"] = json!("ok");
                entry["series"] = json!(series);
                entry["file"] = json!(file);
            }
            Err(e) => {
                failed += 1;
                entry["status"] = json!("error");
                entry["error"] = json!(format!("{e:#}"));
            }
        }
        entries.push(entry);
    }

    let index = json!({
        "prometheus_url": spec.prometheus_url,
        "start": start.to_rfc3339(),
        "end": end.to_rfc3339(),
        "end_capped_at_capture": capped,
        "step_seconds": step,
        "queries": entries,
    });
    std::fs::write(dir.join("index.json"), serde_json::to_vec_pretty(&index)?)?;
    anyhow::ensure!(
        failed == 0,
        "{failed} of {} metric queries failed (see metrics/index.json)",
        queries.len()
    );
    Ok(())
}

/// One `query_range` call → (raw response body, series count).
async fn fetch(
    client: &reqwest::Client,
    url: &str,
    query: &str,
    start: DateTime<Utc>,
    end: DateTime<Utc>,
    step: u32,
) -> anyhow::Result<(Vec<u8>, usize)> {
    let mut resp = client
        .get(url)
        .query(&[
            ("query", query.to_string()),
            ("start", start.timestamp().to_string()),
            ("end", end.timestamp().to_string()),
            ("step", step.to_string()),
        ])
        .send()
        .await?;
    let status = resp.status();
    let mut body = Vec::new();
    while let Some(chunk) = resp.chunk().await? {
        anyhow::ensure!(
            body.len() + chunk.len() <= MAX_RESPONSE_BYTES,
            "response exceeds {} MiB; narrow the query",
            MAX_RESPONSE_BYTES >> 20
        );
        body.extend_from_slice(&chunk);
    }
    let v: Value = serde_json::from_slice(&body)
        .map_err(|e| anyhow::anyhow!("HTTP {status}, body is not Prometheus JSON: {e}"))?;
    if v["status"] != "success" {
        anyhow::bail!(
            "HTTP {status}: {} {}",
            v["errorType"].as_str().unwrap_or(""),
            v["error"].as_str().unwrap_or("(no error message)")
        );
    }
    let series = v["data"]["result"].as_array().map_or(0, Vec::len);
    Ok((body, series))
}

/// `[firing - pre, min(firing + post, now)]`, plus whether the end was capped at `now`.
/// An unparseable firing time falls back to `[now - pre, now]`.
fn query_range(
    firing_ts: &str,
    pre: u32,
    post: u32,
    now: DateTime<Utc>,
) -> (DateTime<Utc>, DateTime<Utc>, bool) {
    let firing = DateTime::parse_from_rfc3339(firing_ts)
        .map(|t| t.with_timezone(&Utc))
        .unwrap_or(now)
        .min(now);
    let start = firing - chrono::Duration::seconds(pre.into());
    let wanted_end = firing + chrono::Duration::seconds(post.into());
    let end = wanted_end
        .min(now)
        .max(start + chrono::Duration::seconds(1));
    (start, end, wanted_end > now)
}

/// The configured step, widened so the range never needs more than `MAX_POINTS` points.
fn step_seconds(start: DateTime<Utc>, end: DateTime<Utc>, min_step: u32) -> u32 {
    let range = (end - start).num_seconds().max(1);
    let needed = (range + MAX_POINTS - 1) / MAX_POINTS;
    (min_step.max(1) as i64).max(needed) as u32
}

/// Substitute `$namespace` / `$pod` / `$container`, escaped for a double-quoted PromQL string.
fn render(query: &str, target: &TargetRef) -> String {
    query
        .replace("$namespace", &promql_escape(&target.namespace))
        .replace("$pod", &promql_escape(&target.pod))
        .replace(
            "$container",
            &promql_escape(target.container.as_deref().unwrap_or("")),
        )
}

fn promql_escape(s: &str) -> String {
    s.replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'-')
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::extract::Query;
    use axum::routing::get;
    use std::collections::HashMap;

    /// reqwest is built with `rustls-no-provider`; `main` installs ring, tests must too.
    fn crypto() {
        let _ = rustls::crypto::ring::default_provider().install_default();
    }

    fn target() -> TargetRef {
        TargetRef {
            namespace: "shop".into(),
            pod: "checkout-7f8b".into(),
            container: None,
        }
    }

    fn at(s: &str) -> DateTime<Utc> {
        DateTime::parse_from_rfc3339(s).unwrap().with_timezone(&Utc)
    }

    #[test]
    fn range_reaches_back_before_the_alert_and_caps_at_capture_time() {
        let now = at("2026-09-19T02:15:00Z");
        let (start, end, capped) = query_range("2026-09-19T02:14:00Z", 300, 300, now);
        assert_eq!(start, at("2026-09-19T02:09:00Z"));
        assert_eq!(end, now);
        assert!(capped);

        // A firing time in the future (clock skew) never queries past now.
        let (_, end, _) = query_range("2026-09-19T03:00:00Z", 300, 300, now);
        assert_eq!(end, now);
    }

    #[test]
    fn step_widens_to_bound_points() {
        let s = at("2026-09-19T00:00:00Z");
        assert_eq!(step_seconds(s, at("2026-09-19T00:10:00Z"), 15), 15);
        // 24h at 15s would be 5760 points; widened to 87s (<= 1000 points).
        assert_eq!(step_seconds(s, at("2026-09-20T00:00:00Z"), 15), 87);
    }

    #[test]
    fn render_escapes_label_values() {
        let t = TargetRef {
            namespace: r#"a"b\c"#.into(),
            pod: "p".into(),
            container: None,
        };
        assert_eq!(
            render(r#"up{namespace="$namespace",pod="$pod"}"#, &t),
            r#"up{namespace="a\"b\\c",pod="p"}"#
        );
    }

    #[test]
    fn names_must_be_file_safe() {
        assert!(valid_name("memory_working_set_bytes"));
        assert!(!valid_name("../../etc/passwd"));
        assert!(!valid_name("Mem"));
        assert!(!valid_name(""));
    }

    /// A fake Prometheus: `bad` in the query → PromQL error, anything else → one series.
    async fn fake_prometheus() -> String {
        crypto();
        let app = axum::Router::new().route(
            "/api/v1/query_range",
            get(|Query(q): Query<HashMap<String, String>>| async move {
                if q["query"].contains("bad") {
                    return axum::Json(json!({ "status": "error", "errorType": "bad_data",
                        "error": "parse error" }));
                }
                axum::Json(
                    json!({ "status": "success", "data": { "resultType": "matrix",
                    "result": [{ "metric": { "pod": q["query"] }, "values": [[1, "1"]] }] } }),
                )
            }),
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{addr}/")
    }

    #[tokio::test]
    async fn writes_raw_results_and_index() {
        let dir = tempfile::tempdir().unwrap();
        let spec = MetricsSpec {
            prometheus_url: fake_prometheus().await,
            queries: vec![],
            step_seconds: 15,
        };
        collect_metrics(
            &spec,
            &target(),
            "2026-09-19T02:14:00Z",
            300,
            300,
            dir.path(),
        )
        .await
        .unwrap();

        let index: Value =
            serde_json::from_slice(&std::fs::read(dir.path().join("metrics/index.json")).unwrap())
                .unwrap();
        let queries = index["queries"].as_array().unwrap();
        assert_eq!(queries.len(), builtin_queries().len());
        assert!(queries
            .iter()
            .all(|q| q["status"] == "ok" && q["series"] == 1));
        // The rendered query (with the target substituted) is what Prometheus received.
        let raw = std::fs::read_to_string(dir.path().join("metrics/memory_working_set_bytes.json"))
            .unwrap();
        assert!(raw.contains(r#"pod=\"checkout-7f8b\""#), "{raw}");
    }

    #[tokio::test]
    async fn a_failed_query_fails_the_collector_but_is_explained() {
        let dir = tempfile::tempdir().unwrap();
        let spec = MetricsSpec {
            prometheus_url: fake_prometheus().await,
            queries: vec![
                MetricQuery {
                    name: "good".into(),
                    query: "up".into(),
                },
                MetricQuery {
                    name: "broken".into(),
                    query: "bad(".into(),
                },
            ],
            step_seconds: 15,
        };
        let err = collect_metrics(&spec, &target(), "x", 60, 60, dir.path())
            .await
            .unwrap_err();
        assert!(err.to_string().contains("1 of 2"), "{err}");

        let index: Value =
            serde_json::from_slice(&std::fs::read(dir.path().join("metrics/index.json")).unwrap())
                .unwrap();
        assert_eq!(index["queries"][0]["status"], "ok");
        assert_eq!(index["queries"][1]["status"], "error");
        assert!(index["queries"][1]["error"]
            .as_str()
            .unwrap()
            .contains("parse error"));
        assert!(dir.path().join("metrics/good.json").exists());
    }

    #[tokio::test]
    async fn unreachable_prometheus_fails_cleanly() {
        crypto();
        let dir = tempfile::tempdir().unwrap();
        let spec = MetricsSpec {
            // Reserved port on localhost with nothing listening.
            prometheus_url: "http://127.0.0.1:9".into(),
            queries: vec![],
            step_seconds: 15,
        };
        assert!(collect_metrics(&spec, &target(), "x", 60, 60, dir.path())
            .await
            .is_err());
        assert!(dir.path().join("metrics/index.json").exists());
    }
}
