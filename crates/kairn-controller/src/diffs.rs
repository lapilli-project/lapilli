//! `diffs/`: before/after pod-template diffs from the revision history Kubernetes already
//! keeps (Deployment → ReplicaSets). No watch, no state. The rules below implement
//! `docs/design-change-diff.md` (v4); each was measured on a live kind cluster because the
//! obvious signals are wrong:
//! - a reused ReplicaSet (rollback) keeps its original `creationTimestamp`;
//! - a managedFields `time` is per manager, and moves on every scale (HPA included);
//! - identical scale events coalesce, so a rollback survives only as the latest timestamp.
//!
//! Part of the `changes` collector: an `error` entry fails that collector (→ PARTIAL) after
//! everything that did succeed has been written.

use std::collections::BTreeMap;
use std::path::Path;

use chrono::{DateTime, Duration, Utc};
use k8s_openapi::api::apps::v1::{
    ControllerRevision, DaemonSet, Deployment, ReplicaSet, StatefulSet,
};
use k8s_openapi::api::core::v1::{ConfigMap, Event, Pod};
use k8s_openapi::apimachinery::pkg::apis::meta::v1::ManagedFieldsEntry;
use kube::api::ListParams;
use kube::{Api, Client, ResourceExt};
use serde_json::{json, Value};

use crate::collector::Redactor;
use crate::specdiff;

const REVISION: &str = "deployment.kubernetes.io/revision";
const REVISION_HISTORY: &str = "deployment.kubernetes.io/revision-history";
const CONTROLLER_MANAGER: &str = "kube-controller-manager";

pub struct DiffCtx<'a> {
    pub firing: DateTime<Utc>,
    pub window_start: DateTime<Utc>,
    pub capture: DateTime<Utc>,
    pub redactor: &'a Redactor,
    /// Follow ConfigMaps whose referenced name changed in the template (opt-in).
    pub config_maps: bool,
}

/// Build `diffs/` for the pod's owner chain. Returns the `timeline.json` events to merge and
/// whether any entry is an `error`.
pub async fn collect_diffs(
    client: &Client,
    ns: &str,
    pod: &Pod,
    ctx: &DiffCtx<'_>,
    stage_dir: &Path,
) -> anyhow::Result<(Vec<Value>, bool)> {
    let mut expected = Vec::new();
    let mut entries = Vec::new();
    let mut timeline = Vec::new();

    let owner = pod
        .owner_references()
        .iter()
        .find(|o| o.controller.unwrap_or(false));
    match owner {
        None => {} // bare pod: nothing expected
        Some(o) if o.kind == "ReplicaSet" => {
            match deployment_entries(client, ns, &o.name, ctx, stage_dir).await {
                Ok(DeploymentResult {
                    expected: e,
                    entries: es,
                    timeline: tl,
                }) => {
                    expected.extend(e);
                    entries.extend(es);
                    timeline.extend(tl);
                }
                Err(e) => {
                    expected.push(obj_ref(ns, "ReplicaSet", &o.name));
                    entries.push(status_entry(
                        ns,
                        "ReplicaSet",
                        &o.name,
                        "error",
                        &e.to_string(),
                    ));
                }
            }
        }
        Some(o) if o.kind == "StatefulSet" || o.kind == "DaemonSet" => {
            expected.push(obj_ref(ns, &o.kind, &o.name));
            match revision_entries(client, ns, pod, &o.kind, &o.name, ctx, stage_dir).await {
                Ok((es, tl)) => {
                    entries.extend(es);
                    timeline.extend(tl);
                }
                Err(e) => entries.push(status_entry(ns, &o.kind, &o.name, "error", &e.to_string())),
            }
        }
        Some(o) => {
            expected.push(obj_ref(ns, &o.kind, &o.name));
            entries.push(status_entry(
                ns,
                &o.kind,
                &o.name,
                "unsupported_kind",
                "diffs cover Deployment, StatefulSet and DaemonSet",
            ));
        }
    }

    let failed = entries.iter().any(|e| e["status"] == "error");
    let index = json!({ "normalization": "v1", "expected": expected, "entries": entries });
    std::fs::create_dir_all(stage_dir.join("diffs"))?;
    std::fs::write(
        stage_dir.join("diffs/index.json"),
        serde_json::to_vec_pretty(&index)?,
    )?;
    Ok((timeline, failed))
}

struct DeploymentResult {
    expected: Vec<Value>,
    entries: Vec<Value>,
    timeline: Vec<Value>,
}

/// One retained revision number and the ReplicaSet whose template it had.
struct Rev<'a> {
    rs: &'a ReplicaSet,
    /// The RS's *current* revision (event/creation time can only be attributed to this one).
    current: bool,
}

async fn deployment_entries(
    client: &Client,
    ns: &str,
    pod_rs_name: &str,
    ctx: &DiffCtx<'_>,
    stage_dir: &Path,
) -> anyhow::Result<DeploymentResult> {
    let rs_api: Api<ReplicaSet> = Api::namespaced(client.clone(), ns);
    let pod_rs = rs_api.get(pod_rs_name).await?;
    let Some(dep_ref) = pod_rs
        .owner_references()
        .iter()
        .find(|o| o.controller.unwrap_or(false))
        .cloned()
    else {
        return Ok(DeploymentResult {
            expected: vec![],
            entries: vec![],
            timeline: vec![],
        });
    };
    if dep_ref.kind != "Deployment" {
        // e.g. an Argo Rollout owns the ReplicaSet: classified by the top controller.
        return Ok(DeploymentResult {
            expected: vec![obj_ref(ns, &dep_ref.kind, &dep_ref.name)],
            entries: vec![status_entry(
                ns,
                &dep_ref.kind,
                &dep_ref.name,
                "unsupported_kind",
                "ReplicaSet owned by a non-Deployment controller",
            )],
            timeline: vec![],
        });
    }
    let expected = vec![obj_ref(ns, "Deployment", &dep_ref.name)];
    let dep: Deployment = match Api::namespaced(client.clone(), ns).get(&dep_ref.name).await {
        Ok(d) => d,
        Err(e) => {
            return Ok(DeploymentResult {
                expected,
                entries: vec![status_entry(
                    ns,
                    "Deployment",
                    &dep_ref.name,
                    "error",
                    &e.to_string(),
                )],
                timeline: vec![],
            })
        }
    };
    let dep_uid = dep.uid().unwrap_or_default();

    // All ReplicaSets of this Deployment, and every revision number each has held.
    let all_rs = rs_api.list(&ListParams::default()).await?.items;
    let owned: Vec<&ReplicaSet> = all_rs
        .iter()
        .filter(|rs| rs.owner_references().iter().any(|o| o.uid == dep_uid))
        .collect();
    let mut revs: BTreeMap<i64, Rev> = BTreeMap::new();
    for rs in &owned {
        if let Some(n) = annotation_i64(rs, REVISION) {
            revs.insert(n, Rev { rs, current: true });
        }
        for n in annotation_list(rs, REVISION_HISTORY) {
            revs.entry(n).or_insert(Rev { rs, current: false });
        }
    }
    let pod_rev = annotation_i64(&pod_rs, REVISION).unwrap_or(0);
    let newest = revs.keys().max().copied().unwrap_or(pod_rev);

    // Scale events of the Deployment (for rollback activation times).
    let events: Api<Event> = Api::namespaced(client.clone(), ns);
    let evs = events
        .list(&ListParams::default().fields(&format!("involvedObject.name={}", dep_ref.name)))
        .await?
        .items;
    let scale = ScaleEvents::from(&evs);

    // Live pods (for the activation sanity check).
    let pods: Api<Pod> = Api::namespaced(client.clone(), ns);
    let live_pods = pods.list(&ListParams::default()).await?.items;

    let activation = |n: i64| -> (Option<DateTime<Utc>>, &'static str) {
        let Some(r) = revs.get(&n) else {
            return (None, "unknown");
        };
        if !r.current {
            return (None, "unknown"); // an earlier activation of a reused RS: no time kept
        }
        let never_reused = annotation_list(r.rs, REVISION_HISTORY).is_empty();
        if never_reused {
            return (r.rs.creation_timestamp().map(|t| t.0), "creationTimestamp");
        }
        let rs_name = r.rs.name_any();
        let Some(t) = scale.latest_up_from_zero(&rs_name) else {
            return (None, "unknown");
        };
        // Accept only a real switch of templates: the predecessor scaled down to 0 at/after.
        let pred = revs.range(..n).next_back().map(|(_, p)| p.rs.name_any());
        let switched = pred.is_some_and(|p| scale.down_to_zero_at_or_after(&p, t));
        let earliest_pod = live_pods
            .iter()
            .filter(|p| p.metadata.deletion_timestamp.is_none())
            .filter(|p| p.owner_references().iter().any(|o| o.name == rs_name))
            .filter_map(|p| p.creation_timestamp().map(|c| c.0))
            .min();
        let sane = earliest_pod.map_or(true, |e| t <= e + Duration::seconds(1));
        if switched && sane {
            (Some(t), "event")
        } else {
            (None, "unknown")
        }
    };

    // Which revisions to report: those activated in [window.start, capture], else the pod's.
    let in_range = |t: Option<DateTime<Utc>>| t.map(|t| t >= ctx.window_start && t <= ctx.capture);
    let mut targets: Vec<i64> = revs
        .keys()
        .copied()
        .filter(|&n| in_range(activation(n).0) == Some(true))
        .collect();
    if targets.is_empty() {
        targets.push(pod_rev);
    }

    let mut entries = Vec::new();
    let mut timeline = Vec::new();
    let tamper = tamper_warnings(&owned);
    for (i, &n) in targets.iter().enumerate() {
        let (changed_at, source) = activation(n);
        let mut entry = json!({
            "namespace": ns, "kind": "Deployment", "name": dep_ref.name,
            "source": "replicaset-history",
            "changed_at": changed_at.map(|t| t.to_rfc3339()),
            "changed_at_source": source,
            "seconds_relative_to_firing": changed_at.map(|t| (t - ctx.firing).num_seconds()),
            "after_firing": changed_at.map(|t| t > ctx.firing),
            "in_range": in_range(changed_at),
            "pod_revision_is_current": pod_rev == newest,
            "warnings": tamper,
        });
        let Some(after) = revs.get(&n) else {
            entry["status"] = json!("error");
            entry["reason"] = json!(format!("revision {n} not found"));
            entries.push(entry);
            continue;
        };
        entry["after"] = json!({ "revision": n.to_string(), "object": format!("ReplicaSet/{}", after.rs.name_any()) });
        let Some(before) = n.checked_sub(1).and_then(|p| revs.get(&p)) else {
            entry["status"] = json!("before_unknown");
            entry["reason"] = json!(if n <= 1 {
                "first revision: nothing before it".to_string()
            } else {
                format!(
                    "revision {} not retained (pruned, or revisionHistoryLimit)",
                    n - 1
                )
            });
            entries.push(entry);
            continue;
        };
        entry["before"] = json!({ "revision": (n - 1).to_string(), "object": format!("ReplicaSet/{}", before.rs.name_any()) });

        let file = format!(
            "diffs/{}/Deployment/{}/{i}.json",
            safe(ns),
            safe(&dep_ref.name)
        );
        let (changes, rendered) = diff_pair(
            &template_of(before.rs),
            &template_of(after.rs),
            ctx.redactor,
            &file,
        );
        if changes.is_empty() {
            entry["status"] = json!("no_change");
        } else {
            entry["status"] = json!("ok");
            entry["kind_of_change"] = json!(if specdiff::is_restart_only(&changes) {
                "restart-only-template"
            } else {
                "spec"
            });
            if specdiff::is_restart_only(&changes) {
                entry["may_apply_out_of_band"] = json!([
                    "ConfigMap/Secret contents read via env or subPath",
                    "mutable image tags (imagePullPolicy: Always)"
                ]);
            }
            let summary = specdiff::summary(&rendered, 5);
            let (actor, actor_reason) = actor_for(&dep, before.rs, after.rs, changed_at);
            entry["actor"] = json!(actor);
            entry["actor_kind"] = json!("fieldManager (client-asserted)");
            if let Some(r) = actor_reason {
                entry["actor_reason"] = json!(r);
            }
            entry["summary"] = json!(summary);
            entry["file"] = json!(file);
            write_file(stage_dir, &file, &rendered)?;
            note_in_place_config(&mut entry, &rendered);
            if ctx.config_maps {
                let cms = configmap_entries(client, ns, &rendered, &entry, ctx, stage_dir).await;
                entries.extend(cms);
            }
            if let Some(t) = changed_at {
                timeline.push(json!({
                    "ts": t.to_rfc3339(), "source": "change", "type": "Normal",
                    "reason": "Rollout",
                    "message": format!("Deployment/{} revision {} → {}{}: {}",
                        dep_ref.name, n - 1, n,
                        actor.map(|a| format!(" by {a}")).unwrap_or_default(),
                        summary.join("; ")),
                }));
            }
        }
        entries.push(entry);
    }

    // Pending edits: the Deployment's template vs the newest ReplicaSet's.
    if let Some(newest_rs) = revs.get(&newest).map(|r| r.rs) {
        let dep_template = dep
            .spec
            .as_ref()
            .map(|s| serde_json::to_value(&s.template).unwrap_or_default())
            .unwrap_or_default();
        let unsynced =
            dep.status.as_ref().and_then(|s| s.observed_generation) < dep.metadata.generation;
        let file = format!(
            "diffs/{}/Deployment/{}/pending.json",
            safe(ns),
            safe(&dep_ref.name)
        );
        let (changes, rendered) =
            diff_pair(&template_of(newest_rs), &dep_template, ctx.redactor, &file);
        if !changes.is_empty() || unsynced {
            write_file(stage_dir, &file, &rendered)?;
            entries.push(json!({
                "namespace": ns, "kind": "Deployment", "name": dep_ref.name,
                "status": if changes.is_empty() { "no_change" } else { "ok" },
                "source": "deployment-spec-pending",
                "reason": if dep.spec.as_ref().and_then(|s| s.paused).unwrap_or(false) {
                    "rollout paused"
                } else {
                    "controller has not rolled this template out yet"
                },
                "before": { "revision": newest.to_string(), "object": format!("ReplicaSet/{}", newest_rs.name_any()) },
                "after": { "object": format!("Deployment/{}", dep_ref.name) },
                "summary": specdiff::summary(&rendered, 5),
                "file": file,
            }));
        }
    }

    Ok(DeploymentResult {
        expected,
        entries,
        timeline,
    })
}

/// StatefulSet / DaemonSet: history lives in ControllerRevisions. `.data` is a
/// strategic-merge wrapper around the template and is immutable; a rollback re-uses the
/// matching revision and only bumps its `.revision`, so the revision's latest managedFields
/// write (only the controller writes it) is when it became the live template.
async fn revision_entries(
    client: &Client,
    ns: &str,
    pod: &Pod,
    kind: &str,
    name: &str,
    ctx: &DiffCtx<'_>,
    stage_dir: &Path,
) -> anyhow::Result<(Vec<Value>, Vec<Value>)> {
    let (uid, managed_fields) = match kind {
        "StatefulSet" => {
            let o: StatefulSet = Api::namespaced(client.clone(), ns).get(name).await?;
            (o.uid(), o.metadata.managed_fields)
        }
        _ => {
            let o: DaemonSet = Api::namespaced(client.clone(), ns).get(name).await?;
            (o.uid(), o.metadata.managed_fields)
        }
    };
    let uid = uid.unwrap_or_default();
    let managed_fields = managed_fields.unwrap_or_default();

    let crs: Api<ControllerRevision> = Api::namespaced(client.clone(), ns);
    let all = crs.list(&ListParams::default()).await?.items;
    let revs: BTreeMap<i64, &ControllerRevision> = all
        .iter()
        .filter(|cr| cr.owner_references().iter().any(|o| o.uid == uid))
        .map(|cr| (cr.revision, cr))
        .collect();

    // Pod → its revision: the `controller-revision-hash` label is the CR name (StatefulSet)
    // or its hash suffix (DaemonSet).
    let hash = pod
        .labels()
        .get("controller-revision-hash")
        .cloned()
        .unwrap_or_default();
    let pod_rev = revs
        .iter()
        .find(|(_, cr)| {
            let n = cr.name_any();
            n == hash || n.ends_with(&format!("-{hash}"))
        })
        .map(|(n, _)| *n);
    let newest = revs.keys().max().copied();

    let activation = |cr: &ControllerRevision| -> Option<DateTime<Utc>> {
        let created = cr.creation_timestamp().map(|t| t.0);
        let last_write = cr
            .metadata
            .managed_fields
            .iter()
            .flatten()
            .filter_map(|mf| mf.time.as_ref().map(|t| t.0))
            .max();
        created.into_iter().chain(last_write).max()
    };
    let in_range = |t: Option<DateTime<Utc>>| t.map(|t| t >= ctx.window_start && t <= ctx.capture);

    let mut targets: Vec<i64> = revs
        .iter()
        .filter(|(_, cr)| in_range(activation(cr)) == Some(true))
        .map(|(n, _)| *n)
        .collect();
    if targets.is_empty() {
        targets.extend(pod_rev);
    }

    let mut entries = Vec::new();
    let mut timeline = Vec::new();
    for (i, &n) in targets.iter().enumerate() {
        let after = revs[&n];
        let changed_at = activation(after);
        let mut entry = json!({
            "namespace": ns, "kind": kind, "name": name,
            "source": "controllerrevision",
            "after": { "revision": n.to_string(), "object": format!("ControllerRevision/{}", after.name_any()) },
            "changed_at": changed_at.map(|t| t.to_rfc3339()),
            "changed_at_source": "managedFields",
            "seconds_relative_to_firing": changed_at.map(|t| (t - ctx.firing).num_seconds()),
            "after_firing": changed_at.map(|t| t > ctx.firing),
            "in_range": in_range(changed_at),
            "pod_revision_is_current": pod_rev.is_some() && pod_rev == newest,
            "warnings": [],
        });
        // The revision before n that still exists. A gap means pruned, or re-used by a
        // rollback (its number moved up): either way n-1's template is not retained as n-1.
        let Some(before) = revs.get(&(n - 1)) else {
            entry["status"] = json!("before_unknown");
            entry["reason"] = json!(if n <= 1 {
                "first revision: nothing before it".to_string()
            } else {
                format!(
                    "revision {} not retained (pruned, or re-used by a rollback)",
                    n - 1
                )
            });
            entries.push(entry);
            continue;
        };
        entry["before"] = json!({ "revision": (n - 1).to_string(), "object": format!("ControllerRevision/{}", before.name_any()) });
        let file = format!("diffs/{}/{kind}/{}/{i}.json", safe(ns), safe(name));
        let (changes, mut rendered) = diff_pair(
            &unwrap_revision(before),
            &unwrap_revision(after),
            ctx.redactor,
            &file,
        );
        if changes.is_empty() {
            entry["status"] = json!("no_change");
            entries.push(entry);
            continue;
        }
        specdiff::mark_probable_defaults(&mut rendered);
        entry["status"] = json!("ok");
        entry["kind_of_change"] = json!(if specdiff::is_restart_only(&changes) {
            "restart-only-template"
        } else {
            "spec"
        });
        let (actor, actor_reason) = match changed_at {
            Some(t) => template_owner_in(
                &managed_fields,
                t - Duration::seconds(5),
                t + Duration::seconds(5),
            ),
            None => (None, Some("change time unknown")),
        };
        let summary = specdiff::summary(&rendered, 5);
        entry["actor"] = json!(actor);
        entry["actor_kind"] = json!("fieldManager (client-asserted)");
        if let Some(r) = actor_reason {
            entry["actor_reason"] = json!(r);
        }
        entry["summary"] = json!(summary);
        entry["file"] = json!(file);
        write_file(stage_dir, &file, &rendered)?;
        note_in_place_config(&mut entry, &rendered);
        if ctx.config_maps {
            let cms = configmap_entries(client, ns, &rendered, &entry, ctx, stage_dir).await;
            entries.extend(cms);
        }
        if let Some(t) = changed_at {
            timeline.push(json!({
                "ts": t.to_rfc3339(), "source": "change", "type": "Normal", "reason": "Rollout",
                "message": format!("{kind}/{name} revision {} → {n}{}: {}", n - 1,
                    actor.map(|a| format!(" by {a}")).unwrap_or_default(), summary.join("; ")),
            }));
        }
        entries.push(entry);
    }
    Ok((entries, timeline))
}

/// A `checksum/*` pod-template annotation changed (Helm's "roll on config change"): the
/// referenced ConfigMap/Secret was overwritten in place, and its previous content is gone.
fn note_in_place_config(entry: &mut Value, rendered: &[Value]) {
    let checksums: Vec<&str> = rendered
        .iter()
        .filter_map(|c| c["display"].as_str())
        .filter(|d| d.starts_with("metadata.annotations.checksum/"))
        .collect();
    if !checksums.is_empty() {
        entry["notes"] = json!(checksums
            .iter()
            .map(|d| format!(
                "{d} changed: a referenced ConfigMap/Secret was overwritten in place; its \
                 previous content is not retained, so it cannot be diffed"
            ))
            .collect::<Vec<_>>());
    }
}

/// Layer 1.5: for every ConfigMap reference whose *name* changed in this template diff
/// (kustomize hash suffixes, immutable ConfigMaps), GET both and diff them key by key.
async fn configmap_entries(
    client: &Client,
    ns: &str,
    rendered: &[Value],
    parent: &Value,
    ctx: &DiffCtx<'_>,
    stage_dir: &Path,
) -> Vec<Value> {
    let refs = rendered.iter().filter(|c| {
        c["op"] == "replace"
            && c["display"].as_str().is_some_and(|d| {
                d.ends_with(".configMap.name")
                    || d.ends_with(".configMapRef.name")
                    || d.ends_with(".configMapKeyRef.name")
            })
    });
    let api: Api<ConfigMap> = Api::namespaced(client.clone(), ns);
    let mut out = Vec::new();
    for r in refs {
        let (Some(old), Some(new)) = (r["before"].as_str(), r["after"].as_str()) else {
            continue;
        };
        let mut entry = json!({
            "namespace": ns, "kind": "ConfigMap", "name": new,
            "source": "configmap-rename",
            "referenced_by": format!("{}/{} {}", parent["kind"].as_str().unwrap_or("?"),
                parent["name"].as_str().unwrap_or("?"), r["display"].as_str().unwrap_or("?")),
            "before": { "object": format!("ConfigMap/{old}") },
            "after": { "object": format!("ConfigMap/{new}") },
            "changed_at": parent["changed_at"], "changed_at_source": parent["changed_at_source"],
            "seconds_relative_to_firing": parent["seconds_relative_to_firing"],
            "after_firing": parent["after_firing"], "in_range": parent["in_range"],
        });
        let (before, after) = match (api.get_opt(old).await, api.get_opt(new).await) {
            (Ok(Some(b)), Ok(Some(a))) => (b, a),
            (Ok(None), Ok(Some(_))) => {
                entry["status"] = json!("before_unknown");
                entry["reason"] = json!("old ConfigMap no longer exists (pruned)");
                out.push(entry);
                continue;
            }
            (Ok(_), Ok(None)) => {
                entry["status"] = json!("error");
                entry["reason"] = json!("the referenced ConfigMap does not exist");
                out.push(entry);
                continue;
            }
            (Err(e), _) | (_, Err(e)) => {
                entry["status"] = json!("error");
                entry["reason"] = json!(e.to_string());
                out.push(entry);
                continue;
            }
        };
        let file = format!("diffs/{}/ConfigMap/{}/0.json", safe(ns), safe(new));
        let changes = configmap_changes(&before, &after, ctx.redactor, &file);
        if changes.is_empty() {
            entry["status"] = json!("no_change");
        } else {
            entry["status"] = json!("ok");
            entry["summary"] = json!(configmap_summary(&changes));
            entry["file"] = json!(file);
            if let Err(e) = write_file(stage_dir, &file, &changes) {
                entry["status"] = json!("error");
                entry["reason"] = json!(e.to_string());
            }
        }
        out.push(entry);
    }
    out
}

/// Key-level changes between two ConfigMaps. `changed` is computed on raw values; what is
/// written is redacted. Multi-line values also get the lines removed/added (redacted).
fn configmap_changes(
    before: &ConfigMap,
    after: &ConfigMap,
    redactor: &Redactor,
    file: &str,
) -> Vec<Value> {
    let mut tally = kairn_bundle::redact::Tally::default();
    let mut out = Vec::new();
    let empty = BTreeMap::new();
    let (bd, ad) = (
        before.data.as_ref().unwrap_or(&empty),
        after.data.as_ref().unwrap_or(&empty),
    );
    let keys: std::collections::BTreeSet<&String> = bd.keys().chain(ad.keys()).collect();
    for k in keys {
        let (b, a) = (bd.get(k), ad.get(k));
        if b == a {
            continue;
        }
        let red = |v: Option<&String>, t: &mut kairn_bundle::redact::Tally| {
            v.map(|v| redactor.policy.redact_config_value(k, v, t))
        };
        let (rb, ra) = (red(b, &mut tally), red(a, &mut tally));
        let mut c = json!({
            "op": match (b, a) { (None, _) => "add", (_, None) => "remove", _ => "replace" },
            "display": format!("data[{k}]"),
            "changed": true,
        });
        if let Some(v) = &rb {
            c["before"] = json!(v);
        }
        if let Some(v) = &ra {
            c["after"] = json!(v);
        }
        if let (Some(rb), Some(ra)) = (&rb, &ra) {
            if rb.contains('\n') || ra.contains('\n') {
                let bl: Vec<&str> = rb.lines().collect();
                let al: Vec<&str> = ra.lines().collect();
                c["lines_removed"] =
                    json!(bl.iter().filter(|l| !al.contains(l)).collect::<Vec<_>>());
                c["lines_added"] = json!(al.iter().filter(|l| !bl.contains(l)).collect::<Vec<_>>());
            }
        }
        out.push(c);
    }
    // binaryData is never shown: only whether each key changed.
    let emptyb = BTreeMap::new();
    let (bb, ab) = (
        before.binary_data.as_ref().unwrap_or(&emptyb),
        after.binary_data.as_ref().unwrap_or(&emptyb),
    );
    let bkeys: std::collections::BTreeSet<&String> = bb.keys().chain(ab.keys()).collect();
    for k in bkeys {
        let (b, a) = (bb.get(k).map(|v| &v.0), ab.get(k).map(|v| &v.0));
        if b != a {
            tally.values += 1;
            out.push(json!({
                "op": match (b, a) { (None, _) => "add", (_, None) => "remove", _ => "replace" },
                "display": format!("binaryData[{k}]"),
                "before": b.map(|_| kairn_bundle::redact::REDACTED),
                "after": a.map(|_| kairn_bundle::redact::REDACTED),
                "changed": true,
            }));
        }
    }
    redactor.record(file, tally);
    out
}

fn configmap_summary(changes: &[Value]) -> Vec<String> {
    let mut lines: Vec<String> = changes
        .iter()
        .take(5)
        .map(|c| {
            let d = c["display"].as_str().unwrap_or("?");
            if let (Some(r), Some(a)) = (c["lines_removed"].as_array(), c["lines_added"].as_array())
            {
                match (
                    r.first().and_then(Value::as_str),
                    a.first().and_then(Value::as_str),
                ) {
                    (Some(r0), Some(a0)) if r.len() == 1 && a.len() == 1 => {
                        format!("{d}: `{}` → `{}`", r0.trim(), a0.trim())
                    }
                    _ => format!("{d}: −{} +{} lines", r.len(), a.len()),
                }
            } else {
                let s = |v: &Value| {
                    v.as_str()
                        .map(str::to_string)
                        .unwrap_or_else(|| v.to_string())
                };
                match c["op"].as_str() {
                    Some("add") => format!("{d}: + {}", s(&c["after"])),
                    Some("remove") => format!("{d}: − {}", s(&c["before"])),
                    _ => format!("{d}: {} → {}", s(&c["before"]), s(&c["after"])),
                }
            }
        })
        .collect();
    if changes.len() > 5 {
        lines.push(format!("(+{} more)", changes.len() - 5));
    }
    lines
}

/// `ControllerRevision.data` = `{"spec":{"template":{…,"$patch":"replace"}}}` → the template.
fn unwrap_revision(cr: &ControllerRevision) -> Value {
    let mut t = cr
        .data
        .as_ref()
        .map(|d| d.0["spec"]["template"].clone())
        .unwrap_or_default();
    if let Some(m) = t.as_object_mut() {
        m.remove("$patch");
    }
    t
}

/// Diff two templates: detect on raw, render values from redacted copies.
fn diff_pair(
    before: &Value,
    after: &Value,
    redactor: &Redactor,
    file: &str,
) -> (Vec<specdiff::Change>, Vec<Value>) {
    let changes = specdiff::diff(&specdiff::normalize(before), &specdiff::normalize(after));
    let red = |t: &Value| {
        let mut wrapped = json!({ "spec": { "template": t } });
        let tally = redactor.policy.redact_object(&mut wrapped);
        redactor.record(file, tally);
        specdiff::normalize(&wrapped["spec"]["template"])
    };
    let rendered = specdiff::render(&changes, &red(before), &red(after));
    (changes, rendered)
}

/// Actor: the Deployment's `f:spec.f:template` owner whose last write matches this change.
fn actor_for(
    dep: &Deployment,
    before: &ReplicaSet,
    after: &ReplicaSet,
    activation: Option<DateTime<Utc>>,
) -> (Option<String>, Option<&'static str>) {
    // How long the old pods may take to stop: Recreate creates the new ReplicaSet only after
    // they are gone, and a rollback scales the old one up only then (measured on kind).
    let grace = before
        .spec
        .as_ref()
        .and_then(|s| s.template.as_ref())
        .and_then(|t| t.spec.as_ref())
        .and_then(|s| s.termination_grace_period_seconds)
        .unwrap_or(30);
    let recreate = dep
        .spec
        .as_ref()
        .and_then(|s| s.strategy.as_ref())
        .and_then(|s| s.type_.as_deref())
        == Some("Recreate");
    let window = if annotation_list(after, REVISION_HISTORY).is_empty() {
        after.creation_timestamp().map(|c| {
            if recreate {
                (
                    c.0 - Duration::seconds(grace + 5),
                    c.0 + Duration::seconds(1),
                )
            } else {
                (c.0 - Duration::seconds(5), c.0 + Duration::seconds(5))
            }
        })
    } else {
        activation.map(|t| (t - Duration::seconds(grace + 5), t + Duration::seconds(1)))
    };
    let Some((lo, hi)) = window else {
        return (None, Some("change time unknown"));
    };
    template_owner_in(
        dep.metadata.managed_fields.as_deref().unwrap_or_default(),
        lo,
        hi,
    )
}

/// Template owners whose last write falls in `[lo, hi]`. The revision object is created
/// right after the write that triggered it, so the latest such write is the trigger; an
/// equal time is a tie and yields no name.
fn template_owner_in(
    managed_fields: &[ManagedFieldsEntry],
    lo: DateTime<Utc>,
    hi: DateTime<Utc>,
) -> (Option<String>, Option<&'static str>) {
    let mut owners: Vec<(DateTime<Utc>, String)> = managed_fields
        .iter()
        .filter(|mf| {
            mf.fields_v1
                .as_ref()
                .and_then(|f| f.0.get("f:spec"))
                .is_some_and(|s| s.get("f:template").is_some())
        })
        .filter_map(|mf| Some((mf.time.as_ref()?.0, mf.manager.clone()?)))
        .filter(|(t, _)| *t >= lo && *t <= hi)
        .collect();
    owners.sort();
    match owners.as_slice() {
        [] => (
            None,
            Some("template owner's last write does not match this change"),
        ),
        [.., (t1, _), (t2, _)] if t1 == t2 => (None, Some("several template owners tie")),
        [.., (_, latest)] => (Some(latest.clone()), None),
    }
}

/// A ReplicaSet template written by anyone but the controller (spoofable hint only).
fn tamper_warnings(rs: &[&ReplicaSet]) -> Vec<String> {
    rs.iter()
        .flat_map(|r| {
            r.metadata.managed_fields.iter().flatten().filter_map(move |mf| {
                let writes_template = mf
                    .fields_v1
                    .as_ref()
                    .and_then(|f| f.0.get("f:spec"))
                    .is_some_and(|s| s.get("f:template").is_some());
                let manager = mf.manager.clone().unwrap_or_default();
                (writes_template && manager != CONTROLLER_MANAGER).then(|| {
                    format!(
                        "ReplicaSet/{} template written by {manager} at {} (hint; manager names are spoofable)",
                        r.name_any(),
                        mf.time.as_ref().map(|t| t.0.to_rfc3339()).unwrap_or_default()
                    )
                })
            })
        })
        .collect()
}

/// `ScalingReplicaSet` events of one Deployment, parsed.
struct ScaleEvents {
    items: Vec<(String, bool, i64, DateTime<Utc>)>, // (rs, up?, to, time)
}

impl ScaleEvents {
    fn from(evs: &[Event]) -> Self {
        let items = evs
            .iter()
            .filter(|e| e.reason.as_deref() == Some("ScalingReplicaSet"))
            .filter_map(|e| {
                let msg = e.message.as_deref()?;
                let msg = msg
                    .strip_prefix("(combined from similar events): ")
                    .unwrap_or(msg);
                let (up, rest) = if let Some(r) = msg.strip_prefix("Scaled up replica set ") {
                    (true, r)
                } else {
                    (false, msg.strip_prefix("Scaled down replica set ")?)
                };
                // The wording differs across Kubernetes versions (measured on kind):
                //   v1.37: "<rs> from <a> to <b>"
                //   v1.30: "<rs> to <b> from <a>", or "<rs> to <b>" for a brand-new RS
                let words: Vec<&str> = rest.split_whitespace().collect();
                let rs = words.first()?.to_string();
                let num = |w: &str| w.parse::<i64>().ok();
                let (from, to) = match words.get(1..)? {
                    ["from", a, "to", b, ..] => (num(a), num(b)?),
                    ["to", b, "from", a, ..] => (num(a), num(b)?),
                    ["to", b, ..] => (None, num(b)?),
                    _ => return None,
                };
                let time = [
                    e.last_timestamp.as_ref().map(|t| t.0),
                    e.series
                        .as_ref()
                        .and_then(|s| s.last_observed_time.as_ref())
                        .map(|t| t.0),
                    e.event_time.as_ref().map(|t| t.0),
                    e.first_timestamp.as_ref().map(|t| t.0),
                ]
                .into_iter()
                .flatten()
                .max()?;
                // Keep only transitions that matter: up from 0, down to 0.
                ((up && from == Some(0)) || (!up && to == 0)).then_some((rs, up, to, time))
            })
            .collect();
        Self { items }
    }

    fn latest_up_from_zero(&self, rs: &str) -> Option<DateTime<Utc>> {
        self.items
            .iter()
            .filter(|(r, up, _, _)| r == rs && *up)
            .map(|(_, _, _, t)| *t)
            .max()
    }

    fn down_to_zero_at_or_after(&self, rs: &str, t: DateTime<Utc>) -> bool {
        self.items
            .iter()
            .any(|(r, up, _, time)| r == rs && !*up && *time >= t)
    }
}

// --- helpers -----------------------------------------------------------------------------

fn template_of(rs: &ReplicaSet) -> Value {
    rs.spec
        .as_ref()
        .and_then(|s| s.template.as_ref())
        .map(|t| serde_json::to_value(t).unwrap_or_default())
        .unwrap_or_default()
}

fn annotation_i64(rs: &ReplicaSet, key: &str) -> Option<i64> {
    rs.annotations().get(key)?.parse().ok()
}

fn annotation_list(rs: &ReplicaSet, key: &str) -> Vec<i64> {
    rs.annotations()
        .get(key)
        .map(|v| v.split(',').filter_map(|n| n.trim().parse().ok()).collect())
        .unwrap_or_default()
}

fn obj_ref(ns: &str, kind: &str, name: &str) -> Value {
    json!({ "namespace": ns, "kind": kind, "name": name })
}

fn status_entry(ns: &str, kind: &str, name: &str, status: &str, reason: &str) -> Value {
    json!({ "namespace": ns, "kind": kind, "name": name, "status": status, "reason": reason })
}

/// Path components come from object names (DNS-1123, so already safe); guard anyway with the
/// same rules `kairn verify` applies on unpack.
fn safe(component: &str) -> String {
    if component.is_empty()
        || component == "."
        || component == ".."
        || component.contains(['/', '\\', '\0'])
    {
        use sha2::{Digest, Sha256};
        let h = Sha256::digest(component.as_bytes());
        h.iter().take(8).map(|b| format!("{b:02x}")).collect()
    } else {
        component.to_string()
    }
}

fn write_file(stage_dir: &Path, rel: &str, v: &[Value]) -> anyhow::Result<()> {
    let path = stage_dir.join(rel);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::write(path, serde_json::to_vec_pretty(v)?)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use k8s_openapi::apimachinery::pkg::apis::meta::v1::Time;

    fn ev(msg: &str, first: &str, last: &str) -> Event {
        Event {
            reason: Some("ScalingReplicaSet".into()),
            message: Some(msg.into()),
            first_timestamp: Some(Time(first.parse().unwrap())),
            last_timestamp: Some(Time(last.parse().unwrap())),
            ..Default::default()
        }
    }

    /// The exact event stream measured on kind: rollout, scale 1→3, rollback (coalesced).
    #[test]
    fn rollback_activation_is_the_latest_coalesced_timestamp() {
        let evs = vec![
            ev(
                "Scaled up replica set web-6dfd from 0 to 1",
                "2026-09-19T04:53:28Z",
                "2026-09-19T04:54:07Z",
            ),
            ev(
                "Scaled up replica set web-5564 from 0 to 1",
                "2026-09-19T04:53:38Z",
                "2026-09-19T04:53:38Z",
            ),
            ev(
                "Scaled down replica set web-6dfd from 1 to 0",
                "2026-09-19T04:53:38Z",
                "2026-09-19T04:53:38Z",
            ),
            ev(
                "Scaled up replica set web-5564 from 1 to 3",
                "2026-09-19T04:53:58Z",
                "2026-09-19T04:53:58Z",
            ),
            ev(
                "Scaled down replica set web-5564 from 1 to 0",
                "2026-09-19T04:54:08Z",
                "2026-09-19T04:54:08Z",
            ),
        ];
        let s = ScaleEvents::from(&evs);
        let t = s.latest_up_from_zero("web-6dfd").unwrap();
        assert_eq!(t.to_rfc3339(), "2026-09-19T04:54:07+00:00");
        assert!(s.down_to_zero_at_or_after("web-5564", t));
        // 1→3 is a scale, not an activation.
        assert_eq!(
            s.latest_up_from_zero("web-5564").unwrap().to_rfc3339(),
            "2026-09-19T04:53:38+00:00"
        );
    }

    /// Kubernetes v1.30 words it "to <b> from <a>" (measured on kind v1.30.0).
    #[test]
    fn older_kubernetes_event_wording_is_understood() {
        let evs = vec![
            ev(
                "Scaled up replica set w-86f6 to 1",
                "2026-09-19T07:37:40Z",
                "2026-09-19T07:37:40Z",
            ),
            ev(
                "Scaled up replica set w-7879 to 1",
                "2026-09-19T07:37:51Z",
                "2026-09-19T07:37:51Z",
            ),
            ev(
                "Scaled down replica set w-86f6 to 0 from 1",
                "2026-09-19T07:37:51Z",
                "2026-09-19T07:37:51Z",
            ),
            ev(
                "Scaled up replica set w-86f6 to 1 from 0",
                "2026-09-19T07:37:52Z",
                "2026-09-19T07:37:52Z",
            ),
            ev(
                "Scaled down replica set w-7879 to 0 from 1",
                "2026-09-19T07:37:52Z",
                "2026-09-19T07:37:52Z",
            ),
        ];
        let s = ScaleEvents::from(&evs);
        let t = s.latest_up_from_zero("w-86f6").unwrap();
        assert_eq!(t.to_rfc3339(), "2026-09-19T07:37:52+00:00");
        assert!(s.down_to_zero_at_or_after("w-7879", t));
        // "to 1" without "from": not provably a scale-up from zero.
        assert!(s.latest_up_from_zero("w-7879").is_none());
    }

    #[test]
    fn aggregated_event_prefix_is_accepted() {
        let evs = vec![ev(
            "(combined from similar events): Scaled up replica set web-a from 0 to 2",
            "2026-09-19T01:00:00Z",
            "2026-09-19T01:05:00Z",
        )];
        assert!(ScaleEvents::from(&evs)
            .latest_up_from_zero("web-a")
            .is_some());
    }

    #[test]
    fn configmap_changes_are_key_level_and_redacted() {
        use k8s_openapi::ByteString;
        let cm = |mode: &str, pw: &str, bin: &[u8]| ConfigMap {
            data: Some(BTreeMap::from([
                ("MODE".to_string(), mode.to_string()),
                (
                    "application.yaml".to_string(),
                    format!("cache:\n  mode: {mode}\ndb:\n  password: {pw}\n"),
                ),
            ])),
            binary_data: Some(BTreeMap::from([(
                "keystore.p12".to_string(),
                ByteString(bin.to_vec()),
            )])),
            ..Default::default()
        };
        let redactor = Redactor::new(kairn_bundle::redact::Policy::default());
        let changes = configmap_changes(
            &cm("lazy", "oldCanary7Qx2Lp9w", b"old-key-bytes"),
            &cm("eager", "newCanary7Qx2Lp9w", b"new-key-bytes"),
            &redactor,
            "diffs/x/ConfigMap/y/0.json",
        );
        let out = serde_json::to_string(&changes).unwrap();
        assert!(!out.contains("Canary7Qx2Lp9w"), "{out}");
        assert!(!out.contains("key-bytes"), "{out}");
        assert_eq!(
            configmap_summary(&changes),
            [
                "data[MODE]: lazy → eager",
                "data[application.yaml]: `mode: lazy` → `mode: eager`",
                "binaryData[keystore.p12]: <redacted> → <redacted>",
            ]
        );
    }

    #[test]
    fn unsafe_path_components_are_hashed() {
        assert_eq!(safe("checkout"), "checkout");
        assert_ne!(safe("../etc"), "../etc");
        assert_eq!(safe("..").len(), 16);
    }
}
