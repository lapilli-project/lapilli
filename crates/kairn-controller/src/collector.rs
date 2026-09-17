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

/// Current + previous-instance log tails for the target pod's containers into `logs/`.
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

    let logs_dir = stage_dir.join("logs");
    std::fs::create_dir_all(&logs_dir)?;

    for container in containers {
        if let Ok(cur) = pods
            .logs(
                &target.pod,
                &LogParams {
                    container: Some(container.clone()),
                    previous: false,
                    tail_lines: Some(2000),
                    ..Default::default()
                },
            )
            .await
        {
            std::fs::write(logs_dir.join(format!("{container}-current.log")), cur)?;
        }
        // Previous (last-terminated) instance — absent if the container never restarted.
        if let Ok(prev) = pods
            .logs(
                &target.pod,
                &LogParams {
                    container: Some(container.clone()),
                    previous: true,
                    tail_lines: Some(2000),
                    ..Default::default()
                },
            )
            .await
        {
            std::fs::write(logs_dir.join(format!("{container}-previous.log")), prev)?;
        }
    }
    Ok(())
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
