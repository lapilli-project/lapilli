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
unsigned commits cannot be merged: the `Every commit carries a DCO sign-off` job checks every
commit in a pull request against its own author, and it is a required check.

## Development workflow

1. Fork the repository and create a topic branch from `main`.
2. Make your change with tests where applicable.
3. Run the local checks — the same commands CI runs (`.github/workflows/ci.yml`):
   ```
   cargo fmt --all
   cargo clippy --workspace --all-targets -- -D warnings
   cargo test --workspace
   # the CLI must also build and pass without its network code
   cargo clippy -p lapilli --no-default-features --all-targets -- -D warnings
   cargo test -p lapilli --no-default-features
   ```
   If your change touches the Go half (`cmd/`, `internal/`, `cases/`, `go.mod`), also the first
   four steps of the `case-tool` job — Go as new as `go.mod` asks for:
   ```
   gofmt -l cmd internal          # prints nothing when formatted
   go mod tidy -diff
   go vet ./...
   go test ./...
   ```
   The job goes on to verify the cases with the built binary, to fail if a cloud SDK has entered
   `go list -deps ./...`, and to run `govulncheck`.
   The tests read the sealed cases under `cases/` and the recorded runs under
   `test/fixtures/case-runs/`; a case you edit must be sealed again (`lapilli-case seal`), and an
   answer key is not edited after an agent has been run against it (`docs/case-format.md`).

   `scripts/release-check.sh` runs the Rust commands plus the fixture, chart and spec checks in one go.
   If your change moves `Cargo.lock`, also run `scripts/attribution-check.sh` (CI does): a new or
   bumped dependency changes which licences we are distributing, and `THIRD-PARTY-LICENSES.md` is
   generated, so it goes stale silently. The command to regenerate it is at the top of `about.toml`.

   **Reviewing a Dependabot `actions` PR: split it.** A green `ci` covers the actions in `ci.yml`
   and `release-gate.yml` and says nothing about the ones in `release.yml`, which runs only on a
   tag — and two of those carry the image digests and the CLI tarballs between jobs. Take the
   covered half, leave `release.yml` for a change of its own, and verify that one with a
   `vX.Y.Z-rc.N` tag. The header of `release.yml` lists which bumps are outstanding and why.
4. Commit with `-s` (DCO) and open a pull request describing the change and its motivation.
   User-visible changes get a line under `## [Unreleased]` in `CHANGELOG.md`; anything that
   touches the bundle format, `lapilli verify` exit codes, CRDs or chart values must follow
   [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md).
5. A maintainer will review. Address feedback; once approved and green, it will be merged.

## What `main` enforces, and what it does not

Written here rather than left in the repository settings, because a rule nobody can read is a rule
nobody can check. The branch ruleset on `main` (2026-10-01):

| Rule | Effect |
|---|---|
| Pull request required | `main` takes no direct push from a contributor |
| Required approvals: **0** | There is one maintainer, and GitHub does not let anyone approve their own pull request — so requiring one approval would stop the project rather than review it. This becomes **1** in the same change that adds the second maintainer (`ROADMAP.md` §2 criterion 4) |
| Review threads must be resolved | A conversation cannot be merged past |
| All **14** `ci` checks must pass | Every job in `.github/workflows/ci.yml`, **including the kind E2E** and, since 2026-10-07, `case-tool`, the Go job that came with `lapilli case`. It is the slowest by far and it is required on purpose: four separate times a change passed the local gate and only a cluster found the defect |
| Branch must be up to date | A pull request green against a stale `main` is re-run against the current one |
| Force-push and deletion | Blocked |
| Bypass | The **admin** role, always. Today that is the single maintainer, who pushes to `main` directly. It is recorded here because a bypass nobody mentions reads as a rule nobody has |

The sign-off in step 4 is checked by the `Every commit carries a DCO sign-off` job, which is one of
the required 14. Until 2026-10-01 this file said unsigned commits could not be merged and no check
anywhere enforced it — the sentence was true of the intent and false of the repository.

One workflow is **not** among them: `replay-diff` (`.github/workflows/replay-diff.yml`). It builds
three scenarios on a kind cluster and asks the cluster and its frozen copy the same commands
(`test/replay-diff`), which takes a quarter of an hour and depends on images being pulled and an
incident forming. It runs when what it tests changes — `internal/replay`, `internal/guard`,
`internal/freeze`, `scenarios/` — once a week, and on request. A change to how a case is replayed
should not be merged with it red; nothing enforces that.

## Reporting bugs and requesting features

Open an issue with clear reproduction steps (for bugs) or the problem you're trying to
solve (for features). For security issues, do **not** open a public issue — see
[`SECURITY.md`](SECURITY.md).

## Project layout

See [`DESIGN.md`](DESIGN.md) for the architecture and the ship-by-ship roadmap.
