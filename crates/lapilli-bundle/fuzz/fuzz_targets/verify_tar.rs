#![no_main]
//! The same path, but the fuzzer mutates the *tar* bytes and we compress them on the way in,
//! so it is not stuck fighting zstd's framing and checksums.
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    let Ok(compressed) = zstd::encode_all(std::io::Cursor::new(data), 1) else { return };
    let opts = lapilli_bundle::verify::VerifyOptions {
        expected_cluster: Some("cluster-a".into()),
        expected_incident: None,
        trusted_key_pem: None,
    };
    let _ = lapilli_bundle::verify::verify_reader(std::io::Cursor::new(compressed), &opts);
});
