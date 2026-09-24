//! Lapilli controller entrypoint.
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
mod perms;
mod reconcile;
mod retention;
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
#[command(name = "lapilli-controller", version)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

/// Everything `run` is configured with. One struct rather than a dozen positional
/// parameters: every one of these is also an env var the chart sets.
#[derive(clap::Args)]
struct RunArgs {
    /// Namespace to create/watch IncidentCapture CRs in.
    #[arg(long, env = "LAPILLI_NAMESPACE", default_value = "lapilli-system")]
    namespace: String,
    /// Cluster id recorded in every bundle.
    #[arg(long, env = "LAPILLI_CLUSTER_ID", default_value = "unknown-cluster")]
    cluster_id: String,
    /// CaptureProfile name to attach to new captures.
    /// How many captures may be collected at once.
    ///
    /// Measured with `test/e2e/storm.sh` on kind, one payload of 20 realistic alerts, a fresh pod
    /// per value, peak read from the kernel's `memory.peak`:
    ///
    /// | concurrency | peak | % of 256 MiB | CPU |
    /// |---|---|---|---|
    /// | 1 | 124.0 MiB | 48.4% | 2.18s |
    /// | **2** | **121.3 MiB** | **47.4%** | **3.19s** |
    /// | 8 | 162.0 MiB | 63.3% | 8.32s |
    /// | 0 (kube-rs default, unbounded) | 183.8 MiB | 71.8% | 5.56s |
    ///
    /// Read it carefully, because an earlier version of this comment did not. Every row is **one
    /// run**, and the peak is the cgroup high-water mark since the pod started, which includes
    /// the informer's first list — so 124.0 at concurrency 1 coming out *above* 121.3 at
    /// concurrency 2 is the noise floor showing, and no two adjacent rows are distinguishable.
    /// What the ends support is only the direction: unbounded peaked about 60 MiB higher than a
    /// bound of 2, and used more CPU for identical work. `2` is chosen for that, not for 121.3.
    ///
    /// Wall clock is deliberately not in the table. The same 20-alert storm has measured between
    /// 1 and 200 seconds depending on whether the metrics collector had anything to fetch, which
    /// is a property of the cluster's Prometheus and the alert's age, not of this setting.
    ///
    /// `0` restores the unbounded default, for an operator who has measured their own cluster.
    #[arg(long, env = "LAPILLI_RECONCILE_CONCURRENCY", default_value_t = 2)]
    reconcile_concurrency: u16,

    /// Most captures one webhook payload may create. `0` = unlimited.
    ///
    /// This bounds a storm at its source, and it exists because nothing else does. The body limit
    /// admits about 990 realistic alerts in one payload, while the largest storm this project has
    /// measured end to end is **50** — which already peaked at 146.8 MiB of the chart's 256 MiB
    /// limit, against 124.0 MiB at 20 alerts. Cost grows with the size of the storm, the growth
    /// was never bounded, and a payload past the measured envelope is not a neutral experiment:
    /// an OOM mid-storm loses the captures already in flight as well as the ones refused.
    ///
    /// The default is the largest measured storm, not a calculated ceiling, and raising it is an
    /// invitation to measure rather than to guess. Alerts past the cap are **counted**
    /// (`lapilli_alerts_dropped_total{reason="payload-cap"}`), logged, and returned in the
    /// webhook's `dropped` field — a cap that loses evidence silently would be the failure this
    /// product exists to remove.
    #[arg(long, env = "LAPILLI_MAX_CAPTURES_PER_PAYLOAD", default_value_t = 50)]
    max_captures_per_payload: u32,
    #[arg(long, env = "LAPILLI_PROFILE", default_value = "default")]
    profile: String,
    /// Address for the webhook server.
    #[arg(long, env = "LAPILLI_LISTEN", default_value = "0.0.0.0:8080")]
    listen: String,
    /// Object-store destinations defined by the admin (JSON list; missing = none).
    #[arg(
        long,
        env = "LAPILLI_DESTINATIONS_FILE",
        default_value = "/etc/lapilli/destinations.json"
    )]
    destinations_file: String,
    /// Address for /healthz (separate, so a NetworkPolicy on the webhook port never
    /// blocks probes).
    #[arg(long, env = "LAPILLI_HEALTH_LISTEN", default_value = "0.0.0.0:8081")]
    health_listen: String,
    /// File holding the webhook bearer token. Unset = unauthenticated webhook.
    #[arg(long, env = "LAPILLI_WEBHOOK_TOKEN_FILE")]
    webhook_token_file: Option<String>,
    /// Notification routes the admin defined (JSON list; missing = notification off).
    #[arg(
        long,
        env = "LAPILLI_NOTIFY_ROUTES_FILE",
        default_value = "/etc/lapilli/notify/routes.json"
    )]
    notify_routes_file: String,
    /// Namespaces the chart scoped this install to (comma-separated). Empty means cluster-wide,
    /// which is what the chart generates when `watchNamespaces` is unset. Used only to ask the
    /// right permission questions (`perms.rs`): a cluster-wide question fails on a namespaced
    /// install, and a single-namespace question cannot tell a ClusterRole from a Role.
    #[arg(long, env = "LAPILLI_WATCH_NAMESPACES", default_value = "")]
    watch_namespaces: String,
    /// Bundle volume ceiling in bytes; the sweep reclaims oldest-first above it. `0` = off.
    /// The chart derives it from `persistence.size`.
    #[arg(long, env = "LAPILLI_RETENTION_MAX_BYTES", default_value = "0")]
    retention_max_bytes: u64,
    /// Reclaim bundles older than this many days. `0` = off. A secondary trim: on the chart's
    /// defaults an age window never engages before the disk fills (docs/design-retention.md).
    #[arg(long, env = "LAPILLI_RETENTION_DAYS", default_value = "0")]
    retention_days: u32,
    /// Refuse to start a capture with less than this free on the bundle volume.
    #[arg(long, env = "LAPILLI_RETENTION_MIN_FREE_BYTES", default_value = "0")]
    retention_min_free_bytes: u64,
    /// Reclaim bundles that reached no destination. On a PVC-only install the local copy is the
    /// only copy, which is why this is explicit and off by default.
    #[arg(
        long,
        env = "LAPILLI_RETENTION_ALLOW_UNEXPORTED",
        default_value = "false"
    )]
    retention_allow_unexported: bool,
    /// Reclaim files whose IncidentCapture is gone. Off by default: nothing in this controller
    /// deletes a capture, so "no live CR" describes human behaviour, and one `delete --all` would
    /// otherwise authorise a mass delete.
    #[arg(
        long,
        env = "LAPILLI_RETENTION_RECLAIM_ORPHANS",
        default_value = "false"
    )]
    retention_reclaim_orphans: bool,
    /// Directory holding each route's path secret at `<route>/path`.
    #[arg(
        long,
        env = "LAPILLI_NOTIFY_SECRETS_DIR",
        default_value = "/etc/lapilli/notify/secrets"
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
            print!(
                "{}",
                serde_yaml_str(&with_profile_rules(CaptureProfile::crd()))?
            );
            Ok(())
        }
        Command::Run(args) => run(*args).await,
    }
}

/// CEL rules the schema derive cannot express, added to the generated CRD so the **API server**
/// refuses the shape rather than the controller discovering it per capture. `collectors` and
/// `deferred` are disjoint: a name in both would seal a bundle that verifies FAILED (malformed
/// coverage) by the controller's own hand. The reconciler keeps the same check for an API server
/// that does not enforce CEL.
///
/// Panics if the generated schema does not have the shape this reaches into: this runs at
/// generation time, and a rule silently not injected is worse than a failed build.
fn with_profile_rules(
    mut crd: k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition,
) -> k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::CustomResourceDefinition {
    use k8s_openapi::apiextensions_apiserver::pkg::apis::apiextensions::v1::ValidationRule;
    let spec = crd
        .spec
        .versions
        .iter_mut()
        .find(|v| v.name == "v1alpha1")
        .and_then(|v| v.schema.as_mut())
        .and_then(|s| s.open_api_v3_schema.as_mut())
        .and_then(|s| s.properties.as_mut())
        .and_then(|p| p.get_mut("spec"))
        .expect("CaptureProfile v1alpha1 schema has a `spec` object to attach rules to");
    spec.x_kubernetes_validations = Some(vec![ValidationRule {
        // `has()` guards: `deferred` is optional, and CEL errors on an absent field.
        rule: "!has(self.deferred) || !has(self.collectors) || \
               !self.collectors.exists(c, c in self.deferred)"
            .into(),
        message: Some(
            "a collector cannot be both in `collectors` and in `deferred`: deferred means \
             \"not run here, kept elsewhere\""
                .into(),
        ),
        ..Default::default()
    }]);
    crd
}

async fn run(args: RunArgs) -> anyhow::Result<()> {
    let RunArgs {
        reconcile_concurrency,
        max_captures_per_payload,
        namespace,
        cluster_id,
        profile,
        listen,
        destinations_file,
        health_listen,
        webhook_token_file,
        notify_routes_file,
        notify_secrets_dir,
        watch_namespaces,
        retention_max_bytes,
        retention_days,
        retention_min_free_bytes,
        retention_allow_unexported,
        retention_reclaim_orphans,
    } = args;
    let retention = retention::Policy {
        max_bytes: retention_max_bytes,
        days: retention_days,
        min_free_bytes: retention_min_free_bytes,
        allow_unexported: retention_allow_unexported,
        reclaim_orphans: retention_reclaim_orphans,
    };
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
        // No ANSI. This is a pod log, never a terminal, and the colour codes wrap every field name
        // so that `kubectl logs … | grep check=` matches nothing — which matters because the
        // permission self-check deliberately keeps its detail here rather than on the
        // unauthenticated /metrics endpoint (docs/metrics.md). The E2E found this the hard way.
        .with_ansi(false)
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
                 (set LAPILLI_WEBHOOK_TOKEN_FILE; the Helm chart does this by default)"
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
        max_captures_per_payload: max_captures_per_payload as usize,
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
            controller: "lapilli".into(),
            instance: std::env::var("POD_NAME").ok(),
        },
    );
    // KMS signing (admin-configured): every bundle is signed with this key.
    let kms = match std::env::var("LAPILLI_SIGNING_KMS_KEY")
        .ok()
        .filter(|k| !k.is_empty())
    {
        Some(k) => {
            let key = lapilli_kms::KmsKey::parse(&k)
                .map_err(|e| anyhow::anyhow!("LAPILLI_SIGNING_KMS_KEY: {e}"))?;
            let kms = Arc::new(sealing::Kms::new(key));
            kms.spawn_preflight();
            tracing::info!(key = %k, "KMS signing on: every bundle is signed with this key");
            Some(kms)
        }
        None => None,
    };
    let bundle_root =
        std::env::var("LAPILLI_BUNDLE_ROOT").unwrap_or_else(|_| "/var/lib/lapilli/bundles".into());
    // Notification (admin-configured). Plain HTTP is a test-only switch, and even then only
    // to a loopback or cluster-local host — a route must also set `insecureHttp`.
    let allow_http = std::env::var("LAPILLI_NOTIFY_ALLOW_HTTP").as_deref() == Ok("true");
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
                               "this route is PLAIN HTTP for tests only (LAPILLI_NOTIFY_ALLOW_HTTP is set)");
            }
        }
        Some(notify::spawn(
            client.clone(),
            routes.clone(),
            std::path::PathBuf::from(&bundle_root),
            notify::Site::from_env(&namespace),
        ))
    };
    let permissions: perms::Shared = Default::default();
    let ctx = Arc::new(Ctx {
        client: client.clone(),
        exporter,
        recorder,
        cluster_id: cluster_id_for_export.clone(),
        bundle_root,
        kms,
        notify: dispatcher,
        min_free_bytes: retention.min_free_bytes,
        started_at: chrono::Utc::now(),
        permissions: permissions.clone(),
    });
    // Kept out of the Arc the controller consumes, so the shutdown path can still drain it.
    let dispatcher = ctx.notify.clone();
    retention::spawn(
        ic_api.clone(),
        std::path::PathBuf::from(&ctx.bundle_root),
        retention.clone(),
        retention::SWEEP_INTERVAL,
    );
    telemetry::spawn_state_poller(
        ic_api.clone(),
        std::path::PathBuf::from(&ctx.bundle_root),
        std::time::Duration::from_secs(30),
    );
    // Ask the API server whether this install actually holds the permissions it needs. The poller
    // above only proves one of them (`list incidentcaptures`); a broken *collector* binding leaves
    // it reading 1 while every capture comes out empty. Reported, never fatal: see perms.rs.
    perms::spawn(
        client.clone(),
        perms::Source {
            own_namespace: namespace.clone(),
            watch_namespaces: watch_namespaces.clone(),
            destinations_file: std::path::PathBuf::from(&destinations_file),
        },
        permissions,
        // Overridable for the E2E, which cannot wait ten minutes to see a pass notice a tightened
        // Role. Not a chart value: a production install has no reason to touch it.
        std::env::var("LAPILLI_PERMS_RECHECK_SECONDS")
            .ok()
            .and_then(|s| s.parse::<u64>().ok())
            .filter(|s| *s > 0)
            .map(std::time::Duration::from_secs)
            .unwrap_or(perms::RECHECK),
    );
    // Before the watch exists, so a large accumulated population is never held. See
    // `retire_finished_on_start` for why this cannot be done through the informer.
    reconcile::retire_finished_on_start(&ic_api, &ctx.bundle_root, ctx.notify.is_some()).await;

    tracing::info!(%namespace, "starting IncidentCapture controller");
    // One signal, two consumers: the controller stops accepting new work and waits for the
    // reconciles in flight, and only then is the notification dispatcher drained.
    //
    // Cancelling in-flight reconciles instead would be a regression introduced by handling
    // SIGTERM at all: before, SIGTERM was unhandled, so a capture mid-collection kept going until
    // the kubelet's SIGKILL. Capture is the product, so a rollout must not cut one short.
    let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();
    tracing::info!(concurrency = reconcile_concurrency, "reconcile concurrency");
    // The watch is SCOPED: a retired capture is not delivered here. Nothing that needs a true
    // population may read this cache — `retention.rs` and the state poller both list instead, and
    // that is load-bearing (an orphan pass reading this would see every retired capture as an
    // orphan). `docs/design-capture-retirement.md`.
    let watch_cfg = WatcherConfig::default().labels(&format!("!{}", reconcile::RETIRED));
    let controller = Controller::new(ic_api, watch_cfg)
        .with_config(
            kube::runtime::controller::Config::default().concurrency(reconcile_concurrency),
        )
        .graceful_shutdown_on(async {
            let _ = stop_rx.await;
        })
        .run(reconcile, error_policy, ctx)
        .for_each(|res| async move {
            match res {
                Ok((obj, _)) => tracing::debug!(?obj, "reconciled"),
                // A scheduled reconcile whose object is no longer in the store. Retirement makes
                // this ordinary: the capture left the watch between being queued and being run,
                // which is the intended outcome and not a fault. It must not land in
                // `lapilli_reconcile_errors_total` — docs/metrics.md defines that series as
                // meaning "a dead watch, no capture will ever be noticed again" and alerts on it,
                // and an alert that fires during normal operation is an alert that gets silenced.
                Err(kube::runtime::controller::Error::ObjectNotFound(ref o)) => {
                    tracing::debug!(object = ?o, "reconcile skipped: the capture left the watch")
                }
                // Counted, not just logged. This arm also carries watcher-stream failures, so
                // without the counter a dead watch — no capture will ever be noticed again —
                // produced a flat `lapilli_reconcile_errors_total` and a log line nobody reads.
                Err(e) => {
                    telemetry::metrics().reconcile_error();
                    tracing::warn!(error = %e, "reconcile error");
                }
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

/// The drain window must cover one flush attempt with room to spare, or the timeout fires while a
/// send is legitimately in progress — and the group was **claimed before it was posted**, so its
/// captures end up marked notified and never announced. Asserted rather than commented, because
/// this arithmetic has already been got wrong once: round 13 cut the flush to a single attempt
/// having read its cost as the request timeout alone, and missed the separate DNS budget in front
/// of it.
const _: () = assert!(
    NOTIFY_DRAIN.as_secs() >= notify::FLUSH_ATTEMPT_MAX.as_secs() + 2,
    "NOTIFY_DRAIN must exceed one flush attempt (lapilli_net::RESOLVE_TIMEOUT + POST_BUDGET_DRAINING)"
);

/// And both budgets have to fit inside the pod's termination grace period, or the kubelet's SIGKILL
/// lands mid-flush and takes the capture *and* the notification with it, with nothing logged. The
/// chart sets `terminationGracePeriodSeconds` explicitly and fails the render if it is smaller than
/// this; the number is repeated here so the two cannot drift apart silently.
pub const SHUTDOWN_BUDGET_SECS: u64 = RECONCILE_GRACE.as_secs() + NOTIFY_DRAIN.as_secs();

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

#[cfg(test)]
mod retention_args {
    use clap::Parser;

    /// `RunArgs` derives `Args`, not `Parser`, so it is reached through the real CLI — which is also
    /// the path the container actually takes.
    fn run_args() -> super::RunArgs {
        match super::Cli::parse_from(["lapilli-controller", "run"]).command {
            super::Command::Run(a) => *a,
            _ => unreachable!("asked for `run`"),
        }
    }

    /// The two dangerous switches must be **off** when the chart passes `"false"`, and reachable when
    /// it passes `"true"`.
    ///
    /// clap models a `bool` field as a flag and silently ignores a flag's `default_value`, so the
    /// question is what a flag does when its env var is present and says `false`. The chart sets every
    /// env var unconditionally, so if presence alone meant `true`, both switches would be on in every
    /// install: retention would destroy the only copy of unexported evidence and would mass delete on
    /// a CR wipe.
    ///
    /// One test, not two, because these mutate the **process** environment: as two tests they raced
    /// each other and the false case failed for that reason alone.
    #[test]
    fn the_dangerous_switches_follow_what_the_chart_says() {
        for (set, expect) in [("false", false), ("true", true)] {
            // SAFETY: single test, and the vars are removed before it returns.
            unsafe {
                std::env::set_var("LAPILLI_RETENTION_ALLOW_UNEXPORTED", set);
                std::env::set_var("LAPILLI_RETENTION_RECLAIM_ORPHANS", set);
            }
            let args = run_args();
            let got = (
                args.retention_allow_unexported,
                args.retention_reclaim_orphans,
            );
            unsafe {
                std::env::remove_var("LAPILLI_RETENTION_ALLOW_UNEXPORTED");
                std::env::remove_var("LAPILLI_RETENTION_RECLAIM_ORPHANS");
            }
            assert_eq!(
                got,
                (expect, expect),
                "LAPILLI_RETENTION_* = {set:?} must parse as {expect}; a flag that treats presence as \
                 true would turn both of these on in every install"
            );
        }
    }
}

#[cfg(test)]
mod shutdown_budget {
    /// The chart's floor for `terminationGracePeriodSeconds` has to keep covering these budgets.
    /// Both numbers were magic before: the chart did not set the grace period at all, relying on
    /// Kubernetes' 30 s default, and raising `RECONCILE_GRACE` here would silently outgrow it. The
    /// schema is `include_str!`'d, so moving the file breaks the build rather than the check.
    #[test]
    fn the_chart_floor_still_covers_the_shutdown_budgets() {
        const SCHEMA: &str = include_str!("../../../charts/lapilli/values.schema.json");
        let v: serde_json::Value = serde_json::from_str(SCHEMA).expect("values.schema.json");
        let floor = v["properties"]["terminationGracePeriodSeconds"]["minimum"]
            .as_u64()
            .expect("terminationGracePeriodSeconds needs a `minimum` in values.schema.json");
        assert!(
            floor >= super::SHUTDOWN_BUDGET_SECS + 3,
            "the chart allows a {floor}s grace period, but shutdown needs {}s plus room to exit; \
             raise the schema minimum (and the `fail` guard in deployment.yaml) to match",
            super::SHUTDOWN_BUDGET_SECS
        );
    }
}
