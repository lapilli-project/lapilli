# Lapilli roadmap

*Last synced: 2026-09-27 (v0.1.0-rc.1 published and smoke-tested). This file is the one place that says
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
  author has run Lapilli on a cluster they operate. **There is one maintainer and no adopter,
  and that — not the code — is what the TOC postpones applications for** (round 29: every
  2025–2026 postponement says "one active maintainer, no adopters listed"; no
  single-maintainer, zero-adopter project was accepted in that period).

So the gap between the current state and the first goal is community work that has not
started: the code work is finished. `DESIGN.md` §9's three conditions (a tagged v0.1, a
kind-cluster demo, an early-adopter signal) are necessary, not sufficient.

## 2. The next leap: ~~v0.1.0, tagged and public~~ → **the first outside install**

Twenty-seven rounds had hardened a product nobody could install, and that is no longer the
sentence: `v0.1.0` is published and installs anonymously from GHCR. Criteria 1 and 2 below are
closed, so **the leap is now criterion 3 alone** — one person who is not the author, on a cluster
they run, reading one bundle. Every further feature before that postpones the only thing the first
goal is waiting for.

What "done" means for the leap:

1. ~~`v0.1.0` is tagged from a green `release-gate`, and the `release` workflow has published the
   image (SBOM + provenance), the OCI chart and the CLI binaries with checksums.~~ **Done
   2026-09-28**, tag on `12f6eb4`, verified from the published files alone (§3 item 7).
2. ~~The repository and packages are public; private vulnerability reporting is on.~~ **Done
   2026-09-27.**
3. At least one person who is not the author has installed the chart on a cluster they run,
   captured one incident, and said what they read from the bundle.
4. The application itself (`cncf/sandbox` issue) can be filed in **2027-Q2** with: a second
   active maintainer (one from a different organization counts double), two or three adopters who agreed to be
   named in `ADOPTERS.md`, outside contributions, six months of public activity, one talk at
   **TAG Operational Resilience** (TAG Observability was archived in 2025-12; HolmesGPT went
   through this TAG and is the project Lapilli will be compared to), and the application's
   fields and the automated pre-check (license, repository age, MAINTAINERS, organisation
   diversity, product separation) self-checked beforehand. Sessions run about every two months
   and a postponement costs six to twelve months, so the first attempt has to be the one.

## 3. Focus now — in order

The order is by what unblocks the leap, and each item says who can do it. Items marked
**owner** cannot be done by the assistant that has done the rest of the work; they are listed
so they stop being invisible.

| # | Item | Who | Why it is first |
|---|---|---|---|
| 0 | **GitHub CI green again — held, and re-earned on 2026-09-28.** Every push since 2026-09-17 was red and nobody looked: the local gate regenerates the compatibility fixtures, so it never noticed that 39 of the 43 `.ieb` files were ignored by git and absent on the runner; and quay.io's `minio/*` repositories became invisible to anyone but MinIO (401 anonymously, "no such manifest" even logged in), so the export suite could not stage its images anywhere. The fixtures are tracked, and the export suite now runs against LocalStack's S3 — the image the KMS suite already pulls from Docker Hub — with the same object-lock, conditional-create and version-history assertions. **The same asymmetry returned twice more and was caught the same way**: the notify hand-off defect (`16b6b4f`) and then three defects in the paths round 30 changed, which only a cluster could judge (§4 "Before the tag", round-30 log §4c). The rule this item really stands for: a push is not finished until `gh run list` says the remote gate agrees | assistant | A red CI on a public repository is the first thing an adopter and a TOC reviewer see; and it means "gate green" was true only on one laptop |
| 1 | **Documents and norms in sync with the code** (this ship: `README`, `DESIGN.md`, every `docs/design-*.md` status line, `spec/`, `CHANGELOG`, `COMPATIBILITY`, this file) | assistant | A public reader's first hour is the docs; a stale claim there costs more trust than a missing feature |
| 2 | **Release mechanics that need no human judgement**: `CHANGELOG` complete for every shipped change with a **Migration** section (the CRD default-collector change, new chart values, the CEL rule needing `--server-side --force-conflicts`), `RELEASE.md` steps re-checked against the workflows, `ADOPTERS.md` present | assistant | `RELEASE.md` step 3 requires it; the tag is a one-way door |
| 3 | **Independent review of the verifier of untrusted input** — **first pass done 2026-09-25**: one non-Claude model (GPT-5) found seven real defects, all fixed (`docs/independent-review-log.md`), plus 15.6 M fuzz executions with no crash (`crates/lapilli-bundle/fuzz/`). Still owed: a second reviewer or a human, and a Gemini pass once its API answers | owner (a human) + assistant | The one review the loop cannot supply for itself; `RELEASE.md` lists it as a before-first-release gate |
| 4 | **Real-cloud KMS smoke** — GCP **done 2026-09-25** (`test/fixtures/kms/`). AWS: **closed by labelling, not testing** (2026-09-27) — the project has no AWS account and will not depend on one; README, `docs/kms.md` and `RELEASE.md` say AWS KMS and real S3 are verified against LocalStack only, until the first AWS adopter | done | Only emulators had run; the KMS path is a headline feature |
| 5 | **Repository public; private vulnerability reporting enabled** — **done 2026-09-27** (the day the private-repo Actions minutes ran out mid-release). Both GHCR packages public too (the org's package-creation policy had to allow Public first — an owner setting, no API) | done | Sandbox and every adopter conversation need a URL that opens |
| 6 | **Names**: `lapilli.dev` is paid for one year (expires 2027-09-21 — a renewal reminder for 2027-08 is all that is needed). crates.io: **published 2026-09-29** — `lapilli`, `lapilli-bundle`, `lapilli-net`, `lapilli-kms`, `lapilli-controller`, all 0.1.0. `cargo install lapilli` verified from the registry alone. All five names held. The CLI crate is `lapilli` (renamed from `lapilli-cli`); its directory stays `crates/lapilli-cli/`. Each crate now has `readme`/`keywords`/`categories`, versioned path dependencies and its own `LICENSE` copy (asserted byte-identical to the root by `scripts/attribution-check.sh`, because `cargo package` cannot reach outside a crate directory). `cargo publish --dry-run` passes in full for the two leaf crates; the three above them cannot be dry-run until their dependencies are on the registry. Order and constraints: `RELEASE.md` step 8. | done | The format identifier `lapilli.dev/ieb/v1` is frozen on that domain |
| 7 | ~~**Tag `v0.1.0`**~~ — **done 2026-09-28.** The tag is `v0.1.0` on `12f6eb4`, signed; the release carries three CLI tarballs with `SHA256SUMS`, each with a Sigstore build-provenance attestation that `gh attestation verify` accepts; the image is a two-platform manifest list with SBOM (394 packages), provenance and full OCI labels; the chart is `oci://ghcr.io/lapilli-project/charts/lapilli:0.1.0`. Verified the way a downloader would, from the published files only: every checksum and attestation, all three tarballs carrying `LICENSE`/`NOTICE`/`THIRD-PARTY-LICENSES.md` and their own `.dep-v0` dependency list (syft reads 195 crates out of the macOS binary), and a fresh kind cluster installing the published chart anonymously and sealing an **OK / 100%** bundle with the downloaded CLI. Published image scan: 0 Critical, 3 High, exactly what `docs/security-scanning.md` accounts for. **The first attempt published half of itself** — the macOS leg's binary had no `cargo-auditable` section on the `macos-14` runner, so `publish` was skipped after the image and chart had gone out; the tag was deleted and re-pushed once the matrix moved to `macos-15` (round-30 log §4c). Original text: **Tag `v0.1.0`** per `RELEASE.md` — freeze the fixture set, bump nothing (the workspace and chart are already `0.1.0`), signed tag, watch `release.yml`, smoke-test the published chart on a fresh kind. **`v0.1.0-rc.1` done 2026-09-27**: `release.yml` ran end to end for the first time (gate on both minors, multi-arch image with SBOM and provenance, OCI chart, three CLI builds with checksums, pre-release notes); the published chart + image + macOS CLI installed on a fresh kind and `lapilli demo` sealed an OK/100% bundle with the previous container's log and the rollout diff. The anonymous path was closed the same day once the org's package policy allowed public packages: with no registry login and no pre-loaded image, `helm install oci://ghcr.io/lapilli-project/charts/lapilli --version 0.1.0-rc.1` on a fresh kind pulled the image from GHCR by digest and `lapilli demo --scenario oomkill` sealed an OK/100% bundle. `RELEASE.md` step 6 holds for rc.1 | owner + assistant | The leap itself |
| 8 | **The early-adopter signal** — the cheapest experiments first: one team that runs Kubernetes without an audit-log pipeline installs it for a week; one ISMS/ISO auditor reads a bundle and says whether it answers a control; one incident on a cluster the author does not operate. Each install carries three counters for ninety days: bundles sealed, bundles opened, postmortems that cite one | **owner** (conversations) | `DESIGN.md` §9's third condition; nothing in the repo can produce it |
| 9 | **A second maintainer and named adopters** — the two things every postponement names. Ship the pitch as part of the alert pipeline's standard (an Alertmanager receiver in the kube-prometheus-stack values example) rather than as post-incident reflection, which no public postmortem ever records as missing evidence | **owner** + assistant (docs, examples, talk material) | Round 29 §2 F1, F5 |

Not on this list on purpose: new collectors, new triggers, new consumers. Under §0 they are not
what the first goal is waiting for.

## 4. After the leap — v0.2 and beyond

Ordered by what adopters are likeliest to hit first; every item keeps its open questions.

### The open fork: a trigger on Pod status (round 16 candidate 3)

Round 16 returned a Warning-event trigger to premise and then pointed past it: *"the complete signal
is not the event stream but **Pod status**: a `restartCount` increment with
`lastState.terminated.reason` present. The controller already watches pods for other reasons and
already reads exactly those fields."* The owner picked retention and the postmortem from that fork
and this candidate has been open since.

`docs/design-trigger-reachability.md` is new evidence for it: a watcher sees a pod's terminal state
*before* deletion, which is exactly the evidence an alert at t+15m cannot reach.

What it costs, so the decision is not taken on the upside alone. It reverses `DESIGN.md` §8.3
(*"read at capture time, not a watcher"*); the controller watches only `IncidentCapture` today; and
round 16 measured the load — 40 crashloopers produced 12 captures in 200 ms, projecting ~1700 bundles
a day with the notification channel silent, which is why that round also killed the per-workload
token bucket as *"a silent 6% sample of an incident is not a defensible artifact for this product"*.
Every one of those problems comes back with it.

### Product (v0.2)

- **Trigger coverage — closed by a measurement, not by a decision.** Three proposals tried to widen
  what the webhook accepts and all three were returned to premise (rounds 31, 32, 33): the first
  chose its scope by which collector already existed, the second its corpus by which bundles already
  existed, the third which rules matter by which rules it could classify. Then somebody measured the
  thing none of them had asked.

  **A CronJob's failed pod and its logs are gone one schedule interval after the failure**, and
  `KubeJobFailed` is `for: 15m` — so for anything running more often than every fifteen minutes the
  evidence is destroyed before the alert exists. That is not a gap in the target shape. An
  operational signal carries a `for:` delay, and everything inside it is unreachable to an
  alert-triggered recorder by construction.

  So the thread closes with a boundary rather than a feature: **Lapilli records evidence that
  outlives the alert and dies before the postmortem**, which is most of what matters (a crashlooping
  pod's dead container, its object, its events, a ReplicaSet's history) and is now what `README.md`
  and `DESIGN.md` §3 say. `docs/design-trigger-reachability.md` carries the measurement.

  What round 29 F4 asked for is therefore **not fixed and not going to be**, on this path. The one
  route to the fast-perishing class is round 16's candidate 3, below, which is an owner's decision
  rather than a next step.


- **An extension surface** for outside contributors — on the Kubernetes-native side
  (collectors) and the consumer side (readers, exporters, notify routes) only. Never a fetcher
  from a vendor's store: that is round 24's returned premise, and the identity sentence forbids
  it (round 29 §2 F6).
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

### Before the tag, from round 30 (the pre-release design-and-security pass)

Round 30 ran six lenses over `f410c15` — design conformance, the cluster, supply chain, privacy
and law, and a non-Claude security devil's advocate — and found one BLOCKER, two Criticals about
already-published material, and a long tail. `docs/design-review-round30.md` is the log. What it
left open:

- **A second maintainer and named adopters** are still the only things the TOC postpones for
  (§3 item 9), and nothing in this round changed that.
- **`redaction.minimumMode`**: anyone who can patch a `CaptureProfile` can set
  `redaction.mode: off`, and from then on every bundle carries env values, args and annotations
  unredacted to every destination. Signing is pinned by the admin and notify routes are
  admin-defined; privacy is the only control that is not. Documented as a trust boundary for
  v0.1.0, an admin floor in v0.2.
- **`lapilli erase <incident>`**: there is no targeted deletion, and the manual path is a trap
  (removing `<incident>.notified` makes a month-old incident get announced to Slack as news) and
  is not journalled, which the design's own "deletion has to be at least as recorded as capture"
  does not allow. `docs/data-handling.md` documents the procedure; the command is v0.2.
- **A replacement real-KMS fixture**: the GCP one was removed because it carried a real project
  id under its signature. A new one needs a throwaway project whose id is nobody's.
- **`postmortem` has no output cap** (`read_file` has `READ_CAP`): a crafted bundle's unbounded
  `field`/`kind`/`name`/`actor`/event messages make an enormous MCP response. Escaping makes it
  harmless, not small. Truncating evidence in a permanent document is its own harm, so this is a
  contract decision rather than a bug fix.
- ~~**`scripts/release-check.sh` does not use `--locked`**~~ — done 2026-09-28: every cargo
  invocation in it passes `--locked`, and the attribution half of that script is now
  `scripts/attribution-check.sh`, which `ci` runs on every pull request (a Dependabot `cargo` bump
  is what makes `THIRD-PARTY-LICENSES.md` stale, and it would not have been caught before the next
  tag).

Added the same day, after the round closed: **the round's own changes had to be judged by a
cluster.** Pushing the batch and reading the remote gate — the definition of green this project
settled on after CI was red for eight days — found three more defects, all in paths round 30 had
changed and never run: a handed-over notification recorded as `repeat` moments after it was sent,
the export suite's LocalStack in a namespace called `s3` (so its Service name was S3's
virtual-hosted form and LocalStack read `localstack` as the bucket), and the tightened
`captureprofiles` grant taking away the `list` the permission self-check narrows its collector
questions with. `docs/design-review-round30.md` §4c has all three; §6 gained the rule. Both gates
are green on `c577fbc` — `ci` 10/10 and `release-gate` on 1.30 and 1.37.

### Small items before the tag (assistant; found by the round-28 audit)

- `producer.image_digest` is `"unknown"` on every chart install: `LAPILLI_IMAGE_DIGEST` is read
  by the controller but never set by the chart, and the container does not know its own digest.
  Needs a small design (downward API cannot provide it; the pod status can, after start) — or
  the layout stops promising it.
- ~~Three verifier corner cases~~ — done: the library path now answers `unreadable` for a bad
  `--key`; the mid-stream limit behaviour is stated in `spec/VERIFY-RESULT.md` as the one
  exception to "a known failure is never hidden"; an unknown `alg` without `--key` carries a
  notice (independent review fixes, 2026-09-25/27).
- **A notify flake, seen once (2026-09-26, CI on `2eb8950`, 1.37):** "a rollout does not lose a
  group that is still coalescing" failed with *the group was lost when the controller was
  terminated (SIGTERM flush)* while the release gate passed the same commit on both minors.
  One in three runs. Either the SIGTERM flush has a real race (a coalescing group lost on
  rollout — a product defect) or the harness's timing is; find out before `v0.1.0`.

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

### Pause criterion (written down so fatigue does not decide it)

If by the end of **2027-Q1** there is neither a second maintainer nor a named adopter, Lapilli
pauses and the goal is asked again (round 29 §3).

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
