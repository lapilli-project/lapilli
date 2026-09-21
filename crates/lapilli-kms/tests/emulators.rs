//! Against local KMS emulators (run by `test/kms/emulators.sh`; skipped otherwise):
//! `LAPILLI_TEST_AWS_KMS_KEY` (LocalStack, with `AWS_ENDPOINT_URL_KMS`) and
//! `LAPILLI_TEST_GCP_KMS_KEY` (gcp-kms-emulator, with `LAPILLI_GCP_KMS_ENDPOINT`).

use lapilli_kms::{KmsKey, KmsSigner};

async fn roundtrip(var: &str) {
    let Ok(key) = std::env::var(var) else {
        eprintln!("{var} not set: skipped");
        return;
    };
    let _ = rustls::crypto::ring::default_provider().install_default();
    let signer = KmsSigner::connect(KmsKey::parse(&key).unwrap())
        .await
        .expect("connect");
    assert_eq!(signer.key_id().len(), 64);
    // Many signatures: KMS returns high-S about half the time, and every stored one must
    // be low-S and verify.
    for i in 0..16u8 {
        let manifest = format!("{{\"n\":{i}}}");
        let signed = signer
            .sign_manifest(manifest.as_bytes())
            .await
            .expect("sign");
        lapilli_bundle::sign::verify_b64(
            signer.public_key_pem(),
            manifest.as_bytes(),
            &signed.signature_b64,
        )
        .expect("verifies");
        use base64::Engine as _;
        use p256::elliptic_curve::scalar::IsHigh;
        let der = base64::engine::general_purpose::STANDARD
            .decode(&signed.signature_b64)
            .unwrap();
        let sig = p256::ecdsa::Signature::from_der(&der).unwrap();
        assert!(!bool::from(sig.s().is_high()), "stored signature is high-S");
        assert_eq!(signed.digest_hex.len(), 64);
    }
}

#[tokio::test]
async fn aws_localstack() {
    roundtrip("LAPILLI_TEST_AWS_KMS_KEY").await;
}

#[tokio::test]
async fn gcp_emulator() {
    roundtrip("LAPILLI_TEST_GCP_KMS_KEY").await;
}
