//! `kairn` CLI — offline bundle verification (`verify`) and a synthetic demo (`demo`).

mod demo;
mod remote;

use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use kairn_bundle::{verify_bundle, SignatureStatus, Verdict, VerifyOptions, VerifyReport};

#[derive(Parser)]
#[command(name = "kairn", version, about = "Kubernetes incident flight recorder")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Verify an Incident Evidence Bundle: a `.ieb` file, an unpacked directory, or an
    /// object in a bucket (`s3://`, `gs://`, or a presigned `https://` URL), streamed
    /// without being stored.
    Verify {
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
    },
    /// Generate a P-256 signing key pair: `kairn.key` (PKCS#8, for the controller's Secret)
    /// and `kairn.pub` (for `kairn verify --key`).
    Keygen {
        /// Directory to write the key pair into.
        #[arg(long, default_value = ".")]
        out_dir: PathBuf,
    },
    /// Stage a synthetic incident on a cluster running Kairn and walk it to a verified `.ieb`.
    Demo(demo::DemoArgs),
    /// Unpack a `.ieb` into a directory (path-traversal and link entries are rejected).
    Unpack {
        bundle: PathBuf,
        /// Destination directory (created if missing).
        dest: PathBuf,
    },
    /// POST an Alertmanager payload (stdin) to the webhook on 127.0.0.1:8080, presenting
    /// the token from KAIRN_WEBHOOK_TOKEN_FILE if set. Run inside the controller pod by
    /// `kairn demo` (the token never leaves the pod). Deliberately no address or file
    /// options: it must not be usable to send the pod's files anywhere. Prints the body.
    #[command(hide = true)]
    PostAlert {
        /// Test-only: send a deliberately wrong token (to check the webhook rejects it).
        #[arg(long)]
        wrong_token: bool,
    },
    /// Write a `.ieb` file to stdout. Used over `kubectl exec` to pull bundles out of the
    /// distroless controller image, which has no `tar` for `kubectl cp`.
    #[command(hide = true)]
    CatBundle { path: PathBuf },
}

/// Exit code for usage errors (`sysexits.h` EX_USAGE), so a typo can't read as PARTIAL (2).
const EXIT_USAGE: u8 = 64;
/// Exit code for "cannot evaluate" (unreadable input, unknown format major, limits).
const EXIT_CANNOT_EVALUATE: u8 = 3;

fn main() -> ExitCode {
    let cli = match Cli::try_parse() {
        Ok(cli) => cli,
        Err(e) => {
            let _ = e.print();
            return match e.kind() {
                clap::error::ErrorKind::DisplayHelp | clap::error::ErrorKind::DisplayVersion => {
                    ExitCode::SUCCESS
                }
                _ => ExitCode::from(EXIT_USAGE),
            };
        }
    };
    match cli.command {
        Command::Verify {
            bundle,
            cluster,
            incident,
            key,
            expect_sha256,
            version_id,
            current_only,
            any_key,
        } => {
            let expect_sha256 = match expect_sha256.map(|h| h.to_ascii_lowercase()) {
                Some(h) if h.len() != 64 || !h.bytes().all(|b| b.is_ascii_hexdigit()) => {
                    eprintln!("--expect-sha256 takes 64 hex digits");
                    return ExitCode::from(EXIT_USAGE);
                }
                h => h,
            };
            let trusted_key_pem = match key.map(std::fs::read_to_string).transpose() {
                Ok(k) => k,
                Err(e) => {
                    eprintln!("cannot evaluate: reading --key: {e}");
                    return ExitCode::from(EXIT_CANNOT_EVALUATE);
                }
            };
            let flags_identity = cluster.is_some() || incident.is_some();
            let mut opts = VerifyOptions {
                expected_cluster: cluster,
                expected_incident: incident,
                trusted_key_pem,
            };
            let source = match bundle.to_str().and_then(remote::parse) {
                None if version_id.is_some() || current_only || any_key => {
                    eprintln!(
                        "--version-id, --current-only and --any-key apply to bucket URLs only"
                    );
                    return ExitCode::from(EXIT_USAGE);
                }
                None => None,
                Some(Ok(source)) => {
                    let s3 = matches!(source, remote::Source::S3 { .. });
                    if (version_id.is_some() || current_only) && !s3 {
                        eprintln!("--version-id and --current-only are supported for s3:// only");
                        return ExitCode::from(EXIT_USAGE);
                    }
                    if any_key && matches!(source, remote::Source::Https { .. }) {
                        eprintln!("--any-key applies to s3:// and gs:// (https:// URLs are never matched against their path)");
                        return ExitCode::from(EXIT_USAGE);
                    }
                    Some(source)
                }
                Some(Err(e)) => {
                    eprintln!("{e}");
                    return ExitCode::from(EXIT_USAGE);
                }
            };
            let result = match source {
                None => verify_local(&bundle, &opts, expect_sha256.is_some()),
                Some(source) => {
                    let from_key = if any_key { None } else { source.key_identity() };
                    if let Some((c, i)) = &from_key {
                        opts.expected_cluster.get_or_insert_with(|| c.clone());
                        opts.expected_incident.get_or_insert_with(|| i.clone());
                    }
                    let origin = match (flags_identity, from_key.is_some()) {
                        (true, true) => "from the flags and the object key",
                        (true, false) => "from --cluster/--incident",
                        (false, true) => "from the object key; --any-key skips",
                        (false, false) if any_key => "not checked: --any-key",
                        (false, false) => {
                            "not checked: the URL doesn't name one; use --cluster/--incident"
                        }
                    };
                    verify_remote(&source, version_id, current_only, &opts, origin)
                }
            };
            match result {
                Ok((mut report, sha256)) => {
                    if let (Some(want), Some(got)) = (&expect_sha256, &sha256) {
                        if want != got {
                            report
                                .problems
                                .push(format!("sha256 {got} is not the expected {want}"));
                            if report.verdict != Verdict::CannotEvaluate {
                                report.verdict = Verdict::Failed;
                            }
                        }
                    }
                    println!("{}", report_line(&report));
                    for p in &report.problems {
                        eprintln!("  - {p}");
                    }
                    if let Some(w) = redaction_warning(&report) {
                        eprintln!("  ! {w}");
                    }
                    ExitCode::from(report.verdict.exit_code() as u8)
                }
                Err(e) => {
                    eprintln!("cannot evaluate: {e}");
                    ExitCode::from(EXIT_CANNOT_EVALUATE)
                }
            }
        }
        Command::Keygen { out_dir } => match keygen(&out_dir) {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                eprintln!("keygen: {e:#}");
                ExitCode::from(1)
            }
        },
        Command::Demo(args) => match demo::run(args) {
            Ok(code) => ExitCode::from(code as u8),
            Err(e) => {
                eprintln!("\n  ✗ demo failed: {e:#}");
                ExitCode::from(1)
            }
        },
        Command::Unpack { bundle, dest } => {
            let r = std::fs::create_dir_all(&dest)
                .map_err(anyhow::Error::from)
                .and_then(|()| kairn_bundle::unpack(&bundle, &dest).map_err(Into::into));
            match r {
                Ok(()) => ExitCode::SUCCESS,
                Err(e) => {
                    eprintln!("unpack: {e:#}");
                    ExitCode::from(1)
                }
            }
        }
        Command::PostAlert { wrong_token } => match post_alert(wrong_token) {
            Ok(body) => {
                println!("{body}");
                ExitCode::SUCCESS
            }
            Err(e) => {
                eprintln!("post-alert: {e:#}");
                ExitCode::from(1)
            }
        },
        Command::CatBundle { path } => match cat_bundle(&path) {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                eprintln!("cat-bundle: {e:#}");
                ExitCode::from(1)
            }
        },
    }
}

/// Verify a local `.ieb` or directory; with `hash`, also the file's SHA-256.
fn verify_local(
    bundle: &std::path::Path,
    opts: &VerifyOptions,
    hash: bool,
) -> Result<(VerifyReport, Option<String>), String> {
    let sha256 = if hash {
        if bundle.is_dir() {
            return Err("--expect-sha256 needs a .ieb file, not a directory".into());
        }
        let mut hasher = <sha2::Sha256 as sha2::Digest>::new();
        let mut f = std::fs::File::open(bundle).map_err(|e| e.to_string())?;
        std::io::copy(&mut f, &mut hasher).map_err(|e| e.to_string())?;
        Some(remote::hex(&sha2::Digest::finalize(hasher)))
    } else {
        None
    };
    let report = verify_bundle(bundle, opts).map_err(|e| e.to_string())?;
    Ok((report, sha256))
}

/// Stream a bucket object into the verifier; prints the `object:` and `identity:` lines.
/// `Err` = exit 3.
#[cfg(feature = "remote")]
fn verify_remote(
    source: &remote::Source,
    version_id: Option<String>,
    current_only: bool,
    opts: &VerifyOptions,
    identity_origin: &str,
) -> Result<(VerifyReport, Option<String>), String> {
    let (object, report) = remote::fetch_and_verify(source, version_id, current_only, opts)
        .map_err(|e| format!("{}: {e}", source.display()))?;
    let version = object
        .version
        .as_deref()
        .map(|v| format!("  version={v}"))
        .unwrap_or_default();
    println!(
        "object: {}  size={}  sha256={}{version}  {}",
        source.display(),
        object.size,
        object.sha256,
        object.history.summary()
    );
    let expected = match (&opts.expected_cluster, &opts.expected_incident) {
        (None, None) => String::new(),
        (c, i) => format!(
            "cluster={} incident={} ",
            c.as_deref().unwrap_or("*"),
            i.as_deref().unwrap_or("*")
        ),
    };
    println!("identity: {expected}({identity_origin})");
    if object.history == remote::History::Unversioned {
        eprintln!(
            "  note: the bucket is not versioned; an overwrite of this key would leave no trace"
        );
    }
    Ok((report, Some(object.sha256)))
}

#[cfg(not(feature = "remote"))]
fn verify_remote(
    source: &remote::Source,
    _version_id: Option<String>,
    _current_only: bool,
    _opts: &VerifyOptions,
    _identity_origin: &str,
) -> Result<(VerifyReport, Option<String>), String> {
    Err(format!(
        "{}: this kairn was built without remote support (the `remote` feature)",
        source.display()
    ))
}

/// One-line verify summary, shared by `kairn verify` and `kairn demo`.
pub(crate) fn report_line(report: &VerifyReport) -> String {
    let sig = match report.signature {
        SignatureStatus::Trusted => "signed:trusted-key",
        SignatureStatus::Unpinned => "signed:unpinned (no --key: signer not established)",
        SignatureStatus::Invalid => "signed:INVALID",
        SignatureStatus::Absent => "unsigned",
    };
    let verdict = match report.verdict {
        Verdict::Ok => "OK",
        Verdict::Partial => "PARTIAL",
        Verdict::Failed => "FAILED",
        Verdict::CannotEvaluate => "CANNOT-EVALUATE",
    };
    if report.producer.is_none() && report.verdict == Verdict::Failed {
        return format!("{verdict}  (not a readable Kairn bundle)");
    }
    if report.verdict == Verdict::CannotEvaluate {
        return format!(
            "{verdict}  format={}",
            report.format.as_deref().unwrap_or("?")
        );
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
        None => None,
        _ => None,
    }
}

fn keygen(out_dir: &std::path::Path) -> anyhow::Result<()> {
    let key = out_dir.join("kairn.key");
    let public = out_dir.join("kairn.pub");
    for p in [&key, &public] {
        anyhow::ensure!(
            !p.exists(),
            "{} already exists; not overwriting",
            p.display()
        );
    }
    std::fs::create_dir_all(out_dir)?;
    let (private_pem, public_pem) = kairn_bundle::generate_key_pair()?;
    write_private(&key, private_pem.as_bytes())?;
    std::fs::write(&public, public_pem)?;
    println!(
        "wrote {} (private, keep it secret) and {}",
        key.display(),
        public.display()
    );
    println!("\nenable signing:");
    println!(
        "  kubectl -n kairn-system create secret generic kairn-signing-key --from-file=key.pem={}",
        key.display()
    );
    println!("  helm upgrade kairn <chart> -n kairn-system --reuse-values \\");
    println!("    --set signing.mode=static --set signing.keySecret=kairn-signing-key");
    println!(
        "verify with:\n  kairn verify <bundle.ieb> --key {}",
        public.display()
    );
    Ok(())
}

/// Create a file readable only by its owner (0600 on Unix).
fn write_private(path: &std::path::Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create_new(true);
    #[cfg(unix)]
    std::os::unix::fs::OpenOptionsExt::mode(&mut opts, 0o600);
    opts.open(path)?.write_all(bytes)
}

/// Minimal HTTP/1.1 POST over a plain TCP connection to the local webhook (no TLS, no
/// dependencies: it only ever talks to 127.0.0.1 inside the controller pod).
fn post_alert(wrong_token: bool) -> anyhow::Result<String> {
    let addr = "127.0.0.1:8080";
    use std::io::{Read, Write};
    let mut body = Vec::new();
    std::io::stdin().read_to_end(&mut body)?;
    let token = if wrong_token {
        Some("wrong-token-for-testing-0000000000000000".to_string())
    } else {
        match std::env::var("KAIRN_WEBHOOK_TOKEN_FILE") {
            Ok(f) => Some(std::fs::read_to_string(f)?.trim().to_string()),
            Err(_) => None,
        }
    };
    let mut req = format!(
        "POST /webhook HTTP/1.1\r\nHost: {addr}\r\nContent-Type: application/json\r\n\
         Content-Length: {}\r\nConnection: close\r\n",
        body.len()
    );
    if let Some(t) = token {
        req.push_str(&format!("Authorization: Bearer {t}\r\n"));
    }
    req.push_str("\r\n");
    let mut stream = std::net::TcpStream::connect(addr)?;
    stream.set_read_timeout(Some(std::time::Duration::from_secs(30)))?;
    stream.write_all(req.as_bytes())?;
    stream.write_all(&body)?;
    let mut resp = String::new();
    stream.read_to_string(&mut resp)?;
    let (head, body) = resp.split_once("\r\n\r\n").unwrap_or((resp.as_str(), ""));
    let status = head.split_whitespace().nth(1).unwrap_or("?");
    anyhow::ensure!(status == "200", "webhook answered {status}: {body}");
    Ok(body.to_string())
}

fn cat_bundle(path: &std::path::Path) -> anyhow::Result<()> {
    // Only bundles: this runs inside the controller pod, so don't make it a generic reader.
    anyhow::ensure!(
        path.extension().is_some_and(|e| e == "ieb"),
        "refusing to read {}: not a .ieb file",
        path.display()
    );
    let mut f = std::fs::File::open(path)?;
    std::io::copy(&mut f, &mut std::io::stdout().lock())?;
    Ok(())
}
