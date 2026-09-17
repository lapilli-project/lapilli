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

use crate::collector::collect_all;
use crate::crd::{CaptureProfile, IncidentCapture, Phase, SigningMode};

pub struct Ctx {
    pub client: Client,
}

#[derive(thiserror::Error, Debug)]
pub enum Error {
    #[error("kube: {0}")]
    Kube(#[from] kube::Error),
    #[error("capture: {0}")]
    Capture(String),
}

const MANAGER: &str = "kairn-controller";

/// Reconcile one IncidentCapture. Runs the whole capture in a single pass for v0.1.
pub async fn reconcile(ic: Arc<IncidentCapture>, ctx: Arc<Ctx>) -> Result<Action, Error> {
    let ns = ic.namespace().unwrap_or_else(|| "default".to_string());
    let name = ic.name_any();
    let gen = ic.metadata.generation;
    let api: Api<IncidentCapture> = Api::namespaced(ctx.client.clone(), &ns);

    // Idempotency: terminal phase for the current generation → nothing to do.
    if let Some(st) = &ic.status {
        let terminal = matches!(st.phase, Phase::Exported | Phase::Failed);
        if terminal && st.observed_generation == gen {
            return Ok(Action::await_change());
        }
    }

    match run_capture(&ic, &ctx).await {
        Ok(bundle_path) => {
            patch_status(&api, &name, Phase::Exported, Some(bundle_path), None, gen).await?;
            tracing::info!(%ns, %name, "capture exported");
        }
        Err(e) => {
            patch_status(&api, &name, Phase::Failed, None, Some(e.to_string()), gen).await?;
            tracing::warn!(%ns, %name, error = %e, "capture failed");
        }
    }
    Ok(Action::await_change())
}

async fn run_capture(ic: &IncidentCapture, ctx: &Ctx) -> Result<String, Error> {
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
    let outcome = collect_all(&ctx.client, &spec.target, &pspec.collectors, &stage).await;

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
    Ok(ieb.to_string_lossy().to_string())
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
