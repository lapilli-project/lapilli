//! Kairn controller entrypoint.
//!
//! `run`    — start the Alertmanager webhook receiver + the IncidentCapture reconcile loop.
//! `crdgen` — print the CRD YAML (CI diffs this against the committed manifests so the YAML
//!            never drifts from the Rust types).

mod collector;
mod crd;
mod diffs;
mod export;
mod metrics;
mod notify;
mod reconcile;
mod sealing;
mod specdiff;
mod telemetry;
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

/// Everything `run` is configured with. One struct rather than a dozen positional
/// parameters: every one of these is also an env var the chart sets.
#[derive(clap::Args)]
struct RunArgs {
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
    /// Object-store destinations defined by the admin (JSON list; missing = none).
    #[arg(
        long,
        env = "KAIRN_DESTINATIONS_FILE",
        default_value = "/etc/kairn/destinations.json"
    )]
    destinations_file: String,
    /// Address for /healthz (separate, so a NetworkPolicy on the webhook port never
    /// blocks probes).
    #[arg(long, env = "KAIRN_HEALTH_LISTEN", default_value = "0.0.0.0:8081")]
    health_listen: String,
    /// File holding the webhook bearer token. Unset = unauthenticated webhook.
    #[arg(long, env = "KAIRN_WEBHOOK_TOKEN_FILE")]
    webhook_token_file: Option<String>,
    /// Notification routes the admin defined (JSON list; missing = notification off).
    #[arg(
        long,
        env = "KAIRN_NOTIFY_ROUTES_FILE",
        default_value = "/etc/kairn/notify/routes.json"
    )]
    notify_routes_file: String,
    /// Directory holding each route's path secret at `<route>/path`.
    #[arg(
        long,
        env = "KAIRN_NOTIFY_SECRETS_DIR",
        default_value = "/etc/kairn/notify/secrets"
    )]
    notify_secrets_dir: String,
}

#[derive(Subcommand)]
enum Command {
    /// Run the webhook receiver + reconcile loop.
    // Boxed only to keep the enum small: `run` is the whole program, `crdgen` prints a schema.
    Run(Box<RunArgs>),
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
        Command::Run(args) => run(*args).await,
    }
}

async fn run(args: RunArgs) -> anyhow::Result<()> {
    let RunArgs {
        namespace,
        cluster_id,
        profile,
        listen,
        destinations_file,
        health_listen,
        webhook_token_file,
        notify_routes_file,
        notify_secrets_dir,
    } = args;
    // The cluster id is part of every incident id and object key: `<cluster>-<16 hex>`
    // must stay a path-safe segment of at most 100 characters.
    anyhow::ensure!(
        (1..=83).contains(&cluster_id.len())
            && cluster_id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
            && cluster_id != "."
            && cluster_id != "..",
        "cluster id {cluster_id:?} must be [A-Za-z0-9._-], at most 83 characters"
    );
    let cluster_id_for_export = cluster_id.clone();
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
    let token = match webhook_token_file.map(std::path::PathBuf::from) {
        Some(f) => {
            // Fail at start rather than accept or reject everything silently.
            let t = std::fs::read_to_string(&f)
                .map_err(|e| anyhow::anyhow!("webhook token file {}: {e}", f.display()))?;
            anyhow::ensure!(
                t.trim().len() >= webhook::MIN_TOKEN_LEN,
                "webhook token in {} is shorter than {} characters",
                f.display(),
                webhook::MIN_TOKEN_LEN
            );
            tracing::info!("webhook requires a bearer token");
            Some(std::sync::Arc::new(webhook::TokenCache::new(f)))
        }
        None => {
            tracing::warn!(
                "webhook is UNAUTHENTICATED: anyone who can reach it can trigger captures \
                 (set KAIRN_WEBHOOK_TOKEN_FILE; the Helm chart does this by default)"
            );
            None
        }
    };
    let wh_state = WebhookState {
        client: client.clone(),
        namespace: namespace.clone(),
        cluster_id,
        profile,
        token,
    };
    let app = router(wh_state);
    let listener = tokio::net::TcpListener::bind(&listen).await?;
    tracing::info!(%listen, "webhook listening");
    let server = tokio::spawn(async move {
        if let Err(e) = axum::serve(listener, app).await {
            tracing::error!(error = %e, "webhook server exited");
        }
    });
    let health = tokio::net::TcpListener::bind(&health_listen).await?;
    tokio::spawn(async move {
        if let Err(e) = axum::serve(health, webhook::health_router()).await {
            tracing::error!(error = %e, "health server exited");
        }
    });

    // Controller.
    let ic_api: Api<IncidentCapture> = Api::namespaced(client.clone(), &namespace);
    let exporter = Arc::new(
        export::Exporter::load(
            &client,
            &namespace,
            cluster_id_for_export.clone(),
            std::path::Path::new(&destinations_file),
        )
        .await,
    );
    let recorder = kube::runtime::events::Recorder::new(
        client.clone(),
        kube::runtime::events::Reporter {
            controller: "kairn".into(),
            instance: std::env::var("POD_NAME").ok(),
        },
    );
    // KMS signing (admin-configured): every bundle is signed with this key.
    let kms = match std::env::var("KAIRN_SIGNING_KMS_KEY")
        .ok()
        .filter(|k| !k.is_empty())
    {
        Some(k) => {
            let key = kairn_kms::KmsKey::parse(&k)
                .map_err(|e| anyhow::anyhow!("KAIRN_SIGNING_KMS_KEY: {e}"))?;
            let kms = Arc::new(sealing::Kms::new(key));
            kms.spawn_preflight();
            tracing::info!(key = %k, "KMS signing on: every bundle is signed with this key");
            Some(kms)
        }
        None => None,
    };
    let bundle_root =
        std::env::var("KAIRN_BUNDLE_ROOT").unwrap_or_else(|_| "/var/lib/kairn/bundles".into());
    // Notification (admin-configured). Plain HTTP is a test-only switch, and even then only
    // to a loopback or cluster-local host — a route must also set `insecureHttp`.
    let allow_http = std::env::var("KAIRN_NOTIFY_ALLOW_HTTP").as_deref() == Ok("true");
    let routes = Arc::new(notify::Routes::load(
        std::path::Path::new(&notify_routes_file),
        std::path::Path::new(&notify_secrets_dir),
        allow_http,
    ));
    for (name, why) in &routes.errors {
        tracing::error!(route = %name, %why, "notification route disabled");
    }
    if !routes.by_name.is_empty() || !routes.errors.is_empty() {
        telemetry::metrics()
            .set_notify_routes(routes.by_name.len() as u64, routes.errors.len() as u64);
    }
    let dispatcher = if routes.by_name.is_empty() {
        if !routes.errors.is_empty() {
            tracing::warn!("every notification route is unusable; notification is off");
        }
        None
    } else {
        for route in routes.by_name.values() {
            tracing::info!(
                route = %route.spec.name,
                endpoint = %route.endpoint.display(),
                format = ?route.spec.format,
                detail = ?route.spec.detail,
                "notification route ready"
            );
            if allow_http && route.endpoint.scheme == "http" {
                tracing::warn!(route = %route.spec.name,
                               "this route is PLAIN HTTP for tests only (KAIRN_NOTIFY_ALLOW_HTTP is set)");
            }
        }
        Some(notify::spawn(
            client.clone(),
            routes.clone(),
            std::path::PathBuf::from(&bundle_root),
            notify::Site::from_env(&namespace),
        ))
    };
    let ctx = Arc::new(Ctx {
        client: client.clone(),
        exporter,
        recorder,
        cluster_id: cluster_id_for_export.clone(),
        bundle_root,
        kms,
        notify: dispatcher,
        started_at: chrono::Utc::now(),
    });
    // Kept out of the Arc the controller consumes, so the shutdown path can still drain it.
    let dispatcher = ctx.notify.clone();
    telemetry::spawn_state_poller(ic_api.clone(), std::time::Duration::from_secs(30));
    tracing::info!(%namespace, "starting IncidentCapture controller");
    // One signal, two consumers: the controller stops accepting new work and waits for the
    // reconciles in flight, and only then is the notification dispatcher drained.
    //
    // Cancelling in-flight reconciles instead would be a regression introduced by handling
    // SIGTERM at all: before, SIGTERM was unhandled, so a capture mid-collection kept going until
    // the kubelet's SIGKILL. Capture is the product, so a rollout must not cut one short.
    let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();
    let controller = Controller::new(ic_api, WatcherConfig::default())
        .graceful_shutdown_on(async {
            let _ = stop_rx.await;
        })
        .run(reconcile, error_policy, ctx)
        .for_each(|res| async move {
            match res {
                Ok((obj, _)) => tracing::debug!(?obj, "reconciled"),
                Err(e) => tracing::warn!(error = %e, "reconcile error"),
            }
        });

    tokio::pin!(controller);
    tokio::select! {
        _ = &mut controller => tracing::warn!("controller stream ended"),
        _ = server => tracing::warn!("webhook task ended"),
        signal = shutdown_signal() => {
            tracing::info!(%signal, "shutting down");
            let _ = stop_tx.send(());
            // The reconciles in flight finish; a capture is worth more than a fast exit. Bounded
            // so a wedged one cannot hold the pod to the end of its grace period.
            if tokio::time::timeout(RECONCILE_GRACE, &mut controller).await.is_err() {
                tracing::warn!(timeout = ?RECONCILE_GRACE,
                               "reconciles still running at shutdown; they resume after restart");
            }
        }
    }
    // Notification groups are claimed before they are posted, so one abandoned here is marked
    // notified and never announced. Kubernetes sends SIGTERM on every rollout, so this is the
    // ordinary path. Bounded well inside the default 30 s grace period.
    if let Some(dispatcher) = dispatcher.as_ref() {
        dispatcher.drain(NOTIFY_DRAIN).await;
    }
    Ok(())
}

/// How long in-flight reconciles get after a shutdown signal, and how long the notification
/// dispatcher gets after them. Both together stay inside the default 30 s
/// `terminationGracePeriodSeconds`; a capture that does not finish is retried after the restart.
const RECONCILE_GRACE: std::time::Duration = std::time::Duration::from_secs(12);
const NOTIFY_DRAIN: std::time::Duration = std::time::Duration::from_secs(10);

/// Resolves on the signals Kubernetes and a terminal actually send, naming which arrived.
///
/// `ctrl_c` alone was wrong in the case that matters: the kubelet sends **SIGTERM**, so every
/// rollout skipped the shutdown path entirely and the pod was killed at the end of its grace
/// period instead.
async fn shutdown_signal() -> &'static str {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        let mut term = match signal(SignalKind::terminate()) {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!(error = %e, "cannot listen for SIGTERM; SIGINT only");
                let _ = tokio::signal::ctrl_c().await;
                return "SIGINT";
            }
        };
        tokio::select! {
            _ = term.recv() => "SIGTERM",
            _ = tokio::signal::ctrl_c() => "SIGINT",
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
        "SIGINT"
    }
}

/// Minimal YAML serialization without pulling a yaml crate: serialize to JSON value then
/// emit YAML via `serde_json` is not YAML, so we use a tiny dependency-free JSON→string.
/// CRD consumers (kubectl) accept JSON as valid YAML, so we emit JSON documents.
fn serde_yaml_str<T: serde::Serialize>(v: &T) -> anyhow::Result<String> {
    // kubectl apply accepts JSON manifests; JSON is a subset of YAML 1.2.
    Ok(serde_json::to_string_pretty(v)? + "\n")
}
