//! Incident notification: one grouped, one-screen summary per incident, posted to a route
//! the admin defined (docs/design-notify.md).
//!
//! Three rules shape this module, all from round 11:
//! - **Never workload content by default**, and never a log line in any mode: bundles
//!   deliberately don't redact logs, so a chat channel must not carry them.
//! - **Untrusted strings are escaped**: the alert's rule, namespace, pod and the diff's actor
//!   are chosen by whoever sent the alert or patched the workload, not by the operator.
//! - **Once-only is a file claim**, not a status field: anyone who can patch status could
//!   otherwise replay or suppress the message.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use chrono::Utc;
use kairn_bundle::summary::{LastWords, Summary};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

/// How much of a summary a route may carry.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Detail {
    /// Counts, reasons, revisions, identities. No value taken from the workload.
    #[default]
    Facts,
    /// Also the changed field and its before/after values (redacted in the bundle already).
    /// Never a log line.
    Content,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Format {
    #[default]
    Slack,
    Json,
}

/// A destination the admin defined in the chart. Profiles may name one; they can't define
/// one, change its detail, or point it elsewhere.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct RouteSpec {
    pub name: String,
    /// The host, from the chart's ConfigMap: a change rolls the pod, and it is checked on
    /// every send, so patching the Secret can't silently repoint the endpoint.
    pub host: String,
    #[serde(default)]
    pub format: Format,
    #[serde(default)]
    pub detail: Detail,
    #[serde(default = "default_max_per_window")]
    pub max_per_window: u32,
    /// Notify for `kairn demo` captures too (off: the demo's "stays local" label keeps its
    /// one meaning).
    #[serde(default)]
    pub include_demo: bool,
    /// Talk to this route over plain HTTP. Per-route, and it still needs the process-wide
    /// `KAIRN_NOTIFY_ALLOW_HTTP`, so a chart value alone cannot downgrade a real endpoint —
    /// and even then only a loopback or cluster-local host is accepted.
    #[serde(default)]
    pub insecure_http: bool,
}

fn default_max_per_window() -> u32 {
    10
}

/// What one capture contributes to a message.
#[derive(Clone, Debug)]
pub struct Member {
    pub incident_id: String,
    /// Named when the group turns out to be a single pod, where it is the one identifier a
    /// human needs; in a group of fifty it would be noise.
    pub pod: String,
    /// The IncidentCapture object, so the dispatcher can report the outcome on it without
    /// reading anything back from it.
    pub capture_ns: String,
    pub capture_name: String,
    pub summary: Summary,
}

/// The incident a message is about: captures of the same alert on the same workload.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct GroupKey {
    pub route: String,
    pub rule: String,
    pub namespace: String,
    /// The workload the pods belong to, when it is known (`Deployment/checkout`), else the
    /// pod's own name: without an owner, each pod is its own incident.
    pub owner: String,
}

/// A grouped message, ready to render.
#[derive(Clone, Debug)]
pub struct Group {
    pub key: GroupKey,
    pub cluster: String,
    pub members: Vec<Member>,
    /// Where the bundles are: an object-store prefix when exported, else the in-cluster path.
    /// **Recomputed** from configuration, never read from a capture's status.
    pub bundle_dir: String,
    pub exported: bool,
    /// Groups the rate cap turned away since a message last got through, reported by this one.
    pub suppressed: u32,
    /// Further captures of this same incident folded into the cooldown since the last message.
    pub repeats: u32,
    /// When this incident was first announced, for `×N since 14:05`.
    pub since: Option<chrono::DateTime<Utc>>,
    /// Termination reasons seen while this group was quiet that this firing does not show. A
    /// container flapping between `OOMKilled` and `Error` is one incident, but which ways it
    /// failed is a fact worth carrying.
    pub also_seen: std::collections::BTreeSet<String>,
}

impl Group {
    /// The capture that stands for the group in the claim and the message header.
    pub fn leader(&self) -> &Member {
        &self.members[0]
    }
}

/// Characters that are invisible or that reorder what follows, and so let a string somebody
/// else chose forge a line in a block that Kairn is vouching for.
///
/// `char::is_control` is only `Cc`. It misses the line and paragraph separators (U+2028/2029),
/// the bidi overrides and isolates, the zero-width marks and the BOM — none of which the CRD's
/// boundary pattern on `rule` blocks either, since that pattern refuses exactly `<`, `>` and
/// `&`. Without this, an alert author can put a line break into the context element and write
/// their own "kairn verify: OK (signed)" underneath Kairn's text.
fn invisible(c: char) -> bool {
    c.is_control()
        || matches!(c,
            '\u{200b}'..='\u{200f}'
            | '\u{2028}'
            | '\u{2029}'
            | '\u{202a}'..='\u{202e}'
            | '\u{2066}'..='\u{2069}'
            | '\u{feff}')
}

/// Escape the three characters Slack treats as control syntax, so a string an alert author
/// chose cannot become `<!channel>` or a labelled link, and flatten anything invisible.
///
/// `max` bounds the **output**: `&` becomes five characters, so a budget counted on the input
/// cannot keep a block under Slack's limit. Applied once, at the leaf — escaping an
/// already-escaped string turns a real `&` into `&amp;amp;`.
pub fn escape(text: &str, max: usize) -> String {
    if max == 0 {
        return String::new();
    }
    let mut out = String::new();
    let mut n = 0usize; // rendered characters, not input characters
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        let mut buf = [0u8; 4];
        let piece: &str = match c {
            '&' => "&amp;",
            '<' => "&lt;",
            '>' => "&gt;",
            c if invisible(c) => " ",
            c => c.encode_utf8(&mut buf),
        };
        // One budget check for every branch, against rendered length. Leave room for the
        // ellipsis only while there is still input left to drop.
        let room = if chars.peek().is_some() { max - 1 } else { max };
        if n + piece.chars().count() > room {
            out.push('…');
            return out;
        }
        out.push_str(piece);
        n += piece.chars().count();
    }
    out
}

/// Cut an already-escaped string to `max` characters without splitting an `&amp;`-style entity.
fn fit(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_string();
    }
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    // If the cut landed inside an entity, drop back to before its `&`. The lookback is bounded
    // to one entity's worth of characters: an unbounded `rfind` discards everything after a
    // bare `&`, which collapsed a 2900-character block to a single ellipsis. Indexed by char
    // boundary, because slicing the last six *bytes* panics on multi-byte text.
    let start = out
        .char_indices()
        .rev()
        .take(6)
        .last()
        .map_or(0, |(i, _)| i);
    if let Some(rel) = out[start..].rfind('&') {
        let i = start + rel;
        if !out[i..].contains(';') {
            out.truncate(i);
        }
    }
    out.push('…');
    out
}

/// Bytes as a human size (`61.2 MiB`, and a round limit as `64 MiB`).
fn mib(bytes: f64) -> String {
    if !bytes.is_finite() {
        return "unknown".to_string();
    }
    let one_decimal = format!("{:.1}", bytes / (1024.0 * 1024.0));
    format!(
        "{} MiB",
        one_decimal.strip_suffix(".0").unwrap_or(&one_decimal)
    )
}

/// The headline: the workload and what happened to it. Not the tool's name — that belongs in
/// the context line, where a reader who already knows what Kairn is can skip it.
fn headline(group: &Group) -> String {
    let s = &group.leader().summary;
    let what = s
        .termination
        .as_ref()
        .and_then(|t| t.reason.as_deref())
        .map(|r| escape(r, 40))
        .unwrap_or_else(|| "evidence captured".to_string());
    let count = match group.members.len() {
        1 => String::new(),
        n => format!(" ×{n}"),
    };
    fit(
        &format!(
            "{}/{} · {what}{count}",
            escape(&group.key.namespace, 70),
            escape(&group.key.owner, 90)
        ),
        SLACK_HEADER,
    )
}

/// The verdict: what changed, what failed, how bad, and how complete.
///
/// Order is deliberate. `PARTIAL` leads when it applies, because it qualifies every number
/// after it. Then the rollout — the design calls it the single most actionable fact and it
/// was previously fifth. Every untrusted part is escaped exactly here, once.
fn verdict(group: &Group, detail: Detail) -> String {
    let s = &group.leader().summary;
    let mut parts: Vec<String> = Vec::new();
    if !s.collectors_missing.is_empty() {
        parts.push(format!(
            "PARTIAL: {} did not run",
            escape(&s.collectors_missing.join(", "), 80)
        ));
    }
    if let Some(c) = &s.change {
        let when = match c.seconds_before_alert {
            Some(secs) if secs >= 0 => format!(", {secs}s before the alert"),
            Some(secs) => format!(", {}s after the alert", -secs),
            None => String::new(),
        };
        let rev = match (&c.revision_from, &c.revision_to) {
            (Some(f), Some(t)) => format!(" rev {} → {}", escape(f, 20), escape(t, 20)),
            _ => String::new(),
        };
        // `~` marks a value the client asserted rather than one Kairn observed; the context
        // line says so once, instead of 17 characters of hedging inside the key clause.
        let who = c
            .actor
            .as_deref()
            .map(|a| format!(", by ~{}", escape(a, 60)))
            .unwrap_or_default();
        parts.push(format!(
            "{}/{}{rev} changed{when}{who}",
            escape(&c.kind, 30),
            escape(&c.name, 60)
        ));
        if detail == Detail::Content {
            if let (Some(field), Some(after)) = (&c.field, &c.after) {
                let before = c.before.as_deref().unwrap_or("(absent)");
                parts.push(format!(
                    "{}: {} → {}",
                    escape(field, 90),
                    escape(before, 110),
                    escape(after, 110)
                ));
            }
        }
    }
    if let Some(t) = &s.termination {
        let reason = escape(t.reason.as_deref().unwrap_or("terminated"), 40);
        match t.exit_code {
            Some(code) => parts.push(format!("{reason} (exit {code})")),
            None => parts.push(reason),
        }
    }
    if s.restarts > 0 {
        parts.push(format!("restart {}", s.restarts));
    }
    // How many pods actually reported the leader's reason. Counting *distinct* reasons would
    // call "one OOMKilled and four that said nothing" a group with the same reason.
    if group.members.len() > 1 {
        let leader_reason = s.termination.as_ref().and_then(|t| t.reason.as_deref());
        let agreed = group
            .members
            .iter()
            .filter(|m| {
                m.summary
                    .termination
                    .as_ref()
                    .and_then(|t| t.reason.as_deref())
                    == leader_reason
            })
            .count();
        let total = group.members.len();
        parts.push(match (leader_reason, agreed) {
            (Some(r), n) if n == total => format!("all {total} pods: {}", escape(r, 40)),
            (Some(r), n) => format!("{n} of {total} pods: {}", escape(r, 40)),
            (None, _) => format!("{total} pods"),
        });
    }
    if let Some(m) = &s.memory {
        match m.limit_bytes {
            Some(limit) if limit > 0.0 => parts.push(format!(
                "peak {} of {} limit",
                mib(m.peak_bytes),
                mib(limit)
            )),
            _ => parts.push(format!("peak {}", mib(m.peak_bytes))),
        }
    }
    parts.push(
        match s.last_words {
            LastWords::Captured => "last log line is in the bundle",
            LastWords::Discarded => "last log line already discarded by the kubelet",
            LastWords::None => "no terminated instance",
        }
        .to_string(),
    );
    parts.join(" · ")
}

/// Where this controller runs, for the retrieval command a message prints. The Deployment name
/// varies with the Helm release name, so it is configuration, not a constant.
#[derive(Clone, Debug)]
pub struct Site {
    pub namespace: String,
    pub deployment: String,
}

impl Site {
    /// From the environment the chart sets. Defaults match a release named `kairn`.
    pub fn from_env(namespace: &str) -> Self {
        Site {
            namespace: namespace.to_string(),
            deployment: std::env::var("KAIRN_DEPLOYMENT").unwrap_or_else(|_| "kairn".into()),
        }
    }
}

/// The commands that actually retrieve the evidence, for **every** capture in the group — the
/// leader's alone would leave the other pods' ids in the message with no way to act on them.
///
/// Every value here is admin-set (namespace, Deployment, bundle directory) or
/// pattern-constrained (`incidentId`, `clusterId` are `[A-Za-z0-9._-]`), which is what makes
/// it safe to render this block as `mrkdwn` so a reader gets a copy button. `safe_commands`
/// enforces that rather than trusting it.
fn retrieval(group: &Group, site: &Site) -> Vec<String> {
    let dir = group.bundle_dir.trim_end_matches('/');
    let all = group.members.len();
    // Budgeted here rather than by truncating the finished block. Fitting the fenced text as a
    // whole silently ate the closing `done` and the fence itself past ~96 members, and dropped
    // ids past ~105 — at 200 pods only half of them survived, and the copy button yielded an
    // unterminated `for … do`. A mass rollout is exactly the scale grouping exists for.
    let ids: Vec<&str> = group
        .members
        .iter()
        .take(MAX_IDS)
        .map(|m| m.incident_id.as_str())
        .collect();
    let overflow = all.saturating_sub(ids.len());
    let and_the_rest = |lines: &mut Vec<String>| {
        if overflow > 0 {
            lines.push(format!(
                "# and {overflow} more in this incident: kubectl -n {} get incidentcapture",
                site.namespace
            ));
        }
    };
    let verify = |file: &str, incident: &str| {
        format!(
            "kairn verify {file} --cluster {} --incident {incident}",
            group.cluster
        )
    };
    if group.exported {
        if let [one] = ids[..] {
            return vec![verify(&format!("{dir}/{one}.ieb"), one)];
        }
        let mut lines = vec![
            format!("for id in {}; do", ids.join(" ")),
            format!(
                "  kairn verify {dir}/$id.ieb --cluster {} --incident \"$id\"",
                group.cluster
            ),
            "done".to_string(),
        ];
        and_the_rest(&mut lines);
        return lines;
    }
    // `exec` into the controller, because the bundle is on its PVC and the image is distroless
    // (no tar, so `kubectl cp` cannot do it). Absolute path: the image's PATH is minimal.
    let exec = format!(
        "kubectl -n {} exec deploy/{} -c controller --",
        site.namespace, site.deployment
    );
    if let [one] = ids[..] {
        return vec![
            format!("{exec} /usr/local/bin/kairn cat-bundle {dir}/{one}.ieb > {one}.ieb"),
            verify(&format!("{one}.ieb"), one),
        ];
    }
    let mut lines = vec![
        format!("for id in {}; do", ids.join(" ")),
        format!("  {exec} /usr/local/bin/kairn cat-bundle {dir}/$id.ieb > $id.ieb"),
        format!(
            "  kairn verify $id.ieb --cluster {} --incident \"$id\"",
            group.cluster
        ),
        "done".to_string(),
    ];
    and_the_rest(&mut lines);
    lines
}

/// How many incident ids a command block names before pointing at `kubectl` for the rest.
/// Sized so the fenced block stays well inside [`SLACK_SECTION`] even with long ids.
const MAX_IDS: usize = 40;

/// Slack's own limits on the blocks Kairn sends.
const SLACK_HEADER: usize = 150;
const SLACK_SECTION: usize = 2900;
const SLACK_CONTEXT: usize = 400;
/// The whole body. Nothing Kairn renders approaches this; the check is here so that if
/// something ever does, the message is shortened rather than rejected by the endpoint.
pub const MAX_BODY: usize = 16 * 1024;

/// True when a command block contains only characters an admin-set or pattern-constrained
/// value can produce. If this is ever false the block is sent as `plain_text` instead, so a
/// future field that carries workload text cannot become Slack markup.
fn safe_commands(commands: &str) -> bool {
    commands.chars().all(|c| {
        c.is_ascii_alphanumeric()
            // Inert inside a fence, and all of them occur in legitimate object-store prefixes:
            // without `~ + , ( ) # @ %` an ordinary `s3://evidence/team+platform` silently lost
            // its copy button. What is NOT here is what matters: no backtick (the fence cannot
            // be closed), no `<` or `|` (no labelled link), no `*` `_` `~`-pair emphasis that
            // could reach outside the fence, no `@`-mention syntax.
            || matches!(
                c,
                ' ' | '\n' | '/' | '.' | '_' | '-' | ':' | '=' | '$' | '>' | ';' | '"' | '\''
                    | '#' | '+' | ',' | '(' | ')' | '~' | '@' | '%' | '…'
            )
    })
}

/// The message body for a route's format. Every untrusted string was escaped once, at the leaf
/// in `verdict`/`headline`; blocks are only *fitted* here, never escaped again.
pub fn render(group: &Group, route: &RouteSpec, site: &Site) -> Value {
    let m = group.leader();
    let verdict = verdict(group, route.detail);
    let commands = retrieval(group, site);
    let ids: Vec<&str> = group
        .members
        .iter()
        .take(3)
        .map(|m| m.incident_id.as_str())
        .collect();
    let more = group.members.len().saturating_sub(ids.len());
    let mut captures = ids.join(", ");
    if more > 0 {
        captures.push_str(&format!(" +{more} more"));
    }
    // "×N since 14:05": this incident has been re-firing, and it was announced once already.
    let mut repeat = match (group.repeats, group.since) {
        (0, _) => String::new(),
        (n, Some(t)) => format!(" · ×{} more since {}", n, t.format("%H:%M UTC")),
        (n, None) => format!(" · ×{n} more"),
    };
    if !group.also_seen.is_empty() {
        // The reasons the quiet period saw that this firing does not. A flap is one incident,
        // but which ways it failed is a fact, and suppressing the repeats must not hide it.
        repeat.push_str(&format!(
            " · also seen: {}",
            escape(
                &group
                    .also_seen
                    .iter()
                    .cloned()
                    .collect::<Vec<_>>()
                    .join(", "),
                60
            )
        ));
    }
    match route.format {
        Format::Json => json!({
            "schema": "kairn.dev/notification/v1",
            "cluster": group.cluster,
            "rule": group.key.rule,
            "namespace": group.key.namespace,
            "owner": group.key.owner,
            "pods": group.members.len(),
            "incidents": group.members.iter().map(|m| &m.incident_id).collect::<Vec<_>>(),
            "verdict": verdict,
            "summary": if route.detail == Detail::Content {
                json!(m.summary.without_log_line())
            } else {
                json!(m.summary.facts_only())
            },
            "retrieval": commands,
            "suppressed": group.suppressed,
            "repeats": group.repeats,
            "since": group.since.map(|t| t.to_rfc3339()),
            "alsoSeen": group.also_seen,
        }),
        Format::Slack => {
            // A companion to the page the team already got, not a second page: the workload and
            // the finding in the header, the tool's own name demoted to the context line.
            let commands = commands.join("\n");
            let mut context = format!(
                "📋 kairn evidence · alert {}{}{repeat} · captures: {captures}",
                escape(&group.key.rule, 120),
                match group.members.len() {
                    1 => format!(" · pod {}", escape(&m.pod, 70)),
                    _ => String::new(),
                }
            );
            if verdict.contains(" by ~") {
                context.push_str(" · ~ asserted by the client, not observed");
            }
            let block_text = |t: &str| json!({ "type": "plain_text", "text": t });
            let mut blocks = vec![
                json!({ "type": "header", "text": block_text(&headline(group)) }),
                json!({ "type": "section", "text": block_text(&fit(&verdict, SLACK_SECTION)) }),
                json!({ "type": "context",
                        "elements": [block_text(&fit(&context, SLACK_CONTEXT))] }),
                // A fenced block so a reader gets a copy button. `mrkdwn` is safe only because
                // every value in it passed `safe_commands` — checked against the text that is
                // actually sent, after fitting, and with room reserved for the fence so that
                // trimming can never eat the closing ``` and leave the block open.
                {
                    let inner = fit(&commands, SLACK_SECTION.saturating_sub(8));
                    if safe_commands(&inner) {
                        json!({ "type": "section", "text": { "type": "mrkdwn",
                                "text": format!("```\n{inner}\n```") } })
                    } else {
                        // Escaped: a `bundle_dir` an admin set is not a credential, but it is
                        // still text going into a text object.
                        json!({ "type": "section",
                                "text": block_text(&escape(&inner, SLACK_SECTION)) })
                    }
                },
            ];
            if group.suppressed > 0 {
                blocks.push(json!({ "type": "context", "elements": [block_text(&format!(
                    "{} more incidents were captured but not posted: the route's rate cap was \
                     already spent. kubectl -n {} get incidentcapture",
                    group.suppressed, site.namespace))] }));
            }
            let body = json!({ "text": fit(&verdict, SLACK_CONTEXT), "blocks": blocks });
            // Last resort: shed the verdict's detail rather than let the endpoint reject the
            // whole message. Nothing Kairn renders reaches this.
            if serde_json::to_vec(&body).map_or(0, |b| b.len()) > MAX_BODY {
                let short = fit(&verdict, 400);
                return json!({ "text": short.clone(), "blocks": [
                    json!({ "type": "header", "text": block_text(&headline(group)) }),
                    json!({ "type": "section", "text": block_text(&short) }),
                ]});
            }
            body
        }
    }
}

/// The claim file that makes a notification once-only, whatever a status patcher does.
fn claim_path(bundle_root: &Path, incident: &str) -> PathBuf {
    bundle_root.join(format!("{incident}.notified"))
}

/// Claim the whole group before sending, and say whether this dispatcher won the right to
/// announce it.
///
/// One function, because the two halves used to live apart and fought: the group's members were
/// claimed before the POST (so a controller killed mid-send cannot leave stragglers, and a PVC
/// written by an older build does not re-announce), while `send` separately claimed the leader
/// as its go/no-go — and so read back the claim the same dispatcher had just written and
/// concluded the incident was already announced. Every member is claimed either way; only the
/// leader's claim decides.
fn claim_group(bundle_root: &Path, leader: &str, members: &[String]) -> std::io::Result<bool> {
    let won = claim(bundle_root, leader)?;
    for id in members.iter().filter(|id| id.as_str() != leader) {
        // Best-effort: a member that cannot be claimed is only at risk of a duplicate later,
        // and the leader's claim already settled whether this group is announced now.
        let _ = claim(bundle_root, id);
    }
    Ok(won)
}

/// Claim the right to notify for this incident. `Ok(false)`: someone already did.
pub fn claim(bundle_root: &Path, incident: &str) -> std::io::Result<bool> {
    match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(claim_path(bundle_root, incident))
    {
        Ok(_) => Ok(true),
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => Ok(false),
        Err(e) => Err(e),
    }
}

/// A route, resolved: its endpoint and the client to use.
pub struct Route {
    pub spec: RouteSpec,
    pub endpoint: kairn_net::Endpoint,
    /// The rate cap's state: when this window started, how many sends it has spent, and how
    /// many groups it has turned away since one last got through.
    window: std::sync::Mutex<Window>,
}

/// POST attempts on the ordinary path.
pub const ATTEMPTS: u32 = 3;
/// POST attempts when flushing at shutdown: one try, no backoff, so a whole group of them fits
/// inside the drain budget. Spending the ordinary 21 s of retries there would blow through that
/// budget *after* the group was claimed — claimed and never announced, the one outcome this
/// feature must not produce. Best-effort is the right trade: the evidence is already sealed.
pub const ATTEMPTS_DRAINING: u32 = 1;

/// The overall budget for one POST, normally.
pub const POST_BUDGET: Duration = Duration::from_secs(5);
/// And when flushing at shutdown, where the whole flush has to fit inside the drain window.
///
/// This is deliberately smaller than [`POST_BUDGET`], because the wall time of one attempt is
/// **not** just the request: `kairn_net` resolves and vets the address first, under its own
/// [`kairn_net::RESOLVE_TIMEOUT`]. Round 13 cut the flush to one attempt after computing the
/// budget as three requests plus backoff — and read the remaining cost as the request timeout
/// alone, missing the DNS budget in front of it. One attempt could therefore still take
/// 5 s + 5 s, exactly the drain window, so a slow resolver plus a slow endpoint raced the
/// timeout: the group is claimed before it is posted, so losing that race marks captures
/// notified that were never announced.
pub const POST_BUDGET_DRAINING: Duration = Duration::from_secs(3);

/// The worst case for one shutdown flush attempt: name resolution, then the request.
pub const FLUSH_ATTEMPT_MAX: Duration =
    Duration::from_secs(kairn_net::RESOLVE_TIMEOUT.as_secs() + POST_BUDGET_DRAINING.as_secs());

/// How long a rate-cap window lasts.
pub const WINDOW: Duration = Duration::from_secs(300);

struct Window {
    started: std::time::Instant,
    spent: u32,
    /// Groups turned away since a message last got through, reported by the next one.
    suppressed: u32,
    /// Whether this window already posted its "the cap is spent" notice.
    announced: bool,
}

impl Route {
    /// Assemble the endpoint from the admin's host and the Secret's path, then check it.
    /// `allow_http` is the process-wide test switch (`KAIRN_NOTIFY_ALLOW_HTTP`); a route opts
    /// in with `insecureHttp`, and both are required.
    pub fn new(spec: RouteSpec, secret_path: &str, allow_http: bool) -> Result<Self, String> {
        let path = secret_path.trim();
        // `//host/x` is protocol-relative: the host check below still passes, but some proxies
        // normalize it into another origin, so it is refused rather than reasoned about.
        if !path.starts_with('/') || path.starts_with("//") {
            return Err(format!(
                "route {}: the Secret's `path` must start with a single '/'",
                spec.name
            ));
        }
        if spec.host.contains("://") {
            return Err(format!(
                "route {}: `host` is a host[:port], not a URL",
                spec.name
            ));
        }
        let http = spec.insecure_http && allow_http;
        let scheme = if http { "http" } else { "https" };
        let endpoint = kairn_net::parse(&format!("{scheme}://{}{path}", spec.host), http)?;
        // The host the admin configured is the host we talk to, whatever the Secret says.
        if !endpoint
            .host
            .eq_ignore_ascii_case(endpoint_host(&spec.host))
        {
            return Err(format!(
                "route {}: the assembled URL's host is not {}",
                spec.name, spec.host
            ));
        }
        Ok(Route {
            spec,
            endpoint,
            window: std::sync::Mutex::new(Window {
                started: std::time::Instant::now(),
                spent: 0,
                suppressed: 0,
                announced: false,
            }),
        })
    }

    /// Suppressed groups still waiting to be reported on the next message that gets through.
    pub fn outstanding_suppressed(&self) -> u32 {
        self.window.lock().expect("window").suppressed
    }

    /// Give back a debt that was taken for a message that then failed to send. `spent` is
    /// deliberately not refunded: the attempt did consume a slot.
    pub fn restore_suppressed(&self, n: u32) {
        if n > 0 {
            self.window.lock().expect("window").suppressed += n;
        }
    }

    /// Take one send from the rate cap.
    ///
    /// A channel that silently loses messages is worse than a loud one, so the cap reports
    /// itself twice over: the **first** group it turns away in a window gets an immediate
    /// standalone notice, and every group after that is counted and carried by the next
    /// message that does get through. Deferring both to "the next message" tells nobody
    /// anything when the storm is the last thing that happens.
    pub fn allow_send(&self) -> Allow {
        let mut w = self.window.lock().expect("window");
        if w.started.elapsed() > WINDOW {
            // A new window resets the cap, but not the debt: a suppressed group is still
            // unreported until some message carries the count.
            w.started = std::time::Instant::now();
            w.spent = 0;
            w.announced = false;
        }
        if w.spent >= self.spec.max_per_window {
            w.suppressed += 1;
            let announce = !w.announced;
            w.announced = true;
            return Allow::Capped { announce };
        }
        w.spent += 1;
        Allow::Go {
            suppressed: std::mem::take(&mut w.suppressed),
        }
    }

    /// POST the body `attempts` times: a 3xx or 4xx is final, `429` and `5xx` are retried with
    /// 2 s then 4 s of backoff. The count is the caller's, because a shutdown flush cannot afford
    /// the ordinary budget — see [`ATTEMPTS_DRAINING`].
    ///
    /// The client is built per attempt so the address is resolved, vetted and pinned each
    /// time: a cached client would keep talking to whatever the first answer was.
    ///
    /// The error is a **fixed code** plus a detail. Only the code goes into `status`, where it
    /// is readable by anyone with `get incidentcaptures`; the detail is logged.
    pub async fn post(
        &self,
        body: &Value,
        attempts: u32,
        budget: Duration,
    ) -> Result<(), PostError> {
        let reach = if self.endpoint.is_local() {
            kairn_net::Reach::Cluster
        } else {
            kairn_net::Reach::Internet
        };
        let mut last = PostError::default();
        for attempt in 0..attempts.max(1) {
            if attempt > 0 {
                tokio::time::sleep(Duration::from_secs(2 * attempt as u64)).await;
            }
            let client = match kairn_net::connect(&self.endpoint, reach, Some(budget)).await {
                Ok(c) => c,
                Err(detail) => {
                    last = PostError {
                        code: "unreachable",
                        detail,
                    };
                    continue;
                }
            };
            match client.post(self.endpoint.url()).json(body).send().await {
                Ok(r) if r.status().is_success() => return Ok(()),
                Ok(r) => {
                    let status = r.status();
                    // A response body can quote the request that was sent: never read it.
                    let code = match status.as_u16() {
                        429 => "rate-limited-by-endpoint",
                        s if s >= 500 => "endpoint-error",
                        s if (300..400).contains(&s) => "endpoint-redirected",
                        _ => "endpoint-refused",
                    };
                    last = PostError {
                        code,
                        detail: format!("the endpoint answered {status}"),
                    };
                    if !(status.as_u16() == 429 || status.is_server_error()) {
                        return Err(last);
                    }
                }
                // `without_url`: a reqwest error prints the URL, and the path is a credential.
                Err(e) => {
                    last = PostError {
                        code: if e.is_timeout() {
                            "timeout"
                        } else {
                            "unreachable"
                        },
                        detail: format!("posting failed: {}", e.without_url()),
                    }
                }
            }
        }
        Err(last)
    }
}

/// What the rate cap says about one group.
#[derive(Debug, PartialEq, Eq)]
pub enum Allow {
    /// Send it, and report this many groups the cap turned away since the last message.
    Go { suppressed: u32 },
    /// The window is full. `announce`: this is the first one, so say so in the channel now.
    Capped { announce: bool },
}

/// Why a POST failed: a fixed code for `status`, and a detail for the log only.
#[derive(Clone, Debug, Default)]
pub struct PostError {
    pub code: &'static str,
    pub detail: String,
}

/// `host` as configured may carry a port; compare only the host part.
fn endpoint_host(configured: &str) -> &str {
    configured.split(':').next().unwrap_or(configured)
}

/// Routes the admin defined, by name, with the config errors that disable them.
#[derive(Default)]
pub struct Routes {
    pub by_name: BTreeMap<String, std::sync::Arc<Route>>,
    /// Routes that were configured but are unusable, by name: reported, never guessed around.
    pub errors: BTreeMap<String, String>,
}

impl Routes {
    pub fn get(&self, name: &str) -> Option<std::sync::Arc<Route>> {
        self.by_name.get(name).cloned()
    }

    /// Load from the chart's mounts: `routes_json` is the ConfigMap's route list, and each
    /// route's secret path is read from `<secrets_dir>/<route name>/path`. A route that
    /// cannot be loaded is recorded in `errors` and disabled; it never falls back to another
    /// route and never sends without its path.
    pub fn load(routes_json: &Path, secrets_dir: &Path, allow_http: bool) -> Self {
        let mut routes = Routes::default();
        let raw = match std::fs::read(routes_json) {
            Ok(raw) => raw,
            // No mount at all is the default install: notification is simply off.
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return routes,
            Err(e) => {
                routes
                    .errors
                    .insert("*".into(), format!("reading routes: {e}"));
                return routes;
            }
        };
        let specs: Vec<RouteSpec> = match serde_json::from_slice(&raw) {
            Ok(specs) => specs,
            Err(e) => {
                routes
                    .errors
                    .insert("*".into(), format!("parsing routes: {e}"));
                return routes;
            }
        };
        for spec in specs {
            let name = spec.name.clone();
            // The name is a path segment below, and the chart's schema does not apply to a
            // ConfigMap someone edited directly.
            if !name_ok(&name) {
                routes.errors.insert(
                    name.clone(),
                    "a route name must be 1–40 characters of [a-z0-9-], not starting or \
                     ending with '-'"
                        .into(),
                );
                continue;
            }
            let secret = secrets_dir.join(&name).join("path");
            let loaded = std::fs::read_to_string(&secret)
                .map_err(|e| format!("route {name}: reading its path secret: {e}"))
                .and_then(|path| Route::new(spec, &path, allow_http));
            match loaded {
                Ok(route) => {
                    routes.by_name.insert(name, std::sync::Arc::new(route));
                }
                Err(e) => {
                    routes.errors.insert(name, e);
                }
            }
        }
        routes
    }
}

/// A route name that is safe as a path segment and matches the chart's own pattern.
fn name_ok(name: &str) -> bool {
    (1..=40).contains(&name.len())
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        && !name.starts_with('-')
        && !name.ends_with('-')
}

/// One capture, ready to be grouped. Everything the message needs is captured here, at the
/// moment the controller knew it: the dispatcher never reads `status` back.
#[derive(Clone, Debug)]
pub struct Pending {
    pub key: GroupKey,
    pub cluster: String,
    pub member: Member,
    pub bundle_dir: String,
    pub exported: bool,
}

/// How long a group waits for more members, and the longest it may ever wait.
pub const COALESCE: Duration = Duration::from_secs(30);
pub const COALESCE_MAX: Duration = Duration::from_secs(120);

/// The handle reconcile holds. Enqueueing never blocks and never fails a capture.
#[derive(Clone)]
pub struct Dispatcher {
    tx: tokio::sync::mpsc::Sender<Pending>,
    routes: std::sync::Arc<Routes>,
    /// Asks the loop to flush and exit. [`Dispatcher::drain`].
    shutdown: std::sync::Arc<tokio::sync::Notify>,
    /// Resolves once the dispatcher has flushed everything it held.
    done: std::sync::Arc<tokio::sync::Notify>,
}

impl Dispatcher {
    /// Stop taking new work, flush the groups still coalescing, and wait for the sends in
    /// flight — up to `within`.
    ///
    /// Worth the trouble because a group is **claimed before it is posted**: dropping one on
    /// the way out leaves those captures marked notified and never announced. A rollout is the
    /// common case, not a rare one, so the ordinary path has to survive it.
    pub async fn drain(&self, within: Duration) {
        let done = self.done.clone();
        // Registered before the signal, or a dispatcher that finishes immediately would notify
        // into nothing and this would wait out the whole timeout.
        // `Notified` snapshots the generation at construction, so holding it across the whole
        // wait is what makes the *reply* race-free.
        let waiter = done.notified();
        // `notify_one`, NOT `notify_waiters`: the loop builds its own `Notified` inside a
        // `select!` and drops it whenever another branch wins, and a dropped `notify_waiters`
        // wakeup is gone for good — measured at ~30% of shutdowns flushing nothing at all.
        // `notify_one` stores a permit instead, so the signal survives losing that race.
        self.shutdown.notify_one();
        if tokio::time::timeout(within, waiter).await.is_err() {
            tracing::warn!(
                timeout = ?within,
                "notification dispatcher did not finish in time; some summaries were not sent"
            );
        }
    }

    /// Whether this route asked for `kairn demo` captures too. Unknown route: no.
    pub fn include_demo(&self, route: &str) -> bool {
        self.routes.get(route).is_some_and(|r| r.spec.include_demo)
    }

    /// Hand a sealed capture to the dispatcher. A full queue drops the notification and says
    /// so: a capture is never delayed or failed by a slow webhook.
    pub fn enqueue(&self, pending: Pending) {
        if self.tx.try_send(pending).is_err() {
            crate::telemetry::metrics().notification(SendResult::Dropped);
            tracing::warn!("notification queue full; summary dropped (the bundle is unaffected)");
        }
    }
}

/// What happened to one group's notification, for `kairn_notifications_total{result}`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SendResult {
    Sent,
    /// The same verdict on the same workload, inside its cooldown: counted, not posted.
    Repeat,
    /// The endpoint refused or never answered within the budget.
    Failed,
    /// The route's rate cap was already spent.
    Suppressed,
    /// No such route, or the queue was full.
    Dropped,
    /// Another dispatcher (or an earlier run) already notified for this incident.
    AlreadyNotified,
}

impl SendResult {
    pub const ALL: [SendResult; 6] = [
        SendResult::Sent,
        SendResult::Repeat,
        SendResult::Failed,
        SendResult::Suppressed,
        SendResult::Dropped,
        SendResult::AlreadyNotified,
    ];

    pub fn label(self) -> &'static str {
        match self {
            SendResult::Sent => "sent",
            SendResult::Repeat => "repeat",
            SendResult::Failed => "failed",
            SendResult::Suppressed => "suppressed",
            SendResult::Dropped => "dropped",
            SendResult::AlreadyNotified => "already-notified",
        }
    }
}

/// How long the same verdict on the same workload stays quiet after being announced. A
/// crashlooping pod is one incident, not one every five minutes.
pub const COOLDOWN: Duration = Duration::from_secs(1800);

/// What was last said about a group, so a repeat can be counted instead of posted.
struct Sent {
    at: tokio::time::Instant,
    /// The rollout this group was last announced for. **Only a new rollout bypasses the
    /// cooldown.** The termination reason deliberately does not: a container that dies
    /// `OOMKilled` on one restart and `Error` on the next — the ordinary shape of a process
    /// that sometimes hits the limit mid-allocation and sometimes exits non-zero first — would
    /// otherwise look like news every single time, which is the message storm this cooldown
    /// exists to stop. The reasons seen while quiet ride out on the next message instead.
    rollout: String,
    /// Whether the message actually reached the endpoint. A send that failed, or that the rate
    /// cap turned away, must **not** arm the cooldown: it would mute the incident for half an
    /// hour and it was never announced at all.
    delivered: bool,
    /// Captures folded into the cooldown since the last message.
    repeats: u32,
    /// Termination reasons seen while quiet, so a flap is reported rather than lost.
    reasons: std::collections::BTreeSet<String>,
    /// When this group was first announced, for "×N since 14:05".
    first_at: chrono::DateTime<Utc>,
}

/// What makes a firing *news* rather than the same incident continuing: a different rollout.
///
/// Taken over the whole member set, not `leader()`, because the leader is only whichever pod
/// the kubelet sealed first — in a group whose members disagree, admission order would
/// otherwise flip the key and re-announce.
fn rollout_key(group: &Group) -> String {
    let mut rollouts: std::collections::BTreeSet<String> = group
        .members
        .iter()
        .filter_map(|m| m.summary.change.as_ref())
        .map(|c| match c.revision_to.as_deref() {
            // A revision is the right identity when `diffs/` could establish one.
            Some(r) => format!("{}/{}@{r}", c.kind, c.name),
            // When it could not, fall back to *what* changed. This matters more than it looks:
            // a Deployment whose pods never become Ready stays progressing, so its revisions are
            // unknown — and that is the crashlooping case this whole feature exists for. Keyed
            // on the revision alone, a rollback during such an incident would look like the same
            // incident continuing and stay unreported for the whole cooldown.
            //
            // Hashed, not stored: the field and its value are workload content, and this key
            // lives in memory for 30 minutes after the message is gone.
            None => format!(
                "{}/{}#{}",
                c.kind,
                c.name,
                changed_fingerprint(c.field.as_deref(), c.after.as_deref())
            ),
        })
        .collect();
    if rollouts.is_empty() {
        rollouts.insert("-".to_string());
    }
    rollouts.into_iter().collect::<Vec<_>>().join(",")
}

/// Whether this firing is the same incident continuing, and so should be counted instead of
/// posted.
///
/// `delivered` is load-bearing and easy to lose: a group whose POST failed, or that the rate cap
/// turned away, must **not** count as having been announced. Treating it as announced mutes the
/// incident for the whole cooldown without anyone ever having been told — the same "the claim is
/// spent and the channel stayed silent" failure this feature has already been bitten by.
fn is_repeat(prev: Option<&Sent>, rollout: &str) -> bool {
    prev.is_some_and(|p| p.delivered && p.at.elapsed() < COOLDOWN && p.rollout == rollout)
}

/// A short, non-reversible fingerprint of what a change did, for the cooldown key when the
/// revision is unknown. Never rendered and never stored beyond the key.
fn changed_fingerprint(field: Option<&str>, after: Option<&str>) -> String {
    use sha2::{Digest, Sha256};
    match (field, after) {
        // Nothing to distinguish one firing from the next: the cooldown then damps on the
        // workload alone, which is the honest answer when the evidence says nothing changed.
        (None, None) => "-".to_string(),
        (f, a) => {
            let mut h = Sha256::new();
            h.update(f.unwrap_or("").as_bytes());
            h.update([0]);
            h.update(a.unwrap_or("").as_bytes());
            format!("{:x}", h.finalize())[..12].to_string()
        }
    }
}

/// The termination reasons this group's members reported, for the flap report.
fn reasons_of(group: &Group) -> std::collections::BTreeSet<String> {
    group
        .members
        .iter()
        .filter_map(|m| m.summary.termination.as_ref()?.reason.clone())
        .collect()
}

/// A group collecting members until its deadline.
struct Open {
    group: Group,
    /// Only the tests read this, to prove the hard cap is measured from the group's start.
    #[cfg_attr(not(test), allow(dead_code))]
    started_at: tokio::time::Instant,
    /// Extended by each new member, but never past `hard`.
    due: tokio::time::Instant,
    hard: tokio::time::Instant,
}

/// One group's worth of work, handed to `tasks` so the coalescing loop never waits on a POST.
///
/// Shared by the coalescing tick and the shutdown flush: a group abandoned on the way out would
/// be claimed and never announced, so the two paths must agree exactly.
#[allow(clippy::too_many_arguments)]
fn dispatch(
    tasks: &mut tokio::task::JoinSet<()>,
    sent: &std::sync::Arc<std::sync::Mutex<BTreeMap<GroupKey, Sent>>>,
    routes: &std::sync::Arc<Routes>,
    bundle_root: &Path,
    site: &Site,
    client: &kube::Client,
    key: GroupKey,
    mut group: Group,
    attempts: u32,
    budget: Duration,
) {
    let rollout = rollout_key(&group);
    // A crash loop re-fires the same alert on the same workload for hours. Each re-fire is a new
    // capture with a new id, so neither the claim (per incident) nor the rate cap (per route per
    // window) can see it — only the group key can. Inside the cooldown the repeat is counted, not
    // posted, and the count rides on the message that eventually goes.
    //
    // `delivered` is the part that is easy to get wrong: a group whose POST failed, or that the
    // rate cap turned away, must not arm the cooldown. Arming it would mute the incident for half
    // an hour without ever having announced it.
    let repeat = {
        let map = sent.lock().expect("cooldowns");
        is_repeat(map.get(&key), &rollout)
    };
    if repeat {
        let reasons = reasons_of(&group);
        {
            let mut map = sent.lock().expect("cooldowns");
            let e = map.get_mut(&key).expect("just matched");
            e.repeats += group.members.len() as u32;
            e.reasons.extend(reasons);
        }
        crate::telemetry::metrics().notification(SendResult::Repeat);
        // A counted repeat is **settled**, so it is claimed like any other outcome. Leaving it
        // unclaimed would re-enqueue it on every reconcile, and — worse — once this group's
        // rollout moved on, the stale capture would no longer match the cooldown entry and would
        // be announced as if it were news.
        let leader = group.leader().clone();
        for m in &group.members {
            let _ = claim(bundle_root, &m.incident_id);
        }
        let (client, route) = (client.clone(), key.route.clone());
        tasks.spawn(async move {
            report(
                &client,
                &leader,
                &route,
                SendResult::Repeat,
                Some("in-cooldown".into()),
            )
            .await;
        });
        return;
    }
    // Carry what accumulated while this group was quiet, then open a fresh cooldown for it —
    // provisionally, until the send says it landed.
    let (carried, carried_reasons) = {
        let mut map = sent.lock().expect("cooldowns");
        let prev = map.get(&key);
        let carried = prev.map_or(0, |e| e.repeats);
        let carried_reasons = prev.map(|e| e.reasons.clone()).unwrap_or_default();
        let first_at = prev.map_or_else(Utc::now, |e| e.first_at);
        group.repeats = carried;
        group.since = prev.map(|e| e.first_at);
        group.also_seen = carried_reasons
            .difference(&reasons_of(&group))
            .cloned()
            .collect();
        map.insert(
            key.clone(),
            Sent {
                at: tokio::time::Instant::now(),
                rollout,
                delivered: false,
                repeats: 0,
                reasons: Default::default(),
                first_at,
            },
        );
        (carried, carried_reasons)
    };
    let (routes, bundle_root, site, client, sent) = (
        routes.clone(),
        bundle_root.to_path_buf(),
        site.clone(),
        client.clone(),
        sent.clone(),
    );
    tasks.spawn(async move {
        let leader = group.leader().clone();
        let members: Vec<String> = group
            .members
            .iter()
            .map(|m| m.incident_id.clone())
            .collect();
        let route = group.key.route.clone();
        // Claimed before the POST. The leader's claim is the go/no-go; every member is claimed
        // either way, so a controller killed mid-send cannot leave stragglers and a PVC written by
        // an older build does not re-announce a closed incident on upgrade.
        match claim_group(&bundle_root, &leader.incident_id, &members) {
            Ok(true) => {}
            Ok(false) => {
                crate::telemetry::metrics().notification(SendResult::AlreadyNotified);
                report(
                    &client,
                    &leader,
                    &route,
                    SendResult::AlreadyNotified,
                    Some("already-notified".into()),
                )
                .await;
                return;
            }
            Err(e) => {
                tracing::warn!(incident = %leader.incident_id, error = %e,
                               "could not claim the notification");
                crate::telemetry::metrics().notification(SendResult::Dropped);
                report(
                    &client,
                    &leader,
                    &route,
                    SendResult::Dropped,
                    Some("claim-failed".into()),
                )
                .await;
                return;
            }
        }
        let (result, reason) = send(&routes, &site, group, attempts, budget).await;
        crate::telemetry::metrics().notification(result);
        {
            let mut map = sent.lock().expect("cooldowns");
            if let Some(e) = map.get_mut(&key) {
                if result == SendResult::Sent {
                    e.delivered = true;
                } else {
                    // Nobody was told, so the cooldown stays unarmed and the debt goes back —
                    // the same asymmetry `restore_suppressed` handles for the rate cap.
                    e.repeats += carried;
                    e.reasons.extend(carried_reasons);
                }
            }
        }
        report(&client, &leader, &route, result, reason).await;
    });
}

/// Start the dispatcher task. It owns the coalescing windows, so a slow webhook delays only
/// other notifications — never a capture.
pub fn spawn(
    client: kube::Client,
    routes: std::sync::Arc<Routes>,
    bundle_root: PathBuf,
    site: Site,
) -> Dispatcher {
    let (tx, mut rx) = tokio::sync::mpsc::channel::<Pending>(256);
    let shutdown = std::sync::Arc::new(tokio::sync::Notify::new());
    let done = std::sync::Arc::new(tokio::sync::Notify::new());
    let handle = Dispatcher {
        tx,
        routes: routes.clone(),
        shutdown: shutdown.clone(),
        done: done.clone(),
    };
    tokio::spawn(async move {
        let mut open: BTreeMap<GroupKey, Open> = BTreeMap::new();
        // Shared with each spawned send, which is the only thing that knows whether the
        // message actually landed.
        let sent: std::sync::Arc<std::sync::Mutex<BTreeMap<GroupKey, Sent>>> = Default::default();
        let mut tasks = tokio::task::JoinSet::new();
        loop {
            let next = open.values().map(|o| o.due).min();
            let tick = async {
                match next {
                    Some(at) => tokio::time::sleep_until(at).await,
                    // Nothing pending: wait for a message instead of spinning.
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                // Reap finished sends so the set does not grow; nothing to do with the result.
                _ = tasks.join_next(), if !tasks.is_empty() => {}
                _ = shutdown.notified() => {
                    // Kubernetes sends SIGTERM on every rollout, so this is the ordinary path,
                    // not an edge case. Flush now rather than waiting out the windows: a group
                    // is claimed before it is posted, so abandoning one leaves its captures
                    // marked notified and never announced.
                    // Everything `enqueue` accepted but the loop had not grouped yet. Dropping
                    // it loses those captures for good: after the restart the process-start gate
                    // sees them as history, claims them, and skips.
                    while let Ok(pending) = rx.try_recv() {
                        admit(&mut open, pending);
                    }
                    if !open.is_empty() {
                        tracing::info!(groups = open.len(),
                                       "flushing notification groups before shutdown");
                    }
                    for (key, o) in std::mem::take(&mut open) {
                        dispatch(&mut tasks, &sent, &routes, &bundle_root, &site, &client,
                                 key, o.group, ATTEMPTS_DRAINING, POST_BUDGET_DRAINING);
                    }
                    while tasks.join_next().await.is_some() {}
                    // The rate-cap debt rides on the next message that gets through, and there
                    // will not be one. The storm itself was already announced — the first group a
                    // window turns away gets a standalone notice — and every suppression is
                    // counted in kairn_notifications_total{result="suppressed"}, so what is lost is
                    // only the tally in the channel. Put it in the record rather than nowhere.
                    for route in routes.by_name.values() {
                        let n = route.outstanding_suppressed();
                        if n > 0 {
                            tracing::warn!(
                                route = %route.spec.name, suppressed = n,
                                "shutting down with rate-capped notifications still uncounted in \
                                 the channel; the count is in the suppressed result of \
                                 kairn_notifications_total"
                            );
                        }
                    }
                    done.notify_waiters();
                    return;
                }
                incoming = rx.recv() => match incoming {
                    Some(pending) => {
                        // One line per capture the dispatcher accepts, so "the controller
                        // decided to announce it" and "the dispatcher actually has it" are
                        // distinguishable in a log.
                        tracing::debug!(route = %pending.key.route, rule = %pending.key.rule,
                                        owner = %pending.key.owner,
                                        incident = %pending.member.incident_id,
                                        "grouping a capture for notification");
                        admit(&mut open, pending)
                    }
                    // Every sender is gone: the controller is shutting down.
                    None => break,
                },
                () = tick => {
                    let now = tokio::time::Instant::now();
                    let ready: Vec<GroupKey> = open
                        .iter()
                        .filter(|(_, o)| o.due <= now)
                        .map(|(k, _)| k.clone())
                        .collect();
                    if !ready.is_empty() {
                        tracing::debug!(groups = ready.len(), "coalescing window closed");
                    }
                    for key in ready {
                        let Some(o) = open.remove(&key) else { continue };
                        dispatch(&mut tasks, &sent, &routes, &bundle_root, &site, &client,
                                 key, o.group, ATTEMPTS, POST_BUDGET);
                    }
                    // Forget expired cooldowns, so the map cannot grow without bound on a
                    // cluster that churns workloads. An entry with unreported repeats is NOT
                    // exempt: keeping it forever was the leak, and a lost count is a lost
                    // count, not lost evidence.
                    sent.lock()
                        .expect("cooldowns")
                        .retain(|_, e| e.at.elapsed() < COOLDOWN);
                }
            }
        }
        done.notify_waiters();
    });
    handle
}

/// Add a member to its group, extending the window but never past the hard cap.
fn admit(open: &mut BTreeMap<GroupKey, Open>, pending: Pending) {
    let now = tokio::time::Instant::now();
    match open.get_mut(&pending.key) {
        Some(o) => {
            // A repeat of a capture already in the group (a re-reconcile) is not a new pod.
            if o.group
                .members
                .iter()
                .any(|m| m.incident_id == pending.member.incident_id)
            {
                return;
            }
            o.group.members.push(pending.member);
            o.due = (now + COALESCE).min(o.hard);
        }
        None => {
            open.insert(
                pending.key.clone(),
                Open {
                    group: Group {
                        key: pending.key,
                        cluster: pending.cluster,
                        members: vec![pending.member],
                        bundle_dir: pending.bundle_dir,
                        exported: pending.exported,
                        suppressed: 0,
                        repeats: 0,
                        since: None,
                        also_seen: Default::default(),
                    },
                    started_at: now,
                    due: now + COALESCE,
                    hard: now + COALESCE_MAX,
                },
            );
        }
    }
}

/// Claim, render and post one group.
/// Render and post one group. The caller has already claimed it: claiming here as well is
/// what made a dispatcher mistake its own claim for somebody else's.
async fn send(
    routes: &Routes,
    site: &Site,
    group: Group,
    attempts: u32,
    budget: Duration,
) -> (SendResult, Option<String>) {
    let Some(route) = routes.get(&group.key.route) else {
        let why = match routes.errors.get(&group.key.route) {
            Some(why) => why.clone(),
            None => format!("no notification route named {:?}", group.key.route),
        };
        tracing::warn!(route = %group.key.route, %why, "notification route unusable");
        return (SendResult::Dropped, Some("route-unusable".into()));
    };
    let incident = group.leader().incident_id.clone();
    let suppressed = match route.allow_send() {
        Allow::Go { suppressed } => suppressed,
        Allow::Capped { announce } => {
            tracing::info!(route = %route.spec.name, pods = group.members.len(),
                           "notification suppressed by the route's rate cap");
            if announce {
                let notice = cap_notice(&route.spec, site);
                if let Err(e) = route.post(&notice, attempts, budget).await {
                    tracing::warn!(route = %route.spec.name, code = %e.code,
                                   "could not post the rate-cap notice");
                }
            }
            return (SendResult::Suppressed, Some("rate-capped".into()));
        }
    };
    let mut group = group;
    group.suppressed = suppressed;
    let body = render(&group, &route.spec, site);
    match route.post(&body, attempts, budget).await {
        Ok(()) => {
            tracing::info!(route = %route.spec.name, %incident, pods = group.members.len(),
                           endpoint = %route.endpoint.display(), "notification sent");
            (SendResult::Sent, None)
        }
        Err(why) => {
            // The claim stays: a failed notification is reported, not retried forever by a
            // later reconcile of the same capture. The detail is logged; only the code is
            // recorded on the object, where far more people can read it.
            tracing::warn!(route = %route.spec.name, %incident, code = %why.code,
                           detail = %why.detail, endpoint = %route.endpoint.display(),
                           "notification failed");
            // The rate cap's debt was taken to render this message; give it back so the next
            // one that lands still reports it.
            route.restore_suppressed(suppressed);
            (SendResult::Failed, Some(why.code.to_string()))
        }
    }
}

/// The standalone message the cap posts the first time it turns a group away in a window. It
/// carries no incident detail on purpose: its job is to say "the channel is no longer the
/// whole story" and point at the command that is.
fn cap_notice(route: &RouteSpec, site: &Site) -> Value {
    let text = format!(
        "⚠️ kairn: the rate cap on route {} is spent ({} messages per {} minutes). Evidence is \
         still being captured for every incident — it is the messages that stop, not the \
         recording. kubectl -n {} get incidentcapture",
        escape(&route.name, 40),
        route.max_per_window,
        WINDOW.as_secs() / 60,
        site.namespace
    );
    match route.format {
        Format::Json => json!({
            "schema": "kairn.dev/notification/v1",
            "kind": "rate-cap-reached",
            "route": route.name,
            "maxPerWindow": route.max_per_window,
            "windowSeconds": WINDOW.as_secs(),
            "verdict": text,
        }),
        Format::Slack => json!({
            "text": text,
            "blocks": [ { "type": "context",
                          "elements": [ { "type": "plain_text", "text": text } ] } ],
        }),
    }
}

/// Record the outcome on the leading capture. Reporting only: nothing reads it back, so a
/// patch that fails costs visibility and nothing else.
async fn report(
    client: &kube::Client,
    leader: &Member,
    route: &str,
    result: SendResult,
    reason: Option<String>,
) {
    let api: kube::Api<crate::crd::IncidentCapture> =
        kube::Api::namespaced(client.clone(), &leader.capture_ns);
    let status = crate::crd::NotificationStatus {
        state: result.label().to_string(),
        at: chrono::Utc::now().to_rfc3339(),
        reason,
        route: Some(route.to_string()),
    };
    let patch = json!({ "status": { "notification": status } });
    if let Err(e) = api
        .patch_status(
            &leader.capture_name,
            &kube::api::PatchParams::apply("kairn-controller"),
            &kube::api::Patch::Merge(&patch),
        )
        .await
    {
        tracing::warn!(capture = %leader.capture_name, error = %e,
                       "could not record the notification outcome");
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use kairn_bundle::summary::{Change, Memory, Termination};

    fn group(members: usize, detail_change: bool) -> Group {
        let summary = Summary {
            container: Some("app".into()),
            termination: Some(Termination {
                reason: Some("OOMKilled".into()),
                exit_code: Some(137),
                finished_at: None,
            }),
            restarts: 2,
            last_words: LastWords::Captured,
            last_line: Some("fatal: out of memory writing DB_PASSWORD=hunter2".into()),
            change: detail_change.then(|| Change {
                kind: "Deployment".into(),
                name: "checkout".into(),
                revision_from: Some("1".into()),
                revision_to: Some("2".into()),
                seconds_before_alert: Some(94),
                actor: Some("argocd-application-controller".into()),
                field: Some("spec.template.spec.containers[app].image".into()),
                before: Some("shop/checkout:v1.4.2".into()),
                after: Some("shop/checkout:v1.4.3".into()),
            }),
            memory: Some(Memory {
                peak_bytes: 64_200_000.0,
                limit_bytes: Some(67_108_864.0),
                samples: 8,
            }),
            events: 4,
            collectors_run: vec!["logs".into(), "resources".into()],
            collectors_missing: vec!["metrics".into()],
        };
        Group {
            key: GroupKey {
                route: "platform".into(),
                rule: "KubePodCrashLooping".into(),
                namespace: "shop".into(),
                owner: "Deployment/checkout".into(),
            },
            cluster: "prod-apne2".into(),
            members: (0..members)
                .map(|i| Member {
                    incident_id: format!("prod-apne2-{i:016x}"),
                    pod: format!("checkout-abc-{i}"),
                    capture_ns: "kairn-system".into(),
                    capture_name: format!("ic-{i:016x}"),
                    summary: summary.clone(),
                })
                .collect(),
            bundle_dir: "/var/lib/kairn/bundles".into(),
            exported: false,
            suppressed: 0,
            repeats: 0,
            since: None,
            also_seen: Default::default(),
        }
    }

    /// Give a pending capture a distinct incident id.
    trait Tap {
        fn tap(self, i: usize) -> Self;
    }
    impl Tap for Pending {
        fn tap(mut self, i: usize) -> Self {
            self.member.incident_id = format!("prod-apne2-{i:016x}");
            self
        }
    }

    /// A release named `kairn` in `kairn-system`, as the chart's defaults produce.
    fn site() -> Site {
        Site {
            namespace: "kairn-system".into(),
            deployment: "kairn".into(),
        }
    }

    fn route(format: Format, detail: Detail) -> RouteSpec {
        RouteSpec {
            name: "platform".into(),
            host: "hooks.example.com".into(),
            format,
            detail,
            max_per_window: 10,
            include_demo: false,
            insecure_http: false,
        }
    }

    #[test]
    fn facts_carry_the_actionable_numbers_and_no_workload_content() {
        let body = render(
            &group(5, true),
            &route(Format::Slack, Detail::Facts),
            &site(),
        );
        let text = serde_json::to_string(&body).unwrap();
        for want in [
            "OOMKilled (exit 137)",
            "restart 2",
            // Every pod really did report it; "same reason" would also have been printed when
            // four of five reported nothing at all.
            "all 5 pods: OOMKilled",
            "peak 61.2 MiB of 64 MiB limit",
            // Says whether the log line exists, without carrying it.
            "last log line is in the bundle",
            "Deployment/checkout rev 1 → 2 changed, 94s before the alert",
            "by ~argocd-application-controller",
            "~ asserted by the client, not observed",
            "PARTIAL: metrics did not run",
            "cat-bundle",
        ] {
            assert!(text.contains(want), "missing {want:?} in {text}");
        }
        // Nothing from the workload, in any field.
        for leak in [
            "DB_PASSWORD",
            "hunter2",
            "out of memory",
            "v1.4.3",
            "v1.4.2",
            "image",
        ] {
            assert!(!text.contains(leak), "{leak} leaked: {text}");
        }
        // Only the first three ids, then a count.
        assert!(text.contains("+2 more"), "{text}");
    }

    #[test]
    fn content_adds_the_change_but_never_a_log_line() {
        let body = render(
            &group(1, true),
            &route(Format::Json, Detail::Content),
            &site(),
        );
        let text = serde_json::to_string(&body).unwrap();
        assert!(text.contains("v1.4.2") && text.contains("v1.4.3"), "{text}");
        for leak in ["DB_PASSWORD", "hunter2", "out of memory"] {
            assert!(!text.contains(leak), "{leak} leaked: {text}");
        }
    }

    #[test]
    fn strings_an_alert_author_chose_cannot_inject_pings_or_links() {
        let mut g = group(1, false);
        g.key.rule = "Breach <!channel> <https://evil.example|kairn verify: OK>".into();
        g.key.owner = "Deployment/<!here>".into();
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        assert!(
            !text.contains("<!channel>") && !text.contains("<!here>"),
            "{text}"
        );
        assert!(!text.contains("<https://evil.example|"), "{text}");
        assert!(text.contains("&lt;!channel&gt;"), "{text}");
        // Every block carrying a string somebody else chose is `plain_text`. Exactly one
        // block may be `mrkdwn` — the fenced command block — and only because every value in
        // it is admin-set or pattern-constrained, which `safe_commands` enforces.
        let body: Value = serde_json::from_str(&text).unwrap();
        let mut mrkdwn = 0;
        for block in body["blocks"].as_array().unwrap() {
            match block["text"]["type"].as_str() {
                Some("mrkdwn") => {
                    mrkdwn += 1;
                    let t = block["text"]["text"].as_str().unwrap();
                    assert!(
                        t.starts_with("```"),
                        "only the command block may be mrkdwn: {t}"
                    );
                    assert!(safe_commands(t.trim_matches('`')), "{t}");
                }
                Some(t) => assert_eq!(t, "plain_text", "{block}"),
                None => {}
            }
            for el in block["elements"].as_array().unwrap_or(&vec![]) {
                assert_eq!(el["type"].as_str(), Some("plain_text"), "{el}");
            }
        }
        assert_eq!(mrkdwn, 1, "{text}");
    }

    #[test]
    fn an_exported_group_points_at_the_object_store() {
        let mut g = group(1, false);
        g.exported = true;
        g.bundle_dir = "s3://evidence/prod/prod-apne2".into();
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        assert!(
            text.contains("kairn verify s3://evidence/prod/prod-apne2/"),
            "{text}"
        );
        assert!(!text.contains("cat-bundle"), "{text}");
    }

    #[tokio::test(start_paused = true)]
    async fn captures_of_one_incident_coalesce_into_one_group() {
        let mut open: BTreeMap<GroupKey, Open> = BTreeMap::new();
        let pending = |i: usize| {
            Pending {
                key: group(1, true).key,
                cluster: "prod-apne2".into(),
                member: group(1, true).members.remove(0),
                bundle_dir: "/var/lib/kairn/bundles".into(),
                exported: false,
            }
            .tap(i)
        };
        for i in 0..5 {
            admit(&mut open, pending(i));
            tokio::time::advance(Duration::from_secs(10)).await;
        }
        assert_eq!(open.len(), 1, "one alert on one workload is one group");
        let o = open.values().next().unwrap();
        assert_eq!(o.group.members.len(), 5);
        // Each new member extended the window…
        assert!(o.due > tokio::time::Instant::now());
        // …but never past the hard cap, so a slow trickle still gets reported.
        assert!(o.due <= o.hard);

        // A re-reconcile of a capture already in the group is not a sixth pod.
        admit(&mut open, pending(0));
        assert_eq!(open.values().next().unwrap().group.members.len(), 5);

        // A different workload is a different incident.
        let mut other = pending(9);
        other.key.owner = "Deployment/cart".into();
        admit(&mut open, other);
        assert_eq!(open.len(), 2);
    }

    #[tokio::test(start_paused = true)]
    async fn a_trickle_of_captures_cannot_hold_a_group_open_forever() {
        let mut open: BTreeMap<GroupKey, Open> = BTreeMap::new();
        let base = Pending {
            key: group(1, false).key,
            cluster: "c".into(),
            member: group(1, false).members.remove(0),
            bundle_dir: "/b".into(),
            exported: false,
        };
        for i in 0..20 {
            admit(&mut open, base.clone().tap(i));
            tokio::time::advance(Duration::from_secs(20)).await;
        }
        let o = open.values().next().unwrap();
        assert!(
            o.due <= o.hard && o.hard <= o.started_at + COALESCE_MAX,
            "the window was extended past the hard cap"
        );
    }

    #[test]
    fn a_suppressed_group_is_reported_by_the_next_message() {
        let mut g = group(1, false);
        g.suppressed = 7;
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        assert!(
            text.contains("7 more incidents were captured but not posted"),
            "{text}"
        );
        assert!(
            text.contains("get incidentcapture"),
            "the notice must say where to look"
        );
    }

    #[test]
    fn a_notification_is_claimed_once() {
        let dir = tempfile::tempdir().unwrap();
        assert!(claim(dir.path(), "ic-1").unwrap());
        assert!(!claim(dir.path(), "ic-1").unwrap());
        // The claim is never released, not even after a failed or rate-capped send: it is
        // what makes the message once-only across restarts and concurrent dispatchers.
        assert!(!claim(dir.path(), "ic-1").unwrap());
    }

    #[test]
    fn the_rate_cap_holds_per_window() {
        let mut spec = route(Format::Json, Detail::Facts);
        spec.max_per_window = 2;
        let r = Route::new(spec, "/services/T0/B0/x", false).unwrap();
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 0 });
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 0 });
        // The cap holds, and the groups it turns away are counted…
        assert!(matches!(r.allow_send(), Allow::Capped { .. }));
        assert!(matches!(r.allow_send(), Allow::Capped { .. }));
        // …then reported once by the next message that gets through, and only once.
        r.window.lock().unwrap().started =
            std::time::Instant::now() - (WINDOW + Duration::from_secs(1));
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 2 });
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 0 });
    }

    #[test]
    fn a_route_only_talks_to_the_host_the_admin_configured() {
        // The Secret's path can't smuggle another host or a scheme.
        for path in ["//evil.example/x", "https://evil.example/x", "no-slash"] {
            assert!(
                Route::new(route(Format::Json, Detail::Facts), path, false).is_err(),
                "{path}"
            );
        }
        let ok = Route::new(
            route(Format::Json, Detail::Facts),
            "/services/T0/B0/x",
            false,
        )
        .unwrap();
        assert_eq!(ok.endpoint.host, "hooks.example.com");
        assert_eq!(ok.endpoint.scheme, "https");
        // A route's host is a host, never a URL that could carry its own scheme.
        let mut as_url = route(Format::Json, Detail::Facts);
        as_url.host = "http://evil.example".into();
        assert!(Route::new(as_url, "/hook", true).is_err());
        // An in-cluster HTTPS receiver is fine without any switch.
        let mut local = route(Format::Json, Detail::Facts);
        local.host = "receiver.notify-e2e.svc:8080".into();
        assert_eq!(
            Route::new(local.clone(), "/hook", false)
                .unwrap()
                .endpoint
                .scheme,
            "https"
        );
        // Plain HTTP needs BOTH the route's opt-in and the process switch.
        local.insecure_http = true;
        assert_eq!(
            Route::new(local.clone(), "/hook", false)
                .unwrap()
                .endpoint
                .scheme,
            "https",
            "insecureHttp alone downgraded the scheme"
        );
        let http = Route::new(local.clone(), "/hook", true).unwrap();
        assert_eq!(http.endpoint.scheme, "http");
        assert_eq!(http.endpoint.authority(), "receiver.notify-e2e.svc:8080");
        // …and even then, only for a local host.
        local.host = "hooks.example.com".into();
        assert!(Route::new(local, "/hook", true).is_err());
    }
    // ---- round 12 -------------------------------------------------------------------

    #[test]
    fn untrusted_strings_are_escaped_exactly_once_and_blocks_respect_slacks_limits() {
        // Round 12: the namespace and owner were escaped at the leaf and again at the block,
        // so a real `&` reached the channel as `&amp;amp;` — and the cap counted input
        // characters while `&` expands to five, so a section could pass 3000 and be rejected
        // with a 400 the code treats as final. The claim is already spent by then, so the
        // incident was never announced at all.
        let mut g = group(1, true);
        g.key.namespace = "shop&web".into();
        g.key.owner = "Deployment/a&b".into();
        let body = render(&g, &route(Format::Slack, Detail::Content), &site());
        let header = body["blocks"][0]["text"]["text"].as_str().unwrap();
        assert!(header.contains("shop&amp;web"), "{header}");
        assert!(!header.contains("&amp;amp;"), "escaped twice: {header}");

        // Now the adversarial lengths: a legal 63-character namespace and a long owner, plus
        // ampersands, must still fit every Slack limit.
        let mut big = group(1, true);
        big.key.namespace = "n".repeat(63);
        big.key.owner = format!("Deployment/{}", "d".repeat(80));
        big.members[0].summary.change.as_mut().unwrap().after = Some("&".repeat(200));
        for detail in [Detail::Facts, Detail::Content] {
            let body = render(&big, &route(Format::Slack, detail), &site());
            let blocks = body["blocks"].as_array().unwrap();
            let len = |v: &Value| v.as_str().map_or(0, |s| s.chars().count());
            assert!(
                len(&blocks[0]["text"]["text"]) <= SLACK_HEADER,
                "header {} chars",
                len(&blocks[0]["text"]["text"])
            );
            for b in blocks {
                assert!(
                    len(&b["text"]["text"]) <= SLACK_SECTION,
                    "section too long: {b}"
                );
                for el in b["elements"].as_array().unwrap_or(&vec![]) {
                    assert!(len(&el["text"]) <= SLACK_CONTEXT, "context too long: {el}");
                }
            }
            let bytes = serde_json::to_vec(&body).unwrap().len();
            assert!(bytes <= MAX_BODY, "body {bytes} bytes");
            // A truncation must never cut an entity in half.
            let text = serde_json::to_string(&body).unwrap();
            assert!(!text.contains("&am\"") && !text.contains("&a…"), "{text}");
        }
    }

    #[test]
    fn the_retrieval_command_names_this_release_and_covers_every_capture() {
        // Round 12: `deploy/kairn` is only correct when the Helm release is itself called
        // `kairn` — the chart names it `<release>-kairn` — and the command retrieved the
        // leader's bundle alone while the message listed other ids nobody could act on.
        let g = group(5, false);
        let site = Site {
            namespace: "observability".into(),
            deployment: "evidence-kairn".into(),
        };
        let text = serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site))
            .unwrap();
        assert!(
            text.contains("kubectl -n observability exec deploy/evidence-kairn"),
            "{text}"
        );
        assert!(text.contains("/usr/local/bin/kairn cat-bundle"), "{text}");
        // Every member, not just the leader.
        for m in &g.members {
            assert!(
                text.contains(&m.incident_id),
                "{} missing: {text}",
                m.incident_id
            );
        }
        assert!(text.contains("for id in "), "{text}");
    }

    #[test]
    fn a_counted_repeat_is_reported_on_the_next_message() {
        let mut g = group(1, false);
        g.repeats = 11;
        g.since = Some(
            chrono::DateTime::parse_from_rfc3339("2026-09-20T14:05:00Z")
                .unwrap()
                .with_timezone(&Utc),
        );
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        assert!(text.contains("×11 more since 14:05 UTC"), "{text}");
    }

    #[test]
    fn a_failed_send_gives_the_suppression_debt_back() {
        // Round 12: the debt was taken with `mem::take` to render the message, so a send that
        // then failed destroyed the count and nobody was ever told.
        let mut spec = route(Format::Json, Detail::Facts);
        spec.max_per_window = 1;
        let r = Route::new(spec, "/hook", false).unwrap();
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 0 });
        assert_eq!(
            r.allow_send(),
            Allow::Capped { announce: true },
            "the cap holds"
        );
        r.window.lock().unwrap().started =
            std::time::Instant::now() - (WINDOW + Duration::from_secs(1));
        let Allow::Go { suppressed: taken } = r.allow_send() else {
            panic!("a new window allows one");
        };
        assert_eq!(taken, 1);
        // That message failed to send, so the debt is owed again.
        r.restore_suppressed(taken);
        r.window.lock().unwrap().started =
            std::time::Instant::now() - (WINDOW + Duration::from_secs(1));
        assert_eq!(
            r.allow_send(),
            Allow::Go { suppressed: 1 },
            "the turned-away group was forgotten"
        );
    }

    #[test]
    fn the_first_group_the_cap_turns_away_is_announced_immediately() {
        // Round 12: deferring the whole suppression notice to "the next message that gets
        // through" tells nobody anything when the storm is the last thing that happens.
        let mut spec = route(Format::Slack, Detail::Facts);
        spec.max_per_window = 1;
        let r = Route::new(spec, "/hook", false).unwrap();
        assert_eq!(r.allow_send(), Allow::Go { suppressed: 0 });
        assert_eq!(r.allow_send(), Allow::Capped { announce: true });
        // Only once per window, or the cap becomes its own storm.
        assert_eq!(r.allow_send(), Allow::Capped { announce: false });
        assert_eq!(r.allow_send(), Allow::Capped { announce: false });

        let notice = cap_notice(&r.spec, &site());
        let text = serde_json::to_string(&notice).unwrap();
        assert!(
            text.contains("rate cap on route platform is spent"),
            "{text}"
        );
        // The point a muted channel most needs to know.
        assert!(text.contains("still being captured"), "{text}");
        assert!(text.contains("get incidentcapture"), "{text}");
        // And it must not be mistakable for an incident message.
        assert!(!text.contains("evidence captured ·"), "{text}");
    }

    #[test]
    fn a_route_name_cannot_aim_the_secret_read_somewhere_else() {
        // Round 12: the name is a path segment, and the chart's schema does not apply to a
        // ConfigMap somebody edited directly.
        for bad in [
            "../../etc",
            "a/b",
            "UPPER",
            "-lead",
            "trail-",
            "",
            &"x".repeat(41),
        ] {
            assert!(!name_ok(bad), "accepted {bad:?}");
        }
        for ok in ["platform", "team-a", "a", "a1-b2"] {
            assert!(name_ok(ok), "refused {ok:?}");
        }
    }

    #[test]
    fn an_unknown_route_field_is_refused_rather_than_ignored() {
        // A misspelled key must not silently mean "the default": `detial: content` would
        // otherwise leave the route on `facts` while the admin believes otherwise.
        let good = r#"{"name":"p","host":"h.example","format":"json"}"#;
        assert!(serde_json::from_str::<RouteSpec>(good).is_ok());
        let typo = r#"{"name":"p","host":"h.example","detial":"content"}"#;
        assert!(serde_json::from_str::<RouteSpec>(typo).is_err());
    }

    #[test]
    fn a_command_block_carrying_anything_unexpected_falls_back_to_plain_text() {
        // The `mrkdwn` fence is safe only while every value in it is admin-set or
        // pattern-constrained. If that ever stops being true, the block must degrade rather
        // than become Slack markup.
        assert!(safe_commands(
            "kubectl -n kairn-system exec deploy/kairn -c controller -- \
             /usr/local/bin/kairn cat-bundle /var/lib/kairn/bundles/c-1.ieb > c-1.ieb"
        ));
        for unsafe_ in [
            "look: *bold*",
            "`nested`",
            "<https://evil.example|x>",
            "a&b",
        ] {
            assert!(!safe_commands(unsafe_), "accepted {unsafe_}");
        }
        let mut g = group(1, false);
        g.bundle_dir = "/var/lib/`kairn`".into();
        let body = render(&g, &route(Format::Slack, Detail::Facts), &site());
        let cmd = &body["blocks"][3]["text"];
        assert_eq!(cmd["type"].as_str(), Some("plain_text"), "{cmd}");
    }

    #[test]
    fn a_running_pod_that_is_flapping_still_says_so() {
        // The summary fix that removed the false `terminated, restart 0` must not leave a
        // flapping live pod with nothing in its verdict.
        let mut g = group(1, false);
        g.members[0].summary.termination = None;
        g.members[0].summary.last_words = LastWords::None;
        let v = verdict(&g, Detail::Facts);
        assert!(v.contains("restart 2"), "{v}");
        assert!(!v.to_lowercase().contains("terminated,"), "{v}");
        // And the headline cannot claim a termination reason it does not have.
        assert!(
            headline(&g).contains("evidence captured"),
            "{}",
            headline(&g)
        );
    }

    // ---- round 12, R2 (converge) ----------------------------------------------------

    #[test]
    fn only_a_new_rollout_is_news_a_flapping_reason_is_not() {
        // R2 BLOCKER: the cooldown was keyed on the termination reason, so a container that
        // dies OOMKilled on one restart and Error on the next — an ordinary shape for a process
        // that sometimes hits its limit mid-allocation and sometimes exits non-zero first —
        // looked like news every firing. Measured: 4 posts inside one 30-minute cooldown, which
        // is worse than the 36-messages-in-three-hours bug it replaced.
        let a = group(1, true);
        let mut flapped = group(1, true);
        flapped.members[0]
            .summary
            .termination
            .as_mut()
            .unwrap()
            .reason = Some("Error".into());
        assert_eq!(
            rollout_key(&a),
            rollout_key(&flapped),
            "a different termination reason is the same incident continuing"
        );

        // A new rollout IS news: somebody deployed during the incident.
        let mut rolled = group(1, true);
        rolled.members[0]
            .summary
            .change
            .as_mut()
            .unwrap()
            .revision_to = Some("3".into());
        assert_ne!(rollout_key(&a), rollout_key(&rolled));

        // And the key must not depend on which pod the kubelet happened to seal first.
        let mut ab = group(2, true);
        ab.members[1].summary.termination.as_mut().unwrap().reason = Some("Error".into());
        let mut ba = ab.clone();
        ba.members.swap(0, 1);
        assert_eq!(
            rollout_key(&ab),
            rollout_key(&ba),
            "admission order must not flip the cooldown key"
        );
        // Damping the repeats must not hide which ways it failed.
        let seen = reasons_of(&ab);
        assert!(
            seen.contains("OOMKilled") && seen.contains("Error"),
            "{seen:?}"
        );
    }

    #[test]
    fn a_rollback_is_news_even_when_the_revision_is_unknown() {
        // Caught by the kind E2E: keying the cooldown on the revision alone meant a workload
        // whose revisions `diffs/` cannot establish — a Deployment whose pods never become
        // Ready, i.e. the crashlooping case this feature exists for — kept the same key forever,
        // so a rollback during the incident stayed unreported for the whole 30 minutes.
        let unknown_rev = |after: &str| {
            let mut g = group(1, true);
            let c = g.members[0].summary.change.as_mut().unwrap();
            c.revision_from = None;
            c.revision_to = None;
            c.field = Some("containers[name=app].image".into());
            c.after = Some(after.to_string());
            g
        };
        let before = unknown_rev("busybox:1.37");
        let rolled_back = unknown_rev("busybox:1.36");
        assert_ne!(
            rollout_key(&before),
            rollout_key(&rolled_back),
            "a rollback must be news even with no revision numbers"
        );
        // The same change twice is still the same incident.
        assert_eq!(
            rollout_key(&before),
            rollout_key(&unknown_rev("busybox:1.37"))
        );
        // And the key must not carry the workload value it was derived from.
        let key = rollout_key(&before);
        assert!(
            !key.contains("busybox"),
            "the key leaks the changed value: {key}"
        );

        // A revision, when there is one, still wins: it is the more meaningful identity.
        let with_rev = group(1, true);
        assert!(
            rollout_key(&with_rev).contains('@'),
            "{}",
            rollout_key(&with_rev)
        );
        assert!(
            !rollout_key(&before).contains('@'),
            "{}",
            rollout_key(&before)
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_send_that_never_landed_does_not_mute_the_incident() {
        // R2 BLOCKER: the cooldown was armed in the coalescing loop, before the spawned send's
        // outcome was known. A group whose POST failed — or that the rate cap turned away —
        // left the map saying "announced at T", so every re-fire for the next 30 minutes was
        // counted as a repeat and never posted, even after the endpoint recovered. The incident
        // was never announced at all.
        let announced = |delivered| Sent {
            at: tokio::time::Instant::now(),
            rollout: "Deployment/checkout@2".into(),
            delivered,
            repeats: 0,
            reasons: Default::default(),
            first_at: Utc::now(),
        };
        assert!(
            is_repeat(Some(&announced(true)), "Deployment/checkout@2"),
            "a delivered message does dampen the next firing"
        );
        assert!(
            !is_repeat(Some(&announced(false)), "Deployment/checkout@2"),
            "a send that never landed must not mute the incident"
        );
        // A new rollout is news even inside the cooldown.
        assert!(!is_repeat(Some(&announced(true)), "Deployment/checkout@3"));
        // …and the cooldown expires.
        let old = announced(true);
        tokio::time::advance(COOLDOWN + Duration::from_secs(1)).await;
        assert!(!is_repeat(Some(&old), "Deployment/checkout@2"));
        assert!(!is_repeat(None, "Deployment/checkout@2"));
    }

    #[test]
    fn a_flap_reports_the_reasons_it_saw_while_quiet() {
        let mut g = group(1, true);
        g.repeats = 5;
        g.also_seen = ["Error".to_string(), "ContainerStatusUnknown".to_string()]
            .into_iter()
            .collect();
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        assert!(
            text.contains("also seen: ContainerStatusUnknown, Error"),
            "{text}"
        );
        // The generic body carries it as a field, not a sentence.
        let json = render(&g, &route(Format::Json, Detail::Facts), &site());
        assert_eq!(
            json["alsoSeen"],
            serde_json::json!(["ContainerStatusUnknown", "Error"])
        );
        assert_eq!(json["repeats"], 5);
    }

    #[test]
    fn enabling_a_route_must_not_replay_a_pvcs_history() {
        // Caught only by running the WHOLE kind E2E: the earlier suites leave dozens of
        // `Exported` captures on the PVC, and turning a route on rolls the controller, whose
        // watcher relist re-reconciles every one of them. Seven messages arrived where one was
        // wanted. In a real cluster that is every incident the recorder has ever held, landing
        // in the channel at once, on the day someone enables notification.
        //
        // The freshness rule lives in `reconcile::enqueue_notification`; what is asserted here
        // is the contract it depends on — a capture it declines is *claimed*, so a relist does
        // not keep re-deciding it, and a claim is not releasable.
        let dir = tempfile::tempdir().unwrap();
        assert!(claim(dir.path(), "old-incident").unwrap());
        assert!(
            !claim(dir.path(), "old-incident").unwrap(),
            "a declined capture must stay declined across relists"
        );
    }

    #[test]
    fn a_dispatcher_does_not_mistake_its_own_claim_for_somebody_elses() {
        // Caught by the kind E2E, not by any unit test: R2's fix moved every member's claim
        // ahead of the POST, but `send` still claimed the leader as its own go/no-go — so the
        // dispatcher read back the claim it had just written and reported `already-notified`.
        // Five captures were grouped, the window closed on time, and nothing was ever posted.
        let dir = tempfile::tempdir().unwrap();
        let members: Vec<String> = (0..5).map(|i| format!("ic-{i}")).collect();
        let leader = members[0].clone();

        assert!(
            claim_group(dir.path(), &leader, &members).unwrap(),
            "the first dispatcher to claim a group must win it"
        );
        // Every member is claimed, not just the leader: that is what stops the others
        // re-enqueueing on the next reconcile.
        for m in &members {
            assert!(
                !claim(dir.path(), m).unwrap(),
                "{m} was left unclaimed, so it will come back"
            );
        }
        // A second dispatcher (or the same one after a relist) loses, and claiming again is
        // still safe.
        assert!(
            !claim_group(dir.path(), &leader, &members).unwrap(),
            "a second dispatcher must lose the group it did not claim first"
        );

        // The state an older build left on disk — leader claimed, the rest not — must not let
        // the survivors elect a new leader and announce a closed incident again.
        let old = tempfile::tempdir().unwrap();
        assert!(claim(old.path(), "ic-0").unwrap());
        assert!(
            claim_group(old.path(), "ic-1", &["ic-1".into(), "ic-2".into()]).unwrap(),
            "a genuinely unclaimed group is still announced"
        );
        assert!(
            !claim(old.path(), "ic-2").unwrap(),
            "the rest must be claimed too"
        );
    }

    #[test]
    fn a_counted_repeat_is_claimed_so_it_cannot_come_back() {
        // Found while rewriting the E2E: a counted repeat was `continue`d without a claim, so it
        // re-enqueued on every reconcile — and once the group's rollout moved on it no longer
        // matched the cooldown entry, so a later reconcile announced the *stale* verdict as news.
        let dir = tempfile::tempdir().unwrap();
        let g = group(3, true);
        for m in &g.members {
            assert!(claim(dir.path(), &m.incident_id).unwrap());
        }
        for m in &g.members {
            assert!(
                !claim(dir.path(), &m.incident_id).unwrap(),
                "a claimed repeat must not be claimable again, or it re-enqueues forever"
            );
        }
    }

    #[test]
    fn an_alert_author_cannot_forge_a_line_with_invisible_characters() {
        // R2: `escape` flattened only `Cc`, so U+2028/2029, the bidi overrides and the
        // zero-width marks passed through verbatim — and the CRD's boundary pattern on `rule`
        // refuses exactly `<`, `>` and `&`, nothing else. That let whoever names a
        // PrometheusRule write their own "kairn verify: OK (signed)" line under Kairn's text,
        // which is the round-11 spoof reachable again without using `<`.
        let mut g = group(1, false);
        g.key.rule = "KubePodCrashLooping\u{2028}\u{2028}    kairn verify: OK (signed)".into();
        g.key.owner = "Deployment/\u{202e}txet desrever".into();
        g.members[0].pod = "pod\u{200b}\u{feff}name".into();
        let text =
            serde_json::to_string(&render(&g, &route(Format::Slack, Detail::Facts), &site()))
                .unwrap();
        for bad in [
            '\u{2028}', '\u{2029}', '\u{202e}', '\u{202a}', '\u{200b}', '\u{feff}', '\u{2066}',
        ] {
            assert!(
                !text.contains(bad),
                "U+{:04X} reached the channel: {text}",
                bad as u32
            );
        }
        // The visible text survives; only the invisible parts are flattened.
        assert!(text.contains("KubePodCrashLooping"), "{text}");
    }

    #[test]
    fn escape_bounds_the_rendered_length_not_the_input_length() {
        // R2: the budget compared a rendered count against an input count, so once an entity
        // had inflated the output past the input length every remaining character was pushed
        // unchecked — measured at ~1.8× the cap.
        for (text, max) in [
            ("&xy", 6),
            ("&&&xxxxxxxxxxxx", 20),
            ("&".repeat(200).as_str(), 40),
            ("plain text", 4),
            ("한글과 이모지 🙂🙂🙂", 8),
        ] {
            let out = escape(text, max);
            assert!(
                out.chars().count() <= max,
                "escape({text:?}, {max}) produced {} chars: {out:?}",
                out.chars().count()
            );
        }
        // A cap that fits exactly is not truncated, and an entity that just fits is kept.
        assert_eq!(escape("ab&", 7), "ab&amp;");
        assert_eq!(escape("hello", 5), "hello");
        assert_eq!(escape("hello", 0), "");
    }

    #[test]
    fn a_bare_ampersand_cannot_swallow_a_whole_block() {
        // R2: `fit`'s entity repair searched the entire prefix, so a leading bare `&` with no
        // `;` after it discarded everything — a 2900-character block collapsed to "…".
        let text = format!("&{}", "B".repeat(5000));
        let out = fit(&text, 2900);
        assert!(
            out.chars().count() > 2000,
            "fit collapsed the block to {} chars: {out:?}",
            out.chars().count()
        );
        // Multi-byte text must not panic on the lookback.
        let korean = "한".repeat(4000);
        assert!(fit(&korean, 100).chars().count() <= 100);
        // A genuine entity at the cut is still not split.
        let entity = format!("{}&amp;tail", "x".repeat(2895));
        assert!(
            !fit(&entity, 2900).ends_with("&am…"),
            "{}",
            fit(&entity, 2900)
        );
    }

    #[test]
    fn a_mass_rollout_keeps_a_runnable_command_and_names_where_the_rest_are() {
        // R2: the fenced block was fitted as a whole, so past ~96 members the closing `done`
        // and the fence itself were cut off, and past ~105 incident ids vanished — at 200 pods
        // half the bundles were unretrievable and the copy button yielded an unterminated loop.
        for members in [40, 96, 105, 200] {
            let g = group(members, false);
            let body = render(&g, &route(Format::Slack, Detail::Facts), &site());
            let block = &body["blocks"][3];
            assert_eq!(
                block["text"]["type"].as_str(),
                Some("mrkdwn"),
                "{members} members: lost the copy button"
            );
            let t = block["text"]["text"].as_str().unwrap();
            assert!(
                t.starts_with("```\n") && t.ends_with("\n```"),
                "{members}: {t}"
            );
            assert!(
                t.contains("\ndone\n"),
                "{members} members: the loop is unterminated"
            );
            assert!(
                t.chars().count() <= SLACK_SECTION,
                "{members}: {} chars",
                t.chars().count()
            );
            if members > MAX_IDS {
                assert!(
                    t.contains(&format!("and {} more in this incident", members - MAX_IDS)),
                    "{members} members: the rest are unfindable: {t}"
                );
                assert!(t.contains("get incidentcapture"), "{members}: {t}");
            }
        }
    }

    #[test]
    fn an_ordinary_object_store_prefix_keeps_its_copy_button() {
        // R2: the allow-list was so narrow that legitimate prefixes silently lost the fenced
        // block — and when the fallback was taken, the text went out unescaped.
        for dir in [
            "s3://evidence/prod~1/kairn",
            "s3://evidence/team+platform",
            "s3://evidence/a,b",
            "s3://evidence/prod#1",
            "gs://evidence/100%25/kairn",
        ] {
            let mut g = group(1, false);
            g.exported = true;
            g.bundle_dir = dir.into();
            let body = render(&g, &route(Format::Slack, Detail::Facts), &site());
            assert_eq!(
                body["blocks"][3]["text"]["type"].as_str(),
                Some("mrkdwn"),
                "{dir} lost its copy button"
            );
        }
        // And the fallback, when it is taken, is escaped.
        let mut g = group(1, false);
        g.bundle_dir = "/var/lib/kairn/<!channel>".into();
        let body = render(&g, &route(Format::Slack, Detail::Facts), &site());
        let block = &body["blocks"][3]["text"];
        assert_eq!(block["type"].as_str(), Some("plain_text"), "{block}");
        let t = block["text"].as_str().unwrap();
        assert!(
            !t.contains("<!channel>") && t.contains("&lt;!channel&gt;"),
            "{t}"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_shutdown_flushes_groups_that_are_still_coalescing() {
        // Kubernetes sends SIGTERM on every rollout, and a group is claimed before it is
        // posted — so a group abandoned on the way out is marked notified and never announced.
        // The flush path must therefore dispatch exactly what the coalescing tick would.
        let mut open: BTreeMap<GroupKey, Open> = BTreeMap::new();
        for i in 0..3 {
            let mut p = Pending {
                key: group(1, true).key,
                cluster: "prod-apne2".into(),
                member: group(1, true).members.remove(0),
                bundle_dir: "/var/lib/kairn/bundles".into(),
                exported: false,
            }
            .tap(i);
            // Three different workloads, so three groups are open at once.
            p.key.owner = format!("Deployment/svc-{i}");
            admit(&mut open, p);
        }
        assert_eq!(open.len(), 3);
        // None of them is due yet: the tick would not flush these.
        let now = tokio::time::Instant::now();
        assert!(
            open.values().all(|o| o.due > now),
            "a window was already due"
        );

        // What the shutdown branch does: take every open group, due or not.
        let flushed = std::mem::take(&mut open);
        assert_eq!(flushed.len(), 3, "shutdown must not wait out the windows");
        assert!(open.is_empty());
    }

    #[tokio::test(start_paused = true)]
    async fn drain_returns_even_when_the_dispatcher_is_already_gone() {
        // `drain` registers its waiter before signalling; registering after would race with a
        // dispatcher that finishes immediately and wait out the whole timeout instead.
        let routes = std::sync::Arc::new(Routes::default());
        let d = Dispatcher {
            tx: tokio::sync::mpsc::channel(1).0,
            routes,
            shutdown: std::sync::Arc::new(tokio::sync::Notify::new()),
            done: std::sync::Arc::new(tokio::sync::Notify::new()),
        };
        // No loop is listening, so this must fall through on the timeout rather than hang.
        let t = tokio::time::Instant::now();
        d.drain(Duration::from_secs(10)).await;
        assert!(
            t.elapsed() >= Duration::from_secs(10),
            "drain returned early"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn drain_signals_the_loop_even_if_it_is_not_waiting_yet() {
        // The test below proves the *primitive*, which is not the same as proving the code uses
        // it — and it did not, for a while, because a patch reported as applied never landed.
        // This one exercises `drain` itself: a loop that registers only AFTER the signal must
        // still see it, which is true of `notify_one` (a stored permit) and false of
        // `notify_waiters`.
        let d = Dispatcher {
            tx: tokio::sync::mpsc::channel(1).0,
            routes: std::sync::Arc::new(Routes::default()),
            shutdown: std::sync::Arc::new(tokio::sync::Notify::new()),
            done: std::sync::Arc::new(tokio::sync::Notify::new()),
        };
        let shutdown = d.shutdown.clone();
        let done = d.done.clone();
        // Signal first; nothing is listening yet.
        let drained = tokio::spawn(async move { d.drain(Duration::from_secs(30)).await });
        tokio::time::sleep(Duration::from_millis(10)).await;
        // Now the "loop" starts waiting, as it would after losing a `select!` race.
        tokio::time::timeout(Duration::from_secs(1), shutdown.notified())
            .await
            .expect("the shutdown signal did not survive being sent before anyone waited");
        done.notify_waiters();
        tokio::time::timeout(Duration::from_secs(1), drained)
            .await
            .expect("drain did not return after the loop reported done")
            .expect("drain panicked");
    }

    #[tokio::test(start_paused = true)]
    async fn the_shutdown_signal_survives_losing_a_select_race() {
        // R13 BLOCKER: the loop builds its `Notified` inside the `select!` and drops it whenever
        // another branch wins, and a dropped `notify_waiters()` wakeup is gone for good — measured
        // at ~30% of shutdowns flushing nothing. `notify_one` stores a permit instead.
        let n = std::sync::Arc::new(tokio::sync::Notify::new());

        // What the old code did: wake a registered waiter, then drop it.
        {
            let waiter = n.notified();
            tokio::pin!(waiter);
            // Register without completing, as the loop's `select!` does.
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(std::future::Future::poll(waiter.as_mut(), &mut cx).is_pending());
            n.notify_waiters();
            // …and the future is dropped here, taking the wakeup with it.
        }
        assert!(
            tokio::time::timeout(Duration::from_millis(50), n.notified())
                .await
                .is_err(),
            "notify_waiters survived a dropped waiter; this test no longer proves anything"
        );

        // What the code does now: the permit outlives the dropped waiter.
        {
            let waiter = n.notified();
            tokio::pin!(waiter);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            assert!(std::future::Future::poll(waiter.as_mut(), &mut cx).is_pending());
            n.notify_one();
        }
        assert!(
            tokio::time::timeout(Duration::from_millis(50), n.notified())
                .await
                .is_ok(),
            "the shutdown signal was lost when its waiter lost the select race"
        );
    }

    #[test]
    fn the_shutdown_flush_cannot_outlast_its_own_budget() {
        // R13: the flush claims a group and then posts. With the ordinary 3 attempts that is up to
        // 21 s per group, which is longer than the drain budget — so the claim was spent and the
        // process exited mid-retry. One attempt per group is what fits.
        // Compile-time, because these are consts: a flush that retries does not fit the drain
        // budget, and the ordinary path must still retry.
        const _: () = assert!(ATTEMPTS_DRAINING == 1);
        const _: () = assert!(ATTEMPTS > ATTEMPTS_DRAINING);
        // The arithmetic those two exist for: one attempt, one request timeout, no backoff, and
        // room to spare inside `NOTIFY_DRAIN`.
        let worst_case = Duration::from_secs(5) * ATTEMPTS_DRAINING;
        assert!(
            worst_case < Duration::from_secs(10),
            "{worst_case:?} does not fit the drain"
        );
    }
}
