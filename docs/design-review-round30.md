# Design review — round 30 (the last look before the tag: does the code match the design, and is it safe to publish?)

Constitution: **v0.7.1**. Target: the whole tree at `f410c15`, asked two questions the owner put
plainly — *does this match our design, is anything different, and is anything missing on the
security side?* Six lenses in one wave: **design conformance** · **the cluster** (an attacker who
can POST to the webhook, owns a workload, reads the PVC, reaches the mcp port, or has the
controller) · **supply chain and release** · **privacy, data handling and law** · a **non-Claude
security devil's advocate** (GPT-5) · and machine scanners (`cargo audit`, `cargo deny`, `grype`
on the published image, `kube-linter` on the render). Then five parallel editing passes on disjoint
file sets, and one arbiter pass.

Independence: partial, one of six lenses non-Claude. Everything below is calibration by the
author's own model family except where GPT-5 is named, and it found what a calibration pass can
find: contradictions between what the tree says and what it does. It cannot say the tree is right.

## 1. The BLOCKER

**Retention deleted the staging directory of a capture that was still collecting.** `decide()`
refuses to reclaim abandoned work that a live capture still holds — keyed on the capture uid in the
directory name, *"never on age alone: a capture may legitimately sit in `Sealing` for days through
a KMS outage"*, which `docs/design-retention.md` spends a paragraph guaranteeing. The parse took
the uid as the text after the **last** `-`. A Kubernetes uid is a UUID with four of them, so
`.staging-<incident>-1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed` yielded `ab8dfbbd4bed` and the live uid
it was compared against was the whole string. The comparison could never be true. `Refusal::InFlight`
was unreachable, and every sweep classified a live capture's staging as abandoned and removed it —
`staging-lost`, the loss of evidence already collected. The unit tests passed because their uids
were `uidA` and `uidB`, which have no hyphen.

Retention is off by default, so the blast radius is installs that turned it on. Fixed by matching
the whole `<incident>-<uid>` tail with a `-`-anchored suffix test, and **mutation-proven**: with the
old comparison restored, the new regression test and the existing one both fail.

## 2. Two Criticals about material that was already public

Both were the arbiter's own work from the preceding two days, which is the useful part of the
finding: the loop had been reviewing its output and not its footprints.

**A real GCP project id shipped inside a signed fixture.** `test/fixtures/kms/` held the bundle from
the 2026-09-25 real-KMS smoke, and the signing key's full resource name — hence a personal project
id — was inside its `logs/controller-current.log`, under the signature. Editing the log would have
broken the only thing the fixture was for. The fixture is removed; the run stays recorded in
`docs/design-kms.md`. **The general defect is the real one**: the controller logged the full KMS key
resource name at INFO in four places, and `logs/` is never redacted, so any adopter capturing their
own controller pod sealed their AWS account number or GCP project id into a bundle. `KmsKey::redacted()`
now elides the account and project at INFO and ERROR; the full name stays at DEBUG and in
`status.seal.key`. A test asserts neither appears. (A test of that fix also carried a real account
number, from this machine's credentials. It does not now.)

**Public claims about a third party.** `ROADMAP.md` and the round-29 log said an employer's cluster
was *"a candidate adopter, with the employer's consent"*, and `docs/design-kms.md` recorded that the
author held that employer's AWS credentials and had weighed using them. The consent was not ours to
publish and the credentials were not ours to discuss. Removed, with a correction note in the round
log rather than a silent edit. Adjacent to it: `MAINTAINERS.md` said *Independent* while three files
said *the founder's employer*, and the file had **no contact column** while `CODE_OF_CONDUCT.md` and
`SECURITY.md` both routed reports to it — so the conduct chain ended nowhere, and with one maintainer
"report to the maintainers" and "report to the person involved" are the same address.
`conduct@cncf.io` is now named **first**, for exactly that case.

## 3. The rest, by lens

**Design conformance.** Export destinations bypassed `lapilli-net` entirely — no host locality, no
link-local refusal, no redirect ban — so `docs/egress.md`'s *"refuses link-local for every endpoint
it parses"* was false for the one path that carries evidence out of the cluster; now parsed, with
the two rules that cannot follow (`object_store` builds its own client, shared with the credential
chain, where refusing link-local breaks EKS Pod Identity and GKE Workload Identity) stated instead
of implied. `upload` verified a path and re-read it, so "only a bundle it verified itself leaves the
cluster" was a claim about a file name; the bytes are read once now. `merge_timeline` re-sorted by
raw timestamp and undid the "untimed events last" rule on the default collector set — the top line
a reader takes as the start of the incident. `spec/IEB-SPEC.md`'s signing table marked unsigned
integrity ✅ against `DESIGN.md` §5's ⚠️ and called KMS v0.2. §7's "read per request" was a 5 s cache.

**The cluster.** The webhook token was worth a read of any pod's logs in the cluster:
`watchNamespaces` was only ever used to decide which permissions to *ask* about, and the target
namespace comes from the alert's labels. A capture outside the watched list is now refused
(`target-not-watched`), with an E2E step, and `DESIGN.md` §7 says what an unset list means. The mcp
sidecar — reachable from any pod, unpacking tars — held the controller's ServiceAccount token while
its own comment said *"no Kubernetes API access of its own"*; the pod now sets
`automountServiceAccountToken: false` and only the controller container mounts a projected token.
The chart granted write on `captureprofiles` that the controller never uses, which would have let a
compromised controller set `redaction.mode: off` or point a notify route at another team.
`webhook.networkPolicy` was a **pod-wide** ingress deny that silently cut `/healthz`, `/metrics` and
the mcp port while four places in the repo — including a design-review record — called it a
restriction on the webhook port. Log collection was bounded by lines only, and a runtime splits at
16 KiB, so the published memory envelope had a workload-controlled term above it.
`lapilli postmortem` interpolated every alert- and workload-chosen string raw into Markdown, so one
forged alert could end a table row and write its own `## Root cause` into a document that is pasted
into a wiki and handed to an LLM.

**Supply chain.** `aws-lc-rs` was compiled into the shipped controller — 41 `aws-lc` strings and 21
OpenSSL strings in the published binary — while three comments in the same manifest said it was not;
one missing `default-features = false`. The base image carried **two** Critical findings that no
refresh could clear (`libssl3`'s fix does not exist in Debian 12; a `libc6` one is *won't fix*), so
the base moved to trixie, digest-pinned: 2 Critical → 0. The release gate ran with `contents: write`
and `packages: write` while creating kind clusters through third-party actions; `id-token` and
`attestations` were granted and used by nothing. No action was SHA-pinned, and
`dtolnay/rust-toolchain@stable` is a force-pushed branch, not a tag. The image build did not use
`--locked`, so the audited lock constrained only one job. And the asymmetry a reviewer would find
first: a project selling offline-verifiable evidence shipped **no way to verify its own downloads** —
`SHA256SUMS` beside the files it describes, no attestation, and no document telling anyone to check
anything.

**Privacy and law.** The artifacts met none of the third-party attribution floor: no `NOTICE`
(Apache Arrow's `object_store` ships one that §4(d) requires passing on), ~28 crates whose terms an
Apache-or-MIT choice does not discharge, no copyright holder anywhere, **zero OCI labels** on the
published image, and an SBOM that enumerated twelve Debian packages and none of the 343 crates
because syft has no package database to read in a static binary. The bundle is in practice a
personal-data export — labels are never redacted (`redact_meta` visits annotations), nor are pod and
host IPs, `nodeName`, `serviceAccountName`, `managedFields`, or any log line — and no page said so.
`.gitignore` covered `*.ieb` but not the unpacked demo directory, `*.summary.json`, `reclaimed.jsonl`
or `.unsent`, so a `git add -A` in a clone could commit somebody's operational data. Five places
promised `strict` mode was *"for a guarantee"* against `redact.rs`'s own comment. `lapilli mcp` told
an LLM `redacted_at_capture: true` for a bundle captured with redaction **off**, computing it from
the path alone while the verify-result document in the same function carried the real mode. Nothing
anywhere said that pointing an MCP client at a bundle sends it to that model's provider — and
`docs/egress.md` actively reassured, because the read is *inbound* and no egress rule is in that
path. There was no erasure procedure, and the obvious manual one re-announces a month-old incident
to Slack. `.unsent` — which holds workload content — was in retention's never-list, implementing
"survive forever" where the requirement was "survive a restart".

**GPT-5's lens** agreed on the mcp service-account token and the unpinned actions, and raised a
TOCTOU on mcp's verify-then-unpack that the cluster critic then **refuted with the threat model**:
the PVC is RWO and mounted read-only in that container, the controller writes with `O_EXCL`, and the
only writer left already holds the signing key. Logged as refuted, not fixed.

## 4. What the round did not fix, and why

- **A second maintainer and named adopters** — the only things the TOC postpones for. Unchanged.
- **`redaction.minimumMode`**, an admin floor on privacy. Signing is pinned by the admin and notify
  routes are admin-defined; redaction is the one privacy control a profile decides, so anyone who
  can patch a `CaptureProfile` can set `off`. Documented as a trust boundary; adding an enforcement
  point on the capture path days before a first tag is how a recorder stops recording. v0.2.
- **`lapilli erase <incident>`** — `docs/data-handling.md` documents the procedure; the command,
  and journalling a manual deletion, is v0.2.
- **Hand-off GC when retention is off** — `.unsent` is age-limited now, but the sweep that reclaims
  it only runs when a retention bound is set, so on a default install it still lives forever, as
  abandoned staging already did. The fix touches startup. v0.2.
- **A replacement real-KMS fixture** — needs a throwaway project whose id is nobody's.
- **`postmortem` has no output cap** where `read_file` has one. Escaping makes a crafted bundle
  harmless, not small. Truncating evidence in a permanent document is its own harm, so this is a
  contract decision.
- **The `>=` in the truncation note** means a tail exactly at the byte bound loses its last line
  though it was not really cut. Accepted: a false "not last words" is safer than a false "last words".
- **A `NOTICE` inside the Helm chart** was proposed and refused. The chart is Lapilli's own templates
  and links nothing, so copying the root `NOTICE` — whose subject is 277 statically linked crates —
  into it would assert an attribution the chart does not carry. That is the same defect class as the
  rest of this round, arriving as a fix. The chart keeps `LICENSE`, and its README says where the
  third-party attribution actually lives.

## 4b. Two things the round found afterwards, in its own fixes

**The attribution gate ran nowhere that would catch what breaks it.** All of it lived in
`scripts/release-check.sh`, which runs before a tag. But `THIRD-PARTY-LICENSES.md` is generated from
`Cargo.lock`, and the thing most likely to move the lock is not an edit — it is the Dependabot
`cargo` group this same round added. A bump would have left the listing stale for however long it
took to reach the next tag, which is to say a licence we had stopped distributing and nothing saying
so. The portable half is now `scripts/attribution-check.sh` and a CI job runs it on every pull
request.

**And that check could pass while looking at nothing.** It reads each dependency's own `NOTICE` out
of the unpacked registry source and *noted* the crates it could not find. On this laptop everything
is unpacked, so it printed one reassuring line; on a fresh runner `cargo fetch` leaves only `.crate`
archives, so all 277 would have been "not vendored, NOTICE not checked" and the job would have gone
green having read nothing. It now unpacks with `cargo metadata` first and **fails** rather than
noting. The pattern is exactly the round's own lesson arriving a third time: a check whose skip path
is quiet is a claim, not a check.

## 4c. And four the cluster and the tag found after the round closed

The round's gate was the local one, which does not run the kind E2E. Pushing the batch and reading
the remote gate — the definition of green this project settled on after CI was red for eight days —
turned up two more, on all three clusters.

**A handed-over notification was reported as a swallowed repeat moments after it was posted.** The
same gap the hand-off exists for puts one capture in two places inside the *successor*: its own open
coalescing group (the capture is newer than this process, so it is not history) and the predecessor's
hand-off. The replay posted and wrote `sent`; the phantom group closed eight milliseconds later, hit
the cooldown the replay had just armed, and patched `status.notification` to `repeat`. Neither the
cooldown nor the claim can arbitrate that — both say "somebody handled it", which is true of the
phantom's own process. Consuming a hand-off now withdraws its members from the open group, and a
recorded `sent` is never replaced by an outcome that posted nothing. Both mutation-proven.

**The export suite's LocalStack lived in a namespace called `s3`.** So the endpoint had to be
`localstack.s3.svc.cluster.local` — which is exactly S3's virtual-hosted form, `<bucket>.s3.<…>`.
LocalStack parsed the Host header that way, took `localstack` as the bucket name, and answered every
path-style `PUT /evidence/…` with `NoSuchBucket: localstack`. The client addressed correctly and the
server read the DNS name; the namespace was the only part anyone controlled, and it is `objstore`
now. This one is the round's own doing: the export endpoint validation it added is what forced the
fully qualified name, and the round had no way to see the consequence because it never ran the suite.

**Tightening `captureprofiles` to `get` took `list` away from the permission self-check.** The round
removed the write verbs the controller never issues — right, and the reason stands — but it wrote the
remaining grant as `get` alone, and cited the check table as its authority. The table said `get`
while `perms.rs` a few lines away **lists** profiles: that list is how the collector checks get
narrowed to what the installed profiles actually need. Denied, the narrowing silently stopped, every
collector check was asked, and an install whose profiles want no logs reported `pods/log` as a
missing permission — `lapilli_permissions_denied 1` where the deferred E2E asserts 0. Nothing failed
loudly; a self-check reported a permission nobody needs as missing, which is the worst thing a
self-check can do. The grant and the table are both `get, list` now, `perms.rs`'s every-verb test
carries both (and the new `get` on the status subresource), and `helm-renders.sh`'s assertion was
rewritten from "get and nothing else" to the property it meant: **both reads, and no write verb
ever** — mutation-proven in both directions.

**And the fourth arrived on the tag itself.** `cargo auditable`, which this round added so every
published binary carries its own dependency list, embeds that list in a `.dep-v0` section — and on
Apple targets it keeps the section from being dead-stripped with a linker flag,
`-Wl,-u,_AUDITABLE_VERSION_INFO`. On the `macos-14` runner image the section was simply absent, so
the packaging assertion — correctly — refused to publish the tarball. By then the image and the
chart had gone out, so `v0.1.0` published half of itself. The same source, rustc and
cargo-auditable version embed it correctly on macOS 15 (ld-1115.7.3), verified by hand, and GitHub
is retiring macos-14 anyway; the matrix moves to `macos-15`, the workflow now prints the section
table and the symbol before judging, and `scripts/release-check.sh` builds the CLI for **this
host's** target and asserts the section — the only pre-tag signal the Apple leg has ever had.

Four findings, four of a kind: a claim in one place, checked against something that repeated the
claim, with the thing that actually decides doing something else. The round's §6 lesson said exactly
this and the round still shipped four more, because the check it reached for was the local gate —
and for the fourth, because two of the machines that build the release cannot be reached from any
gate at all. That is now written into `RELEASE.md` step 4 rather than learned again.

## 5. Verdict

**Applied. `v0.1.0` was not tagged today, and that is the round's result.** The tag was one signed
commit away when this started; the code was feature-complete, the gate was green on two Kubernetes
minors, and a release candidate was already published. What the lenses found was not missing
features but a dozen places where the tree said one thing and did another — and in an evidence tool
those are the same class of defect, because every one of them is a claim a reader would have
believed.

Gate after the round: fmt, clippy in both feature sets, **276 tests**, 47 fixtures byte-identical,
`helm lint`, every chart render, `kube-linter`, `cargo deny`, `actionlint`, promtool, CRD drift.

**Closed on 2026-09-28, one commit later.** The batch went out as `8c98332`, the remote gate refused
it three times over (§4c), and `c577fbc` fixed all three: **309 tests** locally, `ci` green on all
ten jobs including the new attribution one, and `release-gate` green on **both** Kubernetes minors.
That last line is the sentence this verdict was waiting for, and the round is what taught the project
to wait for it rather than for the laptop.

## 6. What this round taught the method

**A pre-release pass has to include the reviewer's own footprints.** Both Criticals were the
arbiter's work from the previous two days — a fixture it built and committed after verifying the
wrong thing (it checked the bundle for the access *token* and not for the project id), and sentences
it wrote about somebody else's consent. The loop was reviewing its output and not its residue, and
no lens was pointed at "what did we publish this week".

**A test's fixtures can hide the defect they were written for.** `uidA` has no hyphen, so the
retention guard's tests passed for two weeks against a parse that could never match a real uid. When
a test's input is a placeholder, the shape of the real input is the thing to assert.

**A round that changes a path the local gate does not exercise is not finished when the local gate
is green.** Round 30 rewrote the export endpoint path and added a webhook refusal, and neither had
ever run on a cluster; §4c is what the first remote gate said about that. The rule is not "run
everything always" — the kind E2E costs forty minutes twice — it is that a round must name which of
its changes only the cluster can judge, and not call itself dry until that run has happened. Stated
here rather than in the constitution because it is the same asymmetry as the 2026-09-17 CI outage and
the notify "flake", arriving a third time in a different disguise.

**"It is documented" and "the document is true" are different checks**, and this round was mostly the
second one. The strongest findings came from reading a claim and then the code that was supposed to
keep it: link-local refusal "for every endpoint", "no Kubernetes API access of its own", "a
restriction on the webhook port", "redacted at capture", "for a guarantee", "with SBOM". Every one
was written in good faith by someone who believed it.
