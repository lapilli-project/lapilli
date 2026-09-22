//! `lapilli` CLI — offline bundle verification (`verify`) and a synthetic demo (`demo`).

mod demo;
mod postmortem;
mod remote;
mod verify_cmd;

pub(crate) use verify_cmd::{redaction_warning, report_line};

use std::path::PathBuf;
use std::process::ExitCode;

use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(
    name = "lapilli",
    version,
    about = "Kubernetes incident flight recorder"
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Verify an Incident Evidence Bundle: a `.ieb` file, an unpacked directory, or an
    /// object in a bucket (`s3://`, `gs://`, or a presigned `https://` URL), streamed
    /// without being stored.
    Verify(verify_cmd::VerifyArgs),
    /// Public keys for `lapilli verify --key`.
    #[command(subcommand)]
    Key(KeyCommand),
    /// Generate a P-256 signing key pair: `lapilli.key` (PKCS#8, for the controller's Secret)
    /// and `lapilli.pub` (for `lapilli verify --key`).
    Keygen {
        /// Directory to write the key pair into.
        #[arg(long, default_value = ".")]
        out_dir: PathBuf,
    },
    /// Stage a synthetic incident on a cluster running Lapilli and walk it to a verified `.ieb`.
    Demo(demo::DemoArgs),
    /// Render a postmortem draft from a bundle: the facts transcribed, the judgement left to
    /// you. Markdown on stdout. It verifies first and the verdict decides how it renders — a
    /// FAILED bundle still prints, behind a banner, and exits 1.
    Postmortem(postmortem::PostmortemArgs),
    /// Unpack a `.ieb` into a directory (path-traversal and link entries are rejected).
    Unpack {
        bundle: PathBuf,
        /// Destination directory (created if missing).
        dest: PathBuf,
    },
    /// POST an Alertmanager payload (stdin) to the webhook on 127.0.0.1:8080, presenting
    /// the token from LAPILLI_WEBHOOK_TOKEN_FILE if set. Run inside the controller pod by
    /// `lapilli demo` (the token never leaves the pod). Deliberately no address or file
    /// options: it must not be usable to send the pod's files anywhere. Prints the body.
    #[command(hide = true)]
    PostAlert {
        /// Test-only: send a deliberately wrong token (to check the webhook rejects it).
        #[arg(long)]
        wrong_token: bool,
    },
    /// Write a `.ieb` file to stdout, to pull a bundle out of the controller pod:
    ///
    ///   kubectl -n lapilli-system exec deploy/lapilli -c controller -- \
    ///     lapilli cat-bundle /var/lib/lapilli/bundles/<incident>.ieb > <incident>.ieb
    ///
    /// This is how an un-exported bundle is retrieved, and it is the command an incident
    /// notification prints. `kubectl cp` cannot do it: the controller image is distroless
    /// and has no `tar`. The bytes are a plain copy, so `lapilli verify` still detects any
    /// corruption in transit.
    CatBundle { path: PathBuf },
}

#[derive(Subcommand)]
enum KeyCommand {
    /// Fetch the public key of the KMS key the controller signs with (asking the KMS
    /// itself, the trust anchor) and write it as SPKI PEM. Credentials as for `lapilli verify
    /// s3://` (AWS_* variables) or GCP (GOOGLE_APPLICATION_CREDENTIALS, gcloud
    /// application-default, or GOOGLE_OAUTH_ACCESS_TOKEN).
    Fetch {
        /// AWS key ARN, or GCP key version (projects/…/cryptoKeyVersions/<n>).
        #[arg(long)]
        kms: String,
        /// Where to write the public key.
        #[arg(long, default_value = "lapilli.pub")]
        out: PathBuf,
    },
}

/// Exit code for usage errors (`sysexits.h` EX_USAGE), so a typo can't read as PARTIAL (2).
pub(crate) const EXIT_USAGE: u8 = 64;

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
        Command::Verify(args) => ExitCode::from(verify_cmd::run(args)),
        Command::Postmortem(args) => match postmortem::run(&args) {
            Ok(code) => ExitCode::from(code as u8),
            Err(e) => {
                eprintln!("lapilli postmortem: {e}");
                ExitCode::from(3)
            }
        },
        Command::Key(KeyCommand::Fetch { kms, out }) => match key_fetch(&kms, &out) {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                eprintln!("key fetch: {e:#}");
                ExitCode::from(1)
            }
        },
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
                .and_then(|()| lapilli_bundle::unpack(&bundle, &dest).map_err(Into::into));
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

#[cfg(feature = "remote")]
fn key_fetch(key: &str, out: &std::path::Path) -> anyhow::Result<()> {
    let key = lapilli_kms::KmsKey::parse(key)?;
    anyhow::ensure!(
        !out.exists(),
        "{} already exists; not overwriting",
        out.display()
    );
    let _ = rustls::crypto::ring::default_provider().install_default();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(1)
        .enable_all()
        .build()?;
    let signer = rt.block_on(lapilli_kms::KmsSigner::connect(key.clone()))?;
    std::fs::write(out, signer.public_key_pem())?;
    println!("wrote {} for {}", out.display(), key.name());
    println!("key_id {}", signer.key_id());
    println!(
        "verify with:\n  lapilli verify <bundle.ieb> --key {}",
        out.display()
    );
    Ok(())
}

#[cfg(not(feature = "remote"))]
fn key_fetch(_key: &str, _out: &std::path::Path) -> anyhow::Result<()> {
    anyhow::bail!("this lapilli was built without remote support (the `remote` feature)")
}

fn keygen(out_dir: &std::path::Path) -> anyhow::Result<()> {
    let key = out_dir.join("lapilli.key");
    let public = out_dir.join("lapilli.pub");
    for p in [&key, &public] {
        anyhow::ensure!(
            !p.exists(),
            "{} already exists; not overwriting",
            p.display()
        );
    }
    std::fs::create_dir_all(out_dir)?;
    let (private_pem, public_pem) = lapilli_bundle::generate_key_pair()?;
    write_private(&key, private_pem.as_bytes())?;
    std::fs::write(&public, public_pem)?;
    println!(
        "wrote {} (private, keep it secret) and {}",
        key.display(),
        public.display()
    );
    println!("\nenable signing:");
    println!(
        "  kubectl -n lapilli-system create secret generic lapilli-signing-key --from-file=key.pem={}",
        key.display()
    );
    println!("  helm upgrade lapilli <chart> -n lapilli-system --reuse-values \\");
    println!("    --set signing.mode=static --set signing.keySecret=lapilli-signing-key");
    println!(
        "verify with:\n  lapilli verify <bundle.ieb> --key {}",
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
        match std::env::var("LAPILLI_WEBHOOK_TOKEN_FILE") {
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
