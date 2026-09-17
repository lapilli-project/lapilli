//! `kairn` CLI — offline bundle verification (`verify`) and a synthetic demo (`demo`).

use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};
use kairn_bundle::{verify_bundle, SignatureStatus, Verdict, VerifyOptions};

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
    /// Produce a synthetic incident bundle (placeholder — wired up with the kind E2E harness).
    Demo,
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
                    println!(
                        "{verdict}  hash_ok={} context_ok={} coverage={:.0}% {sig}",
                        report.hash_ok,
                        report.context_ok,
                        report.coverage_score * 100.0
                    );
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
        Command::Demo => {
            eprintln!("`kairn demo` is not implemented yet — see docs/design-review-round3.md (tracer bullet).");
            ExitCode::from(1)
        }
    }
}
