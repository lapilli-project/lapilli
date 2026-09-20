//! Does this controller actually have the permissions it needs?
//!
//! `kairn_apiserver_poll_ok` proves one thing: the poller's own `list` of IncidentCapture. That
//! is one of the chart's three RBAC bindings. Lose the **collector** binding — the one generated
//! through conditional branches on `watchNamespaces` and `diffs.configMaps`, so the one most
//! likely to be wrong — and every capture comes out empty while that gauge still reads `1`. It
//! does surface eventually, as `kairn_collector_failures_total` and a bundle that verifies as
//! PARTIAL, but only once a capture runs: found during an incident, with the evidence already
//! damaged, rather than at install time.
//!
//! So the controller asks. `SelfSubjectAccessReview` needs no permission of its own — the
//! `system:basic-user` ClusterRole is bound to `system:authenticated`, so even a ServiceAccount
//! with no RoleBinding at all can ask and be told "no", which is exactly the case this exists
//! for. RBAC `resourceNames` is honoured, so a check on a named Secret must use the real name.
//!
//! Nothing here refuses to start. A missing permission is reported, not fatal, for the same
//! reason `/healthz` does not depend on the API server: dying makes it worse. A controller that
//! cannot read pod logs still records everything else, and one that will not start records
//! nothing and cannot even say why.

use std::collections::BTreeMap;
use std::time::Duration;

use k8s_openapi::api::authorization::v1::{
    ResourceAttributes, SelfSubjectAccessReview, SelfSubjectAccessReviewSpec,
};
use kube::api::PostParams;
use kube::{Api, Client};

/// How often the checks are repeated. RBAC drifts: someone tightens a Role months later, and a
/// startup-only answer would still be reporting the install-day truth.
pub const RECHECK: Duration = Duration::from_secs(600);

/// Where a permission is needed.
enum Scope {
    /// The controller's own namespace.
    Own,
    /// Wherever captures happen: the watched namespaces, or cluster-wide when none are named.
    /// Every namespace must allow it — one that does not is a capture that will come out empty.
    Watched,
    /// A named object in the controller's own namespace. RBAC can grant `get` on one Secret by
    /// name, and `SelfSubjectAccessReview` honours that, so the name has to be real.
    Named(Vec<String>),
}

struct Check {
    /// A fixed name, used in the log. Never a namespace and never a Secret name.
    name: &'static str,
    group: &'static str,
    resource: &'static str,
    subresource: Option<&'static str>,
    /// **Every** verb the code issues on this resource, not one canary. RBAC treats `get`, `list`
    /// and `watch` as distinct verbs, so a check that asks about one of them passes on a Role that
    /// grants only that one — and then the operation the `consequence` describes is still
    /// forbidden. Each verb listed here is justified by a call site named in `checks()`.
    verbs: &'static [&'static str],
    scope: Scope,
    /// What stops working without it. Goes in the log line, because "forbidden" on its own sends
    /// nobody anywhere.
    consequence: &'static str,
}

/// What this install actually needs, assembled from what the process already knows: its own
/// namespace, the namespaces the chart scoped it to, and the profile and destinations it read.
pub struct Needs {
    pub own_namespace: String,
    /// Empty means cluster-wide, which is what the chart generates when `watchNamespaces` is unset.
    pub watch_namespaces: Vec<String>,
    /// The `diffs.configMaps` collector is on, so `get configmaps` is needed.
    pub config_maps: bool,
    /// `signing.mode=static`: the key is read from this Secret by name.
    pub signing_secret: Option<String>,
    /// Export destinations that name a credentials Secret.
    pub credential_secrets: Vec<String>,
}

/// The check list, derived from the call sites rather than from the chart. Checking what the chart
/// *grants* would raise a false alarm the day someone correctly tightens an unused permission;
/// checking what the code *calls* is the thing that matters. Every verb below cites where it is
/// issued.
fn checks(needs: &Needs) -> Vec<Check> {
    let mut v = vec![
        Check {
            name: "pods",
            group: "",
            resource: "pods",
            subresource: None,
            // `get` at collector.rs:173/280/376 — a hard `?` at the top of log collection, before
            // `logs()` is ever called. `list` at diffs.rs:210.
            verbs: &["get", "list"],
            scope: Scope::Watched,
            consequence: "no capture can read the pod it was fired about",
        },
        Check {
            name: "pod-logs",
            group: "",
            resource: "pods",
            subresource: Some("log"),
            // collector.rs:215.
            verbs: &["get"],
            scope: Scope::Watched,
            consequence:
                "bundles would carry no logs — including the crashed container's last words",
        },
        Check {
            name: "events",
            group: "",
            resource: "events",
            subresource: None,
            // collector.rs:327.
            verbs: &["list"],
            scope: Scope::Watched,
            consequence: "bundles would carry no cluster events",
        },
        Check {
            name: "replicasets",
            group: "apps",
            resource: "replicasets",
            subresource: None,
            // `get` at collector.rs:304/391 and diffs.rs:136; `list` at diffs.rs:183.
            verbs: &["get", "list"],
            scope: Scope::Watched,
            consequence: "no rollout diff: what changed before the incident would be unknown",
        },
        Check {
            name: "deployments",
            group: "apps",
            resource: "deployments",
            subresource: None,
            // collector.rs:308/404.
            verbs: &["get"],
            scope: Scope::Watched,
            consequence: "the owning Deployment would be missing from every bundle",
        },
        Check {
            name: "statefulsets",
            group: "apps",
            resource: "statefulsets",
            subresource: None,
            // collector.rs:289 and diffs.rs:405.
            verbs: &["get"],
            scope: Scope::Watched,
            consequence: "StatefulSet workloads would be captured without their spec or diff",
        },
        Check {
            name: "daemonsets",
            group: "apps",
            resource: "daemonsets",
            subresource: None,
            // collector.rs:297 and diffs.rs:409.
            verbs: &["get"],
            scope: Scope::Watched,
            consequence: "DaemonSet workloads would be captured without their spec or diff",
        },
        Check {
            name: "controllerrevisions",
            group: "apps",
            resource: "controllerrevisions",
            subresource: None,
            // diffs.rs:417 — `list`, and only `list`. An earlier version of this check asked for
            // `get`, a verb the controller never issues: a check for something unneeded is a false
            // alarm waiting to happen, and it missed the verb that is needed.
            verbs: &["list"],
            scope: Scope::Watched,
            consequence: "StatefulSet and DaemonSet diffs would be unavailable",
        },
        Check {
            name: "captures",
            group: "kairn.dev",
            resource: "incidentcaptures",
            subresource: None,
            // `create` at webhook.rs:265. `list` AND `watch`: the controller is a ListWatch
            // watcher (main.rs, `WatcherConfig::default()`), and `watch` is a separate RBAC verb —
            // grant `list` alone and no alert is ever noticed while every other check reads 1.
            verbs: &["create", "list", "watch"],
            scope: Scope::Own,
            consequence: "alerts cannot become captures, or captures are never noticed at all",
        },
        Check {
            name: "capture-status",
            group: "kairn.dev",
            resource: "incidentcaptures",
            subresource: Some("status"),
            // reconcile.rs:317/569/1050/1071.
            verbs: &["patch"],
            scope: Scope::Own,
            consequence: "captures would never report a phase, an export or a seal",
        },
        Check {
            name: "profile",
            group: "kairn.dev",
            resource: "captureprofiles",
            subresource: None,
            verbs: &["get"],
            scope: Scope::Own,
            consequence: "the CaptureProfile cannot be read, so no capture is configured",
        },
        Check {
            name: "recorded-events",
            group: "events.k8s.io",
            resource: "events",
            subresource: None,
            // `create` for a new Event; `patch` because kube-runtime's Recorder patches an
            // existing series rather than creating it again on a repeat.
            verbs: &["create", "patch"],
            scope: Scope::Own,
            consequence: "a refused or conflicting export would leave nothing in kubectl describe",
        },
    ];
    if needs.config_maps {
        v.push(Check {
            name: "configmaps",
            group: "",
            resource: "configmaps",
            subresource: None,
            verbs: &["get"],
            scope: Scope::Watched,
            consequence: "the diffs collector is on for ConfigMaps but cannot read them",
        });
    }
    if let Some(name) = &needs.signing_secret {
        v.push(Check {
            name: "signing-secret",
            group: "",
            resource: "secrets",
            subresource: None,
            verbs: &["get"],
            scope: Scope::Named(vec![name.clone()]),
            consequence:
                "signing.mode=static but the key Secret is unreadable: nothing can be sealed",
        });
    }
    if !needs.credential_secrets.is_empty() {
        v.push(Check {
            name: "destination-credentials",
            group: "",
            resource: "secrets",
            subresource: None,
            verbs: &["get"],
            scope: Scope::Named(needs.credential_secrets.clone()),
            consequence:
                "an export destination's credentials Secret is unreadable: no upload to it",
        });
    }
    v
}

/// One `SelfSubjectAccessReview`. `Ok(true)` allowed, `Ok(false)` denied, `Err` if the question
/// itself could not be asked — which is not the same answer and must not be reported as one.
/// How long one question may take. `poll_once` in `telemetry.rs` learned this the hard way: without
/// a bound the only ceiling is the client's 295 s read timeout, so one hung request stalls every
/// later check and the gauges sit at their last value through the outage they exist to report.
const ASK_TIMEOUT: Duration = Duration::from_secs(5);

/// And a budget for the whole pass. A per-question bound alone is not enough: with roughly twenty
/// questions, an API server that answers none of them would keep a pass running for a hundred
/// seconds and more. If the budget runs out the remaining checks are `unknown` — which is the
/// truth, and `kairn_permissions_unknown` is what reports it.
const PASS_BUDGET: Duration = Duration::from_secs(30);

/// The answer to one question. `Denied` carries the API server's own `reason`, which names the
/// missing object — "clusterrole … not found" — and used to be thrown away.
enum Answer {
    Allowed,
    Denied(Option<String>),
}

async fn allowed(
    client: &Client,
    c: &Check,
    verb: &str,
    namespace: Option<&str>,
    name: Option<&str>,
) -> Result<Answer, kube::Error> {
    let review = SelfSubjectAccessReview {
        spec: SelfSubjectAccessReviewSpec {
            resource_attributes: Some(ResourceAttributes {
                group: Some(c.group.to_string()),
                resource: Some(c.resource.to_string()),
                subresource: c.subresource.map(str::to_string),
                verb: Some(verb.to_string()),
                namespace: namespace.map(str::to_string),
                name: name.map(str::to_string),
                ..Default::default()
            }),
            ..Default::default()
        },
        ..Default::default()
    };
    let api: Api<SelfSubjectAccessReview> = Api::all(client.clone());
    let done = tokio::time::timeout(ASK_TIMEOUT, api.create(&PostParams::default(), &review))
        .await
        .map_err(|_| {
            kube::Error::Service(
                format!("the API server did not answer within {ASK_TIMEOUT:?}").into(),
            )
        })??;
    // `allowed: false` with an `evaluationError` means the authorizer could not decide. Treating
    // that as a denial would page about a permission that may well exist, so it is an error here.
    let status = done.status.unwrap_or_default();
    if let Some(e) = status.evaluation_error.filter(|_| !status.allowed) {
        return Err(kube::Error::Service(e.into()));
    }
    Ok(if status.allowed {
        Answer::Allowed
    } else {
        Answer::Denied(status.reason.filter(|r| !r.is_empty()))
    })
}

/// What one pass found. `None` means the question could not be asked at all, which is reported as
/// such rather than folded into "denied".
pub struct Report {
    pub results: BTreeMap<&'static str, Option<bool>>,
}

impl Report {
    pub fn missing(&self) -> usize {
        self.results.values().filter(|v| **v == Some(false)).count()
    }
    pub fn unknown(&self) -> usize {
        self.results.values().filter(|v| v.is_none()).count()
    }
}

/// Run every check once, log what is wrong and why it matters, and publish the gauges.
pub async fn check_once(client: &Client, needs: &Needs) -> Report {
    let deadline = tokio::time::Instant::now() + PASS_BUDGET;
    let mut results: BTreeMap<&'static str, Option<bool>> = BTreeMap::new();
    for c in checks(needs) {
        if tokio::time::Instant::now() >= deadline {
            // Not asked at all, so nothing is known. Recording `false` here would page about a
            // permission that may well be held; recording `true` would hide one that is not.
            results.insert(c.name, None);
            continue;
        }
        // Each check is a set of (namespace, name) questions that must ALL be allowed: a
        // permission that holds in one watched namespace and not another is still broken.
        let places: Vec<(Option<String>, Option<String>)> = match &c.scope {
            Scope::Own => vec![(Some(needs.own_namespace.clone()), None)],
            Scope::Watched if needs.watch_namespaces.is_empty() => vec![(None, None)],
            Scope::Watched => needs
                .watch_namespaces
                .iter()
                .map(|ns| (Some(ns.clone()), None))
                .collect(),
            Scope::Named(names) => names
                .iter()
                .map(|n| (Some(needs.own_namespace.clone()), Some(n.clone())))
                .collect(),
        };
        // Every (place, verb) must be allowed. The first failure decides and stops the check: on a
        // broken install the rest of the questions add nothing but audit-log volume, and — for the
        // `Err` arm — continuing would let a later denial overwrite "could not ask" with "denied",
        // collapsing the one distinction this module is built around.
        let mut verdict = Some(true);
        'check: for (ns, name) in places {
            for verb in c.verbs {
                match allowed(client, &c, verb, ns.as_deref(), name.as_deref()).await {
                    Ok(Answer::Allowed) => {}
                    Ok(Answer::Denied(reason)) => {
                        tracing::warn!(
                            check = c.name,
                            verb = verb,
                            resource = %full_resource(&c),
                            namespace = ns.as_deref().unwrap_or("(cluster-wide)"),
                            name = name.as_deref().unwrap_or(""),
                            // The API server names the missing Role or ClusterRole here. Dropping
                            // it left the operator to go and find it themselves.
                            reason = reason.as_deref().unwrap_or(""),
                            "missing permission: {}",
                            c.consequence
                        );
                        verdict = Some(false);
                        break 'check;
                    }
                    Err(e) => {
                        tracing::warn!(check = c.name, verb = verb, error = %e,
                                       "could not ask whether this permission is held");
                        verdict = None;
                        break 'check;
                    }
                }
            }
        }
        results.insert(c.name, verdict);
    }
    let report = Report { results };
    crate::telemetry::metrics().set_permissions(&report);
    match (report.missing(), report.unknown()) {
        (0, 0) => tracing::info!(
            checks = report.results.len(),
            "every permission this install needs is held"
        ),
        (0, u) => tracing::warn!(unknown = u, "some permission checks could not be answered"),
        (m, u) => tracing::warn!(
            missing = m,
            unknown = u,
            "this controller is missing permissions it needs; captures will be incomplete"
        ),
    }
    report
}

fn full_resource(c: &Check) -> String {
    let base = if c.group.is_empty() {
        c.resource.to_string()
    } else {
        format!("{}.{}", c.resource, c.group)
    };
    match c.subresource {
        Some(sub) => format!("{base}/{sub}"),
        None => base,
    }
}

/// A namespace name that the API server would never own. An SSAR about a name like this answers a
/// plain `allowed: false` — the API server does not validate it — so a typo in
/// `KAIRN_WATCH_NAMESPACES` would otherwise become a permanent critical page about a namespace
/// nothing needs.
fn is_dns_label(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 63
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !s.starts_with('-')
        && !s.ends_with('-')
}

/// Assemble [`Needs`] from what the process already has: the namespaces the chart scoped it to,
/// the CaptureProfile it will use, and the destinations file it read.
///
/// The profile is read best-effort. If it cannot be read, the checks that depend on it are simply
/// not asked — a check invented from a guess would raise a false alarm, and a false alarm about a
/// permission is worse than no check at all. `get-profile` reports the unreadable profile itself.
pub async fn needs_from_cluster(
    client: &Client,
    own_namespace: &str,
    profile_name: &str,
    watch_namespaces: &str,
    destinations_file: &std::path::Path,
) -> Needs {
    let watch_namespaces = watch_namespaces
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .filter(|s| {
            is_dns_label(s) || {
                tracing::error!(
                    value = s,
                    "KAIRN_WATCH_NAMESPACES holds something that is not a namespace name; \
                     skipping it rather than reporting a permission it could never grant"
                );
                false
            }
        })
        .map(str::to_string)
        .collect();

    let profile: Api<crate::crd::CaptureProfile> = Api::namespaced(client.clone(), own_namespace);
    let (config_maps, signing_secret) = match profile.get(profile_name).await {
        Ok(p) => (
            p.spec.diffs.config_maps,
            match p.spec.signing.mode {
                crate::crd::SigningMode::Static => p.spec.signing.key_secret.clone(),
                _ => None,
            },
        ),
        Err(e) => {
            tracing::debug!(error = %e, profile = profile_name,
                            "permission self-check: profile unreadable, skipping its checks");
            (false, None)
        }
    };

    let credential_secrets = std::fs::read(destinations_file)
        .ok()
        .and_then(|b| serde_json::from_slice::<Vec<crate::export::DestinationSpec>>(&b).ok())
        .map(|specs| {
            let mut v: Vec<String> = specs
                .into_iter()
                .map(|d| d.credentials_secret.trim().to_string())
                .filter(|s| !s.is_empty())
                .collect();
            v.sort();
            v.dedup();
            v
        })
        .unwrap_or_default();

    Needs {
        own_namespace: own_namespace.to_string(),
        watch_namespaces,
        config_maps,
        signing_secret,
        credential_secrets,
    }
}

/// Check at startup, then every `every` (callers pass [`RECHECK`]; the interval is a parameter so a
/// test can prove the loop actually comes back rather than only reading the constant).
pub fn spawn(client: Client, needs: Needs, every: Duration) {
    tokio::spawn(async move {
        loop {
            check_once(&client, &needs).await;
            tokio::time::sleep(every).await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::telemetry::Metrics;

    fn needs() -> Needs {
        Needs {
            own_namespace: "kairn-system".into(),
            watch_namespaces: vec![],
            config_maps: false,
            signing_secret: None,
            credential_secrets: vec![],
        }
    }

    type Asked = std::sync::Arc<std::sync::Mutex<Vec<String>>>;

    async fn fake_authorizer(deny: Vec<&'static str>, broken: Vec<&'static str>) -> Client {
        fake_recording_authorizer(deny, broken).await.0
    }

    /// A fake API server that answers SelfSubjectAccessReview, and hands back every question it was
    /// asked. Tests assert on the REQUEST, not only on the verdict derived from the answer: round 14
    /// established that inferring a request from an allow/deny cannot distinguish two questions, and
    /// round 15 found `group` surviving every mutation because the fingerprint left it out.
    ///
    /// The key is `<group>/<verb> <resource>[/<sub>]@<namespace>#<name>`, so nothing about a request
    /// is invisible to a test. `deny` and `broken` are substring matches against it.
    async fn fake_recording_authorizer(
        deny: Vec<&'static str>,
        broken: Vec<&'static str>,
    ) -> (Client, Asked) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let asked: Asked = Default::default();
        let sink = asked.clone();
        let app = axum::Router::new().fallback(move |body: String| {
            let deny = deny.clone();
            let broken = broken.clone();
            let sink = sink.clone();
            async move {
                let v: serde_json::Value = serde_json::from_str(&body).unwrap_or_default();
                let a = &v["spec"]["resourceAttributes"];
                let key = format!(
                    "{}/{} {}{}@{}#{}",
                    // The API group. Left out of the first version of this fingerprint, which is
                    // why three mutations of it survived the whole suite.
                    a["group"].as_str().unwrap_or("<absent>"),
                    a["verb"].as_str().unwrap_or(""),
                    a["resource"].as_str().unwrap_or(""),
                    a["subresource"]
                        .as_str()
                        .map(|s| format!("/{s}"))
                        .unwrap_or_default(),
                    a["namespace"].as_str().unwrap_or("*"),
                    a["name"].as_str().unwrap_or(""),
                );
                if let Ok(mut q) = sink.lock() {
                    q.push(key.clone());
                }
                let status = if broken.iter().any(|b| key.contains(b)) {
                    serde_json::json!({ "allowed": false, "evaluationError": "webhook timed out" })
                } else if deny.iter().any(|d| key.contains(d)) {
                    serde_json::json!({
                        "allowed": false,
                        "reason": "rbac: RBAC: clusterrole.rbac.authorization.k8s.io \"kairn-collector\" not found"
                    })
                } else {
                    serde_json::json!({ "allowed": true })
                };
                axum::Json(serde_json::json!({
                    "apiVersion": "authorization.k8s.io/v1",
                    "kind": "SelfSubjectAccessReview",
                    "metadata": {},
                    "spec": v["spec"].clone(),
                    "status": status,
                }))
            }
        });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let cfg = kube::Config::new(format!("http://{addr}/").parse().unwrap());
        (kube::Client::try_from(cfg).unwrap(), asked)
    }

    /// The whole point of the module, pinned as data: every `(group, verb, resource)` the controller
    /// actually issues must be asked about. RBAC treats `get`, `list` and `watch` as distinct verbs,
    /// and the first version of this check asked one canary verb per resource — so a Role granting
    /// only that verb passed while the operation named in the check's own `consequence` was
    /// forbidden. Each entry below cites the call site in `checks()`.
    #[tokio::test]
    async fn every_verb_the_controller_issues_is_asked_about() {
        let (client, asked) = fake_recording_authorizer(vec![], vec![]).await;
        let needs = Needs {
            config_maps: true,
            ..needs()
        };
        check_once(&client, &needs).await;
        let asked = asked.lock().unwrap().clone();
        for want in [
            // Collection reads the pod object before it reads any log (collector.rs:173).
            "/get pods@*#",
            "/list pods@*#",
            "/get pods/log@*#",
            "/list events@*#",
            // apps: `get` for the owning workload, `list` where the code lists.
            "apps/get replicasets@*#",
            "apps/list replicasets@*#",
            "apps/get deployments@*#",
            "apps/get statefulsets@*#",
            "apps/get daemonsets@*#",
            "apps/list controllerrevisions@*#",
            // The controller is a ListWatch watcher: `watch` is its own RBAC verb, and without it
            // no alert is ever noticed.
            "kairn.dev/create incidentcaptures@kairn-system#",
            "kairn.dev/list incidentcaptures@kairn-system#",
            "kairn.dev/watch incidentcaptures@kairn-system#",
            "kairn.dev/patch incidentcaptures/status@kairn-system#",
            "kairn.dev/get captureprofiles@kairn-system#",
            // kube-runtime's Recorder patches a repeated event rather than creating it again.
            "events.k8s.io/create events@kairn-system#",
            "events.k8s.io/patch events@kairn-system#",
            "/get configmaps@*#",
        ] {
            assert!(
                asked.contains(&want.to_string()),
                "never asked {want:?}: {asked:#?}"
            );
        }
        // And the core group is asked as the empty string, not as something that happens to look
        // right — `events` in the core group and in events.k8s.io are different resources.
        assert!(asked.iter().any(|k| k.starts_with("/list events@")));
        assert!(asked
            .iter()
            .any(|k| k.starts_with("events.k8s.io/create events@")));
    }

    /// The label set has to stay fixed however big the cluster is. Namespaces and Secret names are
    /// what would break that, so they are aggregated into one check each rather than labelled.
    #[test]
    fn check_names_do_not_grow_with_the_cluster() {
        let base = checks(&needs()).len();
        let many = Needs {
            watch_namespaces: (0..50).map(|i| format!("team-{i}")).collect(),
            credential_secrets: (0..20).map(|i| format!("creds-{i}")).collect(),
            ..needs()
        };
        assert_eq!(
            checks(&many).len(),
            base + 1,
            "50 namespaces and 20 secrets may add the credentials check and nothing else"
        );
        let names: Vec<_> = checks(&many).iter().map(|c| c.name).collect();
        assert!(names
            .iter()
            .all(|n| !n.contains("team-") && !n.contains("creds-")));
    }

    /// A conditional collector must not be checked when it is off, or the install that does not use
    /// it reports a permission it does not need — a false alarm, which is worse than silence.
    #[test]
    fn conditional_checks_appear_only_when_that_feature_is_on() {
        let off: Vec<_> = checks(&needs()).iter().map(|c| c.name).collect();
        for n in ["configmaps", "signing-secret", "destination-credentials"] {
            assert!(!off.contains(&n), "{n} asked when its feature is off");
        }
        let on = Needs {
            config_maps: true,
            signing_secret: Some("kairn-signing-key".into()),
            credential_secrets: vec!["s3-creds".into()],
            ..needs()
        };
        let names: Vec<_> = checks(&on).iter().map(|c| c.name).collect();
        for n in ["configmaps", "signing-secret", "destination-credentials"] {
            assert!(names.contains(&n), "{n} missing: {names:?}");
        }
    }

    /// An unset `watchNamespaces` means the chart generated a ClusterRole, so the question has to be
    /// asked cluster-wide. Asking about one namespace instead would pass on a ClusterRole and *also*
    /// pass on a Role covering only that namespace — it could not tell a working install from one
    /// whose ClusterRole is missing.
    #[tokio::test]
    async fn a_cluster_wide_install_asks_cluster_wide_questions() {
        let (client, asked) = fake_recording_authorizer(vec![], vec![]).await;
        check_once(&client, &needs()).await;
        let asked = asked.lock().unwrap().clone();
        assert!(asked.contains(&"/list pods@*#".to_string()), "{asked:?}");
        assert!(asked.contains(&"kairn.dev/create incidentcaptures@kairn-system#".to_string()));
    }

    /// And a namespaced install asks per namespace, never cluster-wide — which the Role the chart
    /// generates in that mode refuses, reporting a permission the install does not need.
    #[tokio::test]
    async fn a_namespaced_install_asks_per_namespace() {
        let (client, asked) = fake_recording_authorizer(vec![], vec![]).await;
        let needs = Needs {
            watch_namespaces: vec!["team-a".into(), "team-b".into()],
            ..needs()
        };
        check_once(&client, &needs).await;
        let asked = asked.lock().unwrap().clone();
        for ns in ["team-a", "team-b"] {
            assert!(asked.contains(&format!("/list pods@{ns}#")), "{asked:?}");
        }
        assert!(
            !asked.iter().any(|k| k == "/list pods@*#"),
            "a namespaced install must not ask the cluster-wide question: {asked:?}"
        );
    }

    /// RBAC grants `get secrets` by `resourceNames`, and the API server honours it: on a live
    /// cluster `secrets/mykey` was allowed while `secrets/other` was denied by the same Role. So the
    /// question must carry the name — "can I get any Secret" is a different question, and one a
    /// correctly scoped install refuses.
    #[tokio::test]
    async fn a_named_secret_is_asked_about_by_name() {
        let (client, asked) = fake_recording_authorizer(vec![], vec![]).await;
        let needs = Needs {
            signing_secret: Some("kairn-signing-key".into()),
            credential_secrets: vec!["s3-creds".into(), "gcs-creds".into()],
            ..needs()
        };
        check_once(&client, &needs).await;
        let asked = asked.lock().unwrap().clone();
        for name in ["kairn-signing-key", "s3-creds", "gcs-creds"] {
            assert!(
                asked.contains(&format!("/get secrets@kairn-system#{name}")),
                "no question carried the name {name}: {asked:?}"
            );
        }
        assert!(
            !asked.iter().any(|k| k == "/get secrets@kairn-system#"),
            "a Secret was asked about without its name: {asked:?}"
        );
    }

    /// A denial is reported, the rest still are, and the count reaches /metrics.
    #[tokio::test]
    async fn a_denied_permission_is_counted_and_the_rest_still_are() {
        let client = fake_authorizer(vec!["/get pods/log@"], vec![]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(report.results["pod-logs"], Some(false));
        assert_eq!(report.results["pods"], Some(true));
        assert_eq!(report.missing(), 1, "{:?}", report.results);
        assert_eq!(report.unknown(), 0);

        let m = Metrics::default();
        m.set_permissions(&report);
        let text = m.render();
        assert!(text.contains("kairn_permissions_denied 1\n"), "{text}");
        assert!(text.contains("kairn_permissions_unknown 0\n"), "{text}");
        assert!(
            text.contains("kairn_permission_checks_total{result=\"denied\"} 1\n"),
            "{text}"
        );
        // And nothing names the check: /metrics is unauthenticated, so the capability map stays out
        // of it and goes to the log instead.
        assert!(!text.contains("pod-logs"), "{text}");
    }

    /// Held in one watched namespace and not another is still broken: a capture fired about the
    /// second namespace comes out empty. The optimistic reading would hide exactly the mistake a
    /// per-namespace install makes — forgetting a namespace.
    #[tokio::test]
    async fn a_permission_missing_in_one_watched_namespace_fails_the_check() {
        let client = fake_authorizer(vec!["/list pods@team-b"], vec![]).await;
        let needs = Needs {
            watch_namespaces: vec!["team-a".into(), "team-b".into()],
            ..needs()
        };
        let report = check_once(&client, &needs).await;
        assert_eq!(report.results["pods"], Some(false));
        assert_eq!(report.results["captures"], Some(true));
    }

    /// A check needs EVERY verb it lists. Granting `list` and not `watch` on IncidentCapture is the
    /// install where no alert is ever noticed, and it used to pass.
    #[tokio::test]
    async fn one_missing_verb_out_of_several_fails_the_check() {
        let client = fake_authorizer(vec!["kairn.dev/watch incidentcaptures@"], vec![]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(
            report.results["captures"],
            Some(false),
            "watch is a distinct RBAC verb from list; missing it must fail the check"
        );
        assert_eq!(report.results["capture-status"], Some(true));
    }

    /// "Could not ask" is not "denied", and it must not be counted as one — an authorizer webhook
    /// that times out would otherwise page about a permission that is in fact held.
    #[tokio::test]
    async fn an_unanswerable_question_is_not_a_denial() {
        let client = fake_authorizer(vec![], vec!["events.k8s.io/create events@"]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(report.results["recorded-events"], None);
        assert_eq!(report.missing(), 0, "an unknown must not count as missing");
        assert_eq!(report.unknown(), 1);

        let m = Metrics::default();
        m.set_permissions(&report);
        let text = m.render();
        assert!(text.contains("kairn_permissions_unknown 1\n"), "{text}");
        assert!(text.contains("kairn_permissions_denied 0\n"), "{text}");
        assert!(text.contains("kairn_permission_checks_total{result=\"unknown\"} 1\n"));
    }

    /// The `Err` arm stops the check. Without the `break` a later denial would overwrite "could not
    /// ask" with "denied" — collapsing the one distinction this module is built around, and turning
    /// a flaky authorizer into a page about RBAC.
    #[tokio::test]
    async fn an_unanswerable_verb_is_not_overwritten_by_a_later_denial() {
        // For the `captures` check: `create` cannot be answered, `list` is refused. The unanswerable
        // one comes first, so the verdict must stay unknown.
        let client = fake_authorizer(
            vec!["kairn.dev/list incidentcaptures@"],
            vec!["kairn.dev/create incidentcaptures@"],
        )
        .await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(report.results["captures"], None, "{:?}", report.results);
    }

    /// And a denial stops the check too: the remaining questions add nothing but audit-log volume,
    /// and every SelfSubjectAccessReview is an audited write.
    #[tokio::test]
    async fn a_denial_stops_asking_the_rest_of_that_check() {
        let (client, asked) = fake_recording_authorizer(vec!["/get pods@"], vec![]).await;
        check_once(&client, &needs()).await;
        let asked = asked.lock().unwrap().clone();
        assert!(asked.contains(&"/get pods@*#".to_string()));
        assert!(
            !asked.contains(&"/list pods@*#".to_string()),
            "asking stopped at the first denial, so `list pods` must not have been asked: {asked:?}"
        );
    }

    /// A server that accepts the connection and then never answers.
    async fn fake_blackhole() -> Client {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let app = axum::Router::new()
            .fallback(|| async { tokio::time::sleep(Duration::from_secs(600)).await });
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let cfg = kube::Config::new(format!("http://{addr}/").parse().unwrap());
        kube::Client::try_from(cfg).unwrap()
    }

    /// ONE question must be bounded, in real time. `poll_once` in telemetry.rs learned this: without
    /// a per-request bound the only ceiling is the client's 295 s read timeout, and the pass budget
    /// below cannot save it — that deadline is only tested *between* checks, so a single hung
    /// request never reaches it.
    ///
    /// Deliberately not a paused-time test. Under `start_paused` the read timeout elapses instantly
    /// too, so removing `ASK_TIMEOUT` still passed — the check was vacuous. This one costs about
    /// five seconds and actually holds.
    #[tokio::test]
    async fn one_question_is_bounded_in_real_time() {
        let client = fake_blackhole().await;
        let c = &checks(&needs())[0];
        let began = std::time::Instant::now();
        let outcome = allowed(&client, c, "get", None, None).await;
        let waited = began.elapsed();
        assert!(outcome.is_err(), "a blackholed server cannot answer");
        assert!(
            waited < ASK_TIMEOUT * 3,
            "one question waited {waited:?}; it is not bounded by ASK_TIMEOUT ({ASK_TIMEOUT:?})"
        );
    }

    /// And the pass as a whole is bounded, so an API server that answers nothing cannot keep one
    /// running for one `ASK_TIMEOUT` per question — about twenty of them. Everything it did not get
    /// to is `unknown`, which is the truth.
    #[tokio::test(start_paused = true)]
    async fn a_pass_against_a_silent_api_server_is_bounded() {
        let client = fake_blackhole().await;
        let report =
            tokio::time::timeout(PASS_BUDGET + ASK_TIMEOUT * 2, check_once(&client, &needs()))
                .await
                .expect("check_once must be bounded by PASS_BUDGET");
        assert_eq!(report.missing(), 0, "a timeout is not a denial");
        assert_eq!(report.unknown(), report.results.len());
    }

    /// The loop re-checks. RBAC drifts, so a startup-only answer would keep reporting the
    /// install-day truth for the life of the pod — and nothing pinned that it ever runs again.
    ///
    /// Real time with a short interval, not `start_paused`: under paused time the test's own sleep
    /// auto-advances past the first pass's HTTP round trip and the assertion becomes a race. That is
    /// also why the interval is a parameter rather than read from the constant.
    #[tokio::test]
    async fn the_loop_checks_again_after_its_interval() {
        let (client, asked) = fake_recording_authorizer(vec![], vec![]).await;
        spawn(client, needs(), Duration::from_millis(150));
        let mut first = 0;
        for _ in 0..40 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            first = asked.lock().unwrap().len();
            if first > 0 {
                break;
            }
        }
        assert!(
            first > 0,
            "the first pass must run at startup, not after the interval"
        );
        for _ in 0..40 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            if asked.lock().unwrap().len() >= first * 2 {
                return;
            }
        }
        panic!(
            "asked {first} questions and never came back for a second pass (total {})",
            asked.lock().unwrap().len()
        );
    }

    fn full(c: &Check) -> String {
        full_resource(c)
    }

    #[test]
    fn a_resource_reads_the_way_kubectl_prints_it() {
        let cs = checks(&Needs {
            config_maps: true,
            ..needs()
        });
        let by = |n: &str| full(cs.iter().find(|c| c.name == n).unwrap());
        assert_eq!(by("pods"), "pods");
        assert_eq!(by("pod-logs"), "pods/log");
        assert_eq!(by("replicasets"), "replicasets.apps");
        assert_eq!(by("capture-status"), "incidentcaptures.kairn.dev/status");
        assert_eq!(by("recorded-events"), "events.events.k8s.io");
    }

    #[test]
    fn a_report_separates_denied_from_unanswerable() {
        let r = Report {
            results: [
                ("a", Some(true)),
                ("b", Some(false)),
                ("c", None),
                ("d", Some(false)),
            ]
            .into(),
        };
        assert_eq!(r.missing(), 2);
        assert_eq!(r.unknown(), 1);
    }

    /// `needs_from_cluster` assembles the check list from config, and none of its parsing was
    /// covered: four mutations of it survived the suite. Each case below is one of them.
    #[tokio::test]
    async fn config_is_parsed_into_the_questions_that_should_be_asked() {
        let dir = tempfile::tempdir().unwrap();
        let dests = dir.path().join("destinations.json");
        std::fs::write(
            &dests,
            // A duplicate, a whitespace-only name, and one real one. Whitespace must not become a
            // question about a Secret that cannot exist, and the duplicate must not be asked twice.
            br#"[{"name":"a","url":"s3://b/p","credentialsSecret":"s3-creds"},
                 {"name":"b","url":"s3://b/q","credentialsSecret":"s3-creds"},
                 {"name":"c","url":"s3://b/r","credentialsSecret":"   "},
                 {"name":"d","url":"s3://b/s"}]"#,
        )
        .unwrap();
        // No profile endpoint: `needs_from_cluster` must degrade rather than fail, since an
        // unreadable profile is itself reported by the `profile` check.
        let client = fake_authorizer(vec![], vec![]).await;
        let needs = needs_from_cluster(
            &client,
            "kairn-system",
            "default",
            " team-a , , Team_B ,",
            &dests,
        )
        .await;
        assert_eq!(
            needs.watch_namespaces,
            vec!["team-a".to_string()],
            "empty segments are dropped, and `Team_B` is not a namespace name"
        );
        assert_eq!(needs.credential_secrets, vec!["s3-creds".to_string()]);
        assert!(
            !needs.config_maps,
            "no profile means the ConfigMap check is not invented"
        );
        assert_eq!(needs.signing_secret, None);
    }

    /// A typo in `KAIRN_WATCH_NAMESPACES` must not become a permanent critical page. The API server
    /// does not validate the name in a SelfSubjectAccessReview — on a live cluster
    /// `list pods@Team_A` answered a plain `allowed: false` — so a malformed entry would otherwise
    /// read as a missing permission for a namespace nothing needs.
    #[test]
    fn a_malformed_namespace_is_dropped_not_reported_as_missing() {
        assert!(is_dns_label("team-a"));
        assert!(is_dns_label("a"));
        assert!(
            !is_dns_label("Team_A"),
            "uppercase and underscore are not legal"
        );
        assert!(!is_dns_label("-lead"));
        assert!(!is_dns_label("trail-"));
        assert!(!is_dns_label(""));
        assert!(!is_dns_label(&"x".repeat(64)));
    }
}
