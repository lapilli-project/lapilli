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

use kairn_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
use kairn_bundle::{seal_dir, SealInput, StaticKeySigner};

use crate::collector::{collect_all, CollectCtx, Redactor};
use crate::crd::{CaptureProfile, ExportState, ExportStatus, IncidentCapture, Phase, SigningMode};
use crate::export::{backoff, Exporter, Outcome, MAX_ATTEMPTS};
use kube::runtime::events::{Event, EventType, Recorder};
use kube::Resource;
use std::collections::BTreeMap;

pub struct Ctx {
    pub client: Client,
    pub exporter: Arc<Exporter>,
    pub recorder: Recorder,
}

#[derive(thiserror::Error, Debug)]
pub enum Error {
    #[error("kube: {0}")]
    Kube(#[from] kube::Error),
    #[error("capture: {0}")]
    Capture(String),
}

const MANAGER: &str = "kairn-controller";

/// Reconcile one IncidentCapture: capture once, then drive its object-store exports until
/// every destination is settled.
pub async fn reconcile(ic: Arc<IncidentCapture>, ctx: Arc<Ctx>) -> Result<Action, Error> {
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let name = ic.name_any();
    let gen = ic.metadata.generation;
    let api: Api<IncidentCapture> = Api::namespaced(ctx.client.clone(), &ns);

    let status = ic.status.clone().unwrap_or_default();
    let current = status.observed_generation == gen;
    if current && status.phase == Phase::Failed {
        return Ok(Action::await_change());
    }
    // A capture is a one-time event: once exported it is never re-captured, even if its
    // spec is edited (re-packing would change the local bytes behind an uploaded object).
    let (bundle_path, exports) = if status.phase == Phase::Exported {
        (
            status.bundle_path.clone().unwrap_or_default(),
            status.exports.clone(),
        )
    } else {
        match run_capture(&ic, &ctx).await {
            Ok((bundle_path, destinations)) => {
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
                patch_status(&api, &name, Phase::Failed, None, Some(e.to_string()), gen).await?;
                tracing::warn!(%ns, %name, error = %e, "capture failed");
                return Ok(Action::await_change());
            }
        }
    };
    drive_exports(&api, &ic, &ctx, &bundle_path, exports).await
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
    Ok(match next {
        Some(d) => Action::requeue(d),
        None => Action::await_change(),
    })
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
    api.patch_status(name, &PatchParams::apply(MANAGER), &Patch::Merge(&status))
        .await?;
    Ok(())
}

/// Capture, seal and pack; returns the local bundle path and the profile's destinations.
async fn run_capture(ic: &IncidentCapture, ctx: &Ctx) -> Result<(String, Vec<String>), Error> {
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
    let export_root = std::path::Path::new(&pspec.export.path);
    std::fs::create_dir_all(export_root).map_err(|e| Error::Capture(e.to_string()))?;
    let stage = export_root.join(format!(".staging-{}", spec.incident_id));
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
            kairn_version: env!("CARGO_PKG_VERSION").to_string(),
            image_digest: std::env::var("KAIRN_IMAGE_DIGEST").unwrap_or_else(|_| "unknown".into()),
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

    // Optional signing.
    let signer = load_signer(ctx, &ns, pspec).await?;
    seal_dir(
        &stage,
        input,
        signer.as_ref().map(|s| s as &dyn kairn_bundle::Signer),
    )
    .map_err(|e| Error::Capture(e.to_string()))?;

    // Pack the sealed staging dir into a single portable `.ieb` file (the headline
    // artifact — "one portable file you own"), then remove the staging dir.
    let ieb = export_root.join(format!("{}.ieb", spec.incident_id));
    let _ = std::fs::remove_file(&ieb);
    kairn_bundle::pack(&stage, &ieb).map_err(|e| Error::Capture(e.to_string()))?;
    let _ = std::fs::remove_dir_all(&stage);

    tracing::info!(%ns, %name, path = %ieb.display(), "sealed bundle");
    Ok((
        ieb.to_string_lossy().to_string(),
        pspec.export.destinations.clone(),
    ))
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
    Action::requeue(Duration::from_secs(10))
}
