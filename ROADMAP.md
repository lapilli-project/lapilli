# Lapilli roadmap

*Last synced: 2026-09-25 (round 28, the sync audit). This file is the one place that says
what is left, what comes next and what the project is working on now. `DESIGN.md` §11 keeps the
version table; `README.md` keeps the feature list; both point here for anything about order and
priority. A review round that changes any of this updates this file in the same commit.*

## 0. The constraint everything below is checked against

Lapilli is *triggered by operational signals*, *correlates Kubernetes-native state across the
incident window*, *seals it as an open, portable, offline-verifiable evidence file*, and
**verifies and reads that file itself** — depending on no observability vendor and no AI tool
(`DESIGN.md`, identity block; `docs/design-review-round27.md`).

Goal order (`DESIGN.md` §9): **first a CNCF Sandbox listing, second adoption that could be
sold.** A market or strategy question is answered under the identity, not by rewriting it.

## 1. Where the project stands

- **Built and proven.** Everything the README lists under *Built*: the recorder (webhook →
  `IncidentCapture` → collectors → hash tree → `.ieb`), optional signing (static key, AWS KMS,
  GCP KMS), object-store export, redaction, spec diffs, PromQL windows, notification, retention,
  capture retirement, the permission self-check that follows the installed profiles, the
  perishable profile (`coverage.deferred`), `lapilli verify` (local, directory, `s3://`, `gs://`,
  `https://`, `--output json`), `lapilli postmortem`, and `lapilli mcp` — Lapilli's own reader of
  its evidence, served from where the bundles are.
- **Release gate green — locally.** `scripts/release-check.sh --e2e` on `174b999`
  (2026-09-25): `E2E OK` on kind v1.30.0 and v1.37.0 across every suite (demo, diffs, export,
  kms, notify, deferred, mcp) and `release-check OK`. **GitHub CI, however, has been red since
  2026-09-17** for two reasons the local gate could not see (§3 item 0). Twenty-eight review
  rounds are logged under `docs/design-review-round*.md`; round 28 is the audit that found
  this.
- **Not done.** No tag exists. The repository and its packages are private. The fixture set
  under `test/fixtures/ieb/v0.1.0` is frozen at the tag, not before it. Nobody outside the
  author has run Lapilli on a cluster they operate.

That last line is the whole gap between the current state and the first goal. `DESIGN.md` §9
says Sandbox is applied for *with a tagged v0.1, a kind-cluster demo, and an early-adopter
signal*: the demo exists, the tag and the signal do not.

## 2. The next leap: **v0.1.0, tagged and public**

Twenty-seven rounds have hardened a product nobody can install. Every further feature before
the tag postpones the only thing the first goal needs. So the next leap is not a capability; it
is the release, and the adopter conversations that only a public release makes possible.

What "done" means for the leap:

1. `v0.1.0` is tagged from a green `release-gate`, and the `release` workflow has published the
   image (SBOM + provenance), the OCI chart and the CLI binaries with checksums — the first time
   `release.yml` runs for real.
2. The repository and packages are public; private vulnerability reporting is on
   (`SECURITY.md` depends on it).
3. At least one person who is not the author has installed the chart on a cluster they run,
   captured one incident, and said what they read from the bundle.

## 3. Focus now — in order

The order is by what unblocks the leap, and each item says who can do it. Items marked
**owner** cannot be done by the assistant that has done the rest of the work; they are listed
so they stop being invisible.

| # | Item | Who | Why it is first |
|---|---|---|---|
| 0 | **GitHub CI green again.** Every push since 2026-09-17 was red and nobody looked: the local gate regenerates the compatibility fixtures, so it never noticed that 39 of the 43 `.ieb` files were ignored by git and absent on the runner; and quay.io now requires a login for `minio/*`, so the export suite cannot stage its images there. The fixtures are tracked as of this ship; the export suite now accepts `E2E_IMAGE_MIRROR`, and `scripts/mirror-e2e-images.sh` fills one — **creating that package on the org is an owner decision** (`ghcr.io/lapilli-project/e2e`, or a `docker login quay.io` secret instead) | assistant + **owner** | A red CI on a public repository is the first thing an adopter and a TOC reviewer see; and it means "gate green" was true only on one laptop |
| 1 | **Documents and norms in sync with the code** (this ship: `README`, `DESIGN.md`, every `docs/design-*.md` status line, `spec/`, `CHANGELOG`, `COMPATIBILITY`, this file) | assistant | A public reader's first hour is the docs; a stale claim there costs more trust than a missing feature |
| 2 | **Release mechanics that need no human judgement**: `CHANGELOG` complete for every shipped change with a **Migration** section (the CRD default-collector change, new chart values, the CEL rule needing `--server-side --force-conflicts`), `RELEASE.md` steps re-checked against the workflows, `ADOPTERS.md` present | assistant | `RELEASE.md` step 3 requires it; the tag is a one-way door |
| 3 | **Independent review of the verifier of untrusted input** (`read_ieb`, `Contents`, the manifest parse — `docs/independent-review.md`, `docs/design-review-round5.md`) | **owner** (a human, or a non-Claude model) | The one review the loop cannot supply for itself; `RELEASE.md` lists it as a before-first-release gate |
| 4 | **Real-cloud KMS smoke** (one signature each on AWS KMS and GCP Cloud KMS, `docs/kms.md`) | **owner** | Only emulators have run; the KMS path is a headline feature |
| 5 | **Repository and GHCR packages public; private vulnerability reporting enabled** | **owner** | Sandbox and every adopter conversation need a URL that opens |
| 6 | **Names and renewals**: `lapilli.dev` auto-renew (expires 2027-09-21), `lapilli` on crates.io claimed at first publish | **owner** | The format identifier `lapilli.dev/ieb/v1` is frozen on that domain |
| 7 | **Tag `v0.1.0`** per `RELEASE.md` — freeze the fixture set, bump nothing (the workspace and chart are already `0.1.0`), signed tag, watch `release.yml`, smoke-test the published chart on a fresh kind | owner + assistant | The leap itself |
| 8 | **The early-adopter signal** — the cheapest experiments first: one team that runs Kubernetes without a log store (Loki/Promtail) installs it for a week; one ISMS/ISO auditor reads a bundle and says whether it answers a control; one incident on a cluster the author does not operate | **owner** (conversations) | `DESIGN.md` §9's third condition; nothing in the repo can produce it |

Not on this list on purpose: new collectors, new triggers, new consumers. Under §0 they are not
what the first goal is waiting for.

## 4. After the leap — v0.2 and beyond

Ordered by what adopters are likeliest to hit first; every item keeps its open questions.

### Product (v0.2)

- **Per-alert profile selection.** The webhook attaches one `--profile` to every capture. A
  label-selected profile would let one alert choose the perishable profile and another the
  full one — but a tenant could then route their evidence through another team's profile (its
  notify route, its export destinations) by labelling their own alert. Needs an allow-list
  design and its own round before any code (`docs/design-review-round25.md` §5).
- **Node-level captures** (kubelet/node conditions when the incident is the node, not the pod)
  — `docs/design-trigger-and-load.md` open question 3; larger than a patch.
- **Cluster cost and single-replica topology** — what Lapilli costs the API server per alert
  storm, and what a second replica would mean for the `O_EXCL` claim files and the RWO PVC
  (`docs/design-trigger-and-load.md` open questions 6–7; `docs/design-review-round21.md`).
- **The remaining measurements** from rounds 21–22: the §3 tables re-run with `REPEATS=3`
  (every published row is n=1), 100 and 200 alerts on hardware that is not a laptop, probe
  latency produced by the harness rather than quoted, and an owner for etcd growth (~44,000
  objects a year per rule — retirement bounds the controller, not the cluster).
- **Adopter-driven** — whatever the first three installs report, ahead of anything above.

### Small items before the tag (assistant; found by the round-28 audit)

- `producer.image_digest` is `"unknown"` on every chart install: `LAPILLI_IMAGE_DIGEST` is read
  by the controller but never set by the chart, and the container does not know its own digest.
  Needs a small design (downward API cannot provide it; the pod status can, after start) — or
  the layout stops promising it.
- Three verifier corner cases the spec audit found, none changing a pinned verdict: a bad
  `--key` reaching the library path is coded `signature` where the spec says `unreadable`; a
  mid-stream size limit drops structure problems already found (CANNOT_EVALUATE hides a known
  FAILED); an unknown `alg` skips the `cosign.pub` id check. One spec sentence or one small
  change each.
- `test/mcp/check.sh` (the reference MCP client) is in no gate; add it to `release-check.sh`
  behind `command -v npx`.

### Trust (v0.3, unchanged, opt-in)

keyless + Rekor (spike) · RFC 3161 TSA · embedded TUF root · named-control mapping · signed
pre-redaction commitment with second key custody · `keys/<key_id>.pub` publication. All of it
is *earned later* under Path C; none of it moves before adopters ask.

### Returned to premise, and what would reopen it

- **Phase B, backfill/seal on demand** (`docs/design-record-and-seal.md`). Returned because a
  human-run, unsigned, off-cluster producer merging late data into a signed bundle fights the
  trust model rather than a gap in the format. It reopens only if someone answers *"is a
  backfill a thing that belongs inside the bundle at all?"* with a design in which the
  controller, not a human, seals — and even then it must not make Lapilli a client of other
  stores (§0).
- **Event trigger without an alert rule** (`docs/design-event-trigger.md`, rounds 16 and 19).
  Reopens only with evidence that an operator who wants Lapilli cannot write an alert rule.

### Owner's question, left open on purpose

Whether signing, audit and "standard" should move from *earned later* back toward the pitch.
Round 27 kept Path C's restraint in place and did not decide this; it is the owner's call, and
the identity sentence is compatible with either answer.

## 5. Sandbox application — readiness

What the `cncf/sandbox` application asks for, in substance, and where Lapilli stands:

| Asked | State |
|---|---|
| Public repository, OSI license | private today (§3 item 5); Apache-2.0 ✔ |
| `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `GOVERNANCE.md`, `MAINTAINERS.md`, `SECURITY.md` | present ✔ — governance is honest about a single maintainer and states how that changes |
| A roadmap | this file |
| Adopters | `ADOPTERS.md` exists and is empty; §3 item 8 is how it stops being empty |
| Vendor neutrality | no vendor dependency by construction (§0); the founder's affiliation is listed as Independent |
| TAG alignment and landscape comparison | `DESIGN.md` §3 (landscape, cited) and §9 (TAG Operational Resilience) |
| A release | none yet (§2) |
| DCO | every commit is signed off; enforce it in CI when the repository is public |

Nothing here is applied for at design stage (`DESIGN.md` §9): the application follows the
tag and the first signal, not the other way round.

## 6. How this file stays true

- A review round that adds, closes or reorders any item edits this file in the same commit
  as its log.
- `README.md` *Status & roadmap* and `DESIGN.md` §11 link here and do not repeat the order.
- The date and commit in the first line move whenever the body does.
