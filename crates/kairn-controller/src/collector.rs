//! Collectors gather evidence into a staging directory. Each is failure-isolated: an error
//! is recorded (the collector is simply absent from `collectors_run`, driving the PARTIAL
//! coverage verdict) and never blocks the seal. See DESIGN §4/§6.2.
//!
//! v0.1 collectors:
//!   - `logs`      previous+current container log tails (the timing-sensitive win)
//!   - `resources` the target pod + its owner chain (Pod→ReplicaSet→Deployment, or
//!     StatefulSet/DaemonSet) as JSON
//!   - `events`    Kubernetes events for the pod → events.json + a normalized timeline.json
//!   - `changes`   change *indicators* from free metadata (generation, managedFields, revision)
//!   - `metrics`   Prometheus range queries around the window (see `metrics.rs`)

use std::path::Path;

use k8s_openapi::api::apps::v1::{DaemonSet, Deployment, ReplicaSet, StatefulSet};
use k8s_openapi::api::core::v1::{Event, Pod};
use kube::api::{ListParams, LogParams};
use kube::{Api, Client, ResourceExt};
use serde_json::json;

use crate::crd::{MetricsSpec, TargetRef};

/// Result of running the collector set over a staging dir.
pub struct CollectOutcome {
    pub run: Vec<String>,
    pub intended: Vec<String>,
}

/// Applies the redaction policy at the source and keeps the per-file tally for
/// `redaction.json`.
pub struct Redactor {
    pub policy: kairn_bundle::redact::Policy,
    files: std::sync::Mutex<std::collections::BTreeMap<String, usize>>,
    dropped: std::sync::Mutex<Vec<String>>,
}

impl Redactor {
    pub fn new(policy: kairn_bundle::redact::Policy) -> Self {
        Self {
            policy,
            files: Default::default(),
            dropped: Default::default(),
        }
    }

    /// Serialize a Kubernetes object, redact it, and write it to `stage_dir/rel`.
    fn write_object<T: serde::Serialize>(
        &self,
        stage_dir: &Path,
        rel: &str,
        obj: &T,
    ) -> anyhow::Result<()> {
        let mut v = serde_json::to_value(obj)?;
        let t = self.policy.redact_object(&mut v);
        self.record(rel, t);
        write_json(&stage_dir.join(rel), &v)
    }

    fn text(&self, rel: &str, text: &str) -> String {
        let mut t = kairn_bundle::redact::Tally::default();
        let out = self.policy.redact_text(text, &mut t);
        self.record(rel, t);
        out
    }

    pub(crate) fn record(&self, rel: &str, t: kairn_bundle::redact::Tally) {
        if t.values > 0 {
            *self
                .files
                .lock()
                .unwrap()
                .entry(rel.to_string())
                .or_default() += t.values;
        }
        self.dropped
            .lock()
            .unwrap()
            .extend(t.dropped.into_iter().map(|d| format!("{rel}: {d}")));
    }

    /// The `redaction.json` document for this capture.
    pub fn report(&self) -> serde_json::Value {
        json!({
            "policy_version": kairn_bundle::redact::POLICY_VERSION,
            "mode": self.policy.mode,
            "plaintext_names": self.policy.plaintext,
            "redacted_values": *self.files.lock().unwrap(),
            "dropped_fields": *self.dropped.lock().unwrap(),
            "not_redacted": ["logs/", "metrics/"],
        })
    }
}

/// What the collectors need to know about the capture.
pub struct CollectCtx<'a> {
    pub target: &'a TargetRef,
    pub firing_ts: &'a str,
    pub pre_seconds: u32,
    pub post_seconds: u32,
    pub metrics: Option<&'a MetricsSpec>,
    pub redactor: &'a Redactor,
    /// `diffs.configMaps` from the profile.
    pub diff_config_maps: bool,
}

/// Run the requested collectors into `stage_dir`. Unknown collectors are counted as
/// intended-but-not-run (→ PARTIAL). Each collector is independently failure-isolated.
pub async fn collect_all(
    client: &Client,
    ctx: &CollectCtx<'_>,
    collectors: &[String],
    stage_dir: &Path,
) -> CollectOutcome {
    let target = ctx.target;
    // A profile listing a collector twice must not produce a malformed (FAILED) bundle.
    let mut unique: Vec<String> = Vec::new();
    for c in collectors {
        if !unique.contains(c) {
            unique.push(c.clone());
        }
    }
    let collectors = unique.as_slice();
    let mut run = Vec::new();
    for name in collectors {
        let result = match name.as_str() {
            "logs" => collect_logs(client, target, stage_dir).await,
            "resources" => collect_resources(client, target, ctx.redactor, stage_dir).await,
            "events" => collect_events(client, target, ctx.redactor, stage_dir).await,
            "changes" => collect_changes(client, ctx, stage_dir).await,
            "metrics" => match ctx.metrics {
                Some(spec) => {
                    crate::metrics::collect_metrics(
                        spec,
                        target,
                        ctx.firing_ts,
                        ctx.pre_seconds,
                        ctx.post_seconds,
                        stage_dir,
                    )
                    .await
                }
                None => Err(anyhow::anyhow!(
                    "collector \"metrics\" requested but the profile has no metrics.prometheusUrl"
                )),
            },
            other => Err(anyhow::anyhow!("unknown collector: {other}")),
        };
        match result {
            Ok(()) => run.push(name.clone()),
            Err(e) => {
                tracing::warn!(collector = %name, error = %e, "collector failed (degrading bundle)")
            }
        }
    }
    CollectOutcome {
        run,
        intended: collectors.to_vec(),
    }
}

/// Current + previous-instance log tails for the target pod's containers into `logs/`,
/// plus `logs/index.json` mapping each file to the container instance it came from.
///
/// Two kubelet realities shape this:
/// - In a fast crash loop the *current* instance is often already terminated too, so it —
///   not `previous` — holds the crash's last words; the index records each instance's state
///   so consumers can tell.
/// - The kubelet keeps one dead instance per container, so `previous` can be garbage-
///   collected within seconds. It then answers **200 with an error string as the body**; we
///   refuse to seal that as if it were log content and record the gap in the index instead.
async fn collect_logs(client: &Client, target: &TargetRef, stage_dir: &Path) -> anyhow::Result<()> {
    let pods: Api<Pod> = Api::namespaced(client.clone(), &target.namespace);
    let pod = pods.get(&target.pod).await?;

    let containers: Vec<String> = if let Some(c) = &target.container {
        vec![c.clone()]
    } else {
        pod.spec
            .as_ref()
            .map(|s| s.containers.iter().map(|c| c.name.clone()).collect())
            .unwrap_or_default()
    };
    let statuses = pod
        .status
        .as_ref()
        .and_then(|s| s.container_statuses.clone())
        .unwrap_or_default();

    let logs_dir = stage_dir.join("logs");
    std::fs::create_dir_all(&logs_dir)?;

    let mut index = Vec::new();
    for container in containers {
        let status = statuses.iter().find(|s| s.name == container);
        let mut instances = Vec::new();
        for (which, previous) in [("current", false), ("previous", true)] {
            let state = status.and_then(|s| {
                if previous {
                    s.last_state.as_ref()
                } else {
                    s.state.as_ref()
                }
            });
            let mut entry = instance_info(which, state);
            // An instance that never existed (no restart yet) has no state: nothing to fetch.
            if previous && !state.is_some_and(|s| s.terminated.is_some()) {
                continue;
            }
            let params = LogParams {
                container: Some(container.clone()),
                previous,
                tail_lines: Some(2000),
                ..Default::default()
            };
            match pods.logs(&target.pod, &params).await {
                Ok(body) if is_kubelet_log_error(&body) => {
                    entry["unavailable"] = json!(body.trim());
                }
                Ok(body) => {
                    let file = format!("logs/{container}-{which}.log");
                    std::fs::write(stage_dir.join(&file), body)?;
                    entry["file"] = json!(file);
                }
                Err(e) => entry["unavailable"] = json!(e.to_string()),
            }
            instances.push(entry);
        }
        index.push(json!({ "container": container, "instances": instances }));
    }
    write_json(
        &logs_dir.join("index.json"),
        &json!({ "containers": index }),
    )?;
    Ok(())
}

/// Instance identity + state for `logs/index.json` (container id, running/terminated, reason).
fn instance_info(
    which: &str,
    state: Option<&k8s_openapi::api::core::v1::ContainerState>,
) -> serde_json::Value {
    let mut v = json!({ "which": which, "file": null });
    if let Some(t) = state.and_then(|s| s.terminated.as_ref()) {
        v["state"] = json!("terminated");
        v["container_id"] = json!(t.container_id);
        v["reason"] = json!(t.reason);
        v["exit_code"] = json!(t.exit_code);
        v["finished_at"] = json!(t.finished_at.as_ref().map(|f| f.0.to_rfc3339()));
    } else if let Some(r) = state.and_then(|s| s.running.as_ref()) {
        v["state"] = json!("running");
        v["started_at"] = json!(r.started_at.as_ref().map(|f| f.0.to_rfc3339()));
    } else if let Some(w) = state.and_then(|s| s.waiting.as_ref()) {
        v["state"] = json!("waiting");
        v["reason"] = json!(w.reason);
    }
    v
}

/// The kubelet reports some log failures in-band: HTTP 200 with a one-line error as the
/// body (e.g. after the instance was garbage-collected). Those must not be sealed as logs.
fn is_kubelet_log_error(body: &str) -> bool {
    let b = body.trim_start();
    !b.contains('\n')
        && (b.starts_with("unable to retrieve container logs for ")
            || b.starts_with("failed to try resolving symlinks in path"))
}

/// The target pod and its owner chain (Pod → ReplicaSet → Deployment) as pretty JSON under
/// `resources/`. This is the point-in-time state around the incident.
async fn collect_resources(
    client: &Client,
    target: &TargetRef,
    redactor: &Redactor,
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let dir = stage_dir.join("resources");
    std::fs::create_dir_all(&dir)?;

    let pods: Api<Pod> = Api::namespaced(client.clone(), &target.namespace);
    let pod = pods.get(&target.pod).await?;
    redactor.write_object(stage_dir, "resources/pod.json", &pod)?;

    // StatefulSet / DaemonSet own their pods directly.
    if let Some(name) = controller_owner_of(pod.owner_references(), "StatefulSet") {
        let api: Api<StatefulSet> = Api::namespaced(client.clone(), &target.namespace);
        redactor.write_object(
            stage_dir,
            "resources/statefulset.json",
            &api.get(&name).await?,
        )?;
    }
    if let Some(name) = controller_owner_of(pod.owner_references(), "DaemonSet") {
        let api: Api<DaemonSet> = Api::namespaced(client.clone(), &target.namespace);
        redactor.write_object(
            stage_dir,
            "resources/daemonset.json",
            &api.get(&name).await?,
        )?;
    }

    // Walk the controller owner chain: Pod → ReplicaSet → Deployment.
    if let Some(rs_name) = controller_owner_of(pod.owner_references(), "ReplicaSet") {
        let rss: Api<ReplicaSet> = Api::namespaced(client.clone(), &target.namespace);
        if let Ok(rs) = rss.get(&rs_name).await {
            redactor.write_object(stage_dir, "resources/replicaset.json", &rs)?;
            if let Some(dep_name) = controller_owner_of(rs.owner_references(), "Deployment") {
                let deps: Api<Deployment> = Api::namespaced(client.clone(), &target.namespace);
                if let Ok(dep) = deps.get(&dep_name).await {
                    redactor.write_object(stage_dir, "resources/deployment.json", &dep)?;
                }
            }
        }
    }
    Ok(())
}

/// Kubernetes events for the target pod → `events.json`, and a normalized, time-sorted
/// `timeline.json` merged across what we can see (events for now; more sources later).
async fn collect_events(
    client: &Client,
    target: &TargetRef,
    redactor: &Redactor,
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let events: Api<Event> = Api::namespaced(client.clone(), &target.namespace);
    let lp = ListParams::default().fields(&format!("involvedObject.name={}", target.pod));
    let mut list = events.list(&lp).await?;
    // Event messages can echo spec values (e.g. a failed pull with a token in the URL).
    for e in &mut list.items {
        if let Some(m) = e.message.as_mut() {
            *m = redactor.text("events.json", m);
        }
    }

    write_json(&stage_dir.join("events.json"), &list.items)?;

    // Build a normalized timeline: (timestamp, kind, reason, message).
    let mut timeline: Vec<serde_json::Value> = list
        .items
        .iter()
        .map(|e| {
            let ts = e
                .last_timestamp
                .as_ref()
                .map(|t| t.0.to_rfc3339())
                .or_else(|| e.event_time.as_ref().map(|t| t.0.to_rfc3339()))
                .unwrap_or_default();
            json!({
                "ts": ts,
                "source": "k8s-event",
                "type": e.type_.clone().unwrap_or_default(),
                "reason": e.reason.clone().unwrap_or_default(),
                "message": e.message.clone().unwrap_or_default(),
                // The kubelet aggregates repetitions into ONE object with a rising count: round 16
                // measured `count=11` over 9m17s for a six-restart crash loop. Without these two
                // fields a timeline shows that as a single line at the last occurrence, and a reader
                // cannot tell one event from eleven, or how long it had been going.
                "count": e.count,
                "first_ts": e.first_timestamp.as_ref().map(|t| t.0.to_rfc3339()),
            })
        })
        .collect();
    sort_timeline(&mut timeline);
    write_json(&stage_dir.join("timeline.json"), &timeline)?;
    Ok(())
}

/// Oldest first, and an event whose timestamp could not be read goes **last**.
///
/// A plain string comparison put it first, because the fallback for a missing timestamp is `""`:
/// the top line of the timeline — the one a reader takes as the start of the incident — could be an
/// event with no time at all.
fn sort_timeline(timeline: &mut [serde_json::Value]) {
    timeline.sort_by_key(|e| {
        let ts = e["ts"].as_str().unwrap_or("").to_string();
        (ts.is_empty(), ts)
    });
}

/// Change *indicators* from metadata Kubernetes already carries — NOT a spec diff (that
/// needs a history store; v0.2). Answers "was this changed recently, by whom, roughly when".
async fn collect_changes(
    client: &Client,
    ctx: &CollectCtx<'_>,
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let target = ctx.target;
    let pods: Api<Pod> = Api::namespaced(client.clone(), &target.namespace);
    let pod = pods.get(&target.pod).await?;

    let mut indicators = vec![indicator_for(
        "Pod",
        &pod.name_any(),
        pod.metadata.generation,
        &pod.metadata.creation_timestamp,
        managed_field_actors(&pod),
        pod.annotations()
            .get("deployment.kubernetes.io/revision")
            .cloned(),
    )];

    if let Some(rs_name) = controller_owner_of(pod.owner_references(), "ReplicaSet") {
        let rss: Api<ReplicaSet> = Api::namespaced(client.clone(), &target.namespace);
        if let Ok(rs) = rss.get(&rs_name).await {
            indicators.push(indicator_for(
                "ReplicaSet",
                &rs.name_any(),
                rs.metadata.generation,
                &rs.metadata.creation_timestamp,
                managed_field_actors(&rs),
                rs.annotations()
                    .get("deployment.kubernetes.io/revision")
                    .cloned(),
            ));
            if let Some(dep_name) = controller_owner_of(rs.owner_references(), "Deployment") {
                let deps: Api<Deployment> = Api::namespaced(client.clone(), &target.namespace);
                if let Ok(dep) = deps.get(&dep_name).await {
                    indicators.push(indicator_for(
                        "Deployment",
                        &dep.name_any(),
                        dep.metadata.generation,
                        &dep.metadata.creation_timestamp,
                        managed_field_actors(&dep),
                        dep.annotations()
                            .get("deployment.kubernetes.io/revision")
                            .cloned(),
                    ));
                }
            }
        }
    }

    write_json(
        &stage_dir.join("changes.json"),
        &json!({ "indicators": indicators }),
    )?;

    // Before/after diffs from retained revision history (diffs/).
    let now = chrono::Utc::now();
    let firing = chrono::DateTime::parse_from_rfc3339(ctx.firing_ts)
        .map(|t| t.with_timezone(&chrono::Utc))
        .unwrap_or(now);
    let diff_ctx = crate::diffs::DiffCtx {
        firing,
        window_start: firing - chrono::Duration::seconds(ctx.pre_seconds.into()),
        capture: now,
        redactor: ctx.redactor,
        config_maps: ctx.diff_config_maps,
    };
    let (changes, failed) =
        crate::diffs::collect_diffs(client, &target.namespace, &pod, &diff_ctx, stage_dir).await?;
    merge_timeline(stage_dir, changes)?;
    anyhow::ensure!(!failed, "some diffs failed (see diffs/index.json)");
    Ok(())
}

/// Add events to `timeline.json` (written by the events collector, if it ran) and re-sort.
fn merge_timeline(stage_dir: &Path, extra: Vec<serde_json::Value>) -> anyhow::Result<()> {
    if extra.is_empty() {
        return Ok(());
    }
    let path = stage_dir.join("timeline.json");
    let mut timeline: Vec<serde_json::Value> = std::fs::read(&path)
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default();
    timeline.extend(extra);
    timeline.sort_by(|a, b| {
        a["ts"]
            .as_str()
            .unwrap_or("")
            .cmp(b["ts"].as_str().unwrap_or(""))
    });
    write_json(&path, &timeline)
}

// --- helpers ---

fn controller_owner_of(
    owners: &[k8s_openapi::apimachinery::pkg::apis::meta::v1::OwnerReference],
    kind: &str,
) -> Option<String> {
    owners
        .iter()
        .find(|o| o.kind == kind && o.controller.unwrap_or(false))
        .map(|o| o.name.clone())
}

fn managed_field_actors<K: kube::Resource>(obj: &K) -> Vec<serde_json::Value> {
    obj.meta()
        .managed_fields
        .as_ref()
        .map(|mfs| {
            mfs.iter()
                .map(|mf| {
                    json!({
                        "manager": mf.manager.clone().unwrap_or_default(),
                        "operation": mf.operation.clone().unwrap_or_default(),
                        // "status" writes are controllers reporting, not someone changing spec.
                        "subresource": mf.subresource.clone().unwrap_or_default(),
                        "time": mf.time.as_ref().map(|t| t.0.to_rfc3339()).unwrap_or_default(),
                    })
                })
                .collect()
        })
        .unwrap_or_default()
}

fn indicator_for(
    kind: &str,
    name: &str,
    generation: Option<i64>,
    created: &Option<k8s_openapi::apimachinery::pkg::apis::meta::v1::Time>,
    actors: Vec<serde_json::Value>,
    revision: Option<String>,
) -> serde_json::Value {
    json!({
        "kind": kind,
        "name": name,
        "generation": generation,
        "creationTimestamp": created.as_ref().map(|t| t.0.to_rfc3339()),
        "revision": revision,
        "managedFields": actors,
    })
}

fn write_json<T: serde::Serialize>(path: &Path, value: &T) -> anyhow::Result<()> {
    std::fs::write(path, serde_json::to_vec_pretty(value)?)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    /// The top line of a timeline is read as the start of the incident, so an event whose timestamp
    /// could not be read must not sit there. A plain string comparison put it there, because the
    /// fallback for a missing timestamp is the empty string.
    #[test]
    fn an_event_with_no_timestamp_sorts_last_not_first() {
        use serde_json::json;
        let mut t = vec![
            json!({ "ts": "", "reason": "NoTime" }),
            json!({ "ts": "2026-09-20T01:00:00Z", "reason": "Later" }),
            json!({ "ts": "2026-09-20T00:00:00Z", "reason": "Earlier" }),
        ];
        super::sort_timeline(&mut t);
        let order: Vec<&str> = t.iter().map(|e| e["reason"].as_str().unwrap()).collect();
        assert_eq!(order, vec!["Earlier", "Later", "NoTime"]);
    }

    use super::is_kubelet_log_error;

    #[test]
    fn kubelet_in_band_errors_are_not_logs() {
        assert!(is_kubelet_log_error(
            "unable to retrieve container logs for containerd://dadf16"
        ));
        assert!(!is_kubelet_log_error("[app] FATAL: boom\n"));
        // A real log that merely mentions the phrase on a later line is still a log.
        assert!(!is_kubelet_log_error(
            "starting\nunable to retrieve container logs for x\n"
        ));
    }
}
