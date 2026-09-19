//! `kairn` CLI — offline bundle verification (`verify`) and a synthetic demo (`demo`).

mod demo;

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
    /// Verify an Incident Evidence Bundle — a `.ieb` file or an unpacked directory.
    Verify {
        /// Path to a `.ieb` file or an unpacked bundle directory (containing manifest.json).
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
        } => {
            let trusted_key_pem = match key.map(std::fs::read_to_string).transpose() {
                Ok(k) => k,
                Err(e) => {
                    eprintln!("cannot evaluate: reading --key: {e}");
                    return ExitCode::from(EXIT_CANNOT_EVALUATE);
                }
            };
            let opts = VerifyOptions {
                expected_cluster: cluster,
                expected_incident: incident,
                trusted_key_pem,
            };
            match verify_bundle(&bundle, &opts) {
                Ok(report) => {
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
        Command::CatBundle { path } => match cat_bundle(&path) {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                eprintln!("cat-bundle: {e:#}");
                ExitCode::from(1)
            }
        },
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
