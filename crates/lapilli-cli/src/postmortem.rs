//! `lapilli postmortem` — the draft a human then writes (`docs/design-postmortem.md`).
//!
//! It **transcribes**. Every line is a value that exists in the bundle, followed by the file it
//! came from, and the sections a postmortem needs a human for are emitted empty, with their
//! headings, because a blank heading is an honest prompt and a filled one would be a guess
//! (`DESIGN.md` §2: not an auto-RCA narrative).
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
    out.push_str(&format!("# Incident {incident}\n\n"));
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
        out.push_str(&format!("> - `{}` — {}\n", p.code.as_str(), p.message));
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
        out.push_str(&format!("| Cluster | `{}` |\n", m.incident.cluster_id));
        out.push_str(&format!(
            "| Trigger | `{}` fired at {} |\n",
            m.incident.trigger.rule, m.incident.trigger.firing_ts
        ));
        out.push_str(&format!(
            "| Capture window | {} → {} |\n",
            m.incident.window.start, m.incident.window.end
        ));
        out.push_str(&format!(
            "| Produced by | lapilli {} |\n",
            m.producer.version
        ));
    } else if let Some(c) = &report.cluster_id {
        out.push_str(&format!("| Cluster | `{c}` |\n"));
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
            report.deferred.join(", ")
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
    out.push_str("Reproduce this verdict yourself:\n\n```\n");
    out.push_str(&format!("lapilli verify {}", args.bundle.display()));
    if let Some(k) = &args.key {
        out.push_str(&format!(" --key {}", k.display()));
    }
    out.push_str("\n```\n\n");

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
        out.push_str(&format!("- Container **{c}** — `resources/pod.json`\n"));
    }
    if let Some(t) = &s.termination {
        let reason = t.reason.as_deref().unwrap_or("terminated");
        let code = t
            .exit_code
            .map(|c| format!(" (exit {c})"))
            .unwrap_or_default();
        let at = t
            .finished_at
            .as_deref()
            .map(|a| format!(", at {a}"))
            .unwrap_or_default();
        out.push_str(&format!(
            "- Last terminated instance: **{reason}**{code}{at} — `resources/pod.json` \
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
                 publishing this document:\n\n```\n",
            );
            out.push_str(line);
            out.push_str("\n```\n");
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
        out.push_str(&format!(
            "| {ts} | {times} | `{}` | {} | {first} |\n",
            e["reason"].as_str().unwrap_or(""),
            cell(e["message"].as_str().unwrap_or("")),
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
    let rev = match (&c.revision_from, &c.revision_to) {
        (Some(f), Some(t)) => format!(" revision {f} → {t}"),
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
                ", by `{a}` (a field manager the client asserted, not something Lapilli observed)"
            )
        })
        .unwrap_or_default();
    out.push_str(&format!(
        "**{}/{}**{rev} changed{when}{who} — `diffs/index.json`.\n\n",
        c.kind, c.name
    ));
    if let (Some(field), Some(after)) = (&c.field, &c.after) {
        out.push_str(&format!(
            "| Field | Before | After |\n|---|---|---|\n| `{}` | `{}` | `{}` |\n\n",
            field,
            c.before.as_deref().unwrap_or("(absent)"),
            after
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
        report.redaction_mode.as_deref().unwrap_or("not recorded")
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
/// prompt; a filled one would be a guess, and `DESIGN.md` §2 says this tool does not guess.
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

fn summary_list(v: &[String]) -> String {
    if v.is_empty() {
        "none".to_string()
    } else {
        v.iter()
            .map(|s| format!("`{s}`"))
            .collect::<Vec<_>>()
            .join(", ")
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

/// A pipe inside a value would split the markdown table cell it sits in, and a newline would end
/// the row. Both come from workload text, so neither is hypothetical.
fn cell(s: &str) -> String {
    let flat: String = s
        .chars()
        .map(|c| if c == '\n' || c == '\r' { ' ' } else { c })
        .collect();
    flat.replace('|', "\\|")
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
