//! Bounded local retention — reclaiming what the bundle volume does not need to keep.
//!
//! Design and its review: `docs/design-retention.md`, `docs/design-review-round17.md`.
//!
//! The defect this closes is not "old bundles pile up". It is that **when the volume is full, new
//! captures fail**: the recorder stops recording, and what it stops recording is the incident
//! happening now.
//!
//! Three things about this module are there because a review round put them there, and each is the
//! opposite of what the first design said:
//!
//! **Bytes are the primary bound, not age.** On the chart's 1 GiB default, one alert over a 20-pod
//! Deployment at Alertmanager's hourly repeat fills the volume on day 9 — with a 30-day window
//! having deleted nothing. An age window alone never engages before the disk does.
//!
//! **There are no "sidecars".** [`RECLAIMABLE`] and [`NEVER`] are explicit, in both directions,
//! because the phrase "the `.ieb` and its sidecars" is what nearly deleted the two `O_EXCL` claim
//! files — re-arming notification for month-old incidents and freeing an incident id so a resent
//! webhook could build a new bundle carrying the old one's identity.
//!
//! **A bundle is reclaimable only when a remote copy demonstrably exists**, and that is never read
//! from `status`. `ExportState::settled()` is true for `Refused`, `Conflict` and `Failed` — the three
//! states `docs/metrics.md` defines as "that evidence never reached the destination and never will" —
//! so "settled" would have deleted the only copy exactly when there is no second copy.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

/// The whole pass is bounded. Rounds 14 and 15 each found a loop in this crate with no time budget;
/// this is the place not to make it three.
pub const SWEEP_BUDGET: Duration = Duration::from_secs(60);
/// And the work inside it. Enabling retention on a year-old install would otherwise be ~50,000
/// unlinks and one status patch each in a single pass.
pub const MAX_RECLAIMS_PER_SWEEP: usize = 300;
/// How often the sweep runs. A day-scale window does not need minutes-scale sweeping.
pub const SWEEP_INTERVAL: Duration = Duration::from_secs(900);

/// The orphan pass refuses above this share of the population: that is the signature of a CR wipe
/// (`kubectl delete incidentcapture --all`, or the documented CRD delete-and-recreate upgrade), not
/// of a human tidying one capture.
pub const ORPHAN_REFUSE_COUNT: usize = 100;
pub const ORPHAN_REFUSE_FRACTION: f64 = 0.05;

/// Suffixes retention may remove for a capture it has decided about.
pub const RECLAIMABLE: [&str; 2] = [".ieb", ".summary.json"];
/// Suffixes and paths retention never removes, whatever the policy says.
///
/// `.notified` and `.ieb.owner` are **claims, not sidecars** (`notify.rs`, `reconcile.rs`), and
/// `keys/` holds the archived signing keys — on a local-only install a rotated key exists nowhere
/// else, so removing it makes every bundle it signed unverifiable.
pub const NEVER: [&str; 3] = [".notified", ".ieb.owner", "keys"];

/// What the chart configured. `0` means off for both bounds.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Policy {
    /// The primary bound: reclaim oldest-first until the volume's usage is under this.
    pub max_bytes: u64,
    /// A secondary trim, for the liability argument rather than the capacity one.
    pub days: u32,
    /// Refuse to start a capture with less than this free. Enforced by [`enough_free`].
    pub min_free_bytes: u64,
    /// Reclaim a bundle that reached no destination. Needed for a PVC-only install, where the local
    /// copy is the only copy — which is exactly why it is a separate, explicit switch.
    pub allow_unexported: bool,
    /// Reclaim files whose `IncidentCapture` is gone. Dangerous: "no live CR" is a statement about
    /// human behaviour, since nothing in this controller ever deletes one.
    pub reclaim_orphans: bool,
}

impl Policy {
    /// Nothing to sweep for: neither bound is set and no orphan pass was asked for. The preflight
    /// still runs, and the volume gauge is published regardless.
    pub fn sweeps(&self) -> bool {
        self.max_bytes > 0 || self.days > 0 || self.reclaim_orphans
    }
}

/// Bytes used and available on the filesystem holding `root`, from one `statvfs`.
///
/// O(1), correct when the volume is full, and it counts staging directories and leftovers that
/// summing `*.ieb` cannot. `None` if the call fails — the caller publishes nothing rather than a
/// zero, because a gauge that reads `0 free` when it simply could not look is the failure round 14
/// wrote down twice.
pub fn fs_bytes(root: &Path) -> Option<(u64, u64)> {
    use std::os::unix::ffi::OsStrExt;
    let mut c = Vec::with_capacity(root.as_os_str().as_bytes().len() + 1);
    c.extend_from_slice(root.as_os_str().as_bytes());
    c.push(0);
    // SAFETY: `c` is NUL-terminated and `buf` is the shape statvfs writes.
    let mut buf: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statvfs(c.as_ptr() as *const libc::c_char, &mut buf) };
    if rc != 0 {
        return None;
    }
    let frsize = buf.f_frsize as u64;
    let total = (buf.f_blocks as u64).checked_mul(frsize)?;
    let unused = (buf.f_bfree as u64).checked_mul(frsize)?;
    // `f_bavail`, not `f_bfree`: the reserve is not ours to spend.
    let free = (buf.f_bavail as u64).checked_mul(frsize)?;
    Some((total.saturating_sub(unused), free))
}

/// The preflight. A capture that cannot possibly be sealed should fail **before** it collects, with
/// a reason code, rather than half-collecting and dying on a raw ENOSPC — which surfaces as
/// `"No space left on device (os error 28)"` and honours none of the convention `reconcile.rs` sets
/// for capture errors.
///
/// A `statvfs` that fails is not treated as "full": the capture proceeds. Refusing to record because
/// we could not measure the disk would turn a monitoring gap into an outage.
pub fn enough_free(root: &Path, min_free_bytes: u64) -> Result<(), String> {
    if min_free_bytes == 0 {
        return Ok(());
    }
    match fs_bytes(root) {
        Some((_, free)) if free < min_free_bytes => Err(format!(
            "pvc-full: {free} bytes free on the bundle volume, below the {min_free_bytes} this \
             capture needs; raise persistence.size or enable retention (docs/design-retention.md)"
        )),
        _ => Ok(()),
    }
}

/// Why a file was reclaimed, as the `reason` label and the journal's `reason`.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Reason {
    /// The volume is over `max_bytes` and this was the oldest reclaimable bundle.
    MaxBytes,
    /// Older than `days`.
    Age,
    /// Its `IncidentCapture` is gone.
    Orphan,
    /// Work abandoned by a capture that is no longer live: a staging directory or a pack temp file.
    Abandoned,
}

impl Reason {
    pub fn label(self) -> &'static str {
        match self {
            Reason::MaxBytes => "max-bytes",
            Reason::Age => "age",
            Reason::Orphan => "orphan",
            Reason::Abandoned => "abandoned",
        }
    }
}

/// Why a candidate was left alone. Counted, so "retention cannot keep up" is visible rather than
/// silent — a refusal that nobody can see is the failure mode this whole feature is about.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Refusal {
    /// Not every destination reached `Uploaded`. Includes the terminal failures, deliberately.
    NotUploaded,
    /// No destination is configured and `allowUnexported` is off.
    Unexported,
    /// Still in flight (anything before `Exported`).
    InFlight,
    /// The unlink itself failed — a read-only or WORM-backed volume. The bundle is **not** recorded
    /// as reclaimed: a tool that reports success while the bytes remain is worse than one that
    /// refuses.
    Undeletable,
    /// The orphan pass saw too much of the population missing its CR and stopped.
    OrphanStorm,
}

impl Refusal {
    pub fn label(self) -> &'static str {
        match self {
            Refusal::NotUploaded => "not-uploaded",
            Refusal::Unexported => "unexported",
            Refusal::InFlight => "in-flight",
            Refusal::Undeletable => "undeletable",
            Refusal::OrphanStorm => "orphan-storm",
        }
    }
}

/// What the controller knows about one capture, re-derived rather than read back as a decision.
///
/// `all_uploaded` is computed by the caller from `status.exports`, but only ever to say **no** —
/// the guard is that a `true` cannot be manufactured into a delete by patching status, because the
/// caller also requires the destination set to be non-empty and the phase to be `Exported`.
#[derive(Clone, Debug)]
pub struct Known {
    pub phase_exported: bool,
    pub destinations: usize,
    pub all_uploaded: bool,
}

/// One candidate found on disk.
#[derive(Clone, Debug)]
pub struct Candidate {
    pub path: PathBuf,
    /// The incident id parsed out of the filename, for a bundle or its summary.
    pub incident: Option<String>,
    pub bytes: u64,
    pub age: Duration,
    /// A staging directory or pack temp file, carrying the capture uid from its name.
    pub abandoned_uid: Option<String>,
}

/// Decide one candidate. Split out from the sweep so it can be tested without a filesystem or an
/// API server — and so the table of refusals is one readable function rather than nested `if`s.
pub fn decide(
    c: &Candidate,
    known: &BTreeMap<String, Known>,
    live_uids: &[String],
    policy: &Policy,
    over_bytes: bool,
) -> Result<Reason, Refusal> {
    // Abandoned work is keyed on the capture uid in its name, never on age: a capture may sit in
    // `Sealing` for days through a KMS outage, which reconcile.rs already anticipates.
    if let Some(uid) = &c.abandoned_uid {
        return if live_uids.iter().any(|u| u == uid) {
            Err(Refusal::InFlight)
        } else {
            Ok(Reason::Abandoned)
        };
    }

    let incident = c.incident.as_deref().ok_or(Refusal::InFlight)?;
    let Some(k) = known.get(incident) else {
        // No live CR. Whether that is reclaimable at all is the caller's decision (the orphan
        // switch and the storm guard); here it is only a classification.
        return if policy.reclaim_orphans {
            Ok(Reason::Orphan)
        } else {
            Err(Refusal::NotUploaded)
        };
    };

    if !k.phase_exported {
        return Err(Refusal::InFlight);
    }
    if k.destinations == 0 {
        // The local copy is the only copy. Reclaimable only because a PVC-only install is exactly
        // the one that fills up — and only with the operator's explicit say-so.
        if !policy.allow_unexported {
            return Err(Refusal::Unexported);
        }
    } else if !k.all_uploaded {
        // Includes Refused, Conflict and Failed. `metrics.md`: that evidence never reached the
        // destination and never will, so the local file is the only copy there is.
        return Err(Refusal::NotUploaded);
    }

    if over_bytes {
        return Ok(Reason::MaxBytes);
    }
    if policy.days > 0 && c.age >= Duration::from_secs(policy.days as u64 * 86_400) {
        return Ok(Reason::Age);
    }
    Err(Refusal::InFlight)
}

/// The durable record. Events expire within the hour, `status.local` dies with the CR — and the
/// orphan case is *premised* on a human deleting the CR — and counters reset on restart. So one
/// append-only file at the bundle root is the only thing that can still say, a year later, that
/// Kairn reclaimed a bundle rather than lost it.
pub const JOURNAL: &str = "reclaimed.jsonl";
/// Rotated at this size, keeping one previous generation.
pub const JOURNAL_MAX_BYTES: u64 = 8 * 1024 * 1024;

/// Append one line. Failing to record is a reason **not** to delete, so this returns an error the
/// caller must handle before unlinking anything.
pub fn journal_append(root: &Path, line: &serde_json::Value) -> std::io::Result<()> {
    use std::io::Write;
    let path = root.join(JOURNAL);
    if std::fs::metadata(&path).map(|m| m.len()).unwrap_or(0) >= JOURNAL_MAX_BYTES {
        let _ = std::fs::rename(&path, root.join(format!("{JOURNAL}.1")));
    }
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&path)?;
    writeln!(f, "{line}")?;
    f.sync_all()
}

/// Walk the bundle root once and classify everything in it. Runs on a blocking thread: a slow
/// RWO volume must not hold a tokio worker while the webhook waits.
///
/// Returns candidates plus the count of live-CR-less bundles, which the caller needs for the
/// orphan-storm guard *before* it deletes anything.
pub fn scan(
    root: &Path,
    known: &BTreeMap<String, Known>,
) -> std::io::Result<(Vec<Candidate>, usize)> {
    let now = std::time::SystemTime::now();
    let mut out = Vec::new();
    let mut orphans = 0usize;
    for entry in std::fs::read_dir(root)? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().to_string();
        // Checked before anything else. Today the allowlist further down would exclude these
        // anyway — which is to say this guard is *currently* redundant, and deliberately kept:
        // it is what keeps the allowlist safe if someone ever widens it. `is_protected` is tested
        // directly so the branch is not an untested claim (round 13's vacuous-check lesson).
        if is_protected(&name) {
            continue;
        }
        let meta = entry.metadata()?;
        let age = meta
            .modified()
            .ok()
            .and_then(|m| now.duration_since(m).ok())
            .unwrap_or_default();

        // Abandoned work: `.staging-<incident>-<uid>/` and `.<incident>-<uid>.ieb.tmp`.
        if let Some(uid) = abandoned_uid(&name) {
            out.push(Candidate {
                bytes: if meta.is_dir() {
                    dir_bytes(&entry.path())
                } else {
                    meta.len()
                },
                path: entry.path(),
                incident: None,
                age,
                abandoned_uid: Some(uid),
            });
            continue;
        }
        // Allowlist, not exclusion: only these two suffixes are ever candidates.
        let Some(incident) = RECLAIMABLE.iter().find_map(|sfx| name.strip_suffix(sfx)) else {
            continue;
        };
        if !known.contains_key(incident) {
            orphans += 1;
        }
        out.push(Candidate {
            path: entry.path(),
            incident: Some(incident.to_string()),
            bytes: meta.len(),
            age,
            abandoned_uid: None,
        });
    }
    // Oldest first: the byte ceiling reclaims in the order that frees the most history soonest.
    out.sort_by_key(|c| std::cmp::Reverse(c.age));
    Ok((out, orphans))
}

/// Names retention must never touch, whatever else matches: the two `O_EXCL` claims, the archived
/// signing keys, and the journal that records the reclaims.
pub fn is_protected(name: &str) -> bool {
    NEVER.iter().any(|n| name == *n || name.ends_with(n)) || name.starts_with(JOURNAL)
}

/// `.staging-<incident>-<uid>` or `.<incident>-<uid>.ieb.tmp` → the uid.
fn abandoned_uid(name: &str) -> Option<String> {
    let rest = name.strip_prefix(".staging-").or_else(|| {
        name.strip_prefix('.')
            .and_then(|r| r.strip_suffix(".ieb.tmp"))
    })?;
    rest.rsplit_once('-').map(|(_, uid)| uid.to_string())
}

fn dir_bytes(p: &Path) -> u64 {
    let mut n = 0;
    if let Ok(rd) = std::fs::read_dir(p) {
        for e in rd.flatten() {
            match e.metadata() {
                Ok(m) if m.is_dir() => n += dir_bytes(&e.path()),
                Ok(m) => n += m.len(),
                Err(_) => {}
            }
        }
    }
    n
}

/// Remove one candidate. `Undeletable` rather than a panic on a read-only or WORM-backed volume,
/// and the caller must not record it as reclaimed in that case.
pub fn remove(c: &Candidate) -> Result<(), Refusal> {
    let r = if c.path.is_dir() {
        std::fs::remove_dir_all(&c.path)
    } else {
        std::fs::remove_file(&c.path)
    };
    match r {
        Ok(()) => Ok(()),
        // Already gone is success: another replica or a human got there first.
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => {
            tracing::warn!(path = %c.path.display(), error = %e,
                           "retention could not reclaim this file; it is NOT recorded as reclaimed");
            Err(Refusal::Undeletable)
        }
    }
}

/// The orphan-storm guard. `kubectl delete incidentcapture --all`, or the documented CRD
/// delete-and-recreate upgrade, leaves every bundle without a CR — and the sweep must read that as
/// "something happened to the CRs", not as "delete everything".
pub fn orphan_storm(orphans: usize, population: usize) -> bool {
    orphans > ORPHAN_REFUSE_COUNT
        || (population > 0 && (orphans as f64 / population as f64) > ORPHAN_REFUSE_FRACTION)
}

/// Run the sweep on a timer. Only when the policy asks for something: the volume gauge and the
/// capture preflight are always on and do not depend on this task existing.
pub fn spawn(
    api: kube::Api<crate::crd::IncidentCapture>,
    root: PathBuf,
    policy: Policy,
    every: Duration,
) {
    if !policy.sweeps() {
        tracing::info!(
            "retention is off; the bundle volume is still measured (kairn_bundle_fs_bytes) so the \
             disk filling is visible before it fills"
        );
        return;
    }
    tokio::spawn(async move {
        loop {
            match tokio::time::timeout(SWEEP_BUDGET, sweep_once(&api, &root, &policy)).await {
                Ok(Ok(n)) => {
                    crate::telemetry::metrics().sweep(true);
                    if n > 0 {
                        tracing::info!(reclaimed = n, "retention sweep");
                    }
                }
                Ok(Err(e)) => {
                    crate::telemetry::metrics().sweep(false);
                    tracing::warn!(error = %e, "retention sweep failed");
                }
                Err(_) => {
                    crate::telemetry::metrics().sweep(false);
                    tracing::warn!(budget = ?SWEEP_BUDGET, "retention sweep ran out of budget");
                }
            }
            tokio::time::sleep(every).await;
        }
    });
}

/// One pass. Returns how many files it reclaimed.
async fn sweep_once(
    api: &kube::Api<crate::crd::IncidentCapture>,
    root: &Path,
    policy: &Policy,
) -> anyhow::Result<usize> {
    let (known, live_uids) = list_captures(api).await?;

    // The filesystem walk goes to a blocking thread: a slow RWO volume must not hold a tokio worker
    // while the webhook waits.
    let root_owned = root.to_path_buf();
    let known_for_scan = known.clone();
    let (candidates, orphans) =
        tokio::task::spawn_blocking(move || scan(&root_owned, &known_for_scan)).await??;

    let population = candidates.iter().filter(|c| c.incident.is_some()).count();
    let storm = orphan_storm(orphans, population);
    if storm && policy.reclaim_orphans {
        crate::telemetry::metrics().refused_reclaim(Refusal::OrphanStorm.label());
        tracing::error!(
            orphans, population,
            "refusing the orphan pass: this many bundles without an IncidentCapture looks like the \
             CRs were wiped (kubectl delete --all, or the CRD delete-and-recreate upgrade), not like \
             a human tidying one capture"
        );
    }

    // Bytes first — and the ceiling is on **our own footprint**, not on the filesystem's used
    // bytes. A PVC is usually backed by a filesystem much larger than the request (hostPath,
    // local-path, kind), so `statvfs` used-bytes and `persistence.size` are different quantities:
    // comparing them made the ceiling trip immediately on a kind cluster, which is how this was
    // found. `statvfs` stays where it belongs — the free-space gauge and the capture preflight,
    // which really are about the filesystem.
    let mut own_bytes: u64 = candidates.iter().map(|c| c.bytes).sum();
    let mut over = policy.max_bytes > 0 && own_bytes > policy.max_bytes;

    let mut done = 0usize;
    for c in &candidates {
        if done >= MAX_RECLAIMS_PER_SWEEP {
            tracing::info!(
                cap = MAX_RECLAIMS_PER_SWEEP,
                "retention hit its per-sweep cap; the rest waits for the next pass"
            );
            break;
        }
        let reason = match decide(c, &known, &live_uids, policy, over) {
            Ok(r) => r,
            Err(why) => {
                crate::telemetry::metrics().refused_reclaim(why.label());
                continue;
            }
        };
        if reason == Reason::Orphan && storm {
            continue;
        }

        // Remove first, then record. The other order looks safer — "never delete without a record"
        // — and is worse: a removal that fails leaves a journal line claiming a reclaim that did not
        // happen, which is exactly the "reports success while the object remains" failure DESIGN.md
        // §11 warns about. Measured on a live cluster, where a permission error produced both a
        // warning saying it was NOT reclaimed and a journal line saying it was.
        //
        // The residual is the reverse and smaller: if the journal write fails after a successful
        // removal, the bytes are gone and unrecorded. That is logged at error level with the same
        // JSON, so the record exists somewhere even when the file could not take it.
        if remove(c).is_err() {
            crate::telemetry::metrics().refused_reclaim(Refusal::Undeletable.label());
            continue;
        }
        let line = serde_json::json!({
            "incident": c.incident,
            "path": c.path.file_name().map(|n| n.to_string_lossy()),
            "bytes": c.bytes,
            "reason": reason.label(),
            "at": chrono::Utc::now().to_rfc3339(),
            "exports": c.incident.as_deref().and_then(|i| known.get(i)).map(|k| {
                serde_json::json!({ "destinations": k.destinations, "allUploaded": k.all_uploaded })
            }),
        });
        if let Err(e) = journal_append(root, &line) {
            tracing::error!(
                error = %e, record = %line,
                "reclaimed a file but could not append to the journal; this log line is the record"
            );
        }
        crate::telemetry::metrics().reclaimed(reason.label(), c.bytes);
        done += 1;

        // Stop as soon as we are back under the ceiling; age still trims below that.
        own_bytes = own_bytes.saturating_sub(c.bytes);
        if over {
            over = own_bytes > policy.max_bytes;
        }

        // The CR learns its bundle is gone, so `bundlePath` stops pointing at nothing. Reporting
        // only — the journal is the record.
        if reason != Reason::Orphan && reason != Reason::Abandoned {
            if let Some(incident) = &c.incident {
                if c.path.extension().is_some_and(|e| e == "ieb") {
                    patch_local(api, &known, incident, reason).await;
                }
            }
        }
    }
    Ok(done)
}

/// Every capture, paged. Never `ListParams::default()`: a second unpaginated copy of a large
/// population in a pod limited to 256 MiB is an OOM risk, and an OOMKill discards the capture in
/// flight.
async fn list_captures(
    api: &kube::Api<crate::crd::IncidentCapture>,
) -> anyhow::Result<(BTreeMap<String, Known>, Vec<String>)> {
    use crate::crd::{ExportState, Phase};
    let mut known = BTreeMap::new();
    let mut live = Vec::new();
    let mut token: Option<String> = None;
    loop {
        let mut lp = kube::api::ListParams::default().limit(500);
        if let Some(t) = &token {
            lp = lp.continue_token(t);
        }
        let page = api.list(&lp).await?;
        for ic in &page.items {
            let status = ic.status.clone().unwrap_or_default();
            let uid = ic.metadata.uid.clone().unwrap_or_default();
            live.push(uid.clone());
            known.insert(
                ic.spec.incident_id.clone(),
                Known {
                    phase_exported: status.phase == Phase::Exported,
                    destinations: status.exports.len(),
                    all_uploaded: !status.exports.is_empty()
                        && status
                            .exports
                            .values()
                            .all(|e| e.state == ExportState::Uploaded),
                },
            );
        }
        token = page.metadata.continue_.filter(|t| !t.is_empty());
        if token.is_none() {
            return Ok((known, live));
        }
    }
}

async fn patch_local(
    api: &kube::Api<crate::crd::IncidentCapture>,
    known: &BTreeMap<String, Known>,
    incident: &str,
    reason: Reason,
) {
    let Some(name) = known.get(incident).map(|_| incident) else {
        return;
    };
    // The CR name is not the incident id; find it by listing is wasteful, so patch by the name the
    // webhook derives. `ic-` + the hash tail is how it is built (webhook.rs), and the incident id is
    // `<cluster>-<tail>`, so the tail is what both share.
    let tail = name.rsplit('-').next().unwrap_or_default();
    let cr = format!("ic-{tail}");
    let patch = serde_json::json!({ "status": { "local": {
        "state": "reclaimed",
        "at": chrono::Utc::now().to_rfc3339(),
        "reason": reason.label(),
    }}});
    if let Err(e) = api
        .patch_status(
            &cr,
            &kube::api::PatchParams::apply("kairn.dev/retention"),
            &kube::api::Patch::Merge(&patch),
        )
        .await
    {
        tracing::debug!(capture = %cr, error = %e,
                        "could not record the reclaim on the capture; the journal has it");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn known(exported: bool, destinations: usize, all_uploaded: bool) -> Known {
        Known {
            phase_exported: exported,
            destinations,
            all_uploaded,
        }
    }

    fn bundle(incident: &str, age_days: u64) -> Candidate {
        Candidate {
            path: PathBuf::from(format!("/b/{incident}.ieb")),
            incident: Some(incident.to_string()),
            bytes: 250_000,
            age: Duration::from_secs(age_days * 86_400),
            abandoned_uid: None,
        }
    }

    fn policy() -> Policy {
        Policy {
            max_bytes: 0,
            days: 7,
            min_free_bytes: 0,
            allow_unexported: false,
            reclaim_orphans: false,
        }
    }

    /// The finding that made round 17 rewrite the design. `ExportState::settled()` admits `Refused`,
    /// `Conflict` and `Failed`, and `docs/metrics.md` defines those as evidence that never reached
    /// the destination and never will — so "settled" would delete the only copy in exactly the case
    /// where there is no second copy.
    #[test]
    fn a_destination_that_never_received_it_is_a_refusal_not_a_go_ahead() {
        let mut k = BTreeMap::new();
        k.insert("i1".to_string(), known(true, 1, false));
        assert_eq!(
            decide(&bundle("i1", 30), &k, &[], &policy(), false),
            Err(Refusal::NotUploaded),
            "a bundle whose only destination is refused/conflict/failed must be kept"
        );
        // …and with the upload confirmed it is reclaimable on age.
        k.insert("i1".to_string(), known(true, 1, true));
        assert_eq!(
            decide(&bundle("i1", 30), &k, &[], &policy(), false),
            Ok(Reason::Age)
        );
    }

    /// A PVC-only install has no second copy at all, so reclaiming needs the operator to say so.
    #[test]
    fn an_install_with_no_destination_needs_an_explicit_switch() {
        let mut k = BTreeMap::new();
        k.insert("i1".to_string(), known(true, 0, false));
        assert_eq!(
            decide(&bundle("i1", 30), &k, &[], &policy(), false),
            Err(Refusal::Unexported)
        );
        let opted_in = Policy {
            allow_unexported: true,
            ..policy()
        };
        assert_eq!(
            decide(&bundle("i1", 30), &k, &[], &opted_in, false),
            Ok(Reason::Age)
        );
    }

    /// Anything before `Exported` is mid-flight, whatever its age says.
    #[test]
    fn a_capture_still_in_flight_is_never_reclaimed() {
        let mut k = BTreeMap::new();
        k.insert("i1".to_string(), known(false, 1, true));
        assert_eq!(
            decide(&bundle("i1", 365), &k, &[], &policy(), true),
            Err(Refusal::InFlight)
        );
    }

    /// Abandoned staging is keyed on the capture uid, never on age — a capture can legitimately sit
    /// in `Sealing` for days through a KMS outage.
    #[test]
    fn abandoned_work_is_keyed_on_the_capture_not_on_its_age() {
        let staging = Candidate {
            path: PathBuf::from("/b/.staging-i1-uidA"),
            incident: None,
            bytes: 9_000_000,
            age: Duration::from_secs(60),
            abandoned_uid: Some("uidA".into()),
        };
        assert_eq!(
            decide(
                &staging,
                &BTreeMap::new(),
                &["uidA".to_string()],
                &policy(),
                false
            ),
            Err(Refusal::InFlight),
            "its capture is still live, so this is work in progress and not garbage"
        );
        assert_eq!(
            decide(&staging, &BTreeMap::new(), &[], &policy(), false),
            Ok(Reason::Abandoned),
            "no live capture owns it — and it is uncompressed, so it is the biggest win available"
        );
    }

    /// Bytes are the primary bound. With the volume over its ceiling an uploaded bundle goes even
    /// when it is far inside the age window, because age alone never engages before the disk fills.
    #[test]
    fn over_the_byte_ceiling_age_does_not_have_to_be_reached() {
        let mut k = BTreeMap::new();
        k.insert("i1".to_string(), known(true, 1, true));
        let young = bundle("i1", 0);
        assert_eq!(
            decide(&young, &k, &[], &policy(), false),
            Err(Refusal::InFlight),
            "not over the ceiling and not old: keep"
        );
        assert_eq!(
            decide(&young, &k, &[], &policy(), true),
            Ok(Reason::MaxBytes)
        );
    }

    /// The claim files and the archived keys are not reclaimable at any age, under any policy. The
    /// lists are asserted against each other so a future edit cannot put a name in both.
    #[test]
    fn the_claims_and_the_keys_are_never_in_the_reclaimable_set() {
        for never in NEVER {
            assert!(
                !RECLAIMABLE.contains(&never),
                "{never} must never be reclaimable"
            );
        }
        assert!(NEVER.contains(&".notified"), "the notification claim");
        assert!(NEVER.contains(&".ieb.owner"), "the incident-id claim");
        assert!(NEVER.contains(&"keys"), "the archived signing keys");
    }

    #[test]
    fn a_policy_with_nothing_set_does_not_sweep() {
        assert!(!Policy::default().sweeps());
        assert!(Policy {
            max_bytes: 1,
            ..Policy::default()
        }
        .sweeps());
        assert!(Policy {
            days: 1,
            ..Policy::default()
        }
        .sweeps());
        assert!(Policy {
            reclaim_orphans: true,
            ..Policy::default()
        }
        .sweeps());
    }

    /// `statvfs` on a real directory, and the one thing that must not happen: a failed call must not
    /// read as "no space", because that would turn a monitoring gap into an outage.
    #[test]
    fn free_space_is_measured_and_an_unmeasurable_volume_does_not_block_a_capture() {
        let dir = tempfile::tempdir().unwrap();
        let (used, free) = fs_bytes(dir.path()).expect("statvfs on a temp dir");
        assert!(free > 0, "a writable temp dir has free space");
        assert!(used > 0, "and a non-empty filesystem has used space");

        assert!(enough_free(dir.path(), 1024).is_ok());
        assert!(
            enough_free(dir.path(), u64::MAX).is_err(),
            "an impossible requirement must refuse, with a reason code"
        );
        let err = enough_free(dir.path(), u64::MAX).unwrap_err();
        assert!(err.starts_with("pvc-full:"), "{err}");

        // A path that cannot be measured: the capture proceeds.
        assert!(enough_free(Path::new("/definitely/not/here"), u64::MAX).is_ok());
        assert!(
            enough_free(dir.path(), 0).is_ok(),
            "0 disables the preflight"
        );
    }

    /// The scan is an allowlist. This is the test that would have caught the design's worst bug:
    /// a bundle root holding the two claim files and the key archive must yield the bundle and the
    /// summary and **nothing else**.
    #[test]
    fn the_scan_never_offers_a_claim_file_or_an_archived_key() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        for name in [
            "kind-abc123.ieb",
            "kind-abc123.summary.json",
            "kind-abc123.notified",
            "kind-abc123.ieb.owner",
            "reclaimed.jsonl",
            "reclaimed.jsonl.1",
        ] {
            std::fs::write(root.join(name), b"x").unwrap();
        }
        std::fs::create_dir(root.join("keys")).unwrap();
        std::fs::write(root.join("keys").join("deadbeef.pub"), b"k").unwrap();

        let (found, orphans) = scan(root, &BTreeMap::new()).unwrap();
        let mut names: Vec<String> = found
            .iter()
            .map(|c| c.path.file_name().unwrap().to_string_lossy().to_string())
            .collect();
        names.sort();
        assert_eq!(
            names,
            vec!["kind-abc123.ieb", "kind-abc123.summary.json"],
            "the claims, the key archive and the journal are not candidates"
        );
        assert_eq!(orphans, 2, "both candidates have no live CR here");
        // And the files that must survive are still on disk after a scan.
        assert!(root.join("kind-abc123.notified").exists());
        assert!(root.join("keys").join("deadbeef.pub").exists());
    }

    /// The guard itself, tested directly. Without this the branch is redundant today and therefore
    /// untested, which is how a check stops meaning anything.
    #[test]
    fn the_protected_names_are_recognised_as_such() {
        for protected in [
            "kind-abc123.notified",
            "kind-abc123.ieb.owner",
            "keys",
            "reclaimed.jsonl",
            "reclaimed.jsonl.1",
        ] {
            assert!(is_protected(protected), "{protected} must be protected");
        }
        for candidate in ["kind-abc123.ieb", "kind-abc123.summary.json"] {
            assert!(!is_protected(candidate), "{candidate} is a candidate");
        }
    }

    /// Abandoned work is found by name, including the directory form the first design missed — it
    /// said "a *file* under the bundle root", and staging is a directory holding uncompressed
    /// evidence, which makes it the largest reclaimable thing there is.
    #[test]
    fn abandoned_staging_and_temp_files_are_found_with_their_uid() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        let stage = root.join(".staging-kind-abc123-uidA");
        std::fs::create_dir(&stage).unwrap();
        std::fs::write(stage.join("logs.txt"), vec![b'z'; 4096]).unwrap();
        std::fs::write(root.join(".kind-abc123-uidB.ieb.tmp"), vec![b'y'; 2048]).unwrap();

        let (found, _) = scan(root, &BTreeMap::new()).unwrap();
        let mut uids: Vec<String> = found
            .iter()
            .filter_map(|c| c.abandoned_uid.clone())
            .collect();
        uids.sort();
        assert_eq!(uids, vec!["uidA", "uidB"]);
        let staged = found
            .iter()
            .find(|c| c.abandoned_uid.as_deref() == Some("uidA"))
            .unwrap();
        assert!(
            staged.bytes >= 4096,
            "a staging directory is sized recursively: {}",
            staged.bytes
        );
    }

    #[test]
    fn a_uid_is_parsed_out_of_both_abandoned_shapes() {
        assert_eq!(
            abandoned_uid(".staging-kind-abc123-uidA").as_deref(),
            Some("uidA")
        );
        assert_eq!(
            abandoned_uid(".kind-abc123-uidB.ieb.tmp").as_deref(),
            Some("uidB")
        );
        assert_eq!(abandoned_uid("kind-abc123.ieb"), None);
        assert_eq!(abandoned_uid("kind-abc123.notified"), None);
    }

    /// A wiped CR population must read as "something happened to the CRs", not as a licence to
    /// delete every bundle. The documented CRD upgrade path does exactly that wipe.
    #[test]
    fn a_wiped_cr_population_stops_the_orphan_pass() {
        assert!(!orphan_storm(0, 1000), "nothing missing");
        assert!(!orphan_storm(3, 1000), "a human tidied a few");
        assert!(orphan_storm(60, 1000), "6% is a wipe, not tidying");
        assert!(
            orphan_storm(101, 100_000),
            "the absolute count also stops it"
        );
        assert!(!orphan_storm(0, 0), "an empty root is not a storm");
    }

    /// A file that cannot be removed is refused, and the caller is the one that must not then
    /// record it as reclaimed. A read-only directory is how a WORM-backed volume behaves.
    #[test]
    fn a_volume_that_refuses_the_unlink_yields_undeletable() {
        let dir = tempfile::tempdir().unwrap();
        let sub = dir.path().join("ro");
        std::fs::create_dir(&sub).unwrap();
        let victim = sub.join("kind-abc123.ieb");
        std::fs::write(&victim, b"x").unwrap();
        let mut perms = std::fs::metadata(&sub).unwrap().permissions();
        std::os::unix::fs::PermissionsExt::set_mode(&mut perms, 0o500);
        std::fs::set_permissions(&sub, perms.clone()).unwrap();

        let c = Candidate {
            path: victim.clone(),
            incident: Some("kind-abc123".into()),
            bytes: 1,
            age: Duration::from_secs(1),
            abandoned_uid: None,
        };
        let got = remove(&c);
        // Restore before asserting, so a failure does not leave an unremovable temp dir behind.
        std::os::unix::fs::PermissionsExt::set_mode(&mut perms, 0o700);
        std::fs::set_permissions(&sub, perms).unwrap();
        assert_eq!(got, Err(Refusal::Undeletable));
        assert!(
            victim.exists(),
            "and the bytes are still there, which is the point"
        );
    }

    /// Already gone is success: another replica, or a human, got there first.
    #[test]
    fn a_file_that_is_already_gone_is_not_an_error() {
        let c = Candidate {
            path: PathBuf::from("/definitely/not/here/x.ieb"),
            incident: Some("x".into()),
            bytes: 0,
            age: Duration::from_secs(1),
            abandoned_uid: None,
        };
        assert_eq!(remove(&c), Ok(()));
    }

    #[test]
    fn the_journal_appends_and_rotates() {
        let dir = tempfile::tempdir().unwrap();
        for i in 0..3 {
            journal_append(
                dir.path(),
                &serde_json::json!({ "incident": format!("i{i}"), "reason": "age" }),
            )
            .unwrap();
        }
        let body = std::fs::read_to_string(dir.path().join(JOURNAL)).unwrap();
        assert_eq!(body.lines().count(), 3, "{body}");
        assert!(body.lines().all(|l| l.starts_with('{') && l.ends_with('}')));

        // Rotation keeps the previous generation rather than truncating the record.
        std::fs::write(
            dir.path().join(JOURNAL),
            vec![b'x'; JOURNAL_MAX_BYTES as usize],
        )
        .unwrap();
        journal_append(dir.path(), &serde_json::json!({ "incident": "i9" })).unwrap();
        assert!(dir.path().join(format!("{JOURNAL}.1")).exists());
        assert_eq!(
            std::fs::read_to_string(dir.path().join(JOURNAL))
                .unwrap()
                .lines()
                .count(),
            1
        );
    }
}
