# Lapilli roadmap

*Last synced: 2026-10-07 (round 37, its second pass, and the owner's decisions after it: a second
tool, `lapilli case`, built and not released — §7; the identity and the goal order rewritten — §0;
round 38 run and written up, the replay's tables it found unlike a cluster's repaired, and live compared with frozen command by command — §7; the recorder's state is as of v0.2.0, 2026-10-01). This file is the one place that says
what is left, what comes next and what the project is working on now. `DESIGN.md` §11 keeps the
version table; `README.md` keeps the feature list; both point here for anything about order and
priority. A review round that changes any of this updates this file in the same commit.*

## 0. The constraint everything below is checked against

Lapilli turns a Kubernetes incident into a file that can be checked later without the cluster
it happened on — sealed as evidence for the people who review it, and frozen as a case for the
agents asked to explain it (`DESIGN.md`, identity block).

- **The recorder** is *triggered by operational signals*, *gathers Kubernetes-native state
  across the incident window*, *seals it as an open, portable, offline-verifiable evidence
  file*, and **verifies and reads that file itself** — depending on no observability vendor
  and no AI tool (`docs/design-review-round27.md`). §1–§6 below are checked against this.
- **Cases** *freeze an incident together with its answer key*, *replay it with no cluster*, and
  *grade how an agent investigated* — shipping no agent, no model and no judge. §7 is checked
  against this.

Goal order (`DESIGN.md` §9): **first, that someone actually uses it; second, a CNCF Sandbox
listing; third, adoption that could be sold.** A market or strategy question is answered under
the identity, not by rewriting it.

Both paragraphs are the owner's, rewritten on 2026-10-07 (`docs/design-review-round37.md` §10).
Until then the recorder's sentence was the whole identity — its words are unchanged — and the
first two goals stood the other way round. Where §1–§6 say "the first goal" in the words they
were written in, they mean the listing.

## 1. Where the project stands

- **Built and proven.** Everything the README lists under *Built*: the recorder (webhook →
  `IncidentCapture` → collectors → hash tree → `.ieb`), optional signing (static key, AWS KMS,
  GCP KMS), object-store export, redaction, spec diffs, PromQL windows, notification, retention,
  capture retirement, the permission self-check that follows the installed profiles, the
  perishable profile (`coverage.deferred`), `lapilli verify` (local, directory, `s3://`, `gs://`,
  `https://`, `--output json`), `lapilli postmortem`, and `lapilli mcp` — Lapilli's own reader of
  its evidence, served from where the bundles are.
- **Released, twice, and every gate is green.** `v0.1.0` on 2026-09-28 and **`v0.2.0` on
  2026-10-01**, each published to four channels and each verified from the published files
  alone rather than from the tree: GitHub release (three CLI targets + `SHA256SUMS`, Sigstore
  provenance), a two-platform GHCR image, an OCI chart, and five crates on crates.io. `ci` is
  green on `main` with fourteen required checks (thirteen until `lapilli case` brought its own), and `release-gate` is green on kind 1.30 and
  1.37. The fixture sets under `test/fixtures/ieb/v0.1.0` and `v0.2.0` are frozen at their
  tags. The repository and its packages are public; private vulnerability reporting is on and
  has been used once ([`GHSA-7994-x9mx-vx43`](https://github.com/lapilli-project/lapilli/security/advisories/GHSA-7994-x9mx-vx43)).
  Thirty-five review rounds are logged under `docs/design-review-round*.md`; round 34 stopped
  attacking proposals and measured their premises instead, and **round 35 pointed that at the
  product**: three of the sentences this project leads with do not survive it. The count is a
  record of what was examined, not a claim of quality — independence has never been achieved.
- **`main` is protected.** A pull request, all fourteen `ci` checks including the kind E2E, an
  up-to-date branch and resolved threads; force-push and deletion blocked; required approvals
  **0** because one person cannot approve their own pull request, and the admin role bypasses so
  the maintainer still pushes directly. `CONTRIBUTING.md` carries the table, including the
  bypass — a bypass nobody mentions reads as a rule nobody has.
- **A second tool sits beside the recorder.** `lapilli case` freezes an incident together with
  its answer key, replays it with no cluster and grades how an agent investigated (§7,
  `docs/design-case.md`). Built 2026-10-06 in Go, three synthetic cases, 100 recorded runs, in no
  release. Its job, `case-tool`, is the fourteenth required check since 2026-10-07. It came out of round 37, which first measured that the compliance pivot is refuted,
  that the Sandbox gate asks for people rather than a sector, and that none of four candidate
  identities — the current one included — is something anyone is seen to use.
- **Not done, and this is the whole of it.** **Nobody outside the author has run Lapilli on a
  cluster they operate.** **There is one maintainer and no adopter, and that — not the code — is
  what the TOC postpones applications for** (round 29: every 2025–2026 postponement says "one
  active maintainer, no adopters listed"; no single-maintainer, zero-adopter project was
  accepted in that period). `lapilli.dev` resolves to Cloudflare's nameservers but carries no
  records yet, so the site is served from `lapilli-project.github.io/lapilli/`; the chart is not
  on Artifact Hub.

So the gap between the current state and a Sandbox listing is community work that has not
started: the code work is finished, and two releases have now proven the machinery that
delivers it. `DESIGN.md` §9's three conditions (a tagged v0.1, a kind-cluster demo, an
early-adopter signal) are necessary, not sufficient.

## 2. The next leap: ~~v0.1.0, tagged and public~~ → **the first outside install**

The rounds had refined a product nobody could install, and that is no longer the
sentence: `v0.2.0` is published and installs anonymously from GHCR. Criteria 1 and 2 below are
closed, so **the leap is now criterion 3 alone** — one person who is not the author, on a cluster
they run, reading one bundle. Every further feature before that postpones the only thing the first
goal is waiting for.

That is not a reason to stop fixing what is wrong. `0.2.0` shipped four user-facing defects' worth
of fixes, one of them a privacy defect in `SECURITY.md`'s own stated scope, and every one of them
was found by **measuring a premise** rather than by designing a feature — which is also why rounds
31, 32 and 33 returned three proposals to premise and round 34 did not need to. A first installer
should meet the version without those; shipping it is preparation for the leap, not a detour from
it. What postpones the leap is *new scope*, and v0.2's own scope is defined as "whatever the first
installs report" (`DESIGN.md` §11), which cannot be guessed.

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
| 0 | ~~**GitHub CI green again**~~ — **held since 2026-09-28, and now mechanical.** Every push between 2026-09-17 and then was red and nobody looked, for two reasons the local gate could not see: it regenerates the compatibility fixtures, so it never noticed that 39 of 43 `.ieb` files were git-ignored and absent on the runner, and quay.io's `minio/*` repositories became invisible to anyone but MinIO. **The same asymmetry returned four more times** — the notify hand-off defect, three defects in the paths round 30 changed, a postmortem cell `test/e2e/deferred.sh` greps verbatim, and a retention sweep that deleted a live capture's seal file (round 34). The rule this item stood for is now a `main` ruleset: fourteen required checks, the kind E2E among them, so a change cannot merge on a laptop's word. What remains of it is a habit rather than a task — read the remote gate, not the local one | assistant | A red CI on a public repository is the first thing an adopter and a TOC reviewer see |
| 1 | **Documents and norms in sync with the code** — permanent, and the one this project keeps failing. §1 of *this file* claimed "no tag exists, the repository and its packages are private" for three days after `v0.1.0` shipped, in the document that declares itself the one place saying what is left; `README`'s quickstart handed a new user the release with the redaction defect in it; `docs/COMPATIBILITY.md` still described what stood "between the code and the tag". Round 34 found four defects of exactly this shape in code and comments too (`deny.toml` naming a gate that did not exist, `CONTRIBUTING.md` promising a DCO check that did not exist, a metrics table the render disagreed with, an E2E asserting a proxy instead of its property). The header's rule — a change updates this file in the same commit — is the remedy and is cheap; forgetting it is what costs | assistant | A public reader's first hour is the docs; a stale claim there costs more trust than a missing feature |
| 2 | **Release mechanics that need no human judgement**: `CHANGELOG` complete for every shipped change with a **Migration** section (the CRD default-collector change, new chart values, the CEL rule needing `--server-side --force-conflicts`), `RELEASE.md` steps re-checked against the workflows, `ADOPTERS.md` present | assistant | `RELEASE.md` step 3 requires it; the tag is a one-way door |
| 3 | **Independent review of the verifier of untrusted input** — **first pass done 2026-09-25**: one non-Claude model (GPT-5) found seven real defects, all fixed (`docs/independent-review-log.md`), plus 15.6 M fuzz executions with no crash (`crates/lapilli-bundle/fuzz/`). Still owed: a second reviewer or a human, and a Gemini pass once its API answers | owner (a human) + assistant | The one review the loop cannot supply for itself; `RELEASE.md` lists it as a before-first-release gate |
| 4 | **Real-cloud KMS smoke** — GCP **done 2026-09-25** (`test/fixtures/kms/`). AWS: **closed by labelling, not testing** (2026-09-27) — the project has no AWS account and will not depend on one; README, `docs/kms.md` and `RELEASE.md` say AWS KMS and real S3 are verified against LocalStack only, until the first AWS adopter | done | Only emulators had run; the KMS path is a headline feature |
| 5 | **Repository public; private vulnerability reporting enabled** — **done 2026-09-27** (the day the private-repo Actions minutes ran out mid-release). Both GHCR packages public too (the org's package-creation policy had to allow Public first — an owner setting, no API) | done | Sandbox and every adopter conversation need a URL that opens |
| 6 | **Names**: `lapilli.dev` is paid for one year (expires 2027-09-21 — a renewal reminder for 2027-08 is all that is needed). crates.io: **published 2026-09-29** — `lapilli`, `lapilli-bundle`, `lapilli-net`, `lapilli-kms`, `lapilli-controller`, all 0.1.0. `cargo install lapilli` verified from the registry alone. All five names held. The CLI crate is `lapilli` (renamed from `lapilli-cli`); its directory stays `crates/lapilli-cli/`. Each crate now has `readme`/`keywords`/`categories`, versioned path dependencies and its own `LICENSE` copy (asserted byte-identical to the root by `scripts/attribution-check.sh`, because `cargo package` cannot reach outside a crate directory). `cargo publish --dry-run` passes in full for the two leaf crates; the three above them cannot be dry-run until their dependencies are on the registry. Order and constraints: `RELEASE.md` step 8. | done | The format identifier `lapilli.dev/ieb/v1` is frozen on that domain |
| 7 | ~~**Tag `v0.1.0`**~~ — **done 2026-09-28.** The tag is `v0.1.0` on `12f6eb4`, signed; the release carries three CLI tarballs with `SHA256SUMS`, each with a Sigstore build-provenance attestation that `gh attestation verify` accepts; the image is a two-platform manifest list with SBOM (394 packages), provenance and full OCI labels; the chart is `oci://ghcr.io/lapilli-project/charts/lapilli:0.1.0`. Verified the way a downloader would, from the published files only: every checksum and attestation, all three tarballs carrying `LICENSE`/`NOTICE`/`THIRD-PARTY-LICENSES.md` and their own `.dep-v0` dependency list (syft reads 195 crates out of the macOS binary), and a fresh kind cluster installing the published chart anonymously and sealing an **OK / 100%** bundle with the downloaded CLI. Published image scan: 0 Critical, 3 High, exactly what `docs/security-scanning.md` accounts for. **The first attempt published half of itself** — the macOS leg's binary had no `cargo-auditable` section on the `macos-14` runner, so `publish` was skipped after the image and chart had gone out; the tag was deleted and re-pushed once the matrix moved to `macos-15` (round-30 log §4c). Original text: **Tag `v0.1.0`** per `RELEASE.md` — freeze the fixture set, bump nothing (the workspace and chart are already `0.1.0`), signed tag, watch `release.yml`, smoke-test the published chart on a fresh kind. **`v0.1.0-rc.1` done 2026-09-27**: `release.yml` ran end to end for the first time (gate on both minors, multi-arch image with SBOM and provenance, OCI chart, three CLI builds with checksums, pre-release notes); the published chart + image + macOS CLI installed on a fresh kind and `lapilli demo` sealed an OK/100% bundle with the previous container's log and the rollout diff. The anonymous path was closed the same day once the org's package policy allowed public packages: with no registry login and no pre-loaded image, `helm install oci://ghcr.io/lapilli-project/charts/lapilli --version 0.1.0-rc.1` on a fresh kind pulled the image from GHCR by digest and `lapilli demo --scenario oomkill` sealed an OK/100% bundle. `RELEASE.md` step 6 holds for rc.1 | owner + assistant | The leap itself |
| 8 | **The early-adopter signal.** **Owner's decision, 2026-10-05: the demand test is discarded — no interviews; judge from what can be measured without asking** (round 37 §1c is what was measured instead, and §7 of this file is what followed). The rest of this cell is what the item said until then, kept because the rule in it was fixed before any answer and never run. The step before any of this is the question, and it has never been asked: [`docs/demand-test.md`](docs/demand-test.md) is the script and the **decision rule, fixed before any answer exists** — ≥2 of 5 operators naming expired events or the lost object *unprompted* means continue; 0 unprompted with no specific incident behind the prompted yeses means round 29's pause criterion applies now. Round 35 refuted *"zero adopters proves no demand"* only because nobody had been asked, which makes asking the load-bearing task rather than a courtesy. Then the cheapest experiments: one team that runs Kubernetes without an audit-log pipeline installs it for a week; one ISMS/ISO auditor reads a bundle and says whether it answers a control; one incident on a cluster the author does not operate. Each install carries three counters for ninety days: bundles sealed, bundles opened, postmortems that cite one | **owner** (conversations) | `DESIGN.md` §9's third condition; nothing in the repo can produce it |
| 9 | **A second maintainer and named adopters** — the two things every postponement names. Ship the pitch as part of the alert pipeline's standard (an Alertmanager receiver in the kube-prometheus-stack values example) rather than as post-incident reflection, which no public postmortem ever records as missing evidence | **owner** + assistant (docs, examples, talk material) | Round 29 §2 F1, F5 |

Not on this list on purpose: new collectors, new triggers, new consumers. Under §0 they are not
what a Sandbox listing is waiting for.

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

  What round 29 F4 asked for is **re-opened by round 35**, which decomposed the number this file had
  been quoting against itself. "0 of 41" is `0 accept / 17 drop / **24 unknown**`, and the unknown
  mass is there because Lapilli *infers* its target from a `pod` label (`webhook.rs:312` drops any
  alert without one). Letting the rule author **declare** the target instead — the answer Red Hat
  shipped four years ago, and a mechanism this codebase already has in `lapilli.dev/export`
  (`webhook.rs:421`) — is a product decision, not a physical limit, and it does not touch the
  identity sentence or the frozen `incident.target`.

  **That is now the test, and it carries the stop condition.** Re-measure against the same 155
  kube-prometheus-stack rules after the target can be declared. If the number does not move, the gap
  was a physical limit after all and round 29's pause criterion applies **now** rather than in
  2027-Q1. Written here before the work starts so it cannot be renegotiated after it.

  The route to the fast-perishing class remains round 16's candidate 3, below, which is an owner's
  decision rather than a next step.


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

  **The precondition is now met, and meeting it found a defect.** A floor needs the modes to be
  ordered, and `off < default < strict` was a declaration order, not a measured one: `Mode`
  derives no ordering, and `Off` sorts last while being the weakest. Measured over 25,200 cases
  before any proposal was written — the order rounds 31–33 earned — and containment was **false**:
  a name in `redaction.plaintext` skipped redaction entirely rather than skipping strict's
  widening, so `strict` redacted *less* than `default` on every named surface. Fixed, with the
  invariant pinned by `strict_never_redacts_less_than_default`, and the CHANGELOG carries the
  operator's question. What remains for the floor itself is the mechanism, and the shape is
  settled by precedent rather than open: a `RunArgs` flag (not a bare `std::env::var` — the two
  security-relevant pins, `LAPILLI_SIGNING_KMS_KEY` and `LAPILLI_NOTIFY_ALLOW_HTTP`, are bare
  reads today and get no `--help`), clamping rather than refusing, because refusing loses the
  evidence the product exists to keep and the KMS precedent already clamps and says so in the
  status; plus the chart's two-layer belt, which refuses to render a profile below the floor.

- **`lapilli verify` does not report the redaction exemption list.** It reports the mode; a bundle
  sealed with `strict` and a non-empty `plaintext_names` is a bundle whose exempted names are only
  as redacted as `default` makes them, and a reader classifying it before sharing cannot see that
  without unpacking. Left out of the fix above deliberately: `lapilli verify` is the one command
  whose output carries a compatibility commitment (`docs/COMPATIBILITY.md`), so a new line in it
  is its own change.
- **`lapilli erase <incident>`**: there is no targeted deletion, and the manual path is a trap
  (removing `<incident>.notified` makes a month-old incident get announced to Slack as news) and
  is not journalled, which the design's own "deletion has to be at least as recorded as capture"
  does not allow. `docs/data-handling.md` documents the procedure; the command is v0.2.
- **A replacement real-KMS fixture**: the GCP one was removed because it carried a real project
  id under its signature. A new one needs a throwaway project whose id is nobody's.
- ~~**`postmortem` has no output cap**~~ — done 2026-09-30, and the measurement changed the
  judgement. This item said the fix was a contract decision because truncating evidence is its own
  harm. Measured first (`docs/design-postmortem.md`, last section): a **1,615-byte** bundle that
  verifies **OK** rendered a **1,051,015-byte** document, and the same channel at 256 MiB rendered
  **268 MB** in five seconds — the escaping amplifies 2x to 3x rather than bounding, and `ieb/v1`
  rule 10 fixes no per-member size for `resources/`, `logs/`, `timeline.json` or `diffs/**`, so the
  document was bounded only by the 2 GiB whole-bundle limit.
  But the values that did it are **metadata, not evidence** — a kind, a name, an actor, a field path,
  a cluster id — and `summary.rs` already bounded the two values *closest* to evidence
  (`before`/`after` at 100, `last_line` at 300). So a cap restores consistency with its own
  neighbours instead of making a new contract. The one evidential channel, the event message, took
  the spec's own answer for a truncated log tail: bound it, say how many were cut, and keep the whole
  value in `timeline.json` under the signature.
  The metadata caps went into `summary.rs` rather than the renderer, because the same `Summary` is
  what the MCP `summary` tool serialises — measured at 268 MB in one JSON-RPC frame from the same
  file — so a renderer-side bound would have left that tool open. All 43 frozen fixtures render
  byte-identically before and after.
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

## 7. Lapilli cases — the second line (round 37)

**This is new scope, and §2 says new scope is what postpones the leap.** It was not added by drift.
Round 37 §1c measured the leap's premise — that someone has the recorder's problem often enough to
install it — bottom-up across twenty projects' trackers and found the case only Lapilli covers to be
real and rare; the owner then chose to open a second line rather than look for another gap. §2 above
is unchanged and §3 is changed in one cell, item 8; both still describe the recorder. The identity
in §0 has a sentence above both tools and one for each, and the pitch follows it: `README.md`, the
site and `DESIGN.md` open with that sentence and then name each tool with the state it is in — the
recorder released, cases pre-alpha and in no release.

### Where it stands

- **Built, and taken apart once.** `lapilli case verify | seal | freeze | pack | export-metrics |
  serve | run | packets | report | promq`, three agent adapters, three sealed cases, the scenarios
  that rebuild them, and 100 recorded runs (`docs/design-case.md`, `docs/case-format.md`,
  `docs/case-grading.md`). The day after it was built, two reviewers that had not written it and one
  run of each real agent found twenty-two defects in it and in what was written about it — a guard
  with ways through, a clock that left a case with two times, an agent given its operator's whole
  environment, a `freeze` that started privileged pods (round 37 §9). Those are fixed.
- **Exercised end to end, once each.** On Linux in CI. All three scenarios rebuilt on kind and
  frozen. A Prometheus inside a cluster frozen and compared with itself at the freeze instant: five
  queries, the same values. Claude Code and HolmesGPT each run through the harness against a frozen
  case.
- **Measured twice, and the second time it measured its own instrument short.** First with one
  model, three runs a side, on an instrument later found faulty: 3 of 9 live and 3 of 9 frozen
  (round 37 §3, §9). Then **round 38**, 2026-10-07, under a rule committed before any run: two
  agents, two judges of two model families, 72 runs.
  - *Outcomes were not distinguished.* The agent that can pass passed 5 of 18 live and 5 of 18
    frozen: not shown to differ by more than 28 points, and nothing narrower is claimed. The other
    agent passed nothing in either condition and says nothing about fidelity.
  - *Retrieval is still necessary.* 0 of the 57 runs that lacked decisive evidence passed; 10 of
    the 15 that had it did.
  - *The judges agree, mostly.* Cohen's κ 0.81 where a pass was possible; three disagreements, one
    statement of one key, the process check with the stricter judge each time.
  - *The rule on fidelity was missed in one cell of four*: `describe pod` at 0.76 against a band of
    0.8 to 1.25, though the same command typed in both conditions comes out at 1.00.
  - ***And what no rule looked for was found afterwards, by reading the transcripts.*** A frozen
    case answers some `kubectl get` commands as no cluster does: a listing sorted by a field
    outside `metadata` comes back empty, `-o wide` adds no columns for pods or Deployments, a pod
    asked for by name prints as `NAME AGE`. At least 33 steps in 19 of the 36 frozen runs, none
    live. These steps succeed, so the rule, written about errors, does not count them
    (`docs/design-review-round38.md`, *Outside the rule*). Eighteen runs a side could not show that
    it cost a pass. That is a statement about eighteen.
- **Repaired, and from now on compared rather than read.** The front that stands before the
  snapshot server answers a table request itself, as the API server does, and with it a watch by
  name, a request for an object that is not there, and a blanked Secret
  (`docs/design-case.md` §3). `test/replay-diff` then asks a cluster and its frozen copy the same
  commands — a fixed set about every kind the cluster has, and every command the recorded agents
  typed. On 2026-10-07, three scenarios: **1,346 commands, 1,327 the same, 12 that differ, all of
  three kinds that §8 of the design names** (`explain`, `cluster-info`, `describe secret`); the
  other 7 are the cluster moving while it was asked. The same sweep found five differences nobody
  had read their way to, among them a `rollout status` that reported the wrong Deployment. It runs
  as a workflow, not a required check, when the replay changes and once a week. **No recorded run
  has seen the replay as it is now**, and no agent has been run against it.
- **Not done, and this is the whole of it.** Nobody but the author has run a case, written a case,
  or judged an answer. `lapilli-case` is in no release.

### Next, in order

| # | Item | Who | Why it is here |
|---|---|---|---|
| ~~1~~ | ~~**Re-run the live-against-frozen comparison**~~ — **done 2026-10-07, round 38.** Two agents, two judges of two model families, six runs a side, the rule and the analysis committed before any run. It also took in a second judge with agreement between the two, and a second agent. What it found is above | done | The recorded 3 of 9 against 3 of 9 was measured before the clock, the guard, the adapters and the field-selector filter, and three runs cannot separate 1 in 3 from 0 in 3 |
| ~~1a~~ | ~~**Repair what round 38 found in the replay**~~ — **done 2026-10-07.** The front answers a table request itself where the snapshot server's table is not a cluster's — rows that carry the object when asked (`includeObject=Object`), the wide columns of pods and Deployments, real columns for ReplicaSets, Endpoints, EndpointSlices and events, a table for one object asked for by name. And the Claude Code adapter's permission list built from the guard's own: it refused `kubectl rollout history` twelve times. (Nine more reads it refused are Claude Code's own doing — a filter in `custom-columns`, a pipe into `awk` — and a wider list does not change those) | done | In half the frozen runs an agent was shown an answer no cluster gives. `design-case.md` §8 lists them as known and not repaired, and a known difference that common is not a caveat, it is a defect |
| ~~1b~~ | ~~**The same command against a cluster and against its frozen copy, output compared**~~ — **done 2026-10-07**: `test/replay-diff`, over a fixed set about every kind and every distinct command in the recorded transcripts, on each scenario, ages aside; as a workflow of its own, on a change to the replay and weekly, not required | done | Field selectors and `--tail` were found by a reviewer reading code; the tables by someone reading transcripts for another reason. Twice is a pattern: fidelity has been checked where somebody thought to look. This needs no judge, no model and no reading, and should have come before round 38 |
| 1c | **The kinds the sweep has never seen**: a scenario, or a fourth kind cluster, with a StatefulSet, a Job, a CronJob, an Ingress, a PersistentVolumeClaim, an autoscaler and a custom resource, so that their tables are compared too; and a case frozen from a cluster that is not v1.37 | assistant | The front writes tables as v1.37 does and only for the kinds three small scenarios have. Every other kind keeps the snapshot server's columns, which for some is a name and an age. `design-case.md` §8 says so; saying so is not the same as it being right |
| 1d | **An agent on the repaired replay.** Not round 38 again: a handful of frozen runs of the agent that can pass, to see the transcripts no longer hold an answer a cluster would not give | assistant | The sweep compares commands, not investigations. What an agent does with a faithful answer has not been looked at since the answers became faithful |
| 2 | **Each Kubernetes evidence item naming the command that reaches it**, run against the served case | assistant | Evidence is checked to *exist* in the frozen copy, in both stores, at freeze time and in CI. That a tool reaches a Kubernetes item was checked by hand. A metrics item can already name its query |
| 3 | **A judge who is a person** | owner | Two models agreeing is two models. Round 38 measures their agreement with each other and nothing about their agreement with anyone |
| 4 | **A stronger agent** on the three cases | owner (provider credit) + assistant | Both agents so far run small models, and one of the two passed nothing in 36 runs: a comparison between conditions needs an agent that sometimes passes, and round 38 had one. Whether the cases still separate anything at the top is not known |
| 5 | **A case written by someone else** | **owner** | Independence. Until then every case, the grader and the rubric share one author |
| 6 | **The release gates, before any tag carries `lapilli-case`**: licence attribution for the Go graph beside `THIRD-PARTY-LICENSES.md`, an SBOM, build provenance, `release.yml` building it for the same targets as `lapilli`, `RELEASE.md` and `docs/COMPATIBILITY.md` saying what is and is not promised about the case format | assistant | The recorder's releases are verified from the published files alone; a second binary shipped without those gates would be the first thing in a release that is not |
| 7 | **Redaction for a case frozen from a real cluster** | assistant, after a decision | `freeze` blanks Secret values and nothing else. The recorder's redaction engine is Rust; writing it twice puts security code in two languages, and sharing test vectors under `spec/` is the cheaper half of that |
| 8 | **A path from the recorder to a case** | undecided | A bundle is one pod's window and a case is a whole cluster. Nothing connects them today, and nothing here should say otherwise |
| 9 | **Log and trace stores**, frozen with the same clock; **a held-out set** held by someone other than the author | later | Pod logs come from the API today. Public cases get trained on |

### Not planned

An embedded judge or an embedded agent (`docs/design-case.md` §9, and now a rule: `GOVERNANCE.md`,
*Grading neutrality*). A single score. Grading a fix: other projects do it, and it needs a live
cluster.

### Parked, by the owner's decision

**An issue upstream for what the snapshot server ignores** — field selectors, `logs --tail`. The
front in `internal/replay/fields.go` does both, live against frozen agreed on six selector questions
and on `kubectl describe`, and Lapilli does not wait on crust-gather for either. It accepts more
than a real API server does, `logs --since` is still ignored, and all of it is ours to maintain; the
day that costs something is the day to file it.

### What this does to §5

Nothing yet. A Sandbox application still needs what round 37 §1b measured the gate to ask for — a
repository six months old (2027-03-17 at the earliest), maintainers from more than one organisation,
adopters who can be checked — and a second tool supplies none of the three. The parties with a
reason to co-maintain a neutral set of cases are the projects whose agents it grades.
