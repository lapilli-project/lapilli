//! Optional signing of the bundle manifest, cosign-compatible (v2 detached).
//!
//! Signing is OFF by default (DESIGN §5). The signed payload is the literal bytes of
//! `manifest.json`. **Encoding contract (see `spec/IEB-SPEC.md`, verify against cosign v2 in
//! CI):** the signature is base64 of the **ASN.1 DER** ECDSA signature (P-256, SHA-256) —
//! NOT the fixed-width 64-byte IEEE-P1363 form. RustCrypto normalizes to low-S, which cosign
//! expects.
//!
//! ```text
//! cosign verify-blob --key cosign.pub --signature manifest.sig --insecure-ignore-tlog manifest.json
//! ```

use base64::{engine::general_purpose::STANDARD, Engine as _};
// Import the RustCrypto sign/verify traits anonymously (`as _`) so their methods are in
// scope without their names colliding with our own `Signer` trait defined below.
use p256::ecdsa::{
    signature::{Signer as _, Verifier as _},
    Signature, SigningKey, VerifyingKey,
};
use p256::pkcs8::{DecodePrivateKey, DecodePublicKey, EncodePublicKey, LineEnding};

use crate::BundleError;

/// A signing backend. v0.1 ships `StaticKeySigner`; KMS is a v0.2 impl behind this trait.
pub trait Signer {
    /// Sign `payload`, returning the **base64(DER)** signature string cosign v2 accepts.
    fn sign_b64(&self, payload: &[u8]) -> Result<String, BundleError>;
    /// The public key in SPKI PEM form (what `cosign --key cosign.pub` consumes).
    fn public_key_pem(&self) -> Result<String, BundleError>;
    /// The `ieb/v1` key id: SHA-256 (hex) of the public key's SPKI DER.
    fn key_id(&self) -> Result<String, BundleError> {
        key_id(&self.public_key_pem()?)
    }
}

/// SHA-256 (hex) of an SPKI PEM public key's DER encoding.
pub fn key_id(public_key_pem: &str) -> Result<String, BundleError> {
    let vk = VerifyingKey::from_public_key_pem(public_key_pem)
        .map_err(|e| BundleError::Key(e.to_string()))?;
    let der = vk
        .to_public_key_der()
        .map_err(|e| BundleError::Key(e.to_string()))?;
    Ok(crate::hashtree::sha256_hex(der.as_bytes()))
}

/// Static-key ECDSA P-256 signer. The private key shares the controller trust boundary in
/// v0.1 (see DESIGN §7); use KMS (v0.2) for key/collector isolation.
pub struct StaticKeySigner {
    key: SigningKey,
}

impl StaticKeySigner {
    /// Load from a PKCS#8 PEM private key (`-----BEGIN PRIVATE KEY-----`).
    pub fn from_pkcs8_pem(pem: &str) -> Result<Self, BundleError> {
        let key = SigningKey::from_pkcs8_pem(pem).map_err(|e| BundleError::Key(e.to_string()))?;
        Ok(Self { key })
    }
}

impl Signer for StaticKeySigner {
    fn sign_b64(&self, payload: &[u8]) -> Result<String, BundleError> {
        // RustCrypto `sign` produces a low-S normalized Signature; encode as DER for cosign.
        let sig: Signature = self.key.sign(payload);
        Ok(STANDARD.encode(sig.to_der().as_bytes()))
    }

    fn public_key_pem(&self) -> Result<String, BundleError> {
        let vk = VerifyingKey::from(&self.key);
        vk.to_public_key_pem(LineEnding::LF)
            .map_err(|e| BundleError::Key(e.to_string()))
    }
}

/// Generate a fresh P-256 key pair as `(PKCS#8 PEM private key, SPKI PEM public key)` — the
/// formats `StaticKeySigner` and `kairn verify --key` consume. (cosign's own key files are
/// encrypted in a sigstore-specific format and are not interchangeable.)
pub fn generate_key_pair() -> Result<(String, String), BundleError> {
    use p256::pkcs8::EncodePrivateKey;
    let sk = SigningKey::random(&mut rand_core::OsRng);
    let private = sk
        .to_pkcs8_pem(LineEnding::LF)
        .map_err(|e| BundleError::Key(e.to_string()))?
        .to_string();
    let public = VerifyingKey::from(&sk)
        .to_public_key_pem(LineEnding::LF)
        .map_err(|e| BundleError::Key(e.to_string()))?;
    Ok((private, public))
}

/// Verify a base64(DER) signature over `payload` using an SPKI PEM public key.
/// This is what `kairn verify` uses, and it must agree with `cosign verify-blob`.
pub fn verify_b64(public_key_pem: &str, payload: &[u8], sig_b64: &str) -> Result<(), BundleError> {
    let vk = VerifyingKey::from_public_key_pem(public_key_pem)
        .map_err(|e| BundleError::Key(e.to_string()))?;
    let der = STANDARD
        .decode(sig_b64.trim())
        .map_err(|e| BundleError::Signature(e.to_string()))?;
    let sig = Signature::from_der(&der).map_err(|e| BundleError::Signature(e.to_string()))?;
    vk.verify(payload, &sig)
        .map_err(|_| BundleError::Signature("does not match this key and payload".into()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use p256::ecdsa::SigningKey;
    use p256::pkcs8::EncodePrivateKey;
    use rand_core::OsRng;

    #[test]
    fn sign_then_verify_roundtrip() {
        // NOTE: the *authoritative* test is the CI conformance gate against a pinned cosign
        // v2 binary; this only checks internal self-consistency of the DER encoding.
        let sk = SigningKey::random(&mut OsRng);
        let pem = sk.to_pkcs8_pem(LineEnding::LF).unwrap().to_string();
        let signer = StaticKeySigner::from_pkcs8_pem(&pem).unwrap();

        let payload = br#"{"schema_version":"kairn.dev/ieb/v0"}"#;
        let sig = signer.sign_b64(payload).unwrap();
        let pub_pem = signer.public_key_pem().unwrap();

        verify_b64(&pub_pem, payload, &sig).expect("valid signature must verify");
        // A tampered payload must fail.
        assert!(verify_b64(&pub_pem, b"tampered", &sig).is_err());
    }
}
