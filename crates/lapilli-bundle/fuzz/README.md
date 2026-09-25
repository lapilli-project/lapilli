# Fuzzing the verifier

Three libFuzzer targets over the untrusted-input path (`docs/independent-review.md` scope):

- `verify_reader` — bytes → `verify_reader` (zstd → tar → manifest → hash tree → verdict).
- `verify_tar` — the fuzzer mutates the *tar* and the target compresses it, so the search is
  not spent on zstd framing.
- `manifest_json` — `manifest.json` alone, then the coverage score and the signing bytes.

```sh
rustup toolchain install nightly --profile minimal && cargo install cargo-fuzz
cd crates/lapilli-bundle/fuzz
cp ../../../test/fixtures/ieb/v0.1.0/*.ieb corpus/verify_reader/   # seeds (create the dir first)
cargo +nightly fuzz run verify_tar -- -max_total_time=480 -rss_limit_mb=2048 -timeout=10
```

Runs so far (recorded in `docs/independent-review-log.md`): 2026-09-25, 8 minutes per target
on an M-series laptop, seeded with the 43 fixtures — 432,784 / 364,687 / 14,815,480
executions, no crash, timeout or out-of-memory. That bounds panics and blow-ups the fuzzer
could reach in that time; it says nothing about the logic gaps the review found the same day
(a fuzzer does not know that `Signature/` should have been refused).

Not a workspace member (nightly only); `cargo test --workspace` never builds it.
