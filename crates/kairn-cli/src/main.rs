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
        /// Require a valid signature to pass.
        #[arg(long)]
        require_signature: bool,
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
            require_signature,
        } => {
            let opts = VerifyOptions {
                expected_cluster: cluster,
                expected_incident: incident,
                require_signature,
            };
            match verify_bundle(&bundle, &opts) {
                Ok(report) => {
                    println!("{}", report_line(&report));
                    for p in &report.problems {
                        eprintln!("  - {p}");
                    }
                    ExitCode::from(report.verdict.exit_code() as u8)
                }
                Err(e) => {
                    eprintln!("verify error: {e}");
                    ExitCode::from(1)
                }
            }
        }
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
        SignatureStatus::Valid => "signed:valid",
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
