//! Does this controller actually have the permissions it needs?
//!
//! `lapilli_apiserver_poll_ok` proves one thing: the poller's own `list` of IncidentCapture. That
//! is one of the chart's three RBAC bindings. Lose the **collector** binding — the one generated
//! through conditional branches on `watchNamespaces` and `diffs.configMaps`, so the one most
//! likely to be wrong — and every capture comes out empty while that gauge still reads `1`. It
//! does surface eventually, as `lapilli_collector_failures_total` and a bundle that verifies as
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
/// namespace, the namespaces the chart scoped it to, the destinations it read, and **every**
/// CaptureProfile it can list — profiles are chosen per capture, so whatever any of them could
/// ask a capture to do must be held (docs/design-permissions-by-profile.md, rule 1).
pub struct Needs {
    pub own_namespace: String,
    /// Empty means cluster-wide, which is what the chart generates when `watchNamespaces` is unset.
    pub watch_namespaces: Vec<String>,
    /// Where the collector set came from. Decides whether a collector check that no profile asks
    /// for is *not needed* or simply *unknown*: the two must never read the same (rule 4).
    pub profiles: Profiles,
    /// The union of `spec.collectors` over the listed profiles.
    pub collectors: std::collections::BTreeSet<String>,
    /// Some profile turns `diffs.configMaps` on, so `get configmaps` is needed.
    pub config_maps: bool,
    /// `signing.mode=static` on some profile: the key is read from these Secrets by name.
    pub signing_secrets: Vec<String>,
    /// Export destinations that name a credentials Secret.
    pub credential_secrets: Vec<String>,
}

/// Whether the profile population could be read at all.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Profiles {
    /// `list captureprofiles` answered; this many were found (zero is a real state: no capture
    /// will ever be configured, and it is logged as such).
    Listed(usize),
    /// The list could not be read. The controller cannot say what it needs, which is not the
    /// same as needing nothing.
    Unreadable,
}

/// Which collectors need which check, from the call sites the checks cite. A check absent from
/// this table is unconditional. `metrics` reaches nothing in-cluster and so appears nowhere.
const NEEDED_BY: &[(&str, &[&str])] = &[
    ("pods", &["logs", "resources", "events", "changes"]),
    ("pod-logs", &["logs"]),
    ("events", &["events"]),
    ("replicasets", &["resources", "changes"]),
    ("deployments", &["resources", "changes"]),
    ("statefulsets", &["resources", "changes"]),
    ("daemonsets", &["resources", "changes"]),
    ("controllerrevisions", &["changes"]),
];

/// The collectors a check exists for, or `None` for a check that every install needs.
fn needed_by(check: &str) -> Option<&'static [&'static str]> {
    NEEDED_BY
        .iter()
        .find(|(c, _)| *c == check)
        .map(|(_, by)| *by)
}

/// Is this check worth asking on this install?
enum Need {
    Yes,
    /// No listed profile intends a collector that issues this verb.
    NotNeeded,
    /// The profiles could not be listed, so nobody knows.
    Unknown,
}

fn need_for(check: &str, needs: &Needs) -> Need {
    let Some(by) = needed_by(check) else {
        return Need::Yes;
    };
    match needs.profiles {
        Profiles::Unreadable => Need::Unknown,
        Profiles::Listed(_) if by.iter().any(|c| needs.collectors.contains(*c)) => Need::Yes,
        Profiles::Listed(_) => Need::NotNeeded,
    }
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
            group: "lapilli.dev",
            resource: "incidentcaptures",
            subresource: None,
            // `create` at webhook.rs. `list` AND `watch`: the controller is a ListWatch
            // watcher (main.rs), and `watch` is a separate RBAC verb — grant `list` alone and no
            // alert is ever noticed while every other check reads 1. `patch` on the OBJECT (not
            // the status subresource, which has its own check below) is what `reconcile::retire`
            // writes; without it every capture stays in the watch and the controller grows until
            // it is OOM-killed, with nothing in the logs but a warning per capture. This module's
            // rule is that every verb the code issues is asked about here.
            verbs: &["create", "list", "watch", "patch"],
            scope: Scope::Own,
            consequence: "alerts cannot become captures, or captures are never noticed at all",
        },
        Check {
            name: "capture-status",
            group: "lapilli.dev",
            resource: "incidentcaptures",
            subresource: Some("status"),
            // reconcile.rs:317/569/1050/1071.
            verbs: &["patch"],
            scope: Scope::Own,
            consequence: "captures would never report a phase, an export or a seal",
        },
        Check {
            name: "profile",
            group: "lapilli.dev",
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
    if !needs.signing_secrets.is_empty() {
        v.push(Check {
            name: "signing-secret",
            group: "",
            resource: "secrets",
            subresource: None,
            verbs: &["get"],
            scope: Scope::Named(needs.signing_secrets.clone()),
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
/// truth, and `lapilli_permissions_unknown` is what reports it.
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

/// What one check came to. Four states, because two pairs must never read the same: a question
/// that could not be asked is not a denial, and a check that no profile needs is not one that was
/// skipped by accident (docs/design-review-round24.md, Fix II).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Held,
    Denied,
    /// Could not be asked, or the pass ran out of time before it was.
    Unknown,
    /// No listed profile intends a collector that issues this verb. Recorded, not omitted, so a
    /// perishable-only install reads as "pod-logs: not needed", never as "nothing was asked".
    NotNeeded,
}

/// What one pass found.
#[derive(Debug, Clone)]
pub struct Report {
    pub results: BTreeMap<&'static str, Outcome>,
    /// When the pass finished. A capture that a denial thinned cites this, so an operator can
    /// tell a fresh answer from one that predates their fix.
    pub at: chrono::DateTime<chrono::Utc>,
}

impl Report {
    fn count(&self, o: Outcome) -> usize {
        self.results.values().filter(|v| **v == o).count()
    }
    pub fn missing(&self) -> usize {
        self.count(Outcome::Denied)
    }
    pub fn unknown(&self) -> usize {
        self.count(Outcome::Unknown)
    }
    pub fn not_needed(&self) -> usize {
        self.count(Outcome::NotNeeded)
    }
    /// Checks that were actually put to the API server (held, denied, or tried and unanswered).
    pub fn asked(&self) -> usize {
        self.results.len() - self.not_needed()
    }
    /// The denied checks a collector depends on — what to name when that collector did not run.
    pub fn denied_for(&self, collector: &str) -> Vec<&'static str> {
        self.results
            .iter()
            .filter(|(_, o)| **o == Outcome::Denied)
            .filter(|(name, _)| needed_by(name).is_some_and(|by| by.contains(&collector)))
            .map(|(name, _)| *name)
            .collect()
    }
}

/// The latest report, shared with the reconciler so a thinned capture can name its cause.
/// `None` until the first pass completes; a reader that finds `None` attributes nothing.
pub type Shared = std::sync::Arc<std::sync::RwLock<Option<Report>>>;

/// Run every check once, log what is wrong and why it matters, and publish the gauges.
pub async fn check_once(client: &Client, needs: &Needs) -> Report {
    let deadline = tokio::time::Instant::now() + PASS_BUDGET;
    let mut results: BTreeMap<&'static str, Outcome> = BTreeMap::new();
    for c in checks(needs) {
        match need_for(c.name, needs) {
            Need::Yes => {}
            Need::NotNeeded => {
                results.insert(c.name, Outcome::NotNeeded);
                continue;
            }
            Need::Unknown => {
                results.insert(c.name, Outcome::Unknown);
                continue;
            }
        }
        if tokio::time::Instant::now() >= deadline {
            // Not asked at all, so nothing is known. Recording `Denied` here would page about a
            // permission that may well be held; recording `Held` would hide one that is not.
            results.insert(c.name, Outcome::Unknown);
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
        let mut verdict = Outcome::Held;
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
                        verdict = Outcome::Denied;
                        break 'check;
                    }
                    Err(e) => {
                        tracing::warn!(check = c.name, verb = verb, error = %e,
                                       "could not ask whether this permission is held");
                        verdict = Outcome::Unknown;
                        break 'check;
                    }
                }
            }
        }
        results.insert(c.name, verdict);
    }
    let report = Report {
        results,
        at: chrono::Utc::now(),
    };
    crate::telemetry::metrics().set_permissions(&report);
    match (report.missing(), report.unknown(), needs.profiles) {
        (_, _, Profiles::Unreadable) => tracing::warn!(
            unknown = report.unknown(),
            "the CaptureProfiles could not be listed, so which collector permissions this install \
             needs is unknown — not held, not denied, unknown"
        ),
        (0, 0, _) => tracing::info!(
            asked = report.asked(),
            not_needed = report.not_needed(),
            "every permission this install needs is held"
        ),
        (0, u, _) => tracing::warn!(unknown = u, "some permission checks could not be answered"),
        (m, u, _) => tracing::warn!(
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
/// `LAPILLI_WATCH_NAMESPACES` would otherwise become a permanent critical page about a namespace
/// nothing needs.
fn is_dns_label(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 63
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !s.starts_with('-')
        && !s.ends_with('-')
}

/// What a pass needs in order to work out [`Needs`]: the things the process knows at startup.
/// The profiles are **not** among them — they are listed afresh on every pass, because they are
/// the most runtime-mutable object in the system and a check set frozen at startup would give
/// the drift detector the mirror-image blind spot of the one it exists to close.
#[derive(Clone)]
pub struct Source {
    pub own_namespace: String,
    /// Raw `LAPILLI_WATCH_NAMESPACES`; parsed on every pass so a malformed entry is logged each time.
    pub watch_namespaces: String,
    pub destinations_file: std::path::PathBuf,
}

/// Assemble [`Needs`]: the namespaces the chart scoped the controller to, every CaptureProfile in
/// its namespace, and the destinations file it read.
///
/// If the profiles cannot be listed, the collector checks are recorded as *unknown* rather than
/// skipped. Before this, an unreadable profile was a `debug!` followed by "every permission this
/// install needs is held" — a guess dressed as a verdict.
pub async fn needs_from_cluster(client: &Client, src: &Source) -> Needs {
    let own_namespace = src.own_namespace.as_str();
    let destinations_file = src.destinations_file.as_path();
    let watch_namespaces = src
        .watch_namespaces
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .filter(|s| {
            is_dns_label(s) || {
                tracing::error!(
                    value = s,
                    "LAPILLI_WATCH_NAMESPACES holds something that is not a namespace name; \
                     skipping it rather than reporting a permission it could never grant"
                );
                false
            }
        })
        .map(str::to_string)
        .collect();

    let api: Api<crate::crd::CaptureProfile> = Api::namespaced(client.clone(), own_namespace);
    let mut collectors = std::collections::BTreeSet::new();
    let mut config_maps = false;
    let mut signing_secrets: Vec<String> = Vec::new();
    let profiles = match api.list(&kube::api::ListParams::default()).await {
        Ok(list) => {
            if list.items.is_empty() {
                tracing::warn!(
                    namespace = own_namespace,
                    "no CaptureProfile exists in this namespace: no alert can become a capture \
                     until one does"
                );
            }
            for p in &list.items {
                collectors.extend(p.spec.collectors.iter().cloned());
                config_maps |= p.spec.diffs.config_maps;
                if p.spec.signing.mode == crate::crd::SigningMode::Static {
                    if let Some(s) = &p.spec.signing.key_secret {
                        signing_secrets.push(s.clone());
                    }
                }
            }
            signing_secrets.sort();
            signing_secrets.dedup();
            Profiles::Listed(list.items.len())
        }
        Err(e) => {
            tracing::warn!(error = %e, namespace = own_namespace,
                           "permission self-check: the CaptureProfiles could not be listed; \
                            collector checks are unknown this pass");
            Profiles::Unreadable
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
        profiles,
        collectors,
        config_maps,
        signing_secrets,
        credential_secrets,
    }
}

/// One pass: derive what is needed *now*, ask, and publish the report where the reconciler can
/// see it.
pub async fn pass(client: &Client, src: &Source, shared: &Shared) -> Report {
    let needs = needs_from_cluster(client, src).await;
    let report = check_once(client, &needs).await;
    if let Ok(mut slot) = shared.write() {
        *slot = Some(report.clone());
    }
    report
}

/// Check at startup, then every `every` (callers pass [`RECHECK`]; the interval is a parameter so a
/// test can prove the loop actually comes back rather than only reading the constant). Each pass
/// re-derives its needs: see [`Source`].
pub fn spawn(client: Client, src: Source, shared: Shared, every: Duration) {
    tokio::spawn(async move {
        loop {
            pass(&client, &src, &shared).await;
            tokio::time::sleep(every).await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::telemetry::Metrics;

    /// An install whose one profile intends every collector — the shape the chart's defaults
    /// produce, and the one under which every check is needed.
    fn needs() -> Needs {
        Needs {
            own_namespace: "lapilli-system".into(),
            watch_namespaces: vec![],
            profiles: Profiles::Listed(1),
            collectors: ["logs", "resources", "events", "changes", "metrics"]
                .into_iter()
                .map(String::from)
                .collect(),
            config_maps: false,
            signing_secrets: vec![],
            credential_secrets: vec![],
        }
    }

    type Asked = std::sync::Arc<std::sync::Mutex<Vec<String>>>;

    /// One CaptureProfile as the fake cluster serves it.
    #[derive(Clone)]
    struct Profile {
        collectors: Vec<&'static str>,
        config_maps: bool,
        signing_secret: Option<&'static str>,
    }

    /// The profile population the fake cluster lists, changeable between passes. `None` means the
    /// list endpoint fails, which is what an unreadable population looks like.
    type Profs = std::sync::Arc<std::sync::Mutex<Option<Vec<Profile>>>>;

    fn profiles(p: Option<Vec<Profile>>) -> Profs {
        std::sync::Arc::new(std::sync::Mutex::new(p))
    }

    fn source() -> Source {
        Source {
            own_namespace: "lapilli-system".into(),
            watch_namespaces: String::new(),
            destinations_file: std::path::PathBuf::from("/nonexistent/destinations.json"),
        }
    }

    async fn fake_authorizer(deny: Vec<&'static str>, broken: Vec<&'static str>) -> Client {
        fake_recording_authorizer(deny, broken).await.0
    }

    /// The authorizer plus a `captureprofiles` list endpoint, so `needs_from_cluster` has
    /// something real to derive from. `fake_recording_authorizer` is this with one profile that
    /// intends everything.
    async fn fake_cluster(
        profs: Profs,
        deny: Vec<&'static str>,
        broken: Vec<&'static str>,
    ) -> (Client, Asked) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let asked: Asked = Default::default();
        let sink = asked.clone();
        let list = move || {
            let profs = profs.clone();
            async move {
                let Some(items) = profs.lock().unwrap().clone() else {
                    return (
                        axum::http::StatusCode::INTERNAL_SERVER_ERROR,
                        axum::Json(serde_json::json!({"message": "list refused"})),
                    );
                };
                let items: Vec<serde_json::Value> = items
                    .iter()
                    .enumerate()
                    .map(|(i, p)| {
                        serde_json::json!({
                            "apiVersion": "lapilli.dev/v1alpha1", "kind": "CaptureProfile",
                            "metadata": { "name": format!("p{i}"), "namespace": "lapilli-system" },
                            "spec": {
                                "collectors": p.collectors,
                                "diffs": { "configMaps": p.config_maps },
                                "signing": match p.signing_secret {
                                    Some(s) => serde_json::json!({ "mode": "static", "keySecret": s }),
                                    None => serde_json::json!({ "mode": "none" }),
                                },
                            }
                        })
                    })
                    .collect();
                (
                    axum::http::StatusCode::OK,
                    axum::Json(serde_json::json!({
                        "apiVersion": "lapilli.dev/v1alpha1", "kind": "CaptureProfileList",
                        "metadata": { "resourceVersion": "1" }, "items": items,
                    })),
                )
            }
        };
        let app = axum::Router::new()
            .route(
                "/apis/lapilli.dev/v1alpha1/namespaces/:ns/captureprofiles",
                axum::routing::get(list),
            )
            .fallback(ssar(deny, broken, sink));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        let cfg = kube::Config::new(format!("http://{addr}/").parse().unwrap());
        (kube::Client::try_from(cfg).unwrap(), asked)
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
        fake_cluster(
            profiles(Some(vec![Profile {
                collectors: vec!["logs", "resources", "events", "changes", "metrics"],
                config_maps: false,
                signing_secret: None,
            }])),
            deny,
            broken,
        )
        .await
    }

    /// The SelfSubjectAccessReview half of the fake API server, as a handler.
    fn ssar(
        deny: Vec<&'static str>,
        broken: Vec<&'static str>,
        sink: Asked,
    ) -> impl Fn(
        String,
    ) -> std::pin::Pin<
        Box<dyn std::future::Future<Output = axum::Json<serde_json::Value>> + Send>,
    > + Clone
           + Send
           + 'static {
        move |body: String| {
            let deny = deny.clone();
            let broken = broken.clone();
            let sink = sink.clone();
            Box::pin(async move {
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
                        "reason": "rbac: RBAC: clusterrole.rbac.authorization.k8s.io \"lapilli-collector\" not found"
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
            })
        }
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
            "lapilli.dev/create incidentcaptures@lapilli-system#",
            "lapilli.dev/list incidentcaptures@lapilli-system#",
            "lapilli.dev/watch incidentcaptures@lapilli-system#",
            "lapilli.dev/patch incidentcaptures/status@lapilli-system#",
            "lapilli.dev/get captureprofiles@lapilli-system#",
            // kube-runtime's Recorder patches a repeated event rather than creating it again.
            "events.k8s.io/create events@lapilli-system#",
            "events.k8s.io/patch events@lapilli-system#",
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
            signing_secrets: vec!["lapilli-signing-key".into()],
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
        assert!(asked.contains(&"lapilli.dev/create incidentcaptures@lapilli-system#".to_string()));
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
            signing_secrets: vec!["lapilli-signing-key".into()],
            credential_secrets: vec!["s3-creds".into(), "gcs-creds".into()],
            ..needs()
        };
        check_once(&client, &needs).await;
        let asked = asked.lock().unwrap().clone();
        for name in ["lapilli-signing-key", "s3-creds", "gcs-creds"] {
            assert!(
                asked.contains(&format!("/get secrets@lapilli-system#{name}")),
                "no question carried the name {name}: {asked:?}"
            );
        }
        assert!(
            !asked.iter().any(|k| k == "/get secrets@lapilli-system#"),
            "a Secret was asked about without its name: {asked:?}"
        );
    }

    /// A denial is reported, the rest still are, and the count reaches /metrics.
    #[tokio::test]
    async fn a_denied_permission_is_counted_and_the_rest_still_are() {
        let client = fake_authorizer(vec!["/get pods/log@"], vec![]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(report.results["pod-logs"], Outcome::Denied);
        assert_eq!(report.results["pods"], Outcome::Held);
        assert_eq!(report.missing(), 1, "{:?}", report.results);
        assert_eq!(report.unknown(), 0);

        let m = Metrics::default();
        m.set_permissions(&report);
        let text = m.render();
        assert!(text.contains("lapilli_permissions_denied 1\n"), "{text}");
        assert!(text.contains("lapilli_permissions_unknown 0\n"), "{text}");
        assert!(
            text.contains("lapilli_permission_checks_total{result=\"denied\"} 1\n"),
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
        assert_eq!(report.results["pods"], Outcome::Denied);
        assert_eq!(report.results["captures"], Outcome::Held);
    }

    /// A check needs EVERY verb it lists. Granting `list` and not `watch` on IncidentCapture is the
    /// install where no alert is ever noticed, and it used to pass.
    #[tokio::test]
    async fn one_missing_verb_out_of_several_fails_the_check() {
        let client = fake_authorizer(vec!["lapilli.dev/watch incidentcaptures@"], vec![]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(
            report.results["captures"],
            Outcome::Denied,
            "watch is a distinct RBAC verb from list; missing it must fail the check"
        );
        assert_eq!(report.results["capture-status"], Outcome::Held);
    }

    /// "Could not ask" is not "denied", and it must not be counted as one — an authorizer webhook
    /// that times out would otherwise page about a permission that is in fact held.
    #[tokio::test]
    async fn an_unanswerable_question_is_not_a_denial() {
        let client = fake_authorizer(vec![], vec!["events.k8s.io/create events@"]).await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(report.results["recorded-events"], Outcome::Unknown);
        assert_eq!(report.missing(), 0, "an unknown must not count as missing");
        assert_eq!(report.unknown(), 1);

        let m = Metrics::default();
        m.set_permissions(&report);
        let text = m.render();
        assert!(text.contains("lapilli_permissions_unknown 1\n"), "{text}");
        assert!(text.contains("lapilli_permissions_denied 0\n"), "{text}");
        assert!(text.contains("lapilli_permission_checks_total{result=\"unknown\"} 1\n"));
    }

    /// The `Err` arm stops the check. Without the `break` a later denial would overwrite "could not
    /// ask" with "denied" — collapsing the one distinction this module is built around, and turning
    /// a flaky authorizer into a page about RBAC.
    #[tokio::test]
    async fn an_unanswerable_verb_is_not_overwritten_by_a_later_denial() {
        // For the `captures` check: `create` cannot be answered, `list` is refused. The unanswerable
        // one comes first, so the verdict must stay unknown.
        let client = fake_authorizer(
            vec!["lapilli.dev/list incidentcaptures@"],
            vec!["lapilli.dev/create incidentcaptures@"],
        )
        .await;
        let report = check_once(&client, &needs()).await;
        assert_eq!(
            report.results["captures"],
            Outcome::Unknown,
            "{:?}",
            report.results
        );
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
        spawn(
            client,
            source(),
            Default::default(),
            Duration::from_millis(150),
        );
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
        assert_eq!(by("capture-status"), "incidentcaptures.lapilli.dev/status");
        assert_eq!(by("recorded-events"), "events.events.k8s.io");
    }

    #[test]
    fn a_report_separates_denied_from_unanswerable_from_not_needed() {
        let r = Report {
            results: [
                ("a", Outcome::Held),
                ("b", Outcome::Denied),
                ("c", Outcome::Unknown),
                ("d", Outcome::Denied),
                ("e", Outcome::NotNeeded),
            ]
            .into(),
            at: chrono::Utc::now(),
        };
        assert_eq!(r.missing(), 2);
        assert_eq!(r.unknown(), 1);
        assert_eq!(r.not_needed(), 1);
        assert_eq!(
            r.asked(),
            4,
            "not-needed checks were never put to the API server"
        );
    }

    /// `denied_for` is what the reconciler uses to name a cause at the capture: only checks the
    /// collector depends on, and only the denied ones.
    #[test]
    fn a_collector_is_tied_only_to_the_denied_checks_it_depends_on() {
        let r = Report {
            results: [
                ("pods", Outcome::Held),
                ("pod-logs", Outcome::Denied),
                ("events", Outcome::Denied),
                ("replicasets", Outcome::Unknown),
                ("captures", Outcome::Denied),
            ]
            .into(),
            at: chrono::Utc::now(),
        };
        assert_eq!(r.denied_for("logs"), vec!["pod-logs"]);
        assert_eq!(r.denied_for("events"), vec!["events"]);
        assert!(
            r.denied_for("resources").is_empty(),
            "unknown is not denied, and `captures` is not a collector check"
        );
        assert!(r.denied_for("metrics").is_empty());
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
        // No profile endpoint: `needs_from_cluster` must degrade rather than fail — and it must
        // say that it could not read the profiles, not pretend it read none.
        let (client, _) = fake_cluster(profiles(None), vec![], vec![]).await;
        let needs = needs_from_cluster(
            &client,
            &Source {
                own_namespace: "lapilli-system".into(),
                watch_namespaces: " team-a , , Team_B ,".into(),
                destinations_file: dests,
            },
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
        assert!(needs.signing_secrets.is_empty());
        assert_eq!(needs.profiles, Profiles::Unreadable);
        assert!(needs.collectors.is_empty());
    }

    /// The union. Profiles are chosen per capture, so what any of them could ask for must be held:
    /// two profiles that between them intend `logs` and `changes` need `pods/log` **and**
    /// `controllerrevisions`, and their `signing` Secrets are both asked about by name.
    #[tokio::test]
    async fn needs_are_the_union_over_every_profile() {
        let (client, _) = fake_cluster(
            profiles(Some(vec![
                Profile {
                    collectors: vec!["resources"],
                    config_maps: false,
                    signing_secret: Some("key-a"),
                },
                Profile {
                    collectors: vec!["logs", "changes"],
                    config_maps: true,
                    signing_secret: Some("key-b"),
                },
                Profile {
                    collectors: vec!["resources"],
                    config_maps: false,
                    signing_secret: Some("key-a"),
                },
            ])),
            vec![],
            vec![],
        )
        .await;
        let needs = needs_from_cluster(&client, &source()).await;
        assert_eq!(needs.profiles, Profiles::Listed(3));
        let got: Vec<&str> = needs.collectors.iter().map(String::as_str).collect();
        assert_eq!(got, vec!["changes", "logs", "resources"]);
        assert!(needs.config_maps, "one profile turning it on is enough");
        assert_eq!(
            needs.signing_secrets,
            vec!["key-a".to_string(), "key-b".to_string()],
            "deduplicated, and both named"
        );
    }

    /// A perishable-only install: no profile intends `logs` or `events`, so `pods/log` and
    /// `events` are **not asked** — and are recorded as not needed, which is a different thing
    /// from "nothing was asked". `pods` and the `apps` reads are still needed by `resources` and
    /// `changes`, and still asked.
    #[tokio::test]
    async fn a_check_no_profile_needs_is_not_asked_and_says_so() {
        let (client, asked) = fake_cluster(
            profiles(Some(vec![Profile {
                collectors: vec!["resources", "changes"],
                config_maps: false,
                signing_secret: None,
            }])),
            vec![],
            vec![],
        )
        .await;
        let needs = needs_from_cluster(&client, &source()).await;
        let report = check_once(&client, &needs).await;
        let asked = asked.lock().unwrap().clone();
        assert!(
            !asked.iter().any(|k| k.contains("pods/log")),
            "pods/log must not be asked when no profile intends logs: {asked:?}"
        );
        assert!(!asked.iter().any(|k| k.starts_with("/list events@")));
        assert!(asked.contains(&"/get pods@*#".to_string()), "{asked:?}");
        assert!(asked.contains(&"apps/list controllerrevisions@*#".to_string()));
        assert_eq!(report.results["pod-logs"], Outcome::NotNeeded);
        assert_eq!(report.results["events"], Outcome::NotNeeded);
        assert_eq!(report.results["pods"], Outcome::Held);
        assert_eq!(report.not_needed(), 2);
        assert_eq!(report.missing(), 0);

        // And the metrics tell the two apart: nothing denied, nothing unknown, and the asked
        // count is two short of the full set — visible without naming which two.
        let m = Metrics::default();
        m.set_permissions(&report);
        let text = m.render();
        assert!(text.contains("lapilli_permissions_denied 0\n"), "{text}");
        assert!(
            text.contains(&format!("lapilli_permissions_asked {}\n", report.asked())),
            "{text}"
        );
        assert!(
            text.contains("lapilli_permission_checks_total{result=\"not_needed\"} 2\n"),
            "{text}"
        );
        assert!(!text.contains("pod-logs"), "{text}");
    }

    /// The profiles could not be listed. Before, that was a `debug!` and then "every permission
    /// this install needs is held". Now the collector checks are *unknown* — not held, not denied
    /// — and the unconditional checks are still asked.
    #[tokio::test]
    async fn an_unreadable_profile_list_makes_collector_checks_unknown_not_held() {
        let (client, asked) = fake_cluster(profiles(None), vec![], vec![]).await;
        let needs = needs_from_cluster(&client, &source()).await;
        let report = check_once(&client, &needs).await;
        for c in [
            "pods",
            "pod-logs",
            "events",
            "replicasets",
            "controllerrevisions",
        ] {
            assert_eq!(report.results[c], Outcome::Unknown, "{c}");
        }
        assert_eq!(report.results["captures"], Outcome::Held);
        assert_eq!(report.missing(), 0, "unknown is not a denial");
        assert!(report.unknown() >= 8);
        let asked = asked.lock().unwrap().clone();
        assert!(!asked.iter().any(|k| k.contains(" pods")), "{asked:?}");
        assert!(asked
            .iter()
            .any(|k| k.starts_with("lapilli.dev/list incidentcaptures@")));
    }

    /// Needs are re-derived on every pass. Add `logs` to a profile between two passes and the
    /// second pass asks about `pods/log`; a `Needs` frozen at startup never would.
    #[tokio::test]
    async fn the_loop_re_derives_its_needs_from_the_profiles_each_pass() {
        let profs = profiles(Some(vec![Profile {
            collectors: vec!["resources"],
            config_maps: false,
            signing_secret: None,
        }]));
        let (client, asked) = fake_cluster(profs.clone(), vec![], vec![]).await;
        let shared: Shared = Default::default();
        let src = source();
        pass(&client, &src, &shared).await;
        assert!(
            !asked.lock().unwrap().iter().any(|k| k.contains("pods/log")),
            "first pass: no profile intends logs"
        );
        assert!(shared.read().unwrap().is_some(), "the report is published");

        *profs.lock().unwrap() = Some(vec![Profile {
            collectors: vec!["resources", "logs"],
            config_maps: false,
            signing_secret: None,
        }]);
        pass(&client, &src, &shared).await;
        assert!(
            asked.lock().unwrap().iter().any(|k| k.contains("pods/log")),
            "second pass: the profile now intends logs, so pods/log is asked"
        );
        assert_eq!(
            shared.read().unwrap().as_ref().unwrap().results["pod-logs"],
            Outcome::Held
        );
    }

    /// A typo in `LAPILLI_WATCH_NAMESPACES` must not become a permanent critical page. The API server
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
