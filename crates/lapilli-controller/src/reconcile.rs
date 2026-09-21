//! The IncidentCapture reconcile loop: the phase machine that turns a triggered capture
//! into a sealed, exported bundle. See DESIGN §6.1.
//!
//! Idempotency: once `status.phase` is terminal (Exported/Failed) for the current
//! `metadata.generation`, reconcile is a no-op. The bundle is written under a deterministic
//! path so a re-run never double-captures.

use std::sync::Arc;
use std::time::Duration;

use chrono::Utc;
use kube::api::{Patch, PatchParams};
use kube::runtime::controller::Action;
use kube::{Api, Client, ResourceExt};
use serde_json::json;

use lapilli_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
use lapilli_bundle::{seal_dir, SealInput, StaticKeySigner};

use crate::collector::{collect_all, CollectCtx, Redactor};
use crate::crd::{CaptureProfile, ExportState, ExportStatus, IncidentCapture, Phase, SigningMode};
use crate::export::{backoff, Exporter, Outcome, MAX_ATTEMPTS};
use crate::telemetry::{metrics, CaptureResult};
use kube::runtime::events::{Event, EventType, Recorder};
use kube::Resource;
use std::collections::BTreeMap;

pub struct Ctx {
    pub client: Client,
    pub exporter: Arc<Exporter>,
    pub recorder: Recorder,
    /// This controller's cluster id: the only cluster it records (and signs) bundles for.
    pub cluster_id: String,
    /// The only directory bundles are written to.
    pub bundle_root: String,
    /// KMS signing, when the admin configured it: then every bundle is signed with it and
    /// profiles' `signing` is ignored.
    pub kms: Option<Arc<crate::sealing::Kms>>,
    /// Where a sealed capture's summary is announced, when the admin configured a route.
    /// Enqueueing is non-blocking, so a slow webhook can never delay a capture.
    pub notify: Option<crate::notify::Dispatcher>,
    /// Bytes that must be free on the bundle volume before a capture starts collecting. `0`
    /// disables the check. Without it ENOSPC surfaces mid-collection as a bare
    /// "No space left on device (os error 28)", which honours none of the reason-code convention
    /// below — and by then the capture has already half-collected.
    pub min_free_bytes: u64,
    /// When this process started. Captures older than this are history, not news: see
    /// `enqueue_notification`.
    pub started_at: chrono::DateTime<Utc>,
}

#[derive(thiserror::Error, Debug)]
pub enum Error {
    #[error("kube: {0}")]
    Kube(#[from] kube::Error),
    #[error("capture: {0}")]
    Capture(String),
}

const MANAGER: &str = "lapilli-controller";
/// Annotation that re-drives a failed KMS seal (any new value).
pub const RETRY_SEAL: &str = "lapilli.dev/retry-seal";

/// Reconcile one IncidentCapture: capture once, then drive its object-store exports until
/// every destination is settled.
pub async fn reconcile(ic: Arc<IncidentCapture>, ctx: Arc<Ctx>) -> Result<Action, Error> {
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let name = ic.name_any();
    let gen = ic.metadata.generation;
    let api: Api<IncidentCapture> = Api::namespaced(ctx.client.clone(), &ns);

    let status = ic.status.clone().unwrap_or_default();
    let current = status.observed_generation == gen;
    // A failed seal is re-driven by a new `lapilli.dev/retry-seal` value (the staged data is
    // kept); spec edits never re-capture a sealing capture.
    if let (Some(_), Phase::Failed) = (&ctx.kms, &status.phase) {
        let token = ic.annotations().get(RETRY_SEAL).cloned();
        let seal = status.seal.clone().unwrap_or_default();
        if let Some(token) = token.filter(|t| seal.retry_token.as_deref() != Some(t)) {
            let mut seal = seal;
            seal.attempts = 0;
            seal.reason = None;
            seal.next_attempt_at = None;
            seal.retry_token = Some(token);
            patch_seal(
                &api,
                &name,
                Phase::Sealing,
                &seal,
                Some("retrying the seal".into()),
                gen,
            )
            .await?;
            return Ok(Action::await_change());
        }
    }
    // Failed is final for the current generation; for a capture that reached KMS sealing it
    // is final whatever the generation (its collected data is kept, and only the
    // retry-seal annotation moves it on).
    if status.phase == Phase::Failed && (current || status.seal.is_some()) {
        return Ok(Action::await_change());
    }
    // A capture is a one-time event: once exported it is never re-captured, even if its
    // spec is edited (re-packing would change the local bytes behind an uploaded object).
    let (bundle_path, exports) = if status.phase == Phase::Exported {
        (
            status.bundle_path.clone().unwrap_or_default(),
            status.exports.clone(),
        )
    } else if let (Some(kms), Phase::Sealing) = (&ctx.kms, &status.phase) {
        match seal_with_kms(
            &api,
            &ic,
            &ctx,
            kms,
            status.seal.clone().unwrap_or_default(),
            gen,
        )
        .await?
        {
            Ok((bundle_path, destinations, seal)) => {
                let exports = seed_exports(&ctx.exporter, &ic, &destinations);
                // One patch: the seal record and Exported together (no intermediate state
                // for a concurrent reconcile to misread).
                patch_exported_with(
                    &api,
                    &name,
                    ic.spec.skip_remote_export,
                    &bundle_path,
                    &exports,
                    gen,
                    Some(&seal),
                )
                .await?;
                let root = std::path::Path::new(&ctx.bundle_root);
                let uid = ic.metadata.uid.clone().unwrap_or_default();
                let _ = std::fs::remove_file(crate::sealing::seal_file(
                    &crate::sealing::staging_dir(root, &ic, &uid),
                ));
                tracing::info!(%ns, %name, "capture exported");
                (bundle_path, exports)
            }
            Err(action) => return Ok(action),
        }
    } else {
        if let Some(refusal) = refuse_capture(&ic, &ctx) {
            patch_status(&api, &name, Phase::Failed, None, Some(refusal.clone()), gen).await?;
            tracing::warn!(%ns, %name, reason = %refusal, "capture refused");
            crate::telemetry::metrics().capture(CaptureResult::Refused);
            return Ok(Action::await_change());
        }
        match run_capture(&ic, &ctx).await {
            Ok(Captured::AwaitingKms) => {
                let seal = crate::crd::SealStatus {
                    key: ctx.kms.as_ref().map(|k| k.key.name().to_string()),
                    ..Default::default()
                };
                let note = "collected; signing with KMS (profile signing settings are ignored)";
                patch_seal(&api, &name, Phase::Sealing, &seal, Some(note.into()), gen).await?;
                // The status patch's own watch event drives the first attempt (an immediate
                // requeue could read the pre-patch cache and collect again).
                return Ok(Action::await_change());
            }
            Ok(Captured::Sealed(bundle_path, destinations)) => {
                let exports = seed_exports(&ctx.exporter, &ic, &destinations);
                patch_exported(
                    &api,
                    &name,
                    ic.spec.skip_remote_export,
                    &bundle_path,
                    &exports,
                    gen,
                )
                .await?;
                tracing::info!(%ns, %name, "capture exported");
                (bundle_path, exports)
            }
            Err(e) => {
                // Capture errors start with their reason code (`incident-id-in-use: …`).
                let message = match &e {
                    Error::Capture(m) => m.clone(),
                    other => other.to_string(),
                };
                patch_status(&api, &name, Phase::Failed, None, Some(message), gen).await?;
                tracing::warn!(%ns, %name, error = %e, "capture failed");
                crate::telemetry::metrics().capture(CaptureResult::Failed);
                return Ok(Action::await_change());
            }
        }
    };
    drive_exports(&api, &ic, &ctx, &bundle_path, exports).await
}

/// Captures this controller must not make, whoever created the `IncidentCapture`. A sealed
/// (and possibly signed) bundle names its cluster and incident: creating a capture must not
/// get the controller to vouch for another cluster, and the incident id becomes a file name.
fn refuse_capture(ic: &IncidentCapture, ctx: &Ctx) -> Option<String> {
    if ic.spec.cluster_id != ctx.cluster_id {
        return Some(format!(
            "cluster-mismatch: this controller records cluster {:?}, not {:?}",
            ctx.cluster_id, ic.spec.cluster_id
        ));
    }
    if !crate::export::path_safe(&ic.spec.incident_id) {
        return Some(
            "invalid-incident-id: incident ids are [A-Za-z0-9._-], at most 100 characters".into(),
        );
    }
    // Ids of the webhook's form (`<cluster>-<16 hex>`) belong to the webhook's own capture
    // (`ic-<same hex>`): nobody may claim an alert's incident id ahead of it.
    if let Some(hex) = ic
        .spec
        .incident_id
        .strip_prefix(&format!("{}-", ctx.cluster_id))
        .filter(|h| h.len() == 16 && h.bytes().all(|b| b.is_ascii_hexdigit()))
    {
        if ic.name_any() != format!("ic-{hex}") {
            return Some(format!(
                "reserved-incident-id: {} is the webhook's id for capture ic-{hex}",
                ic.spec.incident_id
            ));
        }
    }
    None
}

/// One pending entry per destination the profile names (none for local-only captures).
fn seed_exports(
    exporter: &Exporter,
    ic: &IncidentCapture,
    destinations: &[String],
) -> BTreeMap<String, ExportStatus> {
    if ic.spec.skip_remote_export {
        return BTreeMap::new();
    }
    destinations
        .iter()
        .map(|d| {
            let status = ExportStatus {
                url: exporter.object_url(d, &ic.spec.incident_id),
                ..Default::default()
            };
            (d.clone(), status)
        })
        .collect()
}

/// Attempt every due, unsettled export; requeue for the next due one.
async fn drive_exports(
    api: &Api<IncidentCapture>,
    ic: &IncidentCapture,
    ctx: &Ctx,
    bundle_path: &str,
    mut exports: BTreeMap<String, ExportStatus>,
) -> Result<Action, Error> {
    let name = ic.name_any();
    let now = Utc::now();
    let names: Vec<String> = exports.keys().cloned().collect();
    for dest in names {
        let entry = exports.get_mut(&dest).expect("key from the map");
        if entry.state.settled() || !due(entry, now) {
            continue;
        }
        let outcome = ctx
            .exporter
            .upload(
                &dest,
                &ic.spec.cluster_id,
                &ic.spec.incident_id,
                std::path::Path::new(bundle_path),
            )
            .await;
        entry.attempts += 1;
        entry.last_attempt_at = Some(Utc::now().to_rfc3339());
        let event = match outcome {
            Outcome::Uploaded {
                url,
                sha256,
                version,
            } => {
                entry.state = ExportState::Uploaded;
                entry.url = Some(url);
                entry.sha256 = Some(sha256);
                entry.version_id = version;
                entry.reason = None;
                entry.uploaded_at = Some(Utc::now().to_rfc3339());
                None
            }
            Outcome::Refused { reason } => {
                entry.state = ExportState::Refused;
                entry.reason = Some(reason.into());
                Some((
                    "ExportRefused",
                    format!("export to {dest} refused: {reason}"),
                ))
            }
            Outcome::Conflict { url } => {
                entry.state = ExportState::Conflict;
                entry.reason = Some("conflict".into());
                Some((
                    "ExportConflict",
                    format!("{url} already holds different bytes; not overwritten"),
                ))
            }
            Outcome::Retry { reason } => {
                entry.reason = Some(reason.into());
                if entry.attempts >= MAX_ATTEMPTS {
                    entry.state = ExportState::Failed;
                    Some((
                        "ExportFailed",
                        format!("export to {dest} gave up after {MAX_ATTEMPTS} attempts: {reason}"),
                    ))
                } else {
                    None
                }
            }
        };
        let summary = summarize(&exports);
        let entry = &exports[&dest];
        let patch =
            json!({ "status": { "exports": { dest.clone(): entry }, "exportSummary": summary } });
        api.patch_status(&name, &PatchParams::apply(MANAGER), &Patch::Merge(&patch))
            .await?;
        metrics().export_attempt(entry.state == ExportState::Uploaded);
        if entry.state == ExportState::Uploaded {
            // Beside the bundles, once per key per destination. A bundle whose signing key is
            // later disabled is otherwise unverifiable, and the bucket is the copy that outlives
            // the cluster. Never allowed to affect the export's own outcome.
            ctx.exporter
                .copy_archived_keys(&dest, std::path::Path::new(&ctx.bundle_root))
                .await;
        }
        tracing::info!(capture = %name, destination = %dest, state = ?entry.state, reason = ?entry.reason, "export attempt");
        if let Some((reason, note)) = event {
            let _ = ctx
                .recorder
                .publish(
                    &Event {
                        type_: EventType::Warning,
                        reason: reason.into(),
                        note: Some(note),
                        action: "Export".into(),
                        secondary: None,
                    },
                    &ic.object_ref(&()),
                )
                .await;
        }
    }
    // Requeue for the earliest unsettled export.
    let next = exports
        .values()
        .filter(|e| !e.state.settled())
        .map(|e| next_attempt_in(e, Utc::now()))
        .min();
    match next {
        Some(d) => Ok(Action::requeue(d)),
        None => {
            // Every destination has settled (or there were none), so the message can say
            // truthfully where the bundle is.
            enqueue_notification(ic, ctx, &exports).await;
            Ok(Action::await_change())
        }
    }
}

/// Hand this capture to the notification dispatcher, if the admin configured one and the
/// profile names a route. Never fails a reconcile: a summary is a convenience, the bundle is
/// the product.
async fn enqueue_notification(
    ic: &IncidentCapture,
    ctx: &Ctx,
    exports: &BTreeMap<String, ExportStatus>,
) {
    // Why a capture was not announced is a question an admin will ask, and until now nothing
    // answered it: every path out of here was a bare `return`. These are rare, one-per-capture
    // events, so they are logged rather than counted.
    let skip = |why: &str| {
        tracing::info!(capture = %ic.name_any(), incident = %ic.spec.incident_id, %why,
                       "capture not announced");
    };
    let Some(dispatcher) = &ctx.notify else {
        return; // notification is not configured at all; saying so per capture would be noise
    };
    let root = std::path::Path::new(&ctx.bundle_root);
    // Already claimed by a dispatcher (this run or an earlier one): the steady state costs
    // one stat, not an API call. This is correct only because the dispatcher claims **every**
    // member of a group, not just the one it names in the message — otherwise the other pods
    // of a grouped incident would re-enqueue here on every relist and announce it again.
    if root
        .join(format!("{}.notified", ic.spec.incident_id))
        .exists()
    {
        return; // already announced (or folded into a group that was): the steady state
    }
    // Only captures this process has seen from the start are announced. A watcher relist
    // re-reconciles every `Exported` capture on the PVC, so without this the first time an admin
    // configures a route — which rolls the controller — every incident the recorder has ever held
    // would land in the channel at once. That is the "muted the first night" failure arriving on
    // the day notification is enabled.
    //
    // Deliberately the process start rather than a duration: a wall-clock window is either too
    // wide to stop the replay or too narrow to survive a long `Sealing` wait during a KMS
    // outage, where the capture is hours old and its message is still wanted.
    //
    // Claimed rather than merely skipped, so a relist does not keep re-deciding it.
    let created = ic.metadata.creation_timestamp.as_ref().map(|t| t.0);
    if created.is_some_and(|c| c < ctx.started_at) {
        let _ = crate::notify::claim(root, &ic.spec.incident_id);
        skip("it predates this controller process; enabling a route does not replay history");
        return;
    }
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let profiles: Api<CaptureProfile> = Api::namespaced(ctx.client.clone(), &ns);
    let profile = match profiles.get(&ic.spec.profile).await {
        Ok(p) => p,
        Err(e) => {
            skip(&format!(
                "its profile {} could not be read: {e}",
                ic.spec.profile
            ));
            return;
        }
    };
    let route_name = profile.spec.notify.route.trim();
    if route_name.is_empty() {
        skip(&format!(
            "profile {} names no notification route (spec.notify.route)",
            ic.spec.profile
        ));
        return;
    }
    // `lapilli.dev/export: local` — what `lapilli demo` sets — means "this never leaves the
    // cluster". A route has to say it wants those captures announced.
    if ic.spec.skip_remote_export && !dispatcher.include_demo(route_name) {
        skip("it is labelled lapilli.dev/export: local and the route has no includeDemo");
        return;
    }
    let sidecar = root.join(format!("{}.summary.json", ic.spec.incident_id));
    let summary = match std::fs::read(&sidecar) {
        Ok(raw) => match serde_json::from_slice::<lapilli_bundle::summary::Summary>(&raw) {
            Ok(s) => s,
            Err(e) => {
                skip(&format!("its summary could not be read back: {e}"));
                return;
            }
        },
        Err(e) => {
            skip(&format!("it has no summary at {}: {e}", sidecar.display()));
            return;
        }
    };
    if summary.is_empty() {
        // Nothing an on-call engineer could act on; a "we captured something" ping is the
        // message that gets the channel muted.
        skip("its summary says nothing actionable (no termination, restarts, change or metrics)");
        return;
    }
    // The workload the pods belong to, which is what one incident is. Without a resolved
    // owner each pod is its own incident, which is the honest grouping.
    let owner = match &summary.change {
        Some(c) => format!("{}/{}", c.kind, c.name),
        None => format!("Pod/{}", ic.spec.target.pod),
    };
    // Where the bundle is, recomputed from this controller's configuration — never from
    // `status`, which anyone with patch access could point elsewhere.
    let uploaded = exports
        .iter()
        .find(|(_, e)| e.state == ExportState::Uploaded)
        .and_then(|(dest, _)| ctx.exporter.object_url(dest, &ic.spec.incident_id));
    let (bundle_dir, exported) = match uploaded {
        Some(url) => (
            url.rsplit_once('/')
                .map(|(dir, _)| dir.to_string())
                .unwrap_or(url),
            true,
        ),
        None => (ctx.bundle_root.clone(), false),
    };
    tracing::info!(capture = %ic.name_any(), route = %route_name, %exported,
                   "announcing this capture");
    dispatcher.enqueue(crate::notify::Pending {
        key: crate::notify::GroupKey {
            route: route_name.to_string(),
            rule: ic.spec.trigger.rule.clone(),
            namespace: ic.spec.target.namespace.clone(),
            owner,
        },
        cluster: ic.spec.cluster_id.clone(),
        member: crate::notify::Member {
            incident_id: ic.spec.incident_id.clone(),
            pod: ic.spec.target.pod.clone(),
            capture_ns: ns,
            capture_name: ic.name_any(),
            summary,
        },
        bundle_dir,
        exported,
    });
}

fn due(e: &ExportStatus, now: chrono::DateTime<Utc>) -> bool {
    next_attempt_in(e, now).is_zero()
}

fn next_attempt_in(e: &ExportStatus, now: chrono::DateTime<Utc>) -> Duration {
    let Some(last) = e
        .last_attempt_at
        .as_deref()
        .and_then(|t| chrono::DateTime::parse_from_rfc3339(t).ok())
    else {
        return Duration::ZERO;
    };
    let at = last.with_timezone(&Utc)
        + chrono::Duration::from_std(backoff(e.attempts)).unwrap_or_default();
    (at - now).to_std().unwrap_or(Duration::ZERO)
}

fn summarize(exports: &BTreeMap<String, ExportStatus>) -> String {
    exports
        .iter()
        .map(|(k, v)| {
            format!(
                "{k}={}",
                serde_json::to_value(v.state)
                    .ok()
                    .and_then(|s| s.as_str().map(str::to_string))
                    .unwrap_or_default()
            )
        })
        .collect::<Vec<_>>()
        .join(",")
}

async fn patch_exported(
    api: &Api<IncidentCapture>,
    name: &str,
    skip_remote: bool,
    bundle_path: &str,
    exports: &BTreeMap<String, ExportStatus>,
    gen: Option<i64>,
) -> Result<(), Error> {
    patch_exported_with(api, name, skip_remote, bundle_path, exports, gen, None).await
}

#[allow(clippy::too_many_arguments)]
async fn patch_exported_with(
    api: &Api<IncidentCapture>,
    name: &str,
    skip_remote: bool,
    bundle_path: &str,
    exports: &BTreeMap<String, ExportStatus>,
    gen: Option<i64>,
    seal: Option<&crate::crd::SealStatus>,
) -> Result<(), Error> {
    let mut status = json!({
        "status": {
            "phase": Phase::Exported,
            "bundlePath": bundle_path,
            "message": null,
            "observedGeneration": gen,
        }
    });
    if !exports.is_empty() {
        status["status"]["exports"] = json!(exports);
        status["status"]["exportSummary"] = json!(summarize(exports));
    }
    if skip_remote {
        status["status"]["exportSummary"] = json!("local-only");
    }
    if let Some(seal) = seal {
        status["status"]["seal"] = json!(seal);
    }
    api.patch_status(name, &PatchParams::apply(MANAGER), &Patch::Merge(&status))
        .await?;
    Ok(())
}

/// The result of a capture: sealed now (no KMS, or a bundle this capture already made), or
/// collected and waiting for KMS signing (`Sealing`).
enum Captured {
    Sealed(String, Vec<String>),
    AwaitingKms,
}

/// Capture, then seal and pack (or, with KMS, stage for signing).
async fn run_capture(ic: &IncidentCapture, ctx: &Ctx) -> Result<Captured, Error> {
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let name = ic.name_any();
    let spec = &ic.spec;

    // Resolve the profile.
    let profiles: Api<CaptureProfile> = Api::namespaced(ctx.client.clone(), &ns);
    let profile = profiles
        .get(&spec.profile)
        .await
        .map_err(|e| Error::Capture(format!("profile {}: {e}", spec.profile)))?;
    let pspec = &profile.spec;

    let capture_started = Utc::now();

    // Stage into a temp dir under the export path so the final move is on the same fs.
    // Bundles live under one root the controller owns (not wherever a profile says):
    // the incident-id claim below is only exclusive within one directory.
    let export_root = std::path::Path::new(&ctx.bundle_root);
    if pspec.export.path.trim_end_matches('/') != ctx.bundle_root.trim_end_matches('/') {
        return Err(Error::Capture(format!(
            "export-path-not-allowed: bundles are written under {}, not {}",
            ctx.bundle_root, pspec.export.path
        )));
    }
    std::fs::create_dir_all(export_root).map_err(|e| Error::Capture(e.to_string()))?;
    // Claim the incident id before touching anything: the owner file is created exclusively
    // (O_EXCL) and holds this capture's UID. Another capture with the same id is refused,
    // whether this one is collecting, sealing, done or failed; a retry of this capture
    // finds its own claim and carries on (or adopts its finished bundle).
    let uid = ic
        .metadata
        .uid
        .clone()
        .filter(|u| !u.is_empty())
        .ok_or_else(|| Error::Capture("capture has no uid".into()))?;
    let ieb = export_root.join(format!("{}.ieb", spec.incident_id));
    let owner_file = export_root.join(format!("{}.ieb.owner", spec.incident_id));
    match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&owner_file)
    {
        Ok(mut f) => {
            use std::io::Write;
            f.write_all(uid.as_bytes())
                .and_then(|()| f.sync_all())
                .map_err(|e| Error::Capture(e.to_string()))?;
        }
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
            let owner = std::fs::read_to_string(&owner_file).unwrap_or_default();
            if owner.trim() != uid {
                return Err(Error::Capture(format!(
                    "incident-id-in-use: incident {} is claimed by capture uid {}; its \
                     bundle is never overwritten",
                    spec.incident_id,
                    owner.trim()
                )));
            }
        }
        Err(e) => return Err(Error::Capture(e.to_string())),
    }
    if ieb.exists() {
        return Ok(Captured::Sealed(
            ieb.to_string_lossy().to_string(),
            pspec.export.destinations.clone(),
        ));
    }
    // Preflight: refuse before collecting rather than dying part-way through. A capture that
    // cannot possibly be sealed should say so with its reason code.
    crate::retention::enough_free(export_root, ctx.min_free_bytes).map_err(Error::Capture)?;

    let stage = export_root.join(format!(".staging-{}-{uid}", spec.incident_id));
    // With KMS, data this capture already collected is never collected again: resume.
    if ctx.kms.is_some() && crate::sealing::seal_file(&stage).exists() {
        return Ok(Captured::AwaitingKms);
    }
    let _ = std::fs::remove_dir_all(&stage);
    std::fs::create_dir_all(&stage).map_err(|e| Error::Capture(e.to_string()))?;

    // Collect (failure-isolated).
    let redactor = Redactor::new(pspec.redaction.policy());
    let collect_ctx = CollectCtx {
        target: &spec.target,
        firing_ts: &spec.trigger.firing_ts,
        pre_seconds: pspec.pre_seconds,
        post_seconds: pspec.post_seconds,
        metrics: pspec.metrics.as_ref(),
        redactor: &redactor,
        diff_config_maps: pspec.diffs.config_maps,
    };
    let outcome = collect_all(&ctx.client, &collect_ctx, &pspec.collectors, &stage).await;
    for _ in 0..outcome.intended.len().saturating_sub(outcome.run.len()) {
        metrics().collector_failure();
    }
    // Written before sealing, so it is covered by the hash tree like every other file.
    std::fs::write(
        stage.join("redaction.json"),
        serde_json::to_vec_pretty(&redactor.report()).map_err(|e| Error::Capture(e.to_string()))?,
    )
    .map_err(|e| Error::Capture(e.to_string()))?;

    // Build the manifest input.
    let window = window_from(
        &spec.trigger.firing_ts,
        pspec.pre_seconds,
        pspec.post_seconds,
    );
    let sealed_at = Utc::now();
    let input = SealInput {
        incident: IncidentIdentity {
            id: spec.incident_id.clone(),
            cluster_id: spec.cluster_id.clone(),
            trigger: Trigger {
                rule: spec.trigger.rule.clone(),
                firing_ts: spec.trigger.firing_ts.clone(),
            },
            window,
        },
        producer: Producer {
            version: env!("CARGO_PKG_VERSION").to_string(),
            image_digest: std::env::var("LAPILLI_IMAGE_DIGEST")
                .unwrap_or_else(|_| "unknown".into()),
        },
        coverage: Coverage {
            collectors_run: outcome.run,
            collectors_intended: outcome.intended,
        },
        timing: Timing {
            capture_started: capture_started.to_rfc3339(),
            sealed_at: sealed_at.to_rfc3339(),
            capture_to_seal_ms: (sealed_at - capture_started).num_milliseconds().max(0) as u64,
        },
    };

    // KMS: record what sealing needs and stop here; signing is its own phase.
    if ctx.kms.is_some() {
        let tree = lapilli_bundle::HashTree::from_dir(&stage)
            .map_err(|e| Error::Capture(e.to_string()))?;
        crate::sealing::save(
            &stage,
            &crate::sealing::SavedSeal {
                input,
                tree,
                destinations: pspec.export.destinations.clone(),
            },
        )
        .map_err(Error::Capture)?;
        return Ok(Captured::AwaitingKms);
    }

    let partial = input.coverage.is_partial();
    let capture_seconds = input.timing.capture_to_seal_ms as f64 / 1000.0;

    // Optional signing.
    let signer = load_signer(ctx, &ns, pspec).await?;
    if let Some(s) = signer.as_ref() {
        if let (Ok(pem), Ok(id)) = (
            lapilli_bundle::Signer::public_key_pem(s),
            lapilli_bundle::Signer::key_id(s),
        ) {
            archive_public_key(export_root, &pem, &id);
        }
    }
    seal_dir(
        &stage,
        input,
        signer.as_ref().map(|s| s as &dyn lapilli_bundle::Signer),
    )
    .map_err(|e| Error::Capture(e.to_string()))?;

    // Pack the sealed staging dir into a single portable `.ieb` file (the headline
    // artifact — "one portable file you own"), then remove the staging dir.
    // Pack under a temporary name, then rename into place: the claim above makes this
    // capture the only writer of this incident's bundle.
    pack_into_place(
        export_root,
        &stage,
        &ieb,
        &spec.incident_id,
        &uid,
        spec.target.container.as_deref(),
    )?;
    let bytes = std::fs::metadata(&ieb).map(|m| m.len()).unwrap_or(0);
    metrics().sealed(capture_seconds, bytes, partial);
    metrics().capture(CaptureResult::Sealed);
    tracing::info!(%ns, %name, path = %ieb.display(), "sealed bundle");
    Ok(Captured::Sealed(
        ieb.to_string_lossy().to_string(),
        pspec.export.destinations.clone(),
    ))
}

/// Archive the signer's public half next to the bundles it signs, named by its own key id.
///
/// The point is key rotation: a bundle signed with a KMS key that is later disabled cannot be
/// verified afterwards, because the public half is no longer fetchable from the KMS — and the
/// runbook's "keep a copy first" step depends on somebody remembering. This keeps it without
/// anyone remembering.
///
/// **It is not a trust anchor.** `lapilli verify --key` takes the key the auditor chose, out of
/// band, and that must stay true: anyone who can write this directory or the bucket it is
/// exported to could replace a bundle and a key together. What the copy gives you is the ability
/// to verify *once you know the key id you expect* — which the manifest, `status.seal` and the
/// startup log all record. Naming the file by its key id means the name and the content check
/// each other.
fn archive_public_key(root: &std::path::Path, pem: &str, id: &str) {
    let dir = root.join("keys");
    if std::fs::create_dir_all(&dir).is_err() {
        return;
    }
    // `create_new`: the content is fixed by the name, so an existing file is already correct.
    match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(dir.join(format!("{id}.pub")))
    {
        Ok(mut f) => {
            use std::io::Write;
            if f.write_all(pem.as_bytes())
                .and_then(|()| f.sync_all())
                .is_ok()
            {
                tracing::info!(key_id = %id, "archived the signing key's public half beside the bundles");
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(e) => tracing::warn!(key_id = %id, error = %e, "could not archive the public key"),
    }
}

/// Pack under a temporary name, then rename into place: the incident-id claim makes this
/// capture the only writer of this bundle. Removes the staging data afterwards.
fn pack_into_place(
    root: &std::path::Path,
    stage: &std::path::Path,
    ieb: &std::path::Path,
    incident: &str,
    uid: &str,
    container: Option<&str>,
) -> Result<(), Error> {
    // The summary is read from the staged files, and packing deletes them, so it is written
    // here — the one place staging is removed. Without it a notification that is retried
    // after a restart would have nothing left to describe.
    // Never the log line. The sidecar sits outside the hash tree and outside the signature,
    // nothing reads it, and nothing prunes it — so a `.ieb` deleted for retention would leave
    // the container's last words behind it in plaintext.
    let summary = lapilli_bundle::summary::Summary::from_dir(stage, container).without_log_line();
    let tmp = root.join(format!(".{incident}-{uid}.ieb.tmp"));
    let _ = std::fs::remove_file(&tmp);
    lapilli_bundle::pack(stage, &tmp).map_err(|e| Error::Capture(e.to_string()))?;
    std::fs::rename(&tmp, ieb).map_err(|e| Error::Capture(e.to_string()))?;
    // Best-effort: a missing summary costs a thinner message, never the bundle.
    if let Ok(json) = serde_json::to_vec_pretty(&summary) {
        let _ = std::fs::write(root.join(format!("{incident}.summary.json")), json);
    }
    let _ = std::fs::remove_dir_all(stage);
    Ok(())
}

async fn load_signer(
    ctx: &Ctx,
    ns: &str,
    pspec: &crate::crd::CaptureProfileSpec,
) -> Result<Option<StaticKeySigner>, Error> {
    if pspec.signing.mode != SigningMode::Static {
        return Ok(None);
    }
    let secret_name = pspec
        .signing
        .key_secret
        .as_ref()
        .ok_or_else(|| Error::Capture("signing mode=static requires keySecret".into()))?;
    use k8s_openapi::api::core::v1::Secret;
    let secrets: Api<Secret> = Api::namespaced(ctx.client.clone(), ns);
    let secret = secrets.get(secret_name).await?;
    let data = secret
        .data
        .and_then(|mut d| d.remove("key.pem"))
        .ok_or_else(|| Error::Capture(format!("secret {secret_name} missing key.pem")))?;
    let pem = String::from_utf8(data.0).map_err(|e| Error::Capture(e.to_string()))?;
    let signer =
        StaticKeySigner::from_pkcs8_pem(&pem).map_err(|e| Error::Capture(e.to_string()))?;
    Ok(Some(signer))
}

fn window_from(firing_ts: &str, pre: u32, post: u32) -> Window {
    match chrono::DateTime::parse_from_rfc3339(firing_ts) {
        Ok(t) => {
            let start = t - chrono::Duration::seconds(pre as i64);
            let end = t + chrono::Duration::seconds(post as i64);
            Window {
                start: start.to_rfc3339(),
                end: end.to_rfc3339(),
            }
        }
        // If the firing timestamp is unparseable, record it verbatim as a degenerate window.
        Err(_) => Window {
            start: firing_ts.to_string(),
            end: firing_ts.to_string(),
        },
    }
}

/// One KMS signing attempt for a capture in `Sealing`. `Ok(Ok(..))`: sealed and packed
/// (the caller records it with Exported in one patch); `Ok(Err(action))`: not yet (status
/// patched, requeue at the next attempt) or failed.
#[allow(clippy::type_complexity)]
async fn seal_with_kms(
    api: &Api<IncidentCapture>,
    ic: &IncidentCapture,
    ctx: &Ctx,
    kms: &crate::sealing::Kms,
    mut seal: crate::crd::SealStatus,
    gen: Option<i64>,
) -> Result<Result<(String, Vec<String>, crate::crd::SealStatus), Action>, Error> {
    use crate::sealing::SealError;
    let name = ic.name_any();
    // Never sign for another cluster or with an unsafe id, whatever changed since.
    if let Some(refusal) = refuse_capture(ic, ctx) {
        patch_status(api, &name, Phase::Failed, None, Some(refusal), gen).await?;
        crate::telemetry::metrics().capture(CaptureResult::Refused);
        return Ok(Err(Action::await_change()));
    }
    let uid = ic.metadata.uid.clone().unwrap_or_default();
    let root = std::path::Path::new(&ctx.bundle_root);
    let stage = crate::sealing::staging_dir(root, ic, &uid);
    let ieb = root.join(format!("{}.ieb", ic.spec.incident_id));
    let owner = std::fs::read_to_string(root.join(format!("{}.ieb.owner", ic.spec.incident_id)))
        .unwrap_or_default();
    // Already sealed and packed by an earlier attempt of this capture (e.g. the status
    // patch after packing didn't land): adopt it, never sign again.
    if ieb.exists() && !uid.is_empty() && owner.trim() == uid {
        let destinations = crate::sealing::saved_destinations(&stage).unwrap_or_default();
        return Ok(Ok((ieb.to_string_lossy().to_string(), destinations, seal)));
    }
    // Not yet due (a reconcile can come early, e.g. after a watch event).
    if let Some(next) = seal
        .next_attempt_at
        .as_deref()
        .and_then(|t| chrono::DateTime::parse_from_rfc3339(t).ok())
    {
        let wait = next.with_timezone(&Utc) - Utc::now();
        if wait > chrono::Duration::zero() {
            return Ok(Err(Action::requeue(wait.to_std().unwrap_or_default())));
        }
    }
    // Sign, then pack; a packing failure is retried under the same budget (never the 10 s
    // error policy, which would ask KMS to sign again every 10 s).
    // Before the signature, not after: if the KMS key is disabled between signing here and an
    // audit later, this copy is the only way the bundle stays verifiable.
    if let Ok(signer) = kms.signer().await {
        archive_public_key(root, signer.public_key_pem(), signer.key_id());
    }
    let result = match crate::sealing::attempt(kms, &stage, ic, &ctx.cluster_id).await {
        Ok(sealed) => match pack_into_place(
            root,
            &stage,
            &ieb,
            &ic.spec.incident_id,
            &uid,
            ic.spec.target.container.as_deref(),
        ) {
            Ok(()) => {
                let bytes = std::fs::metadata(&ieb).map(|m| m.len()).unwrap_or(0);
                metrics().sealed(sealed.capture_seconds, bytes, sealed.partial);
                metrics().capture(CaptureResult::Sealed);
                Ok(sealed)
            }
            Err(e) => {
                metrics().seal_pack_failure();
                Err(SealError::Retry("seal-io-error", e.to_string()))
            }
        },
        Err(e) => Err(e),
    };
    match result {
        Ok(sealed) => {
            seal.attempts += 1;
            seal.reason = None;
            seal.next_attempt_at = None;
            seal.key = Some(kms.key.name().to_string());
            seal.key_id = Some(sealed.key_id);
            seal.manifest_sha256 = Some(sealed.manifest_sha256.clone());
            seal.request_id = sealed.request_id.clone();
            tracing::info!(capture = %name, key = %kms.key.name(), manifest_sha256 = %sealed.manifest_sha256,
                request_id = ?sealed.request_id, "sealed bundle, signed with KMS");
            Ok(Ok((
                ieb.to_string_lossy().to_string(),
                sealed.destinations,
                seal,
            )))
        }
        Err(SealError::Fatal(message)) => {
            patch_seal(api, &name, Phase::Failed, &seal, Some(message.clone()), gen).await?;
            metrics().capture(CaptureResult::Failed);
            publish(ctx, ic, "SealFailed", message).await;
            Ok(Err(Action::await_change()))
        }
        Err(e) => {
            let (reason, detail) = match e {
                SealError::Kms(k) => (k.kind.reason(), k.message),
                SealError::Retry(reason, detail) => (reason, detail),
                SealError::Fatal(_) => unreachable!(),
            };
            seal.attempts += 1;
            seal.reason = Some(reason.to_string());
            tracing::warn!(capture = %name, attempts = seal.attempts, %reason, %detail, "KMS seal attempt failed");
            if seal.attempts >= MAX_ATTEMPTS {
                seal.next_attempt_at = None;
                let message = format!(
                    "{reason}: gave up after {} attempts ({detail}); the collected data is \
                     kept: set the {RETRY_SEAL} annotation to retry",
                    seal.attempts
                );
                patch_seal(api, &name, Phase::Failed, &seal, Some(message.clone()), gen).await?;
                metrics().capture(CaptureResult::Failed);
                publish(ctx, ic, "SealFailed", message).await;
                return Ok(Err(Action::await_change()));
            }
            let wait = backoff(seal.attempts);
            seal.next_attempt_at = Some(
                (Utc::now() + chrono::Duration::from_std(wait).unwrap_or_default()).to_rfc3339(),
            );
            patch_seal(
                api,
                &name,
                Phase::Sealing,
                &seal,
                Some(format!("{reason}: {detail}")),
                gen,
            )
            .await?;
            if seal.attempts == 1 {
                publish(
                    ctx,
                    ic,
                    "SealDelayed",
                    format!("KMS signing failed, retrying with backoff: {reason}: {detail}"),
                )
                .await;
            }
            Ok(Err(Action::requeue(wait)))
        }
    }
}

async fn publish(ctx: &Ctx, ic: &IncidentCapture, reason: &str, note: String) {
    let _ = ctx
        .recorder
        .publish(
            &Event {
                type_: EventType::Warning,
                reason: reason.into(),
                note: Some(note),
                action: "Seal".into(),
                secondary: None,
            },
            &ic.object_ref(&()),
        )
        .await;
}

async fn patch_seal(
    api: &Api<IncidentCapture>,
    name: &str,
    phase: Phase,
    seal: &crate::crd::SealStatus,
    message: Option<String>,
    gen: Option<i64>,
) -> Result<(), Error> {
    let status = json!({ "status": {
        "phase": phase, "seal": seal, "message": message, "observedGeneration": gen } });
    api.patch_status(name, &PatchParams::apply(MANAGER), &Patch::Merge(&status))
        .await?;
    Ok(())
}

async fn patch_status(
    api: &Api<IncidentCapture>,
    name: &str,
    phase: Phase,
    bundle_path: Option<String>,
    message: Option<String>,
    gen: Option<i64>,
) -> Result<(), Error> {
    let status = json!({
        "status": {
            "phase": phase,
            "bundlePath": bundle_path,
            "message": message,
            "observedGeneration": gen,
        }
    });
    api.patch_status(name, &PatchParams::apply(MANAGER), &Patch::Merge(&status))
        .await?;
    Ok(())
}

/// Error policy: retry after a short backoff.
pub fn error_policy(_ic: Arc<IncidentCapture>, _err: &Error, _ctx: Arc<Ctx>) -> Action {
    metrics().reconcile_error();
    Action::requeue(Duration::from_secs(10))
}
