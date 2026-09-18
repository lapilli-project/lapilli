//! Collectors gather evidence into a staging directory. Each is failure-isolated: an error
//! is recorded (the collector is simply absent from `collectors_run`, driving the PARTIAL
//! coverage verdict) and never blocks the seal. See DESIGN §4/§6.2.
//!
//! v0.1 collectors:
//!   - `logs`      previous+current container log tails (the timing-sensitive win)
//!   - `resources` the target pod + its owner chain (Pod→ReplicaSet→Deployment) as YAML/JSON
//!   - `events`    Kubernetes events for the pod → events.json + a normalized timeline.json
//!   - `changes`   change *indicators* from free metadata (generation, managedFields, revision)

use std::path::Path;

use k8s_openapi::api::apps::v1::{Deployment, ReplicaSet};
use k8s_openapi::api::core::v1::{Event, Pod};
use kube::api::{ListParams, LogParams};
use kube::{Api, Client, ResourceExt};
use serde_json::json;

use crate::crd::TargetRef;

/// Result of running the collector set over a staging dir.
pub struct CollectOutcome {
    pub run: Vec<String>,
    pub intended: Vec<String>,
}

/// Run the requested collectors into `stage_dir`. Unknown collectors are counted as
/// intended-but-not-run (→ PARTIAL). Each collector is independently failure-isolated.
pub async fn collect_all(
    client: &Client,
    target: &TargetRef,
    collectors: &[String],
    stage_dir: &Path,
) -> CollectOutcome {
    let mut run = Vec::new();
    for name in collectors {
        let result = match name.as_str() {
            "logs" => collect_logs(client, target, stage_dir).await,
            "resources" => collect_resources(client, target, stage_dir).await,
            "events" => collect_events(client, target, stage_dir).await,
            "changes" => collect_changes(client, target, stage_dir).await,
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
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let dir = stage_dir.join("resources");
    std::fs::create_dir_all(&dir)?;

    let pods: Api<Pod> = Api::namespaced(client.clone(), &target.namespace);
    let pod = pods.get(&target.pod).await?;
    write_json(&dir.join("pod.json"), &pod)?;

    // Walk the controller owner chain: Pod → ReplicaSet → Deployment.
    if let Some(rs_name) = controller_owner_of(pod.owner_references(), "ReplicaSet") {
        let rss: Api<ReplicaSet> = Api::namespaced(client.clone(), &target.namespace);
        if let Ok(rs) = rss.get(&rs_name).await {
            write_json(&dir.join("replicaset.json"), &rs)?;
            if let Some(dep_name) = controller_owner_of(rs.owner_references(), "Deployment") {
                let deps: Api<Deployment> = Api::namespaced(client.clone(), &target.namespace);
                if let Ok(dep) = deps.get(&dep_name).await {
                    write_json(&dir.join("deployment.json"), &dep)?;
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
    stage_dir: &Path,
) -> anyhow::Result<()> {
    let events: Api<Event> = Api::namespaced(client.clone(), &target.namespace);
    let lp = ListParams::default().fields(&format!("involvedObject.name={}", target.pod));
    let list = events.list(&lp).await?;

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
            })
        })
        .collect();
    timeline.sort_by(|a, b| {
        a["ts"]
            .as_str()
            .unwrap_or("")
            .cmp(b["ts"].as_str().unwrap_or(""))
    });
    write_json(&stage_dir.join("timeline.json"), &timeline)?;
    Ok(())
}

/// Change *indicators* from metadata Kubernetes already carries — NOT a spec diff (that
/// needs a history store; v0.2). Answers "was this changed recently, by whom, roughly when".
async fn collect_changes(
    client: &Client,
    target: &TargetRef,
    stage_dir: &Path,
) -> anyhow::Result<()> {
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
    Ok(())
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
