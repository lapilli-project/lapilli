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
    /// Write a `.ieb` file to stdout. Used over `kubectl exec` to pull bundles out of the
    /// distroless controller image, which has no `tar` for `kubectl cp`.
    #[command(hide = true)]
    CatBundle { path: PathBuf },
}

fn main() -> ExitCode {
    let cli = Cli::parse();
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
                    eprintln!("verify error: reading --key: {e}");
                    return ExitCode::from(1);
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
                    eprintln!("verify error: {e}");
                    ExitCode::from(1)
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
    };
    format!(
        "{verdict}  hash_ok={} context_ok={} coverage={:.0}% {sig}",
        report.hash_ok,
        report.context_ok,
        report.coverage_score * 100.0
    )
}

/// Bundles are shared; say so loudly when one was captured with redaction off.
pub(crate) fn redaction_warning(report: &VerifyReport) -> Option<&'static str> {
    match report.redaction_mode.as_deref() {
        Some("off") => Some(
            "WARNING: captured with redaction OFF — resources/ may contain credentials in \
             plaintext; review before sharing",
        ),
        None => Some("note: no redaction.json (pre-v0.2 bundle) — env values are not redacted"),
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
