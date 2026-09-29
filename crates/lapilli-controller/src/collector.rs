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

/// Fields that policy v1 leaves in plaintext **inside** the files it does redact — the object
/// bodies under `resources/`, both sides of every `diffs/**` entry, and object metadata. The
/// policy's candidate set is env values, `command`/`args` (including `sh -c` scripts and exec
/// probes), probe and lifecycle HTTP header values, annotations and event messages; everything
/// else in an object is written as the API returned it, in `default` and in `strict` alike.
///
/// This list is what makes `redaction.json` readable as a privacy statement instead of an
/// invitation to assume the complement of `not_redacted` was cleaned. It is deliberately about
/// *classes of value a reader might expect to be removed* — identities, addresses, actors,
/// references — not an exhaustive schema walk, which would go stale on every Kubernetes minor.
/// Every entry is a field path in a captured API object, so one list covers `resources/**` and
/// both sides of `diffs/**`. The prose version, with what each field carries in a real cluster
/// and why it is left readable, is [`docs/data-handling.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/data-handling.md); two things it records cannot be said
/// as an object field path and are stated there instead: in `events.json` and `timeline.json`
/// only the event `message` passes through redaction (`involvedObject`, `source.host`,
/// `reportingComponent`/`reportingInstance` and the Event's own metadata do not), and
/// `manifest.json`'s `incident.target` is the namespace and pod in plain text by design.
const NOT_REDACTED_FIELDS: &[&str] = &[
    "metadata.labels",
    "spec.template.metadata.labels",
    "metadata.managedFields",
    "metadata.ownerReferences",
    "spec.containers[].image",
    "spec.initContainers[].image",
    "spec.imagePullSecrets[].name",
    "spec.containers[].env[].valueFrom",
    "spec.containers[].envFrom[]",
    "spec.nodeName",
    "spec.nodeSelector",
    "spec.tolerations",
    "spec.affinity",
    "spec.serviceAccountName",
    "spec.volumes[]",
    "status.podIP",
    "status.podIPs[]",
    "status.hostIP",
    "status.hostIPs[]",
];

/// Applies the redaction policy at the source and keeps the per-file tally for
/// `redaction.json`.
pub struct Redactor {
    pub policy: lapilli_bundle::redact::Policy,
    files: std::sync::Mutex<std::collections::BTreeMap<String, usize>>,
    dropped: std::sync::Mutex<Vec<String>>,
}

impl Redactor {
    pub fn new(policy: lapilli_bundle::redact::Policy) -> Self {
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
        let mut t = lapilli_bundle::redact::Tally::default();
        let out = self.policy.redact_text(text, &mut t);
        self.record(rel, t);
        out
    }

    pub(crate) fn record(&self, rel: &str, t: lapilli_bundle::redact::Tally) {
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
    ///
    /// `not_redacted` used to be the whole statement, and it listed two path prefixes. A consumer
    /// reading it concluded that everything *else* had been redacted, which is not what policy v1
    /// does: inside the files it does visit it redacts env values, argv, probe and lifecycle
    /// header values, annotations and event messages, and it leaves the rest of the object alone.
    /// Labels, image references, IP addresses, `nodeName`, `serviceAccountName`, `managedFields`
    /// and object references (`secretKeyRef`/`imagePullSecrets` *names*) all survive in every
    /// mode, `strict` included.
    ///
    /// Those are field paths, not path prefixes, and `not_redacted` can only hold paths without
    /// becoming two kinds of list in one array — a reader cannot tell whether `metadata.labels` is
    /// a file or a field. So they go in a sibling `not_redacted_fields`, and `not_redacted` keeps
    /// its meaning: whole trees the policy never visits. Both lists are **advisory** until
    /// [`spec/IEB-SPEC.md`](https://github.com/lapilli-project/lapilli/blob/main/spec/IEB-SPEC.md) makes them normative (§7 fixes only `mode`); a verifier does not check
    /// them, so a bundle that omits them is still valid and `lapilli mcp` reports them as recorded
    /// rather than as fact. See DESIGN §4, [`docs/design-change-diff.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/design-change-diff.md), and
    /// [`docs/data-handling.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/data-handling.md) for the prose these two lists are the machine-readable form of.
    pub fn report(&self) -> serde_json::Value {
        json!({
            "policy_version": lapilli_bundle::redact::POLICY_VERSION,
            "mode": self.policy.mode,
            "plaintext_names": self.policy.plaintext,
            "redacted_values": *self.files.lock().unwrap(),
            "dropped_fields": *self.dropped.lock().unwrap(),
            // Whole trees the policy never visits.
            "not_redacted": ["logs/", "metrics/"],
            // Fields that survive inside the files it does visit.
            "not_redacted_fields": NOT_REDACTED_FIELDS,
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

/// Per-instance byte bound on a log tail.
///
/// `tail_lines: Some(2000)` is not a memory bound, because a *line* is workload-controlled.
/// containerd and CRI-O split a log entry at **16 KiB**, and the kubelet hands each fragment
/// back as its own line, so 2000 lines is up to **32 MiB** for one container instance —
/// multiplied by the containers in the pod, by current+previous, and by
/// `reconcileConcurrency` captures in flight. The whole response is read into a `String`
/// before it is written. Against the chart's `resources.limits.memory: 256Mi`, whose envelope
/// was measured with busybox (whose entire log is one short line), that is a workload able to
/// invalidate a published bound and to OOM the controller mid-storm, taking every capture in
/// flight with it. The webhook's `MAX_BODY` exists against the same failure class.
///
/// 4 MiB is **2 KiB per line across the full 2000-line tail**, so for any workload whose
/// average line is under that the line bound stays the operative one and this constant never
/// engages. Worst case in flight at the default `reconcileConcurrency: 2` is 2 captures × one
/// body each, counted twice for the HTTP buffer and the `String` = **16 MiB**, about 6% of the
/// 256Mi limit and comfortably inside the ~130 MiB the storm measurement left unused. Unbounded,
/// the same arithmetic is 128 MiB, which the headroom does not cover.
///
/// The kubelet spends the byte budget **forward from the start of the tail window**, so a tail
/// this bound truncates is missing its *newest* lines. That is the expensive end, which is why
/// hitting it is recorded per instance in `logs/index.json` rather than left silent — see
/// [`truncation_note`].
const LOG_LIMIT_BYTES: i64 = 4 << 20;

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
    // Fetches that failed for a reason that is *not* "this workload has no such log".
    // Collected rather than returned early so `index.json` still records them, then turned
    // into a collector failure below. See the comment on the `Err` arm.
    let mut fetch_errors: Vec<String> = Vec::new();
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
                // Lines alone do not bound memory; see LOG_LIMIT_BYTES.
                limit_bytes: Some(LOG_LIMIT_BYTES),
                ..Default::default()
            };
            match pods.logs(&target.pod, &params).await {
                Ok(body) if is_kubelet_log_error(&body) => {
                    entry["unavailable"] = json!(body.trim());
                }
                Ok(body) => {
                    let file = format!("logs/{container}-{which}.log");
                    // A bounded read is not a failed read, but it must not read as a complete
                    // one either: the index carries the bound and the byte count.
                    if let Some(note) = truncation_note(body.len(), LOG_LIMIT_BYTES) {
                        entry["truncated"] = note;
                    }
                    std::fs::write(stage_dir.join(&file), body)?;
                    entry["file"] = json!(file);
                }
                // Anything else is the collector failing to do its job, not the workload
                // having nothing to say: a 403 because `pods/log` was not granted, the API
                // server refusing, a timeout. Recording it only in `index.json` used to leave
                // `logs` in `collectors_run`, so a bundle whose every log entry was
                // "...403 Forbidden..." verified OK at coverage 100% — while an `events`
                // denial, a hard `?` below, correctly gave PARTIAL. That asymmetry was the
                // defect (docs/design-review-round24.md §3).
                Err(e) => {
                    entry["unavailable"] = json!(e.to_string());
                    fetch_errors.push(format!("{container}/{which}: {e}"));
                }
            }
            instances.push(entry);
        }
        index.push(json!({ "container": container, "instances": instances }));
    }
    // Written before the failure so the denial itself is sealed and hashed, even though the
    // collector is about to be left out of `collectors_run`.
    write_json(
        &logs_dir.join("index.json"),
        &json!({ "containers": index }),
    )?;
    if !fetch_errors.is_empty() {
        anyhow::bail!(
            "could not read {} log(s): {}",
            fetch_errors.len(),
            fetch_errors.join("; ")
        );
    }
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

/// The `truncated` block for `logs/index.json`, or `None` if the tail fit inside the bound.
///
/// A log file on its own cannot say whether it is the whole tail, so a reader (and `lapilli
/// verify`'s summary, which reports "the crash's last words" as the last line of the newest
/// terminated instance's file) would take a truncated tail for a complete one. This is the same
/// place and the same shape as `unavailable`: a per-instance fact, present only when it happened.
///
/// It does **not** make the bundle PARTIAL. The bound is a producer setting that was honoured,
/// not a collector that failed — the same reason a 2000-line tail is not PARTIAL, and the same
/// reason `coverage.deferred` "does not change the verdict" (IEB-SPEC rule 6). `logs` stays in
/// `collectors_run`.
///
/// The comparison is `>=`, not `>`: the kubelet stops once the budget is spent, so a truncated
/// read comes back at exactly the limit. A tail that happens to be exactly `limit` bytes and was
/// *not* cut is therefore reported as truncated — a false positive in the direction evidence
/// should err, since the API itself only promises "slightly more or slightly less".
fn truncation_note(bytes: usize, limit: i64) -> Option<serde_json::Value> {
    (bytes as i64 >= limit).then(|| {
        json!({
            "limit_bytes": limit,
            "bytes": bytes,
            // The budget is spent forward from the start of the tail window, so what is
            // missing is the newest lines — possibly the crash's last words.
            "cut": "newest",
        })
    })
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
    // Through `sort_timeline`, not a plain `ts` comparison: this re-sorts the *whole* list, so a
    // string compare here undid the "no timestamp goes last" rule the events collector applied
    // and put an untimed event back on the top line. The default profile runs both `events` and
    // `changes`, so that was the default path.
    sort_timeline(&mut timeline);
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
    /// `redaction.json` is the bundle's machine-readable privacy statement, and it used to say
    /// only that `logs/` and `metrics/` were untouched — from which a consumer concluded the rest
    /// had been cleaned. The fields policy v1 leaves in plaintext inside the files it *does*
    /// redact are listed too, as field paths in their own list rather than stuffed into the
    /// path-prefix one.
    #[test]
    fn the_redaction_record_names_the_fields_it_leaves_in_plaintext() {
        let r = super::Redactor::new(lapilli_bundle::redact::Policy {
            mode: lapilli_bundle::redact::Mode::Strict,
            plaintext: vec!["LOG_LEVEL".into()],
        });
        let doc = r.report();
        assert_eq!(doc["mode"], "strict");
        assert_eq!(
            doc["not_redacted"],
            serde_json::json!(["logs/", "metrics/"]),
            "the path list keeps its meaning: whole trees the policy never visits"
        );
        let fields = doc["not_redacted_fields"].as_array().expect("field list");
        // Every claim in the strengthened prose has an entry here: a compliance reader is told
        // what survives, in `strict` as much as in `default`.
        for want in [
            "metadata.labels",
            "metadata.managedFields",
            "spec.containers[].image",
            "spec.nodeName",
            "spec.serviceAccountName",
            "status.podIP",
        ] {
            assert!(
                fields.iter().any(|f| f == want),
                "{want} is not redacted and must be listed: {fields:?}"
            );
        }
        assert!(
            fields.iter().all(|f| !f.as_str().unwrap().contains('/')),
            "a field path is not a bundle path: {fields:?}"
        );
    }

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

    /// `merge_timeline` re-sorts the whole list, so it has to apply the same rule. It used to use a
    /// plain `ts` string comparison, which put an untimed event back on the top line — and the
    /// default profile runs both `events` and `changes`, so that was the default path.
    #[test]
    fn merging_rollout_entries_keeps_an_untimed_event_off_the_top_line() {
        use serde_json::json;
        let dir = tempfile::tempdir().unwrap();
        super::write_json(
            &dir.path().join("timeline.json"),
            &vec![
                json!({ "ts": "2026-09-20T00:30:00Z", "source": "k8s-event", "reason": "BackOff" }),
                json!({ "ts": "", "source": "k8s-event", "reason": "NoTime" }),
            ],
        )
        .unwrap();
        super::merge_timeline(
            dir.path(),
            vec![json!({ "ts": "2026-09-20T00:10:00Z", "source": "rollout", "reason": "Updated" })],
        )
        .unwrap();
        let merged: Vec<serde_json::Value> =
            serde_json::from_slice(&std::fs::read(dir.path().join("timeline.json")).unwrap())
                .unwrap();
        let order: Vec<&str> = merged
            .iter()
            .map(|e| e["reason"].as_str().unwrap())
            .collect();
        assert_eq!(
            order,
            vec!["Updated", "BackOff", "NoTime"],
            "the untimed event must stay last after a merge, not become the start of the incident"
        );
    }

    /// A tail cut by the byte bound must not read as the whole tail. The bound is per instance and
    /// large enough that an ordinary 2000-line tail never reaches it.
    #[test]
    fn a_tail_at_the_byte_bound_is_recorded_as_truncated() {
        assert_eq!(
            super::truncation_note(500, 4096),
            None,
            "a short tail is whole"
        );
        assert_eq!(
            super::truncation_note(4095, 4096),
            None,
            "one byte under the bound is whole"
        );
        let note = super::truncation_note(4096, 4096).expect("a tail at the bound is truncated");
        assert_eq!(note["limit_bytes"], 4096);
        assert_eq!(note["bytes"], 4096);
        assert_eq!(
            note["cut"], "newest",
            "the budget is spent forward from the tail start, so the newest lines are what is gone"
        );
        // 2 KiB per line across the whole 2000-line tail: ordinary logs never reach it.
        assert_eq!(super::LOG_LIMIT_BYTES, 4 * 1024 * 1024);
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

    /// A fake API server that serves one crash-looped pod and answers every log request with
    /// `code`. 200 serves `body` as the log.
    async fn fake_apiserver(code: u16, body: &'static str) -> kube::Client {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let pod = serde_json::json!({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": { "name": "checkout-1", "namespace": "shop" },
            "spec": { "containers": [ { "name": "app" } ] },
            "status": { "containerStatuses": [ {
                "name": "app", "ready": false, "restartCount": 3, "image": "app:1",
                "imageID": "", "state": { "running": { "startedAt": "2026-09-23T00:00:01Z" } },
                "lastState": { "terminated": {
                    "exitCode": 137, "reason": "OOMKilled", "containerID": "containerd://dad",
                    "startedAt": "2026-09-23T00:00:00Z", "finishedAt": "2026-09-23T00:00:01Z"
                } } } ] }
        });
        let app = axum::Router::new()
            .route(
                "/api/v1/namespaces/shop/pods/checkout-1/log",
                axum::routing::get(move || async move {
                    (axum::http::StatusCode::from_u16(code).unwrap(), body)
                }),
            )
            .route(
                "/api/v1/namespaces/shop/pods/checkout-1",
                axum::routing::get(move || async move { axum::Json(pod) }),
            );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let cfg = kube::Config::new(format!("http://{addr}/").parse().unwrap());
        kube::Client::try_from(cfg).unwrap()
    }

    fn target() -> super::TargetRef {
        super::TargetRef {
            namespace: "shop".into(),
            pod: "checkout-1".into(),
            container: None,
        }
    }

    /// The defect this replaces: a `pods/log` denial was recorded in `index.json` and the
    /// collector still returned `Ok`, so `logs` landed in `collectors_run` and the bundle
    /// verified OK at coverage 100% with no log bytes at all. An `events` denial was a hard
    /// `?` and correctly gave PARTIAL — the asymmetry was the bug.
    #[tokio::test]
    async fn a_denied_log_read_fails_the_collector_rather_than_reading_as_coverage() {
        let dir = tempfile::tempdir().unwrap();
        let client = fake_apiserver(403, "forbidden").await;
        let err = super::collect_logs(&client, &target(), dir.path())
            .await
            .expect_err("a 403 on pods/log must fail the collector");
        assert!(
            err.to_string().contains("could not read"),
            "the error should name the failure: {err}"
        );
        // The denial is still sealed: the index exists and says why each fetch came back empty.
        let index: serde_json::Value =
            serde_json::from_slice(&std::fs::read(dir.path().join("logs/index.json")).unwrap())
                .unwrap();
        let text = index.to_string();
        assert!(
            text.contains("unavailable"),
            "the index must record the denial, got {text}"
        );
        assert!(
            !dir.path().join("logs/app-previous.log").exists(),
            "nothing may be sealed as a log"
        );
    }

    /// The other half: the kubelet reports a garbage-collected instance in-band, with HTTP 200
    /// and a one-line error as the body. That is a fact about the workload, not a failure of
    /// the collector, so it must stay soft or every crash-loop capture would go PARTIAL.
    #[tokio::test]
    async fn a_kubelet_in_band_error_stays_soft() {
        let dir = tempfile::tempdir().unwrap();
        let client = fake_apiserver(
            200,
            "unable to retrieve container logs for containerd://dadf16",
        )
        .await;
        super::collect_logs(&client, &target(), dir.path())
            .await
            .expect("an absent log is not a collector failure");
        assert!(
            !dir.path().join("logs/app-previous.log").exists(),
            "the in-band error must not be sealed as a log"
        );
    }

    /// A workload whose lines are long enough to spend the whole byte budget: the tail comes back at
    /// the bound, the bytes are still sealed (a bounded read is evidence, not a failure, so `logs`
    /// stays in `collectors_run` and the bundle does not go PARTIAL), and `logs/index.json` says per
    /// instance that what is there is not the whole tail.
    #[tokio::test]
    async fn a_tail_cut_by_the_byte_bound_is_sealed_and_the_index_says_so() {
        let dir = tempfile::tempdir().unwrap();
        let body: &'static str =
            Box::leak("x".repeat(super::LOG_LIMIT_BYTES as usize).into_boxed_str());
        let client = fake_apiserver(200, body).await;
        super::collect_logs(&client, &target(), dir.path())
            .await
            .expect("a bounded read is not a collector failure");
        assert_eq!(
            std::fs::metadata(dir.path().join("logs/app-current.log"))
                .unwrap()
                .len(),
            super::LOG_LIMIT_BYTES as u64,
            "the bytes that were read are still sealed"
        );
        let index: serde_json::Value =
            serde_json::from_slice(&std::fs::read(dir.path().join("logs/index.json")).unwrap())
                .unwrap();
        let current = index["containers"][0]["instances"]
            .as_array()
            .unwrap()
            .iter()
            .find(|i| i["which"] == "current")
            .expect("the current instance is indexed");
        assert_eq!(current["file"], "logs/app-current.log");
        assert_eq!(current["truncated"]["limit_bytes"], super::LOG_LIMIT_BYTES);
        assert_eq!(current["truncated"]["bytes"], super::LOG_LIMIT_BYTES);
        assert_eq!(current["truncated"]["cut"], "newest");
        assert!(
            current["unavailable"].is_null(),
            "a truncated read is not an unavailable one"
        );
    }
}
