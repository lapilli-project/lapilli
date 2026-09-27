//! The IncidentCapture reconcile loop: the phase machine that turns a triggered capture
//! into a sealed, exported bundle. See DESIGN §6.1.
//!
//! Idempotency: once `status.phase` is terminal (Exported/Failed) for the current
//! `metadata.generation`, reconcile is a no-op. The bundle is written under a deterministic
//! path so a re-run never double-captures.

use std::sync::Arc;
use std::time::Duration;

use chrono::{Timelike, Utc};
use kube::api::{Patch, PatchParams};
use kube::runtime::controller::Action;
use kube::{Api, Client, ResourceExt};
use serde_json::json;

use lapilli_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
use lapilli_bundle::{seal_dir, SealInput, StaticKeySigner};

use crate::collector::{collect_all, CollectCtx, Redactor};
use crate::crd::{
    CaptureProfile, ExportState, ExportStatus, IncidentCapture, Phase, SigningMode, StatusMessage,
};
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
    /// The permission self-check's latest report (`perms.rs`). Read after collection so a
    /// collector that did not run can be tied to the denial that stopped it, at the capture.
    pub permissions: crate::perms::Shared,
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
            patch_status(
                &api,
                &name,
                Phase::Failed,
                None,
                Some(StatusMessage::new(&refusal)),
                gen,
            )
            .await?;
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
                patch_status(
                    &api,
                    &name,
                    Phase::Failed,
                    None,
                    Some(StatusMessage::new(&message)),
                    gen,
                )
                .await?;
                tracing::warn!(%ns, %name, error = %e, "capture failed");
                crate::telemetry::metrics().capture(CaptureResult::Failed);
                return Ok(Action::await_change());
            }
        }
    };
    drive_exports(&api, &ic, &ctx, &bundle_path, exports, current).await
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
    // The belt for what the webhook now filters: a capture can also be created by hand, or by an
    // older webhook. Without a pod there is nothing to collect, and sealing anyway produced a
    // signed bundle with empty `events.json`, empty `timeline.json` and empty metric results
    // that still reported `Exported`. An evidence recorder must refuse rather than report
    // success for nothing.
    if ic.spec.target.pod.trim().is_empty() {
        return Some(
            "no-target: this capture names no pod, so there is nothing to record".to_string(),
        );
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
    // `status.observedGeneration == metadata.generation`. Retirement needs it: a capture whose
    // spec has moved under us must not be taken out of the watch on a stale reading.
    current: bool,
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
                        note: Some(crate::crd::bounded(&note, crate::crd::MESSAGE_MAX)),
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
            let notified = enqueue_notification(ic, ctx, &exports).await;
            match notified {
                // Nothing left for this capture, ever: stop holding it in the watch.
                Notified::Settled if current => {
                    retire(api, ic).await;
                    Ok(Action::await_change())
                }
                // Settled, but the spec moved under us: leave it watched and let the next
                // reconcile decide against a fresh reading.
                Notified::Settled => Ok(Action::await_change()),
                // The `.notified` claim appears when the dispatcher settles, and for every
                // member of a group except the leader no watch event follows — so this reconcile
                // has to come back and look, or the capture is never retired.
                Notified::Enqueued => Ok(Action::requeue(
                    crate::notify::COALESCE_MAX + Duration::from_secs(15),
                )),
                // Fail closed: try again soon, retire nothing.
                Notified::Deferred => Ok(Action::requeue(Duration::from_secs(60))),
            }
        }
    }
}

/// The instant before which a capture counts as history, for the replay guard in
/// [`enqueue_notification`].
///
/// **Kubernetes stores `creationTimestamp` at one-second resolution.** Comparing it against a
/// sub-second `Utc::now()` therefore misjudges every capture created in the same wall-clock second
/// the process started: its timestamp truncates to `HH:MM:SS.000`, which is earlier than a start of
/// `HH:MM:SS.157`, so a capture that is 0.7 s *newer* than the process reads as older than it and
/// is silently filed as history — claimed, never announced.
///
/// That is a one-second window after every start in which the first capture is never announced, and
/// it is reachable in ordinary use: a controller that rolls while an incident is firing, or a test
/// that fires the moment readiness passes. It cost this project an E2E failure whose cause went
/// unexplained through two earlier investigations.
///
/// Truncating the start to the same resolution as the value it is compared against removes the
/// asymmetry. The cost is that a capture created up to a second *before* this process really
/// started is now announced rather than filed as history — which is the right answer anyway: it is
/// a second old, not a replay of the past.
fn history_before(started_at: chrono::DateTime<Utc>) -> chrono::DateTime<Utc> {
    started_at.with_nanosecond(0).unwrap_or(started_at)
}

/// Retire what is already finished, **before** the controller's watch is built.
///
/// This is the part the first draft of the design got wrong, and it got it wrong in the direction
/// that matters: to retire a capture through the informer the controller must first load it into
/// the watch cache. An install with 7,000 accumulated captures would therefore have held all of
/// them — 7,000 x >=19.4 KB, about 136 MiB against a 256 MiB limit, with the ~110 MiB a storm
/// needs on top — so **the release that exists to prevent the OOM would have been the OOM**, on
/// the one code path it added. It would also have pushed 7,000 retirement reconciles through the
/// same two slots the live incident path uses.
///
/// So the first sweep does not go through the informer at all. One page at a time, counted and
/// dropped: peak cost is a page, not the population.
///
/// Deliberately weaker than the reconcile path: it retires only what is unambiguously finished
/// (`Exported`, current generation, every export settled, a `.notified` claim on disk or no
/// dispatcher configured). It never enqueues a notification and never reads a CaptureProfile, so
/// it cannot announce history and cannot be wrong about a route. Anything it is unsure of is left
/// for the reconcile that will see it.
///
/// Best effort. A failure here logs and returns: a recorder that will not start is worse than one
/// with a large cache, and every PATCH is durable so the next start continues.
pub async fn retire_finished_on_start(
    api: &Api<IncidentCapture>,
    bundle_root: &str,
    notify_configured: bool,
) {
    const PAGE: u32 = 500;
    let root = std::path::Path::new(bundle_root);
    let mut token: Option<String> = None;
    let (mut seen, mut retired) = (0usize, 0usize);
    loop {
        let mut lp = kube::api::ListParams::default().limit(PAGE);
        if let Some(t) = &token {
            lp = lp.continue_token(t);
        }
        let page = match api.list(&lp).await {
            Ok(p) => p,
            Err(e) => {
                tracing::warn!(error = %e, seen, retired,
                               "the startup retirement sweep stopped early; it resumes next start");
                return;
            }
        };
        for ic in &page.items {
            seen += 1;
            if !finished_on_disk(ic, root, notify_configured) {
                continue;
            }
            retire(api, ic).await;
            retired += 1;
        }
        token = page.metadata.continue_.filter(|t| !t.is_empty());
        if token.is_none() {
            break;
        }
    }
    if retired > 0 || seen > 0 {
        tracing::info!(seen, retired, "startup retirement sweep done");
    }
}

/// The sweep's predicate. No API reads beyond the page it was given, and no profile lookup.
fn finished_on_disk(ic: &IncidentCapture, root: &std::path::Path, notify_configured: bool) -> bool {
    let Some(status) = ic.status.as_ref() else {
        return false;
    };
    if status.phase != Phase::Exported || status.observed_generation != ic.metadata.generation {
        return false;
    }
    if !status.exports.values().all(|e| e.state.settled()) {
        return false;
    }
    if ic
        .metadata
        .labels
        .as_ref()
        .is_some_and(|l| l.contains_key(RETIRED))
    {
        return false; // already retired: nothing to do, and nothing to count
    }
    // With no dispatcher there is nothing to announce. With one, the only thing this sweep will
    // trust is a claim already on disk — it will not decide a route's business from here.
    !notify_configured
        || root
            .join(format!("{}.notified", ic.spec.incident_id))
            .exists()
}

/// The label that takes a capture out of the controller's watch. See
/// `docs/design-capture-retirement.md`.
pub const RETIRED: &str = "lapilli.dev/retired";

/// Retire a capture: it keeps its CR, its status and its bundle, and stops being delivered to the
/// reconciler.
///
/// Called **only** from the one place that has established there is nothing left to do — phase
/// `Exported`, every export settled, notification settled, and the status current for this
/// generation. `Failed` is never retired: `lapilli.dev/retry-seal` re-drives it and a spec edit
/// re-drives the pre-seal case, and neither reaches an object the watch no longer delivers.
///
/// The patch carries the observed `resourceVersion`. The predicate was computed from the
/// reflector's snapshot, so a `retry-seal` annotation or a spec edit landing in between would
/// otherwise be swallowed — the object would leave the watch carrying work nobody saw. That is
/// most likely at the worst moment: an operator mass-annotating after a KMS outage. On conflict
/// the retirement is abandoned and the next reconcile decides again; a missed retirement costs
/// one object's worth of cache, a swallowed edit costs the evidence.
///
/// `Patch::Merge`, never `Patch::Apply`: under a shared field manager server-side apply would
/// prune the status fields that manager owns.
async fn retire(api: &Api<IncidentCapture>, ic: &IncidentCapture) {
    let name = ic.name_any();
    if ic
        .metadata
        .labels
        .as_ref()
        .is_some_and(|l| l.contains_key(RETIRED))
    {
        return; // already retired; the relist that delivered it will not repeat
    }
    let Some(rv) = ic.metadata.resource_version.clone() else {
        return; // no version to guard with: leave it watched rather than guess
    };
    let patch = serde_json::json!({
        "metadata": { "resourceVersion": rv, "labels": { RETIRED: "true" } }
    });
    match api
        .patch(
            &name,
            &kube::api::PatchParams::default(),
            &kube::api::Patch::Merge(&patch),
        )
        .await
    {
        Ok(_) => {
            crate::telemetry::metrics().capture_retired();
            tracing::info!(
                capture = %name,
                "capture retired from the watch: nothing left to do; \
                 `kubectl label incidentcapture <name> lapilli.dev/retired-` puts it back"
            );
        }
        Err(kube::Error::Api(ae)) if ae.code == 409 => {
            tracing::info!(capture = %name, "retirement skipped: the capture changed underneath");
        }
        Err(e) => {
            tracing::warn!(capture = %name, error = %e, "could not retire this capture");
        }
    }
}

/// What became of this capture's announcement, as far as *this* reconcile can tell.
///
/// Retirement needs this distinction and cannot infer it. Every path out of
/// [`enqueue_notification`] used to be a bare `return`, and two of them are transient failures
/// that today rely on the next relist to try again — so treating "no claim was written" as
/// "nothing to announce" would retire a capture whose message was merely postponed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Notified {
    /// Nothing more will happen: already claimed, or a negative that was actually determined
    /// (no dispatcher, no route, an empty summary, a demo capture the route excludes).
    Settled,
    /// Handed to the dispatcher. The `.notified` claim appears when it settles, up to
    /// [`crate::notify::COALESCE_MAX`] later, and only a stat can see it — the dispatcher's
    /// status patch lands on the group's *leading* capture only, so the other members get no
    /// watch event at all.
    Enqueued,
    /// Could not be determined: a profile GET or a summary read failed for a reason that may
    /// not repeat. Fail closed — try again, retire nothing.
    Deferred,
}

/// Hand this capture to the notification dispatcher, if the admin configured one and the
/// profile names a route. Never fails a reconcile: a summary is a convenience, the bundle is
/// the product.
async fn enqueue_notification(
    ic: &IncidentCapture,
    ctx: &Ctx,
    exports: &BTreeMap<String, ExportStatus>,
) -> Notified {
    // Why a capture was not announced is a question an admin will ask, and until now nothing
    // answered it: every path out of here was a bare `return`. These are rare, one-per-capture
    // events, so they are logged rather than counted.
    let skip = |why: &str| {
        tracing::info!(capture = %ic.name_any(), incident = %ic.spec.incident_id, %why,
                       "capture not announced");
    };
    let Some(dispatcher) = &ctx.notify else {
        return Notified::Settled; // not configured at all; saying so per capture would be noise
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
        return Notified::Settled; // already announced (or folded into a group that was)
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
    // Claimed rather than merely skipped, so a relist does not keep re-deciding it — but claimed
    // *as history*: the process this one replaced may be flushing this very capture right now
    // (after a pod deletion the replacement starts before the old pod's SIGTERM), and its flush
    // must be able to tell this claim from one that stands for a message.
    let created = ic.metadata.creation_timestamp.as_ref().map(|t| t.0);
    if created.is_some_and(|c| c < history_before(ctx.started_at)) {
        let _ = crate::notify::claim_history(root, &ic.spec.incident_id);
        skip("it predates this controller process; enabling a route does not replay history");
        return Notified::Settled;
    }
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let profiles: Api<CaptureProfile> = Api::namespaced(ctx.client.clone(), &ns);
    let profile = match profiles.get(&ic.spec.profile).await {
        Ok(p) => p,
        Err(e) => {
            // Transient: an API blip, or RBAC not yet bound. Today the next relist retries
            // this, and retirement must not take that away.
            skip(&format!(
                "its profile {} could not be read: {e}",
                ic.spec.profile
            ));
            return Notified::Deferred;
        }
    };
    let route_name = profile.spec.notify.route.trim();
    if route_name.is_empty() {
        skip(&format!(
            "profile {} names no notification route (spec.notify.route)",
            ic.spec.profile
        ));
        return Notified::Settled;
    }
    // `lapilli.dev/export: local` — what `lapilli demo` sets — means "this never leaves the
    // cluster". A route has to say it wants those captures announced.
    if ic.spec.skip_remote_export && !dispatcher.include_demo(route_name) {
        skip("it is labelled lapilli.dev/export: local and the route has no includeDemo");
        return Notified::Settled;
    }
    let sidecar = root.join(format!("{}.summary.json", ic.spec.incident_id));
    let summary = match std::fs::read(&sidecar) {
        Ok(raw) => match serde_json::from_slice::<lapilli_bundle::summary::Summary>(&raw) {
            Ok(s) => s,
            Err(e) => {
                // A corrupt sidecar will not fix itself, but it is also not proof that nothing
                // should be announced. Fail closed.
                skip(&format!("its summary could not be read back: {e}"));
                return Notified::Deferred;
            }
        },
        Err(e) => {
            skip(&format!("it has no summary at {}: {e}", sidecar.display()));
            return Notified::Deferred;
        }
    };
    if summary.is_empty() {
        // Nothing an on-call engineer could act on; a "we captured something" ping is the
        // message that gets the channel muted.
        skip("its summary says nothing actionable (no termination, restarts, change or metrics)");
        return Notified::Settled;
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
    Notified::Enqueued
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

/// For each intended collector that did not run, the Event note tying it to a denied permission
/// — when, and only when, the latest self-check found one that collector depends on. A missing
/// report (no pass yet), a collector that ran, or a check that is held or unknown all attribute
/// nothing: the existing warning log stands, and nothing is claimed that was not observed.
fn attribute_denials(
    report: Option<&crate::perms::Report>,
    intended: &[String],
    run: &[String],
) -> Vec<String> {
    let Some(r) = report else {
        return Vec::new();
    };
    let at = r.at.to_rfc3339_opts(chrono::SecondsFormat::Secs, true);
    intended
        .iter()
        .filter(|c| !run.contains(c))
        .filter_map(|c| {
            let checks = r.denied_for(c);
            (!checks.is_empty()).then(|| {
                format!(
                    "{c} did not run; the permission self-check at {at} found {} denied in the \
                     namespaces this controller captures from — the bundle is PARTIAL for that \
                     reason, not because the workload had nothing to say",
                    checks.join(", ")
                )
            })
        })
        .collect()
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
    // The CRD's CEL rule refuses this at the API server; this is the same rule for an API server
    // that did not enforce it. Sealing would produce a bundle that verifies FAILED (malformed
    // coverage) by the controller's own hand, which is worse than a capture refused with the
    // reason on it.
    if let Some(both) = pspec.deferred.iter().find(|d| pspec.collectors.contains(d)) {
        return Err(Error::Capture(format!(
            "profile-invalid: {both} is in both collectors and deferred in profile {}",
            spec.profile
        )));
    }

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
    // Counted here, after every guard that can still refuse the capture, so "captures started"
    // in the metric's HELP is what it counts.
    if !pspec.deferred.is_empty() {
        metrics().deferred_capture();
    }
    let outcome = collect_all(&ctx.client, &collect_ctx, &pspec.collectors, &stage).await;
    for _ in 0..outcome.intended.len().saturating_sub(outcome.run.len()) {
        metrics().collector_failure();
    }
    // A collector that did not run has already made the bundle PARTIAL; that is the honest
    // verdict and it stands. What the verdict cannot say is *why*, and "forbidden" in a log line
    // three hops away is not an answer at the capture. So if the latest permission self-check
    // found a denial that collector depends on, say so where `kubectl describe` looks. Nothing is
    // refused on the strength of a self-check: a report can predate the fix, and a bundle with
    // the other collectors in it is more evidence than none
    // (docs/design-permissions-by-profile.md, rule 5).
    let report = ctx.permissions.read().ok().and_then(|r| r.clone());
    for note in attribute_denials(report.as_ref(), &outcome.intended, &outcome.run) {
        publish(ctx, ic, "CollectorDenied", note.into()).await;
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
            target: Some(lapilli_bundle::manifest::Target {
                namespace: spec.target.namespace.clone(),
                pod: spec.target.pod.clone(),
            }),
        },
        producer: Producer {
            version: env!("CARGO_PKG_VERSION").to_string(),
            image_digest: std::env::var("LAPILLI_IMAGE_DIGEST")
                .unwrap_or_else(|_| "unknown".into()),
        },
        coverage: Coverage {
            collectors_run: outcome.run,
            collectors_intended: outcome.intended,
            // The profile's declared omissions (IEB rule 6). Empty for every profile that does
            // not set it, which seals to exactly the manifest it did before the field existed.
            deferred: pspec.deferred.clone(),
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
        patch_status(
            api,
            &name,
            Phase::Failed,
            None,
            Some(StatusMessage::new(&refusal)),
            gen,
        )
        .await?;
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
            let message = StatusMessage::new(&message);
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
                // The instruction comes before the detail on purpose: `detail` is AWS's and
                // GCP's text, and if it led, a truncation at the end would drop the only
                // sentence that tells an operator what to do (`design-review-round19.md` R6).
                let message = StatusMessage::new(format!(
                    "{reason}: gave up after {} attempts; the collected data is kept: set the \
                     {RETRY_SEAL} annotation to retry ({})",
                    seal.attempts,
                    crate::crd::bounded(&detail, crate::crd::DETAIL_MAX)
                ));
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
                Some(StatusMessage::new(format!(
                    "{reason}: {}",
                    crate::crd::bounded(&detail, crate::crd::DETAIL_MAX)
                ))),
                gen,
            )
            .await?;
            if seal.attempts == 1 {
                publish(
                    ctx,
                    ic,
                    "SealDelayed",
                    StatusMessage::new(format!(
                        "KMS signing failed, retrying with backoff: {reason}: {}",
                        crate::crd::bounded(&detail, crate::crd::DETAIL_MAX)
                    )),
                )
                .await;
            }
            Ok(Err(Action::requeue(wait)))
        }
    }
}

/// Takes a bounded message, not a String: `events.k8s.io/v1` caps `note` at 1024 bytes, the
/// API server rejects a longer one, and the result is discarded below — so an unbounded note
/// would drop the Event an operator alerts on, with no trace.
async fn publish(ctx: &Ctx, ic: &IncidentCapture, reason: &str, note: crate::crd::StatusMessage) {
    let _ = ctx
        .recorder
        .publish(
            &Event {
                type_: EventType::Warning,
                reason: reason.into(),
                note: Some(note.into_string()),
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
    message: Option<crate::crd::StatusMessage>,
    gen: Option<i64>,
) -> Result<(), Error> {
    let status = json!({ "status": {
        "phase": phase, "seal": seal, "message": message.map(|m| m.into_string()),
        "observedGeneration": gen } });
    api.patch_status(name, &PatchParams::apply(MANAGER), &Patch::Merge(&status))
        .await?;
    Ok(())
}

async fn patch_status(
    api: &Api<IncidentCapture>,
    name: &str,
    phase: Phase,
    bundle_path: Option<String>,
    message: Option<crate::crd::StatusMessage>,
    gen: Option<i64>,
) -> Result<(), Error> {
    let status = json!({
        "status": {
            "phase": phase,
            "bundlePath": bundle_path,
            "message": message.map(|m| m.into_string()),
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

#[cfg(test)]
mod tests {
    use super::*;

    /// Build a capture from JSON: the CRD's own deserialisation, so a test cannot construct a
    /// shape the API server would not produce.
    fn capture(json: serde_json::Value) -> IncidentCapture {
        serde_json::from_value(json).expect("valid IncidentCapture")
    }

    fn exported(phase: &str, gen: i64, observed: i64, export_state: &str) -> IncidentCapture {
        capture(json!({
            "apiVersion": "lapilli.dev/v1alpha1",
            "kind": "IncidentCapture",
            "metadata": { "name": "ic-1", "namespace": "lapilli-system", "generation": gen },
            "spec": {
                "incidentId": "inc-1", "clusterId": "c", "profile": "default",
                "trigger": { "rule": "R", "firingTs": "2026-09-22T00:00:00Z" },
                "target": { "namespace": "default", "pod": "p" },
            },
            "status": {
                "phase": phase,
                "observedGeneration": observed,
                "exports": { "d1": { "state": export_state, "url": "s3://b/k" } },
            },
        }))
    }

    /// The whole point of the mechanism: a capture with nothing left to do leaves the watch.
    #[test]
    fn an_exported_capture_with_settled_exports_and_no_notification_retires() {
        let dir = tempfile::tempdir().unwrap();
        let ic = exported("Exported", 1, 1, "uploaded");
        assert!(finished_on_disk(&ic, dir.path(), false));
    }

    /// The BLOCKER the design review found. `lapilli.dev/retry-seal` is a documented recovery
    /// (docs/kms.md) that only a reconcile can act on, and a spec edit re-drives the pre-seal
    /// case — so a retired Failed capture silently ignores both, and the status message the
    /// controller itself writes becomes a lie.
    #[test]
    fn a_failed_capture_is_never_retired() {
        let dir = tempfile::tempdir().unwrap();
        for state in ["uploaded", "refused", "failed"] {
            let ic = exported("Failed", 1, 1, state);
            assert!(
                !finished_on_disk(&ic, dir.path(), false),
                "Failed must stay watched (export state {state}): retry-seal needs a reconcile"
            );
        }
    }

    /// A pending export is a retry that is still due.
    #[test]
    fn a_pending_export_keeps_the_capture_watched() {
        let dir = tempfile::tempdir().unwrap();
        let ic = exported("Exported", 1, 1, "pending");
        assert!(!finished_on_disk(&ic, dir.path(), false));
    }

    /// Refused, Conflict and Failed are settled: the attempt is over and recorded. A capture
    /// whose destination vanished from the config lands here, which is the population the
    /// design was unsure about — it does retire.
    #[test]
    fn refused_and_conflict_are_settled_enough_to_retire() {
        let dir = tempfile::tempdir().unwrap();
        for state in ["refused", "conflict", "failed"] {
            let ic = exported("Exported", 1, 1, state);
            assert!(
                finished_on_disk(&ic, dir.path(), false),
                "export state {state} is settled"
            );
        }
    }

    /// The spec moved under us: decide again against a fresh reading rather than on a stale one.
    #[test]
    fn a_stale_generation_keeps_the_capture_watched() {
        let dir = tempfile::tempdir().unwrap();
        let ic = exported("Exported", 2, 1, "uploaded");
        assert!(!finished_on_disk(&ic, dir.path(), false));
    }

    /// With a dispatcher configured the sweep trusts only a claim already on disk. It will not
    /// read a CaptureProfile and decide a route's business from the startup path — that is what
    /// the reconcile is for, and guessing here would announce history or skip an announcement.
    #[test]
    fn with_notification_configured_the_sweep_waits_for_the_claim() {
        let dir = tempfile::tempdir().unwrap();
        let ic = exported("Exported", 1, 1, "uploaded");
        assert!(
            !finished_on_disk(&ic, dir.path(), true),
            "no claim yet: leave it to the reconcile"
        );
        std::fs::write(dir.path().join("inc-1.notified"), b"").unwrap();
        assert!(
            finished_on_disk(&ic, dir.path(), true),
            "the claim is the dispatcher saying it is done with every member of the group"
        );
    }

    /// The one-second window that cost an E2E failure twice before anyone found it.
    ///
    /// Kubernetes stores `creationTimestamp` at one-second resolution, so a capture created 0.7 s
    /// AFTER the process started carries `HH:MM:SS.000` while `started_at` is `HH:MM:SS.157` — and
    /// a raw comparison files it as history: claimed, never announced. The guard must compare at
    /// the same resolution as the value it is judging.
    #[test]
    fn a_capture_created_in_the_same_second_as_the_start_is_not_history() {
        use chrono::TimeZone;
        let started = Utc
            .with_ymd_and_hms(2026, 9, 23, 2, 26, 58)
            .unwrap()
            .checked_add_signed(chrono::Duration::milliseconds(157))
            .unwrap();
        let cutoff = history_before(started);

        // What the API server actually stores for a capture made at .899 in that same second.
        let created = Utc.with_ymd_and_hms(2026, 9, 23, 2, 26, 58).unwrap();
        assert!(
            !(created < cutoff),
            "a capture from the same second as the start must not read as history"
        );
        // Naive comparison: the bug, kept here so the test says what it is guarding against.
        assert!(
            created < started,
            "the raw comparison is what got this wrong"
        );

        // A genuinely older capture is still history.
        let old = Utc.with_ymd_and_hms(2026, 9, 23, 2, 26, 57).unwrap();
        assert!(old < cutoff, "the previous second is still history");
    }

    /// Already retired: not counted again, and not patched again.
    #[test]
    fn an_already_retired_capture_is_not_retired_twice() {
        let dir = tempfile::tempdir().unwrap();
        let mut ic = exported("Exported", 1, 1, "uploaded");
        ic.metadata
            .labels
            .get_or_insert_with(Default::default)
            .insert(RETIRED.to_string(), "true".to_string());
        assert!(!finished_on_disk(&ic, dir.path(), false));
    }

    fn report(results: &[(&'static str, crate::perms::Outcome)]) -> crate::perms::Report {
        crate::perms::Report {
            results: results.iter().cloned().collect(),
            at: chrono::DateTime::parse_from_rfc3339("2026-09-24T10:00:00Z")
                .unwrap()
                .with_timezone(&Utc),
            asked: results.len(),
        }
    }

    /// The attribution is a claim about cause, so it is made only when the cause was observed:
    /// a denied check the missing collector depends on, in the latest report.
    #[test]
    fn a_missing_collector_is_tied_to_the_denial_that_stopped_it() {
        use crate::perms::Outcome::*;
        let s = |v: &[&str]| v.iter().map(|x| x.to_string()).collect::<Vec<_>>();
        let r = report(&[("pods", Held), ("pod-logs", Denied), ("events", Held)]);
        let notes = attribute_denials(Some(&r), &s(&["logs", "resources"]), &s(&["resources"]));
        assert_eq!(notes.len(), 1, "{notes:?}");
        assert!(notes[0].starts_with("logs did not run;"), "{}", notes[0]);
        assert!(notes[0].contains("pod-logs"), "{}", notes[0]);
        assert!(
            notes[0].contains("2026-09-24T10:00:00Z"),
            "the check's time, so a stale answer can be told from a fresh one: {}",
            notes[0]
        );
    }

    #[test]
    fn nothing_is_attributed_without_an_observed_cause() {
        use crate::perms::Outcome::*;
        let s = |v: &[&str]| v.iter().map(|x| x.to_string()).collect::<Vec<_>>();
        // No pass has completed yet.
        assert!(attribute_denials(None, &s(&["logs"]), &s(&[])).is_empty());
        // The collector ran, whatever the report says.
        let r = report(&[("pod-logs", Denied)]);
        assert!(attribute_denials(Some(&r), &s(&["logs"]), &s(&["logs"])).is_empty());
        // The collector did not run, but nothing it depends on was denied: held is not a cause,
        // and neither is unknown.
        let r = report(&[("pods", Held), ("pod-logs", Unknown), ("captures", Denied)]);
        assert!(
            attribute_denials(Some(&r), &s(&["logs"]), &s(&[])).is_empty(),
            "`captures` is not a collector check and `unknown` is not a denial"
        );
    }
}
