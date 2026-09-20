//! `kairn verify`: local files and directories, bucket objects, and the two renderings of
//! the result, human text (not stable) and `--output json` (`kairn.dev/verify-result/v1`,
//! stable: docs/COMPATIBILITY.md §2).

use std::path::PathBuf;

use kairn_bundle::{
    verify_bundle, verify_reader, Problem, ProblemCode, SignatureStatus, Verdict, VerifyOptions,
    VerifyReport, VERIFY_MAX_BYTES,
};
use serde_json::{json, Value};

use crate::remote::{self, History};

/// The JSON document's schema id. Additive changes only within v1.
pub(crate) const RESULT_SCHEMA: &str = "kairn.dev/verify-result/v1";

#[derive(clap::Args)]
pub(crate) struct VerifyArgs {
    /// A `.ieb` file, an unpacked bundle directory (containing manifest.json), or
    /// s3://bucket/key, gs://bucket/key, https://… (a presigned URL). Credentials come
    /// from the environment: AWS_* variables (for a profile or SSO, first run
    /// `eval "$(aws configure export-credentials --format env)"`), or
    /// GOOGLE_APPLICATION_CREDENTIALS / gcloud application-default login.
    bundle: PathBuf,
    /// Expected cluster id (fail-closed on mismatch).
    #[arg(long)]
    cluster: Option<String>,
    /// Expected incident id (fail-closed on mismatch).
    #[arg(long)]
    incident: Option<String>,
    /// Trusted public key (SPKI PEM) of the producer, obtained out of band. When given,
    /// the bundle must be signed by it; without it a signature proves nothing about who
    /// sealed the bundle (reported as `signed:unpinned`).
    #[arg(long)]
    key: Option<PathBuf>,
    /// The SHA-256 the bundle file or object must have (e.g. from
    /// `status.exports.<name>.sha256`); FAILED on mismatch.
    #[arg(long, value_name = "HEX")]
    expect_sha256: Option<String>,
    /// s3:// only: verify this version of the object instead of the current one
    /// (e.g. `status.exports.<name>.versionId`).
    #[arg(long)]
    version_id: Option<String>,
    /// s3:// only: skip the version history check (which needs s3:ListBucketVersions).
    /// Without it, a key written more than once is FAILED.
    #[arg(long)]
    current_only: bool,
    /// For s3:// and gs://, don't expect the cluster and incident named by the object
    /// key (`<prefix>/<cluster>/<incident>.ieb`, Kairn's export layout).
    #[arg(long)]
    any_key: bool,
    /// `text` (for people; not stable) or `json` (one `kairn.dev/verify-result/v1`
    /// document on stdout, nothing on stderr; stable). Usage errors (exit 64) are always
    /// text on stderr.
    #[arg(long, value_enum, default_value_t = Output::Text)]
    output: Output,
}

#[derive(Clone, Copy, PartialEq, Eq, clap::ValueEnum)]
pub(crate) enum Output {
    Text,
    Json,
}

/// Where the bytes came from and what is known about them.
struct Input {
    kind: &'static str,
    location: String,
    size: Option<u64>,
    sha256: Option<String>,
    version_id: Option<String>,
    history: Option<History>,
}

/// What the bundle was expected to be, and where that expectation came from.
struct Expected {
    cluster: Option<String>,
    incident: Option<String>,
    source: &'static str,
}

/// Everything either rendering needs. `report` is `None` when nothing could be evaluated
/// (unreadable input); `problems` then holds why.
struct Outcome {
    input: Input,
    expected: Expected,
    report: Option<VerifyReport>,
    verdict: Verdict,
    problems: Vec<Problem>,
}

/// Run `kairn verify`; returns the exit code.
pub(crate) fn run(a: VerifyArgs) -> u8 {
    let usage = |msg: &str| {
        eprintln!("{msg}");
        crate::EXIT_USAGE
    };
    let expect_sha256 = match a.expect_sha256.map(|h| h.to_ascii_lowercase()) {
        Some(h) if h.len() != 64 || !h.bytes().all(|b| b.is_ascii_hexdigit()) => {
            return usage("--expect-sha256 takes 64 hex digits");
        }
        h => h,
    };
    let source = match a.bundle.to_str().and_then(remote::parse) {
        None if a.version_id.is_some() || a.current_only || a.any_key => {
            return usage("--version-id, --current-only and --any-key apply to bucket URLs only");
        }
        None => None,
        Some(Ok(source)) => {
            let s3 = matches!(source, remote::Source::S3 { .. });
            if (a.version_id.is_some() || a.current_only) && !s3 {
                return usage("--version-id and --current-only are supported for s3:// only");
            }
            if a.any_key && matches!(source, remote::Source::Https { .. }) {
                return usage(
                    "--any-key applies to s3:// and gs:// (https:// URLs are never matched \
                     against their path)",
                );
            }
            Some(source)
        }
        Some(Err(e)) => return usage(&e),
    };
    if source.is_none() && expect_sha256.is_some() && a.bundle.is_dir() {
        return usage("--expect-sha256 needs a .ieb file, not a directory");
    }

    let flags_identity = a.cluster.is_some() || a.incident.is_some();
    let mut opts = VerifyOptions {
        expected_cluster: a.cluster,
        expected_incident: a.incident,
        trusted_key_pem: None,
    };
    let from_key = match &source {
        Some(s) if !a.any_key => s.key_identity(),
        _ => None,
    };
    if let Some((c, i)) = &from_key {
        opts.expected_cluster.get_or_insert_with(|| c.clone());
        opts.expected_incident.get_or_insert_with(|| i.clone());
    }
    let expected = Expected {
        cluster: opts.expected_cluster.clone(),
        incident: opts.expected_incident.clone(),
        source: match (flags_identity, from_key.is_some()) {
            (true, true) => "flags+object-key",
            (true, false) => "flags",
            (false, true) => "object-key",
            (false, false) => "none",
        },
    };
    let input = match &source {
        None => Input {
            kind: if a.bundle.is_dir() {
                "directory"
            } else {
                "file"
            },
            location: a.bundle.display().to_string(),
            size: None,
            sha256: None,
            version_id: None,
            history: None,
        },
        Some(s) => Input {
            kind: s.kind(),
            location: s.display(),
            size: None,
            sha256: None,
            version_id: a.version_id.clone(),
            history: None,
        },
    };

    // The trusted key is the operator's input: a key that can't be used says nothing about
    // the bundle, so it is "cannot evaluate", never a signature failure.
    let key = a.key.map(|path| {
        std::fs::read_to_string(&path)
            .map_err(|e| e.to_string())
            .and_then(|pem| {
                let id = kairn_bundle::sign::key_id(&pem)
                    .map_err(|e| format!("not a usable public key: {e}"))?;
                // A file Kairn archived is named by its own key id, so the name and the content
                // check each other. A mismatch means this file is not the key it claims to be —
                // refuse rather than verify a bundle against a key that was swapped under a
                // familiar name. Ordinary names (`kairn.pub`) are unaffected.
                if let Some(stem) = path.file_stem().and_then(|s| s.to_str()) {
                    let looks_archived = stem.len() == 64
                        && stem
                            .bytes()
                            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase());
                    if looks_archived && stem != id {
                        return Err(format!(
                            "the file is named for key id {stem} but holds {id}"
                        ));
                    }
                }
                Ok(pem)
            })
            .map_err(|e| format!("--key {}: {e}", path.display()))
    });
    let mut outcome = match key.transpose() {
        Err(e) => unreadable(input, expected, Problem::new(ProblemCode::Unreadable, e)),
        Ok(pem) => {
            opts.trusted_key_pem = pem;
            match &source {
                None => {
                    let hash = if expect_sha256.is_some() {
                        Hash::Required
                    } else if a.output == Output::Json {
                        Hash::IfBundle
                    } else {
                        Hash::No
                    };
                    verify_local(&a.bundle, &opts, hash, input, expected)
                }
                Some(source) => {
                    verify_remote(source, a.version_id, a.current_only, &opts, input, expected)
                }
            }
        }
    };

    if let (Some(want), Some(got)) = (&expect_sha256, outcome.input.sha256.clone()) {
        if *want != got {
            // A fact about the input, whatever its format: decisive.
            outcome.fail(Problem::new(
                ProblemCode::Digest,
                format!("sha256 {got} is not the expected {want}"),
            ));
        }
    }

    let code = outcome.verdict.exit_code() as u8;
    match a.output {
        Output::Text => print_text(&outcome, source.is_some(), a.any_key),
        Output::Json => {
            use std::io::Write;
            // One document or none: if stdout is gone, nobody got a result.
            let mut out = std::io::stdout().lock();
            if writeln!(out, "{}", to_json(&outcome))
                .and_then(|()| out.flush())
                .is_err()
            {
                return EXIT_NO_RESULT;
            }
        }
    }
    code
}

/// Exit code when the JSON document could not be written (stdout closed).
const EXIT_NO_RESULT: u8 = 3;

impl Outcome {
    /// A decisive failure: FAILED, whatever else is known (precedence FAILED >
    /// CANNOT_EVALUATE > PARTIAL > OK, spec/VERIFY-RESULT.md).
    fn fail(&mut self, problem: Problem) {
        self.problems.push(problem);
        self.verdict = Verdict::Failed;
        if let Some(r) = &mut self.report {
            r.verdict = Verdict::Failed;
        }
    }

    /// Something needed for a positive answer is unknown: CANNOT_EVALUATE, unless the
    /// bundle already FAILED (a known failure is never hidden behind "cannot evaluate").
    fn unresolved(&mut self, problem: Problem) {
        self.problems.push(problem);
        if self.verdict != Verdict::Failed {
            self.verdict = Verdict::CannotEvaluate;
            if let Some(r) = &mut self.report {
                r.verdict = Verdict::CannotEvaluate;
            }
        }
    }
}

fn unreadable(input: Input, expected: Expected, problem: Problem) -> Outcome {
    Outcome {
        input,
        expected,
        report: None,
        verdict: if problem.code == ProblemCode::Custody {
            Verdict::Failed
        } else {
            Verdict::CannotEvaluate
        },
        problems: vec![problem],
    }
}

fn from_report(input: Input, expected: Expected, report: VerifyReport) -> Outcome {
    Outcome {
        input,
        expected,
        verdict: report.verdict,
        problems: report.problems.clone(),
        report: Some(report),
    }
}

/// Whether to hash a local file to its end.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Hash {
    No,
    /// `--expect-sha256`: always (up to the byte limit).
    Required,
    /// For the JSON report: only when the format's rules ran. A stream that isn't a bundle
    /// (`/dev/zero`, a truncated file) is not read to the limit just to report its digest.
    IfBundle,
}

/// A local `.ieb` is opened **once**: the bytes that are hashed are the bytes that are
/// verified (no second open that a rename or a FIFO could feed differently), under the
/// same byte limit as a remote object.
fn verify_local(
    path: &std::path::Path,
    opts: &VerifyOptions,
    hash: Hash,
    mut input: Input,
    expected: Expected,
) -> Outcome {
    if path.is_dir() {
        return match verify_bundle(path, opts) {
            Ok(report) => from_report(input, expected, report),
            Err(e) => unreadable(
                input,
                expected,
                Problem::new(ProblemCode::Unreadable, e.to_string()),
            ),
        };
    }
    let file = match std::fs::File::open(path) {
        Ok(f) => f,
        Err(e) => {
            return unreadable(
                input,
                expected,
                Problem::new(ProblemCode::Unreadable, e.to_string()),
            )
        }
    };
    let mut body = remote::Body::new(file, VERIFY_MAX_BYTES);
    let report = verify_reader(&mut body, opts);
    if let Some(problem) = body.failure.take() {
        // A read error or the byte limit while verifying: the verdict above is about a
        // truncated stream, not the bundle.
        return unreadable(input, expected, problem);
    }
    let over_limits = report.verdict == Verdict::CannotEvaluate && report.format.is_none();
    let hash = match hash {
        Hash::No => false,
        Hash::Required => true,
        Hash::IfBundle => report.producer.is_some(),
    };
    if !hash || over_limits {
        return from_report(input, expected, report);
    }
    body.drain();
    let mut outcome;
    match body.failure.take() {
        None => {
            input.size = Some(body.bytes_read());
            input.sha256 = Some(body.sha256());
            outcome = from_report(input, expected, report);
        }
        Some(problem) => {
            outcome = from_report(input, expected, report);
            outcome.unresolved(problem);
        }
    }
    outcome
}

#[cfg(feature = "remote")]
fn verify_remote(
    source: &remote::Source,
    version_id: Option<String>,
    current_only: bool,
    opts: &VerifyOptions,
    mut input: Input,
    expected: Expected,
) -> Outcome {
    match remote::fetch_and_verify(source, version_id, current_only, opts) {
        Ok(fetched) => {
            let object = fetched.object;
            input.size = Some(object.size);
            input.sha256 = Some(object.sha256);
            input.version_id = object.version;
            input.history = Some(object.history);
            let mut outcome = from_report(input, expected, fetched.report);
            if let Some(p) = fetched.unresolved {
                outcome.unresolved(p);
            }
            outcome
        }
        Err(nf) => {
            input.history = nf.history;
            unreadable(input, expected, nf.problem)
        }
    }
}

#[cfg(not(feature = "remote"))]
fn verify_remote(
    _source: &remote::Source,
    _version_id: Option<String>,
    _current_only: bool,
    _opts: &VerifyOptions,
    input: Input,
    expected: Expected,
) -> Outcome {
    unreadable(
        input,
        expected,
        Problem::new(
            ProblemCode::Unreadable,
            "this kairn was built without remote support (the `remote` feature)",
        ),
    )
}

fn verdict_str(v: Verdict) -> &'static str {
    match v {
        Verdict::Ok => "OK",
        Verdict::Partial => "PARTIAL",
        Verdict::Failed => "FAILED",
        Verdict::CannotEvaluate => "CANNOT_EVALUATE",
    }
}

fn signature_str(s: SignatureStatus) -> &'static str {
    match s {
        SignatureStatus::Trusted => "trusted",
        SignatureStatus::Unpinned => "unpinned",
        SignatureStatus::Invalid => "invalid",
        SignatureStatus::Absent => "absent",
    }
}

/// The `kairn.dev/verify-result/v1` document. Every member is always present (null when
/// unknown or not evaluated), so consumers never guess between "absent" and "false".
fn to_json(o: &Outcome) -> Value {
    let history = o.input.history.as_ref().map(|h| {
        let (state, versions, markers, latest, truncated, reason) = match h {
            History::Listed {
                versions,
                delete_markers,
                fetched_is_latest,
                truncated,
            } => (
                "listed",
                json!(versions),
                json!(delete_markers),
                json!(fetched_is_latest),
                json!(truncated),
                Value::Null,
            ),
            History::Unversioned => (
                "unversioned",
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
            ),
            History::Unavailable => (
                "unavailable",
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
            ),
            History::NotChecked(why) => (
                "not-checked",
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
                json!(why),
            ),
        };
        json!({
            "state": state,
            "versions": versions,
            "delete_markers": markers,
            "fetched_is_latest": latest,
            "truncated": truncated,
            "reason": reason,
        })
    });
    // `bundle` is null when no Kairn manifest could be read. Its checks are null unless
    // the format's rules actually ran (a report from them always names the producer).
    let bundle = o
        .report
        .as_ref()
        .filter(|r| r.format.is_some() || r.producer.is_some())
        .map(|r| {
            let ran = r.producer.is_some();
            let checked = |v: Value| if ran { v } else { Value::Null };
            json!({
                "format": r.format,
                "producer_version": r.producer,
                "cluster_id": r.cluster_id,
                "incident_id": r.incident_id,
                "hash_ok": checked(json!(r.hash_ok)),
                "context_ok": checked(json!(r.context_ok)),
                "coverage_score": checked(json!(r.coverage_score)),
                "partial": checked(json!(r.partial)),
                "signature": checked(json!(signature_str(r.signature))),
                "redaction_mode": r.redaction_mode,
            })
        });
    json!({
        "schema": RESULT_SCHEMA,
        "kairn_version": env!("CARGO_PKG_VERSION"),
        "verdict": verdict_str(o.verdict),
        "exit_code": o.verdict.exit_code(),
        "input": {
            "type": o.input.kind,
            "location": o.input.location,
            "size": o.input.size,
            "sha256": o.input.sha256,
            "version_id": o.input.version_id,
            "history": history,
        },
        "expected": {
            "cluster": o.expected.cluster,
            "incident": o.expected.incident,
            "source": o.expected.source,
        },
        "bundle": bundle,
        "problems": o.problems.iter().map(|p| json!({
            "code": p.code.as_str(),
            "message": p.message,
        })).collect::<Vec<_>>(),
    })
}

fn print_text(o: &Outcome, remote: bool, any_key: bool) {
    use std::io::Write;
    let mut out = String::new();
    if remote && o.report.is_some() {
        let version = o
            .input
            .version_id
            .as_deref()
            .map(|v| format!("  version={v}"))
            .unwrap_or_default();
        let history = o
            .input
            .history
            .as_ref()
            .map(|h| format!("  {}", h.summary()))
            .unwrap_or_default();
        out.push_str(&format!(
            "object: {}  size={}  sha256={}{version}{history}\n",
            o.input.location,
            o.input.size.unwrap_or_default(),
            o.input.sha256.as_deref().unwrap_or("?"),
        ));
        let expected = match (&o.expected.cluster, &o.expected.incident) {
            (None, None) => String::new(),
            (c, i) => format!(
                "cluster={} incident={} ",
                c.as_deref().unwrap_or("*"),
                i.as_deref().unwrap_or("*")
            ),
        };
        let origin = match o.expected.source {
            "flags+object-key" => "from the flags and the object key",
            "flags" => "from --cluster/--incident",
            "object-key" => "from the object key; --any-key skips",
            _ if any_key => "not checked: --any-key",
            _ => "not checked: the URL doesn't name one; use --cluster/--incident",
        };
        out.push_str(&format!("identity: {expected}({origin})\n"));
    }
    match &o.report {
        Some(report) => out.push_str(&format!("{}\n", report_line(report))),
        None if o.verdict == Verdict::Failed => out.push_str("FAILED\n"),
        None => {}
    }
    // stdout may be a closed pipe (`| head`): the exit code still carries the verdict.
    let _ = std::io::stdout().lock().write_all(out.as_bytes());
    let prefix = if o.report.is_none() {
        format!("cannot evaluate: {}: ", o.input.location)
    } else {
        "  - ".to_string()
    };
    for p in &o.problems {
        let prefix = if o.report.is_none() && p.code == ProblemCode::Custody {
            format!("{}: ", o.input.location)
        } else {
            prefix.clone()
        };
        eprintln!("{prefix}{p}");
    }
    if let Some(w) = o.report.as_ref().and_then(redaction_warning) {
        eprintln!("  ! {w}");
    }
    if o.input.history == Some(History::Unversioned) {
        eprintln!(
            "  note: the bucket is not versioned; an overwrite of this key would leave no trace"
        );
    }
}

fn verdict_text(v: Verdict) -> &'static str {
    match v {
        Verdict::Ok => "OK",
        Verdict::Partial => "PARTIAL",
        Verdict::Failed => "FAILED",
        Verdict::CannotEvaluate => "CANNOT-EVALUATE",
    }
}

/// One-line verify summary, shared by `kairn verify` and `kairn demo`.
pub(crate) fn report_line(report: &VerifyReport) -> String {
    let sig = match report.signature {
        SignatureStatus::Trusted => "signed:trusted-key",
        SignatureStatus::Unpinned => "signed:unpinned (no --key: signer not established)",
        SignatureStatus::Invalid => "signed:INVALID",
        SignatureStatus::Absent => "unsigned",
    };
    let verdict = verdict_text(report.verdict);
    if report.producer.is_none() {
        // The format's rules never ran: say what was found, nothing more.
        return match report.format.as_deref() {
            Some(f) => format!("{verdict}  format={f}"),
            None => format!("{verdict}  (not a readable Kairn bundle)"),
        };
    }
    format!(
        "{verdict}  hash_ok={} context_ok={} coverage={:.0}% {sig}  (format {}, produced by kairn {})",
        report.hash_ok,
        report.context_ok,
        report.coverage_score * 100.0,
        report
            .format
            .as_deref()
            .and_then(|f| f.rsplit('/').next())
            .unwrap_or("?"),
        report.producer.as_deref().unwrap_or("?")
    )
}

/// Bundles are shared; say so loudly when one was captured with redaction off.
pub(crate) fn redaction_warning(report: &VerifyReport) -> Option<&'static str> {
    match report.redaction_mode.as_deref() {
        Some("off") => Some(
            "WARNING: captured with redaction OFF — resources/ may contain credentials in \
             plaintext; review before sharing",
        ),
        _ => None,
    }
}
