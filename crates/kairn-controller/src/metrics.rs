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
//!
//! `prometheusUrl` is chosen by whoever edits the profile, and the response body lands in a
//! bundle, so this collector treats it as an outbound request target that has to be earned.
//! The rules for that are not this module's: [`kairn_net`] owns them for every outbound
//! endpoint Kairn is handed (strict syntax, no user info, plain HTTP only to a cluster-local
//! host, every resolved address vetted and then pinned into the client, no redirects). This
//! module adds only what is specific to a Prometheus *base* URL ([`validate_endpoint`]) and
//! treats a 3xx as an error ([`fetch`]). A configuration that doesn't pass fails this
//! collector only — the capture degrades to PARTIAL.
//!
//! One consequence of the shared rules: an IP literal in brackets (`http://[::1]:9090`) is
//! **not** accepted. Write a name, or an IPv4 literal (`http://127.0.0.1:9090`).

use std::path::Path;
use std::time::Duration;

use chrono::{DateTime, Utc};
use serde_json::{json, Value};

use crate::crd::{MetricQuery, MetricsSpec, TargetRef};

/// Per-query HTTP timeout. A capture must not hang on a slow Prometheus. (The TCP connect
/// timeout is [`kairn_net`]'s, so a black-holed address still fails fast.)
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

    // Validate before anything is created on disk: a misconfigured URL is a configuration
    // error, and failing here leaves no half-written `metrics/` in the bundle.
    let endpoint = validate_endpoint(&spec.prometheus_url)?;
    let client = build_client(&endpoint).await?;
    // `endpoint.url()` is the normalized base, so the URL printed in an error is the URL
    // contacted.
    let url = format!("{}/api/v1/query_range", endpoint.url());

    let dir = stage_dir.join("metrics");
    std::fs::create_dir_all(&dir)?;

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

/// Parse and vet `metrics.prometheusUrl`.
///
/// [`kairn_net::parse`] does the work every configured endpoint needs, with
/// `allow_http_local`: in-cluster Prometheus (the normal case) has no TLS, while a cleartext
/// query to a public host would leak the target's namespace and pod. It also means an IP
/// literal in brackets is refused — see this module's docs.
///
/// What is added here is specific to this field: `prometheusUrl` is a **base** that
/// `/api/v1/query_range` is appended to, so a query string or fragment cannot be carried over
/// from the configuration (it would be silently dropped, or worse, split the URL that is
/// actually requested from the one that was configured), and the path has to be a plain
/// prefix. The base is normalized (trailing slash trimmed) so the URL printed is the URL
/// contacted.
fn validate_endpoint(raw: &str) -> anyhow::Result<kairn_net::Endpoint> {
    let ctx = || format!("invalid metrics.prometheusUrl {raw:?}");
    anyhow::ensure!(!raw.is_empty(), "metrics.prometheusUrl is empty");
    let mut endpoint =
        kairn_net::parse(raw, true).map_err(|e| anyhow::anyhow!("{}: {e}", ctx()))?;

    anyhow::ensure!(
        !endpoint.path.contains(['?', '#']),
        "{}: must be a base URL, without a query string or fragment \
         (the collector appends /api/v1/query_range)",
        ctx()
    );
    anyhow::ensure!(
        endpoint
            .path
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"/-._~".contains(&b)),
        "{}: path may only be a plain prefix ([A-Za-z0-9/-._~])",
        ctx()
    );
    // Trailing slash trimmed once, here: `<base>/api/v1/query_range` is then exactly what is
    // requested, logged and recorded in `metrics/index.json`.
    endpoint.path = endpoint.path.trim_end_matches('/').to_string();
    Ok(endpoint)
}

/// The HTTP client for one capture, from [`kairn_net::connect`]: the host is resolved once,
/// every address is vetted, and the vetted addresses are pinned into the client, so the
/// connection cannot land on an address that was not checked (no DNS-rebinding window).
/// Redirects are not followed — a 3xx is [`fetch`]'s error.
///
/// [`kairn_net::Reach::Cluster`], not `Internet`: an in-cluster Prometheus is reached at a
/// private ClusterIP or pod IP, and a sidecar at 127.0.0.1, so refusing "internal" addresses
/// the way a public-facing web app would would refuse the normal case. Link-local
/// (169.254.169.254 and friends) is refused for every reach, which is the escalation that
/// matters here — including when DNS is what points there.
async fn build_client(endpoint: &kairn_net::Endpoint) -> anyhow::Result<reqwest::Client> {
    kairn_net::connect(endpoint, kairn_net::Reach::Cluster, Some(QUERY_TIMEOUT))
        .await
        .map_err(|e| anyhow::anyhow!("{e}"))
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
    // Redirects are not followed (the client's policy is `none`), so a 3xx reaches us as a
    // response: a Prometheus API never answers one, and following it would hand the choice of
    // what this capture fetches to whoever controls the endpoint.
    anyhow::ensure!(
        !status.is_redirection(),
        "HTTP {status}: the endpoint redirected to {}; redirects are not followed, point \
         metrics.prometheusUrl at the Prometheus API directly",
        resp.headers()
            .get(reqwest::header::LOCATION)
            .and_then(|v| v.to_str().ok())
            .unwrap_or("(no Location)")
    );
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

    #[test]
    fn an_in_cluster_prometheus_url_is_accepted_and_normalized() {
        let ep = validate_endpoint("http://prometheus-operated.monitoring.svc:9090").unwrap();
        assert_eq!(ep.url(), "http://prometheus-operated.monitoring.svc:9090");
        assert_eq!(
            (ep.host.as_str(), ep.port, ep.scheme.as_str()),
            ("prometheus-operated.monitoring.svc", Some(9090), "http")
        );

        // A single-label service name (only resolvable through the pod's search list) and a
        // loopback sidecar are cluster-local too; a path prefix survives, its trailing slash
        // does not — the base is what `/api/v1/query_range` is appended to.
        assert_eq!(
            validate_endpoint("http://prom:9090/").unwrap().url(),
            "http://prom:9090"
        );
        assert_eq!(
            validate_endpoint("http://127.0.0.1:9090/prometheus/")
                .unwrap()
                .url(),
            "http://127.0.0.1:9090/prometheus"
        );

        // https needs no locality; the port stays implicit (kairn-net fills 443 to resolve).
        let ep = validate_endpoint("https://prom.example").unwrap();
        assert_eq!((ep.url().as_str(), ep.port), ("https://prom.example", None));
    }

    /// The base-URL rules this module adds on top of [`kairn_net::parse`], plus a spot check
    /// that the shared rules still reach `metrics.prometheusUrl` (their own cases live in
    /// `kairn-net`).
    #[test]
    fn a_query_string_fragment_or_odd_path_is_refused() {
        for raw in [
            // `/api/v1/query_range` is appended to this, so it has to be a bare base.
            "http://prom:9090/api?x=1",
            "http://prom:9090/#f",
            // a path outside the conservative prefix alphabet
            "http://prom:9090/api%2fv1",
            "http://prom:9090/a;b",
        ] {
            assert!(validate_endpoint(raw).is_err(), "{raw:?} should be refused");
        }

        for raw in [
            "",
            "http://user:pw@prom/",
            "ftp://x",
            "file:///etc/passwd",
            "http://prom .example",
            "http://prom\\@169.254.169.254/",
            "http:///api",
            "prom:9090",
            // plain http to a host that is not cluster-local: a cleartext query would leak
            // the target's namespace and pod
            "http://prom.example",
            // the metadata endpoint written out in full (cleartext to a non-local host)
            "http://169.254.169.254/latest/meta-data/",
            // an IP literal in brackets is not a shape kairn-net accepts
            "https://[fe80::1]:9090",
            "http://[::ffff:169.254.169.254]/",
        ] {
            assert!(validate_endpoint(raw).is_err(), "{raw:?} should be refused");
        }
    }

    /// A redirect must not be followed: the endpoint could send the capture — and the body
    /// that lands in the bundle — anywhere, including the node's metadata endpoint.
    #[tokio::test]
    async fn a_redirecting_endpoint_fails_instead_of_being_followed() {
        crypto();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        std::thread::spawn(move || {
            use std::io::{Read, Write};
            for stream in listener.incoming().take(builtin_queries().len()) {
                let mut s = stream.unwrap();
                let _ = s.read(&mut [0u8; 1024]);
                let _ = s.write_all(
                    b"HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/latest/meta-data/\r\n\
                      Content-Length: 0\r\nConnection: close\r\n\r\n",
                );
            }
        });

        let dir = tempfile::tempdir().unwrap();
        let spec = MetricsSpec {
            prometheus_url: format!("http://{addr}"),
            queries: vec![],
            step_seconds: 15,
        };
        let err = collect_metrics(&spec, &target(), "x", 60, 60, dir.path())
            .await
            .unwrap_err();
        assert!(err.to_string().contains("metric queries failed"), "{err}");

        let index: Value =
            serde_json::from_slice(&std::fs::read(dir.path().join("metrics/index.json")).unwrap())
                .unwrap();
        let first = index["queries"][0]["error"].as_str().unwrap();
        assert!(first.contains("302"), "{first}");
        assert!(first.contains("redirects are not followed"), "{first}");
        // Nothing was fetched, so no response body was staged.
        assert!(!dir
            .path()
            .join("metrics/memory_working_set_bytes.json")
            .exists());
    }

    /// A name is resolved once and every address vetted before the client exists, so a name
    /// pointing at the metadata endpoint never gets connected to. Resolution is exercised
    /// here against `localhost` (hermetic); the refusal itself is `kairn-net`'s test, and
    /// [`a_refused_url_fails_the_collector_without_staging_anything`] covers a literal.
    #[tokio::test]
    async fn a_resolvable_local_name_is_vetted_and_pinned() {
        crypto();
        let ep = validate_endpoint("http://localhost:9090").unwrap();
        build_client(&ep).await.unwrap();
    }

    /// A configuration error fails this collector (→ PARTIAL) before anything is staged.
    #[tokio::test]
    async fn a_refused_url_fails_the_collector_without_staging_anything() {
        crypto();
        let dir = tempfile::tempdir().unwrap();
        let spec = MetricsSpec {
            // Written as a literal, over https so the URL itself is well-formed: the
            // refusal has to come from the address check.
            prometheus_url: "https://169.254.169.254/".into(),
            queries: vec![],
            step_seconds: 15,
        };
        let err = collect_metrics(&spec, &target(), "x", 60, 60, dir.path())
            .await
            .unwrap_err();
        assert!(format!("{err:#}").contains("link-local"), "{err:#}");
        assert!(!dir.path().join("metrics").exists());
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
