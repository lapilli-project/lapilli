#![no_main]
//! The whole untrusted path as `lapilli verify` sees a file: zstd frame → tar → manifest →
//! hash tree → verdict. Must never panic, never allocate past the spec's limits.
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    let opts = lapilli_bundle::verify::VerifyOptions::default();
    let _ = lapilli_bundle::verify::verify_reader(std::io::Cursor::new(data), &opts);
});
