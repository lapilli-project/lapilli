# Contributing to Lapilli

Thanks for your interest in contributing. Lapilli is an Apache-2.0, community-driven project
and welcomes contributions of all kinds: code, documentation, issue triage, design
feedback, and reviews.

## Ground rules

- Be respectful. All participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
- Discuss significant changes first. For anything beyond a small fix — especially changes
  to the architecture or the IEB format spec — open an issue or design discussion before
  writing a large PR. See [`GOVERNANCE.md`](GOVERNANCE.md).

## Developer Certificate of Origin (DCO)

All commits **must** be signed off under the [Developer Certificate of Origin](https://developercertificate.org/).
This certifies you wrote the patch or otherwise have the right to submit it under the
project's license. Add the sign-off automatically with:

```
git commit -s -m "your message"
```

This appends a `Signed-off-by: Your Name <your@email>` line to the commit. PRs with
unsigned commits cannot be merged.

## Development workflow

1. Fork the repository and create a topic branch from `main`.
2. Make your change with tests where applicable.
3. Run the local checks — the same commands CI runs (`.github/workflows/ci.yml`):
   ```
   cargo fmt --all
   cargo clippy --workspace --all-targets -- -D warnings
   cargo test --workspace
   # the CLI must also build and pass without its network code
   cargo clippy -p lapilli-cli --no-default-features --all-targets -- -D warnings
   cargo test -p lapilli-cli --no-default-features
   ```
   `scripts/release-check.sh` runs all of that plus the fixture, chart and spec checks in one go.
   If your change moves `Cargo.lock`, also run `scripts/attribution-check.sh` (CI does): a new or
   bumped dependency changes which licences we are distributing, and `THIRD-PARTY-LICENSES.md` is
   generated, so it goes stale silently. The command to regenerate it is at the top of `about.toml`.
4. Commit with `-s` (DCO) and open a pull request describing the change and its motivation.
   User-visible changes get a line under `## [Unreleased]` in `CHANGELOG.md`; anything that
   touches the bundle format, `lapilli verify` exit codes, CRDs or chart values must follow
   [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md).
5. A maintainer will review. Address feedback; once approved and green, it will be merged.

## Reporting bugs and requesting features

Open an issue with clear reproduction steps (for bugs) or the problem you're trying to
solve (for features). For security issues, do **not** open a public issue — see
[`SECURITY.md`](SECURITY.md).

## Project layout

See [`DESIGN.md`](DESIGN.md) for the architecture and the ship-by-ship roadmap.
