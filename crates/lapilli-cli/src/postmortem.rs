//! `lapilli postmortem` — the draft a human then writes ([`docs/design-postmortem.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/design-postmortem.md)).
//!
//! It **transcribes**. Every line is a value that exists in the bundle, followed by the file it
//! came from, and the sections a postmortem needs a human for are emitted empty, with their
//! headings, because a blank heading is an honest prompt and a filled one would be a guess
//! ([`DESIGN.md`](https://github.com/lapilli-project/lapilli/blob/main/DESIGN.md) §2: not an auto-RCA narrative).
//!
//! The verdict decides how it renders, not *whether* it renders. Round 18 rejected the first
//! design's refusal-on-FAILED: a FAILED bundle is exactly when someone needs to see what it
//! *claims*, and printing nothing sends them to `cat` the files with no verdict attached to
//! anything. The blast radius comes from a document that looks trustworthy, not from the facts
//! being visible. So: visible, banner, non-zero exit.

use std::path::{Path, PathBuf};

use lapilli_bundle::{
    verify_bundle, verify_bundle_dir, BundleError, Manifest, ProblemCode, SignatureStatus, Summary,
    Verdict, VerifyOptions, VerifyReport,
};
use serde_json::Value;

/// Markdown is a human surface. `COMPATIBILITY.md` §2 says so, and this is the code that makes
/// the statement true: nothing here is a parsing contract.
#[derive(clap::Args, Debug)]
pub struct PostmortemArgs {
    /// A `.ieb` file or an already-unpacked bundle directory. Local only: `lapilli verify` grew
    /// remote reading with a byte limit, a redirect policy and its own exit code, and a rendering
    /// command that inherits all of it gains four failure modes for a convenience
    /// `lapilli fetch | lapilli postmortem` already provides.
    pub bundle: PathBuf,
    /// SPKI PEM public key obtained out of band. Optional on purpose: requiring it would make the
    /// command unusable on the majority of installs, where signing is off by default, and
    /// `signed:unpinned` is an honest string that says authenticity was not established.
    #[arg(long, value_name = "FILE")]
    pub key: Option<PathBuf>,
    /// Include the log line the container died on, marked as workload content.
    ///
    /// Off by default. The first design had this backwards and argued that the notification
    /// defaults the other way "because a chat channel is a broadcast, and this is run by somebody
    /// who already holds the bundle". The bundle holder is not the audience: the *output* is a
    /// document pasted into a wiki, which is a broader audience than the Slack channel and a
    /// permanent one.
    #[arg(long)]
    pub include_log_line: bool,
}

/// What `lapilli postmortem` produces, before anything is printed. `markdown` is `None` exactly
/// when the verdict could not be reached (CANNOT_EVALUATE) — then `refused` says why and
/// `exit_code` is 3, as on the command line. Split from `run` so `lapilli mcp` can return the
/// document without writing to a stdout it does not own.
pub struct Rendered {
    pub markdown: Option<String>,
    /// The exit code `lapilli postmortem` returns is `verdict.exit_code()`.
    pub verdict: Verdict,
    pub refused: Option<String>,
}

pub fn run(args: &PostmortemArgs) -> Result<i32, BundleError> {
    let r = document(args)?;
    match r.markdown {
        Some(md) => print!("{md}"),
        None => {
            eprintln!(
                "cannot evaluate this bundle, so there is nothing to transcribe: {}",
                r.refused.as_deref().unwrap_or("no reason recorded")
            );
            eprintln!("  lapilli verify {} --output json", args.bundle.display());
        }
    }
    Ok(r.verdict.exit_code())
}

pub fn document(args: &PostmortemArgs) -> Result<Rendered, BundleError> {
    let key_pem = match &args.key {
        Some(p) => Some(std::fs::read_to_string(p)?),
        None => None,
    };
    let opts = VerifyOptions {
        expected_cluster: None,
        expected_incident: None,
        trusted_key_pem: key_pem,
    };

    let is_dir = args.bundle.is_dir();
    let report = if is_dir {
        verify_bundle_dir(&args.bundle, &opts)?
    } else {
        verify_bundle(&args.bundle, &opts)?
    };

    // CANNOT_EVALUATE is not a milder FAILED, it is the opposite of PARTIAL: the verdict itself
    // could not be reached, so there is no verified hash tree to render from and every line would
    // be a fact with nothing behind it. Exit 3 is the code the CLI already uses for "this says
    // nothing about the bundle".
    if report.verdict == Verdict::CannotEvaluate {
        return Ok(Rendered {
            markdown: None,
            verdict: report.verdict,
            refused: Some(
                report
                    .problems
                    .first()
                    .map(|p| p.message.clone())
                    .unwrap_or_else(|| "no reason recorded".into()),
            ),
        });
    }

    // Rendering needs the files themselves; verification does not. `read_ieb_from` streams and
    // keeps only `path → sha256` (`verify.rs`'s `Contents`), which is what bounds memory for a
    // 1 GiB bundle — so a `.ieb` has to be unpacked to be read. Into a temp dir this process
    // owns, through `unpack`, which rejects traversal and link entries.
    //
    // On a FAILED verdict these are bytes that did NOT verify. Extracting them is the point:
    // the document exists to show what the bundle *claims*, behind a banner that says the claim
    // is unverified. Nothing here is written outside the temp dir.
    let staged;
    let dir: Option<&Path> = if is_dir {
        Some(&args.bundle)
    } else {
        staged = tempfile::tempdir()?;
        match lapilli_bundle::unpack(&args.bundle, staged.path()) {
            Ok(()) => Some(staged.path()),
            // A structurally malformed bundle — a traversal path, a link entry, a duplicate —
            // is one `unpack` refuses, and refusing is correct. But the VERDICT was reached:
            // `lapilli verify` says FAILED and exits 1. Losing that here and reporting "cannot
            // evaluate" instead would make this command contradict the verifier on the same
            // bytes. So the document renders what the verdict knows and says plainly that
            // nothing could be transcribed.
            Err(e) => {
                eprintln!("lapilli postmortem: the bundle could not be read: {e}");
                None
            }
        }
    };

    let manifest: Option<Manifest> = dir
        .and_then(|d| std::fs::read(d.join("manifest.json")).ok())
        .and_then(|b| serde_json::from_slice(&b).ok());
    let mut summary = dir.map(|d| Summary::from_dir(d, None)).unwrap_or_default();
    if !args.include_log_line {
        summary = summary.without_log_line();
    }

    let digest = if is_dir {
        manifest
            .as_ref()
            .map(|m| Digest::TreeRoot(m.hash_tree.root.clone()))
            .unwrap_or(Digest::Unavailable)
    } else {
        // Streamed: the file was verified without being held in memory, and a 1 GiB bundle
        // should not be the exception the digest makes.
        match std::fs::File::open(&args.bundle) {
            Ok(f) => {
                use sha2::Digest as _;
                let mut h = sha2::Sha256::new();
                match std::io::copy(&mut std::io::BufReader::new(f), &mut h) {
                    Ok(_) => Digest::File(format!("{:x}", h.finalize())),
                    Err(_) => Digest::Unavailable,
                }
            }
            Err(_) => Digest::Unavailable,
        }
    };

    Ok(Rendered {
        markdown: Some(render(
            &report,
            manifest.as_ref(),
            &summary,
            dir,
            args,
            &digest,
        )),
        verdict: report.verdict,
        refused: None,
    })
}

// ------------------------------------------------------------------------------ rendering -----

/// What a reader can check to confirm they hold the same bytes. A `.ieb` has one digest; an
/// unpacked directory does not, so the hash-tree root stands in — and the document says which
/// of the two it is rather than printing a hex string and letting the reader assume.
pub enum Digest {
    File(String),
    TreeRoot(String),
    Unavailable,
}

fn render(
    report: &VerifyReport,
    manifest: Option<&Manifest>,
    summary: &Summary,
    dir: Option<&Path>,
    args: &PostmortemArgs,
    digest: &Digest,
) -> String {
    let mut out = String::new();
    let incident = manifest
        .map(|m| m.incident.id.as_str())
        .or(report.incident_id.as_deref())
        .unwrap_or("(unknown)");

    banner(&mut out, report);
    // The incident id is deliberately unconstrained by the CRD (`crd.rs`, and round 19's R4),
    // and a bundle that never passed through an API server has whatever its author wrote. It is
    // the document's H1, which is the one line a reader trusts most.
    out.push_str(&format!("# Incident {}\n\n", md(incident)));
    header(&mut out, report, manifest, args, digest);
    let Some(dir) = dir else {
        out.push_str(
            "The verdict above is everything this document can say. The bundle could not be \
             read far enough to transcribe anything from it — see the banner for why — so the \
             sections that would carry the facts are not rendered rather than rendered empty.\n\n",
        );
        human(&mut out);
        return out;
    };
    captured(&mut out, summary, args);
    timeline(&mut out, dir);
    changed(&mut out, summary);
    inventory(&mut out, report, summary);
    human(&mut out);
    out
}

/// A FAILED bundle renders with this first, and **which** failure it was decides the wording.
///
/// Round 18's P4 was that the commonest FAILED is a *misfiling*, and telling an incident review
/// "the evidence failed integrity" when the true statement is "this may be the wrong cluster" is
/// a worse error than silence. The first implementation of this function had two branches and
/// therefore made the same mistake in the other direction: it told a reader that a malformed tar
/// archive meant "the bytes do not match what was sealed". A verdict carries one of six codes;
/// four of them are not integrity failures at all.
fn banner(out: &mut String, report: &VerifyReport) {
    if report.verdict != Verdict::Failed {
        return;
    }
    let has = |c: ProblemCode| report.problems.iter().any(|p| p.code == c);

    if has(ProblemCode::Structure) || has(ProblemCode::NotABundle) || has(ProblemCode::Manifest) {
        out.push_str(
            "> ## ⚠ This is not a well-formed bundle\n>\n\
             > The archive or its manifest does not obey the `ieb/v1` rules, so nothing below was\n\
             > read from a structure Lapilli recognises. This is **not** a statement that someone\n\
             > tampered with it — a truncated download and a hand-edited tar look the same from\n\
             > here. Get the bundle again before concluding anything.\n\n",
        );
    } else if has(ProblemCode::Integrity) {
        out.push_str(
            "> ## ⚠ The evidence did not verify\n>\n\
             > The files do not match the hash tree that was sealed with them. **Treat every line\n\
             > below as unverified.** It is printed because a failed bundle is exactly when you\n\
             > need to see what it claims — not because any of it can be relied on.\n\n",
        );
    } else if has(ProblemCode::Signature) {
        out.push_str(
            "> ## ⚠ The files are intact, but the signature is not\n>\n\
             > Every file matches the hash tree. What failed is the signature: it is missing,\n\
             > undeclared, invalid, or by a key other than the one you supplied. So the facts\n\
             > below are internally consistent, and **who sealed them is not established.**\n\n",
        );
    } else if has(ProblemCode::Context) {
        out.push_str(
            "> ## ⚠ This bundle is intact, but it may be about the wrong place\n>\n\
             > The bytes match what was sealed. What could **not** be re-derived is which cluster\n\
             > and incident this belongs to, so the facts below may be correctly recorded about\n\
             > something other than the incident you are reviewing. Check the identity before\n\
             > quoting any of it.\n\n",
        );
    } else {
        // A FAILED verdict always carries at least one problem, so this is unreachable today.
        // It says so rather than guessing a cause, because guessing a cause is the defect the
        // rest of this function exists to avoid.
        out.push_str(
            "> ## ⚠ This bundle did not verify\n>\n\
             > The verifier returned FAILED without a code this document knows how to describe.\n\
             > Run `lapilli verify --output json` and read the problems directly.\n\n",
        );
    }
    for p in &report.problems {
        // The code is one of six fixed strings; the message quotes what the bundle contained —
        // a file name from its hash tree, a cluster id — so it is escaped like any other value.
        out.push_str(&format!("> - `{}` — {}\n", p.code.as_str(), md(&p.message)));
    }
    out.push('\n');
}

fn header(
    out: &mut String,
    report: &VerifyReport,
    manifest: Option<&Manifest>,
    args: &PostmortemArgs,
    digest: &Digest,
) {
    let signing = match report.signature {
        SignatureStatus::Trusted => "signed:trusted-key".to_string(),
        SignatureStatus::Unpinned => {
            "signed:unpinned (no --key given, so authenticity is not established)".to_string()
        }
        SignatureStatus::Invalid => "signed:INVALID".to_string(),
        SignatureStatus::Absent => "unsigned".to_string(),
    };
    out.push_str("| | |\n|---|---|\n");
    out.push_str(&format!(
        "| Verdict | **{}** |\n",
        verdict_word(report.verdict)
    ));
    if let Some(m) = manifest {
        out.push_str(&format!("| Cluster | {} |\n", code(&m.incident.cluster_id)));
        // The rule is whoever wrote the PrometheusRule; the firing timestamp is the alert's
        // `startsAt`, which the CRD now shapes but a hand-written CR or a crafted bundle does
        // not — and when it does not parse it is also what `window.start`/`end` say
        // (`reconcile.rs`'s `window_from`), so one unparsed value reaches three cells.
        out.push_str(&format!(
            "| Trigger | {} fired at {} |\n",
            code(&m.incident.trigger.rule),
            md(&m.incident.trigger.firing_ts)
        ));
        out.push_str(&format!(
            "| Capture window | {} → {} |\n",
            md(&m.incident.window.start),
            md(&m.incident.window.end)
        ));
        out.push_str(&format!(
            "| Produced by | lapilli {} |\n",
            md(&m.producer.version)
        ));
    } else if let Some(c) = &report.cluster_id {
        out.push_str(&format!("| Cluster | {} |\n", code(c)));
    }
    out.push_str(&format!("| Signing | {signing} |\n"));
    out.push_str(&format!(
        "| Coverage | {:.0}% of intended collectors ran |\n",
        report.coverage_score * 100.0
    ));
    // Right under the percentage, because that is where 100% would otherwise be read as
    // "everything": a deferred set makes the number a fraction of less.
    if !report.deferred.is_empty() {
        out.push_str(&format!(
            "| Deferred | {} — not intended; kept elsewhere by the operator's choice |\n",
            md(&report.deferred.join(", "))
        ));
    }
    // Without this a reader cannot tell whether they hold the same bytes this document was
    // rendered from, and "self-checkable artifact" — the claim that replaced an invented
    // duration in the design — would not be true of the output.
    match digest {
        Digest::File(hex) => out.push_str(&format!("| Bundle SHA-256 | `{hex}` |\n")),
        Digest::TreeRoot(hex) => out.push_str(&format!(
            "| Hash-tree root | `{hex}` (a directory has no single artifact digest; this is the \
             root `manifest.json` commits to) |\n"
        )),
        Digest::Unavailable => {}
    }
    out.push('\n');

    // The claim this document makes that a typed wiki page cannot: every fact below can be
    // re-derived from bytes whose integrity the reader checks themselves.
    out.push_str("Reproduce this verdict yourself:\n\n");
    // A path is not trusted input either: under `lapilli mcp` the agent chooses it, and a file
    // name on Linux may hold a backtick or a newline. The fence is sized to the content rather
    // than the command being escaped, because a command that has to be un-escaped before it runs
    // is not a reproduction instruction.
    let mut cmd = format!("lapilli verify {}", args.bundle.display());
    if let Some(k) = &args.key {
        cmd.push_str(&format!(" --key {}", k.display()));
    }
    // One line, unlike the log line: a command spread over several lines is not the thing this
    // block claims to be, and a path containing a newline is not runnable either way. `fenced`
    // deals with the rest, the backticks included.
    fenced(out, &cmd.replace(['\n', '\r'], " "));
    out.push('\n');

    if report.partial {
        out.push_str(&format!(
            "**PARTIAL: {} did not run.** The sections those collectors feed are thin or absent \
             below — that is the bundle being incomplete, not the incident being quiet.\n\n",
            summary_list(&report_missing(report))
        ));
    }
}

fn captured(out: &mut String, s: &Summary, args: &PostmortemArgs) {
    out.push_str("## What was captured\n\n");
    let before = out.len();
    if let Some(c) = &s.container {
        out.push_str(&format!(
            "- Container **{}** — `resources/pod.json`\n",
            md(c)
        ));
    }
    if let Some(t) = &s.termination {
        let reason = md(t.reason.as_deref().unwrap_or("terminated"));
        let exit = t
            .exit_code
            .map(|c| format!(" (exit {c})"))
            .unwrap_or_default();
        let at = t
            .finished_at
            .as_deref()
            .map(|a| format!(", at {}", md(a)))
            .unwrap_or_default();
        out.push_str(&format!(
            "- Last terminated instance: **{reason}**{exit}{at} — `resources/pod.json` \
             `lastState.terminated`\n"
        ));
    }
    if s.restarts > 0 {
        out.push_str(&format!(
            "- Restart count **{}** — `resources/pod.json` `status.containerStatuses[].restartCount`\n",
            s.restarts
        ));
    }
    if let Some(m) = &s.memory {
        let peak = bytes(m.peak_bytes);
        match m.limit_bytes {
            Some(l) if l > 0.0 => out.push_str(&format!(
                "- Memory peaked at **{peak}** against a **{}** limit ({:.0}%, {} samples) — `metrics/`\n",
                bytes(l),
                m.peak_bytes / l * 100.0,
                m.samples
            )),
            _ => out.push_str(&format!(
                "- Memory peaked at **{peak}** ({} samples); no limit was set — `metrics/`\n",
                m.samples
            )),
        }
    }
    match (&s.last_line, args.include_log_line) {
        (Some(line), true) => {
            out.push_str(
                "\nThe log line the container died on — **workload content**, review before \
                 publishing this document:\n\n",
            );
            // The most workload-controlled string in the document, and the one that must stay
            // verbatim: `fenced` sizes the fence to the line instead of escaping it, so a line
            // containing a row of backticks cannot end the block and continue as Markdown.
            fenced(out, line);
        }
        // A tail the collector cut at its byte bound holds log lines but not the last ones, so
        // neither "is in this bundle" nor `--include-log-line` would be true here: `Summary`
        // leaves `last_line` empty in that case, and offering the flag would send a reader after
        // something the rerun cannot print. Ahead of the `Captured` arm for that reason.
        _ if s
            .log_truncated
            .as_ref()
            .is_some_and(|t| !t.kept_the_newest_lines()) =>
        {
            out.push_str(
                "- The container's log tail was **cut at the collector's byte limit**, and the end \
                 that was dropped is the one nearest the crash — so the bundle holds log lines but \
                 **not** the container's last words. Read `logs/index.json`'s `truncated` block and \
                 the file itself (`spec/IEB-SPEC.md`, the `logs/index.json` section).\n",
            );
        }
        // Only offer the flag when there is something to include. The bundle can say three
        // different things here and they are not interchangeable: the line is present, the
        // kubelet had already discarded it, or the container never terminated at all.
        (_, false) if matches!(s.last_words, lapilli_bundle::summary::LastWords::Captured) => {
            out.push_str(
                "- The container's last log line **is in this bundle** but is not printed here. \
                 Rerun with `--include-log-line` to include it, and read it before publishing \
                 this document: it is workload content.\n",
            );
        }
        _ => {
            out.push_str(&format!(
                "- The container's last log line is {}.\n",
                last_words(s)
            ));
        }
    }
    if out.len() == before {
        out.push_str("Nothing in this bundle describes the container.\n");
    }
    out.push('\n');
}

fn timeline(out: &mut String, dir: &Path) {
    out.push_str("## Timeline\n\n");
    let events: Option<Vec<Value>> = std::fs::read(dir.join("timeline.json"))
        .ok()
        .and_then(|b| serde_json::from_slice(&b).ok());
    let Some(events) = events else {
        out.push_str("`timeline.json` is not in this bundle.\n\n");
        return;
    };
    if events.is_empty() {
        out.push_str("`timeline.json` is present and empty: no events were collected.\n\n");
        return;
    }
    out.push_str(
        "Ordered by `lastTimestamp`, which is the order events were last **seen**, not the order \
         they occurred: Kubernetes coalesced repeats before Lapilli read them. `×n` is that \
         coalescing count, and `first` is when the run started — without them one restart and two \
         hundred restarts read identically.\n\n",
    );
    out.push_str("| Last seen | | Reason | Message | First seen |\n|---|---|---|---|---|\n");
    for e in events.iter().take(TIMELINE_MAX) {
        let ts = e["ts"].as_str().unwrap_or("(no timestamp)");
        let count = e["count"].as_i64().unwrap_or(1);
        let times = if count > 1 {
            format!("×{count}")
        } else {
            String::new()
        };
        let first = e["first_ts"].as_str().unwrap_or("");
        // Four of the five cells are strings the cluster wrote into `timeline.json`: the
        // timestamps, the event reason and the event message. Only `×n` is computed here.
        out.push_str(&format!(
            "| {} | {times} | {} | {} | {} |\n",
            md(ts),
            code(e["reason"].as_str().unwrap_or("")),
            md(e["message"].as_str().unwrap_or("")),
            md(first),
        ));
    }
    if events.len() > TIMELINE_MAX {
        out.push_str(&format!(
            "\n{} further events are in `timeline.json`; this table shows the first {TIMELINE_MAX}.\n",
            events.len() - TIMELINE_MAX
        ));
    }
    out.push('\n');
}

fn changed(out: &mut String, s: &Summary) {
    out.push_str("## What changed before it\n\n");
    let Some(c) = &s.change else {
        out.push_str(
            "No spec change was found in the capture window — `diffs/index.json`. That is not the \
             same as nothing having changed: only the window was searched.\n\n",
        );
        return;
    };
    // Every value in this section came out of a pod template or an annotation, and redaction does
    // not change that: a redacted value is still a string the workload chose the shape of.
    let rev = match (&c.revision_from, &c.revision_to) {
        (Some(f), Some(t)) => format!(" revision {} → {}", md(f), md(t)),
        _ => String::new(),
    };
    let when = match c.seconds_before_alert {
        Some(s) if s >= 0 => format!(", {s}s before the alert"),
        Some(s) => format!(", {}s after the alert", -s),
        None => String::new(),
    };
    let who = c
        .actor
        .as_deref()
        .map(|a| {
            format!(
                ", by {} (a field manager the client asserted, not something Lapilli observed)",
                code(a)
            )
        })
        .unwrap_or_default();
    out.push_str(&format!(
        "**{}/{}**{rev} changed{when}{who} — `diffs/index.json`.\n\n",
        md(&c.kind),
        md(&c.name)
    ));
    if let (Some(field), Some(after)) = (&c.field, &c.after) {
        out.push_str(&format!(
            "| Field | Before | After |\n|---|---|---|\n| {} | {} | {} |\n\n",
            code(field),
            code(c.before.as_deref().unwrap_or("(absent)")),
            code(after)
        ));
    }
}

fn inventory(out: &mut String, report: &VerifyReport, s: &Summary) {
    out.push_str("## Evidence inventory\n\n");
    out.push_str("| | |\n|---|---|\n");
    out.push_str(&format!(
        "| Collectors that ran | {} |\n",
        summary_list(&s.collectors_run)
    ));
    out.push_str(&format!(
        "| Collectors that did not | {} |\n",
        summary_list(&s.collectors_missing)
    ));
    out.push_str(&format!(
        "| Collectors deferred | {} |\n",
        summary_list(&s.collectors_deferred)
    ));
    out.push_str(&format!("| Events collected | {} |\n", s.events));
    out.push_str(&format!(
        "| Redaction | {} |\n",
        md(report.redaction_mode.as_deref().unwrap_or("not recorded"))
    ));
    out.push_str(&format!(
        "| Hash tree | {} |\n",
        if report.hash_ok {
            "matched every file"
        } else {
            "**did not match**"
        }
    ));
    out.push_str(&format!(
        "| Identity | {} |\n",
        if report.context_ok {
            "re-derived"
        } else {
            "**could not be re-derived**"
        }
    ));
    out.push_str("\nRedaction is best-effort by design (`DESIGN.md` §5). A clean redaction record is not a guarantee that no workload content is present.\n\n");
}

/// The sections a postmortem needs a human for, emitted empty. A blank heading is an honest
/// prompt; a filled one would be a guess, and [`DESIGN.md`](https://github.com/lapilli-project/lapilli/blob/main/DESIGN.md) §2 says this tool does not guess.
fn human(out: &mut String) {
    out.push_str(
        "---\n\n\
         *Everything above is transcribed from the bundle. Everything below is yours — Lapilli \
         does not have the information to write it.*\n\n\
         ## Impact\n\n\n## Root cause\n\n\n## Contributing factors\n\n\n## Action items\n\n",
    );
}

// -------------------------------------------------------------------------------- helpers -----

const TIMELINE_MAX: usize = 60;

fn verdict_word(v: Verdict) -> &'static str {
    match v {
        Verdict::Ok => "OK",
        Verdict::Partial => "PARTIAL",
        Verdict::Failed => "FAILED",
        Verdict::CannotEvaluate => "CANNOT_EVALUATE",
    }
}

fn last_words(s: &Summary) -> &'static str {
    match s.last_words {
        lapilli_bundle::summary::LastWords::Captured => "in the bundle",
        lapilli_bundle::summary::LastWords::Discarded => {
            "**not in the bundle** — the kubelet had already discarded it when Lapilli asked"
        }
        lapilli_bundle::summary::LastWords::None => "not applicable: no terminated instance",
    }
}

/// Collector names as read from the bundle's `coverage` — and a `coverage` is a list of strings a
/// producer wrote, not an enum, so each one is a code span built by [`code`].
fn summary_list(v: &[String]) -> String {
    if v.is_empty() {
        "none".to_string()
    } else {
        v.iter().map(|s| code(s)).collect::<Vec<_>>().join(", ")
    }
}

fn report_missing(report: &VerifyReport) -> Vec<String> {
    // The PARTIAL problem's code is `partial` (spec/VERIFY-RESULT.md). This filtered on a code
    // that does not exist, so the banner read "PARTIAL: none did not run" on every partial bundle.
    report
        .problems
        .iter()
        .filter(|p| p.code == lapilli_bundle::ProblemCode::Partial)
        .map(|p| p.message.clone())
        .collect()
}

// --------------------------------------------------------- untrusted values in Markdown -----
//
// Everything in this document that is not a fixed string came out of a bundle, and a bundle's
// strings were chosen by whoever named the alert rule, wrote the pod template, or emitted the
// event. This document is worse to inject than the chat message `notify.rs` builds: it is
// permanent, it is pasted into a wiki, and `lapilli mcp`'s `postmortem` tool hands it to an LLM
// as context. A value that can end a table row can write its own `## Root cause`.
//
// So every such value goes through [`md`] (Markdown text) or [`code`] (a code span) — at the
// leaf, exactly once. Double-escaping is its own defect: `notify.rs` shipped it once and has a
// test for it, and the equivalent here is `no_value_is_escaped_twice`.

/// Characters that are not drawn, or that move the cursor. A control character (which includes
/// the newline that would end a table row, the CR that would do it on its own, and the ESC that
/// starts an ANSI sequence), the two Unicode line separators, the bidi overrides and isolates,
/// the zero-width marks and the BOM.
///
/// The same set `notify.rs` flattens, for the same reason. Not shared with it because that code
/// lives in the controller *binary*, so there is nothing for the CLI to import; the only way to
/// share it is to move it into `lapilli-bundle`, which is a change to the verifier's crate and not
/// one to make in a security fix. If the two ever disagree, they are the same bug twice.
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

/// One untrusted value, ready to sit in Markdown **text**: a table cell, bold text, a list item,
/// a heading.
///
/// Backslash escapes rather than HTML entities, so the raw document stays readable — `\|` renders
/// as `|` — and only the punctuation that can change structure or misrepresent the bytes is
/// touched: the table cell separator, the backslash that would escape it, the code-span mark, the
/// link brackets, and `<` and `&`, which open raw HTML and an entity (`&amp;` in a log line has to
/// render as `&amp;`, not as `&`, or the document quotes something the workload did not write).
///
/// `*` and `_` are deliberately **not** escaped, which is `notify.rs`'s stance on the same values:
/// emphasis cannot cross a cell boundary or start a block, so the worst it does is make a word
/// italic — and the kubelet writes `pod_namespace(uid)` into event messages, so escaping `_` puts
/// backslashes through the most-read cell in the document for nothing.
///
/// A timestamp, a pod name, an event message and a `Deployment/checkout` therefore come through
/// unchanged, which is the point: an escape that mangles ordinary values is a worse document than
/// the injection it prevents.
///
/// Block-level injection (`## Root cause`, `> `, `- `) needs a line start, and there is none
/// left: every invisible character, newline included, is flattened to a space first.
fn md(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            c if invisible(c) => out.push(' '),
            '\\' | '`' | '[' | ']' | '<' | '>' | '&' | '|' => {
                out.push('\\');
                out.push(c);
            }
            c => out.push(c),
        }
    }
    out
}

/// One untrusted value as a Markdown **code span**, backticks included.
///
/// The span is opened with one more backtick than the longest run inside the value (CommonMark
/// 6.1), so a value full of backticks cannot close it — and nothing inside needs escaping, so a
/// field path or an env var reads exactly as it does in the cluster. The exception is `|`, which
/// GFM splits a table cell on *even inside a code span*, and which GFM therefore lets a
/// backslash escape there.
fn code(s: &str) -> String {
    let flat: String = s
        .chars()
        .map(|c| if invisible(c) { ' ' } else { c })
        .collect();
    let flat = flat.replace('|', "\\|");
    if flat.is_empty() {
        // What this rendered before there was a function for it, for a value the bundle left
        // empty: an empty code span, not a stray pair of backticks with a cell's worth of text
        // swallowed after it.
        return "``".to_string();
    }
    let ticks = "`".repeat(backtick_run(&flat) + 1);
    // A span that begins or ends with a backtick needs one space of padding on *both* sides;
    // the renderer strips exactly that pair again.
    let pad = if flat.starts_with('`') || flat.ends_with('`') {
        " "
    } else {
        ""
    };
    format!("{ticks}{pad}{flat}{pad}{ticks}")
}

/// The longest run of consecutive backticks in `s`.
fn backtick_run(s: &str) -> usize {
    let mut best = 0;
    let mut run = 0;
    for c in s.chars() {
        if c == '`' {
            run += 1;
            best = best.max(run);
        } else {
            run = 0;
        }
    }
    best
}

/// Write `body` as a fenced block whose fence nothing in `body` can close early.
///
/// Two values reach this and neither may be escaped: the container's last log line, which is
/// evidence and has to read as the workload wrote it, and the `lapilli verify` command, which has
/// to be copy-pasteable. A fence longer than any backtick run inside keeps both verbatim. What is
/// still flattened is what is not drawn — an ANSI escape repaints a terminal-rendered document
/// and a bidi override reverses it — with newlines left alone, because a code block may have them.
fn fenced(out: &mut String, body: &str) {
    let fence = "`".repeat(backtick_run(body).max(2) + 1);
    out.push_str(&fence);
    out.push('\n');
    for c in body.chars() {
        out.push(if c != '\n' && invisible(c) { ' ' } else { c });
    }
    if !body.ends_with('\n') {
        out.push('\n');
    }
    out.push_str(&fence);
    out.push('\n');
}

fn bytes(b: f64) -> String {
    const UNITS: [&str; 4] = ["B", "KiB", "MiB", "GiB"];
    let mut v = b;
    let mut u = 0;
    while v >= 1024.0 && u < UNITS.len() - 1 {
        v /= 1024.0;
        u += 1;
    }
    if u == 0 {
        format!("{v:.0} {}", UNITS[u])
    } else {
        format!("{v:.1} {}", UNITS[u])
    }
}

#[cfg(test)]
mod injection {
    use super::*;
    use lapilli_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger};
    use lapilli_bundle::summary::{Change, LastWords, Termination};
    use lapilli_bundle::{HashTree, Problem, ProblemCode};

    /// Everything a forged alert or a workload can put in a string, in one value: the pipe that
    /// splits a table cell, the newline that ends the row, a heading that would claim the section
    /// the document leaves for a human, a backtick pair, a link, an ANSI colour escape, a Unicode
    /// line separator and raw HTML.
    const FORGED: &str = "boom | x\n\n## Root cause\n\nthe database was fine\n\n\
                          `verified` [click](http://evil.example) \u{1b}[31mred\u{1b}[0m\
                          \u{2028}<b>bold</b> & more";

    /// Unescaped `|` — what GFM splits a table row on. A `\|` does not count, inside a code span
    /// or out of one (GFM tables §4.10).
    fn pipes(line: &str) -> usize {
        let mut n = 0;
        let mut esc = false;
        for c in line.chars() {
            if esc {
                esc = false;
                continue;
            }
            match c {
                '\\' => esc = true,
                '|' => n += 1,
                _ => {}
            }
        }
        n
    }

    fn manifest(rule: &str, firing: &str) -> Manifest {
        Manifest {
            schema_version: lapilli_bundle::manifest::SCHEMA_VERSION.to_string(),
            incident: IncidentIdentity {
                id: "kind-lapilli-1a2b3c4d".into(),
                cluster_id: "kind-lapilli".into(),
                trigger: Trigger {
                    rule: rule.into(),
                    firing_ts: firing.into(),
                },
                window: lapilli_bundle::Window {
                    start: firing.into(),
                    end: firing.into(),
                },
                target: None,
            },
            producer: Producer {
                version: "0.1.0".into(),
                image_digest: "sha256:0".into(),
            },
            signing: None,
            hash_tree: HashTree {
                files: Default::default(),
                root: "0".repeat(64),
            },
            coverage: Coverage {
                collectors_run: vec!["logs".into()],
                collectors_intended: vec!["logs".into()],
                deferred: Vec::new(),
            },
            timing: Timing {
                capture_started: firing.into(),
                sealed_at: firing.into(),
                capture_to_seal_ms: 1,
            },
        }
    }

    fn report() -> VerifyReport {
        VerifyReport {
            verdict: Verdict::Ok,
            format: Some(lapilli_bundle::manifest::SCHEMA_VERSION.into()),
            producer: Some("0.1.0".into()),
            cluster_id: Some("kind-lapilli".into()),
            incident_id: Some("kind-lapilli-1a2b3c4d".into()),
            hash_ok: true,
            context_ok: true,
            coverage_score: 1.0,
            partial: false,
            signature: SignatureStatus::Absent,
            problems: Vec::new(),
            redaction_mode: Some("default".into()),
            collectors_run: vec!["logs".into()],
            collectors_intended: vec!["logs".into()],
            deferred: Vec::new(),
        }
    }

    fn args() -> PostmortemArgs {
        PostmortemArgs {
            bundle: PathBuf::from("/var/lib/lapilli/bundles/x.ieb"),
            key: None,
            include_log_line: false,
        }
    }

    /// A postmortem is read in a terminal as often as in a wiki, and round 33's untrusted-input
    /// lens claimed OSC 8 (hyperlink) and OSC 52 (clipboard write) "pass through `md()` untouched".
    /// They do not: an OSC sequence opens with ESC and closes with BEL or ESC-backslash, and
    /// `invisible` begins with `char::is_control`, which is both. Kept as a test rather than a
    /// note, because the claim is plausible enough that somebody will raise it again.
    #[test]
    fn an_osc_sequence_cannot_reach_a_terminal_through_md() {
        let osc8 = "\u{1b}]8;;https://evil.example\u{1b}\\click\u{1b}]8;;\u{1b}\\";
        let osc52 = "\u{1b}]52;c;cm0gLXJmIC8=\u{7}";
        for hostile in [osc8, osc52] {
            let out = md(hostile);
            assert!(
                !out.contains('\u{1b}'),
                "an ESC reached the document: {out:?}"
            );
            assert!(
                !out.contains('\u{7}'),
                "a BEL reached the document: {out:?}"
            );
        }
    }

    /// The whole point of escaping at all: an ordinary bundle must read exactly as it did before.
    /// An escape that turns `checkout-7d9f8b-zx4q2` into `checkout\-7d9f8b\-zx4q2`, or a field
    /// path into a line of backslashes, is a worse document than the injection it prevents.
    #[test]
    fn an_ordinary_value_is_not_touched() {
        for s in [
            "checkout-7d9f8b-zx4q2",
            "2026-09-17T02:14:33.123456789Z",
            "Deployment/checkout",
            "OOMKilled",
            "BackOff",
            "spec.template.spec.containers.0.image",
            "ghcr.io/acme/checkout:v1.4.2@sha256:abc",
            "kube-controller-manager",
            "Readiness probe failed: HTTP probe failed with statuscode: 503",
            // What the kubelet actually writes into an event message, underscore and all.
            "Back-off restarting failed container app in pod checkout-7d9f8b_shop(3f8a1c2e)",
            "CACHE_WARMUP_TIMEOUT_SECONDS",
            "memory pressure *now*",
            "메모리 부족",
        ] {
            assert_eq!(md(s), s, "md() changed an ordinary value");
            assert_eq!(
                code(s),
                format!("`{s}`"),
                "code() changed an ordinary value"
            );
        }
        // A field path with array indices: brackets are escaped in text, and left alone inside a
        // code span — which is where the renderer puts them, because a backslash inside a code
        // span is drawn rather than applied.
        let path = "spec.containers[0].env[3].value";
        assert_eq!(code(path), format!("`{path}`"));
        assert_eq!(md(path), "spec.containers\\[0\\].env\\[3\\].value");
    }

    /// The finding (F3): every value the timeline, the header and the diff table interpolate is
    /// alert- or workload-controlled, and only the event message was escaped. One forged value
    /// wrote its own `## Root cause` into the section the document reserves for a human.
    #[test]
    fn a_forged_value_cannot_break_out_of_the_table_it_sits_in() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("timeline.json"),
            serde_json::to_vec(&serde_json::json!([{
                "ts": FORGED, "first_ts": FORGED, "count": 2,
                "reason": FORGED, "message": FORGED,
            }]))
            .unwrap(),
        )
        .unwrap();

        let mut out = String::new();
        header(
            &mut out,
            &report(),
            Some(&manifest(FORGED, FORGED)),
            &args(),
            &Digest::Unavailable,
        );
        timeline(&mut out, dir.path());
        let mut s = Summary {
            container: Some(FORGED.into()),
            termination: Some(Termination {
                reason: Some(FORGED.into()),
                exit_code: Some(137),
                finished_at: Some(FORGED.into()),
            }),
            change: Some(Change {
                kind: FORGED.into(),
                name: FORGED.into(),
                revision_from: Some(FORGED.into()),
                revision_to: Some(FORGED.into()),
                seconds_before_alert: Some(42),
                actor: Some(FORGED.into()),
                field: Some(FORGED.into()),
                before: Some(FORGED.into()),
                after: Some(FORGED.into()),
            }),
            collectors_run: vec![FORGED.into()],
            ..Default::default()
        };
        s.last_words = LastWords::None;
        changed(&mut out, &s);
        captured(&mut out, &s, &args());
        inventory(&mut out, &report(), &s);

        // Not one of the forged values may become a line of its own — which is what every
        // block-level construct needs.
        assert!(
            !out.lines()
                .any(|l| l.trim_start().starts_with("## Root cause")),
            "a value wrote its own heading:\n{out}"
        );
        assert!(
            !out.contains("the database was fine\n"),
            "a value reached a line of its own:\n{out}"
        );
        // Nothing that is not drawn survives: the ANSI escape that repaints a terminal-rendered
        // document, and the line separator that a Markdown renderer treats as a line break.
        for bad in ['\u{1b}', '\u{2028}', '\r'] {
            assert!(
                !out.contains(bad),
                "U+{:04X} reached the document:\n{out}",
                bad as u32
            );
        }
        // Every table row still has the number of cells its header declares.
        for line in out.lines().filter(|l| l.starts_with('|')) {
            let n = pipes(line);
            assert!(
                (3..=6).contains(&n),
                "a row grew to {n} cells: {line:?}\n{out}"
            );
        }
        // The forged code span cannot close the span it was put in: the opener is widened.
        assert!(out.contains("``"), "no widened code span in:\n{out}");
    }

    /// The mistake `notify.rs` made and has a test for: escaping a value that was already
    /// escaped, which turns a real `&` into `&amp;amp;` — here, a real `|` into `\\|`.
    #[test]
    fn no_value_is_escaped_twice() {
        let mut out = String::new();
        header(
            &mut out,
            &report(),
            Some(&manifest("Kube|Crash", "2026-09-17T02:14:33Z")),
            &args(),
            &Digest::Unavailable,
        );
        assert!(out.contains("`Kube\\|Crash`"), "{out}");
        assert!(!out.contains("\\\\|"), "escaped twice:\n{out}");

        let mut out = String::new();
        let s = Summary {
            change: Some(Change {
                kind: "Deployment".into(),
                name: "a&b".into(),
                revision_from: None,
                revision_to: None,
                seconds_before_alert: None,
                actor: None,
                field: None,
                before: None,
                after: None,
            }),
            ..Default::default()
        };
        changed(&mut out, &s);
        assert!(out.contains("a\\&b"), "{out}");
        assert!(!out.contains("a\\\\&b"), "escaped twice:\n{out}");
    }

    /// A code span is closed by a run of backticks as long as the one that opened it, so a value
    /// full of backticks needs a longer opener — not a backslash, which a code span draws rather
    /// than applies.
    #[test]
    fn a_code_span_is_widened_past_the_backticks_inside_it() {
        assert_eq!(code("a`b"), "``a`b``");
        assert_eq!(code("a``b"), "```a``b```");
        assert_eq!(code("`"), "`` ` ``");
        assert_eq!(code("`a`"), "`` `a` ``");
        assert_eq!(code(""), "``");
        assert_eq!(code("a|b"), "`a\\|b`");
    }

    /// The log line is evidence and stays verbatim, so it is fenced rather than escaped — and a
    /// fence has to be longer than any run of backticks in what it holds, or the line closes it
    /// and continues as Markdown.
    #[test]
    fn the_log_line_cannot_close_its_own_fence() {
        let mut out = String::new();
        let s = Summary {
            last_line: Some("FATAL ```\n## Root cause\n\nnot the cache\n```".into()),
            last_words: LastWords::Captured,
            ..Default::default()
        };
        captured(
            &mut out,
            &s,
            &PostmortemArgs {
                bundle: PathBuf::from("/x.ieb"),
                key: None,
                include_log_line: true,
            },
        );
        assert!(out.contains("````\n"), "the fence was not widened:\n{out}");
        // Inside the block the bytes are the workload's, unescaped.
        assert!(out.contains("FATAL ```"), "{out}");
        // And the block is still closed exactly once, by the widened fence.
        assert_eq!(out.matches("````").count(), 2, "{out}");
        // An ANSI escape is the exception: it is not drawn, it repaints.
        let mut out = String::new();
        let s = Summary {
            last_line: Some("FATAL \u{1b}[2J\u{1b}[H lapilli verify: OK".into()),
            last_words: LastWords::Captured,
            ..Default::default()
        };
        captured(
            &mut out,
            &s,
            &PostmortemArgs {
                bundle: PathBuf::from("/x.ieb"),
                key: None,
                include_log_line: true,
            },
        );
        assert!(!out.contains('\u{1b}'), "{out}");
    }

    /// The bundle path is chosen by whoever runs the command — and under `lapilli mcp` that is an
    /// agent, working on file names a cluster wrote.
    #[test]
    fn the_reproduce_command_cannot_close_its_own_fence() {
        let mut out = String::new();
        header(
            &mut out,
            &report(),
            None,
            &PostmortemArgs {
                bundle: PathBuf::from("/tmp/a```\n## Root cause\n\nb.ieb"),
                key: None,
                include_log_line: false,
            },
            &Digest::Unavailable,
        );
        assert!(out.contains("````\n"), "the fence was not widened:\n{out}");
        assert!(
            !out.lines().any(|l| l.starts_with("## Root cause")),
            "{out}"
        );
    }

    /// The banner quotes the verifier's problem messages, and those quote what was in the bundle:
    /// a file name from its hash tree, a cluster id it claimed.
    #[test]
    fn a_problem_message_cannot_write_a_heading_into_the_banner() {
        let mut r = report();
        r.verdict = Verdict::Failed;
        r.problems = vec![Problem {
            code: ProblemCode::Integrity,
            message: format!("file does not match: {FORGED}"),
        }];
        let mut out = String::new();
        banner(&mut out, &r);
        for line in out.lines() {
            assert!(
                line.is_empty() || line.starts_with('>'),
                "a problem message escaped the blockquote: {line:?}\n{out}"
            );
        }
    }
}
