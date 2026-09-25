#![no_main]
//! manifest.json alone: parse, then the two derived views the verifier trusts —
//! the coverage score and the canonical signing bytes.
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    if let Ok(m) = serde_json::from_slice::<lapilli_bundle::Manifest>(data) {
        let _ = m.coverage.score();
        let _ = m.coverage.is_partial();
        let _ = m.to_signing_bytes();
    }
});
