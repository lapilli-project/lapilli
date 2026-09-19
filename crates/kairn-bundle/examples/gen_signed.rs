//! Generate a signed bundle into a directory, for signing-conformance checks.
//! Usage: `cargo run -p kairn-bundle --example gen_signed -- <out_dir>`
//!
//! Writes: a sample captured file, `manifest.json`, `signature/manifest.sig` (base64 DER),
//! and `signature/cosign.pub` (SPKI PEM). External tools (openssl, or a pinned cosign) can
//! then verify `signature/manifest.sig` over `manifest.json` with `signature/cosign.pub`.

use std::fs;
use std::path::PathBuf;

use kairn_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
use kairn_bundle::sign::StaticKeySigner;
use kairn_bundle::{seal_dir, SealInput};
use p256::ecdsa::SigningKey;
use p256::pkcs8::{EncodePrivateKey, LineEnding};
use rand_core::OsRng;

fn main() {
    let out = PathBuf::from(
        std::env::args()
            .nth(1)
            .expect("usage: gen_signed <out_dir>"),
    );
    fs::create_dir_all(out.join("logs")).unwrap();
    fs::write(out.join("logs/app-previous.log"), b"panic: out of memory\n").unwrap();
    fs::write(out.join("logs/index.json"), b"{}").unwrap();
    fs::write(out.join("redaction.json"), br#"{"mode":"default"}"#).unwrap();

    // Fresh key; also write the PKCS#8 private key so a pinned cosign can re-verify if wanted.
    let sk = SigningKey::random(&mut OsRng);
    let priv_pem = sk.to_pkcs8_pem(LineEnding::LF).unwrap().to_string();
    fs::write(out.join("cosign.key.pem"), &priv_pem).unwrap();
    let signer = StaticKeySigner::from_pkcs8_pem(&priv_pem).unwrap();

    let input = SealInput {
        incident: IncidentIdentity {
            id: "inc-demo".into(),
            cluster_id: "kind-kairn".into(),
            trigger: Trigger {
                rule: "KubePodCrashLooping".into(),
                firing_ts: "2026-09-17T02:14:33Z".into(),
            },
            window: Window {
                start: "2026-09-17T02:09:33Z".into(),
                end: "2026-09-17T02:19:33Z".into(),
            },
        },
        producer: Producer {
            kairn_version: "0.1.0".into(),
            image_digest: "sha256:demo".into(),
        },
        coverage: Coverage {
            collectors_run: vec!["logs".into()],
            collectors_intended: vec!["logs".into()],
        },
        timing: Timing {
            capture_started: "2026-09-17T02:14:34Z".into(),
            sealed_at: "2026-09-17T02:14:36Z".into(),
            capture_to_seal_ms: 2000,
        },
    };

    seal_dir(&out, input, Some(&signer)).unwrap();
    println!("wrote signed bundle to {}", out.display());
}
