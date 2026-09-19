//! Kairn controller entrypoint.
//!
//! `run`    — start the Alertmanager webhook receiver + the IncidentCapture reconcile loop.
//! `crdgen` — print the CRD YAML (CI diffs this against the committed manifests so the YAML
//!            never drifts from the Rust types).

mod collector;
mod crd;
mod metrics;
mod reconcile;
mod webhook;

use std::sync::Arc;

use clap::{Parser, Subcommand};
use futures::StreamExt;
use kube::runtime::watcher::Config as WatcherConfig;
use kube::runtime::Controller;
use kube::{Api, Client, CustomResourceExt};

use crd::{CaptureProfile, IncidentCapture};
use reconcile::{error_policy, reconcile, Ctx};
use webhook::{router, WebhookState};

#[derive(Parser)]
#[command(name = "kairn-controller", version)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Run the webhook receiver + reconcile loop.
    Run {
        /// Namespace to create/watch IncidentCapture CRs in.
        #[arg(long, env = "KAIRN_NAMESPACE", default_value = "kairn-system")]
        namespace: String,
        /// Cluster id recorded in every bundle.
        #[arg(long, env = "KAIRN_CLUSTER_ID", default_value = "unknown-cluster")]
        cluster_id: String,
        /// CaptureProfile name to attach to new captures.
        #[arg(long, env = "KAIRN_PROFILE", default_value = "default")]
        profile: String,
        /// Address for the webhook server.
        #[arg(long, env = "KAIRN_LISTEN", default_value = "0.0.0.0:8080")]
        listen: String,
    },
    /// Print the CRD YAML (both CRDs) to stdout.
    Crdgen,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Command::Crdgen => {
            // Two YAML documents, deterministic order, for the committed-manifest diff check.
            print!("{}", serde_yaml_str(&IncidentCapture::crd())?);
            println!("---");
            print!("{}", serde_yaml_str(&CaptureProfile::crd())?);
            Ok(())
        }
        Command::Run {
            namespace,
            cluster_id,
            profile,
            listen,
        } => run(namespace, cluster_id, profile, listen).await,
    }
}

async fn run(
    namespace: String,
    cluster_id: String,
    profile: String,
    listen: String,
) -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,kube=warn".into()),
        )
        .init();

    // kube uses rustls-tls; rustls 0.23 requires an explicitly installed crypto provider.
    rustls::crypto::ring::default_provider()
        .install_default()
        .map_err(|_| anyhow::anyhow!("failed to install rustls ring crypto provider"))?;

    let client = Client::try_default().await?;

    // Webhook server.
    let wh_state = WebhookState {
        client: client.clone(),
        namespace: namespace.clone(),
        cluster_id,
        profile,
    };
    let app = router(wh_state);
    let listener = tokio::net::TcpListener::bind(&listen).await?;
    tracing::info!(%listen, "webhook listening");
    let server = tokio::spawn(async move {
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!(error = %e, "webhook server exited");
        }
    });

    // Controller.
    let ic_api: Api<IncidentCapture> = Api::namespaced(client.clone(), &namespace);
    let ctx = Arc::new(Ctx {
        client: client.clone(),
    });
    tracing::info!(%namespace, "starting IncidentCapture controller");
    let controller = Controller::new(ic_api, WatcherConfig::default())
        .run(reconcile, error_policy, ctx)
        .for_each(|res| async move {
            match res {
                Ok((obj, _)) => tracing::debug!(?obj, "reconciled"),
                Err(e) => tracing::warn!(error = %e, "reconcile error"),
            }
        });

    tokio::select! {
        _ = controller => tracing::warn!("controller stream ended"),
        _ = server => tracing::warn!("webhook task ended"),
        _ = tokio::signal::ctrl_c() => tracing::info!("shutdown signal"),
    }
    Ok(())
}

/// Minimal YAML serialization without pulling a yaml crate: serialize to JSON value then
/// emit YAML via `serde_json` is not YAML, so we use a tiny dependency-free JSON→string.
/// CRD consumers (kubectl) accept JSON as valid YAML, so we emit JSON documents.
fn serde_yaml_str<T: serde::Serialize>(v: &T) -> anyhow::Result<String> {
    // kubectl apply accepts JSON manifests; JSON is a subset of YAML 1.2.
    Ok(serde_json::to_string_pretty(v)? + "\n")
}
