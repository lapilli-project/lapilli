# Design review — round 24 (converge)

Constitution: **v0.6.0**. Target: `docs/design-record-and-seal.md`.
Snapshot attacked: **`22a603b`** — the round-23 fixes, which the arbiter wrote itself.
Critics rotated: none of the three had cleared the fix it was given.

## Verdict first

**Not one of the eleven round-23 fixes held cleanly. Round 2 produced eleven NEW-BLOCKERs.**

But the failures are not scattered — they are all on **one side of the design**:

| Half | Round-24 result |
|---|---|
| **Phase A** (perishable capture) | Survives. Three cheap, named fixes; the premise held both rounds |
| **Phase B** (`seal` / backfill) | **Every mechanism broke**: identity, signature, redaction, notice, credentials, naming |

So the verdict is split, and the split is the finding:

- **Phase A → APPLY and proceed** (with the three fixes below).
- **Phase B → RETURN-TO-PREMISE.** Not KILL: the goal is sound. But every mechanism failed
  *for the same underlying reason*, which means patching the next round would be the fourth
  attempt at a mechanism the format is refusing. That reason is stated at the end.

A third, unrelated outcome: the loop found a **defect in shipped code**, independent of this
design. It is the most valuable single thing either round produced and it ships on its own.

## 1. Phase B — how each mechanism broke

### The signature fix was unimplementable

`docs/design-record-and-seal.md` promised a `parent` block carrying the phase-A bundle's
root, `key_id` and signature bytes, so a consumer could verify the core.

- **The signed payload is the literal bytes of `manifest.json`** (`spec/IEB-SPEC.md:320`, and
  §"Why these rules": *"the signed payload is the literal manifest bytes … exactly cosign's
  blob model"*). `hash_tree_root` is a field *inside* those bytes, not the preimage. ECDSA
  verification from a root hash is impossible. The block carries a signature without the
  thing it signed.
- **And the doc's promise is false against the shipped verifier.** `verify.rs:945-952` takes
  the `(None, None, Some(_))` arm for an unsigned bundle with a caller key →
  `SignatureStatus::Absent`; `verify.rs:1021-1022` then sets
  `sig_failed = trusted_key.is_some() && signature != Trusted` → **true** → `Verdict::Failed`,
  exit 1. `lapilli verify sealed.ieb --key controller.pub` **fails**. The document claimed the
  opposite in so many words.
- **`parent` is also forgeable.** The sealed bundle is unsigned by construction, so an
  adversary rewrites `logs-backfill/`, recomputes the bundle's own tree, and copies `parent`
  byte-for-byte. `spec/IEB-SPEC.md:375` already names this posture. An attacker-copyable
  provenance assertion that verifies OK at coverage 100% is **worse than silence**.
- And `spec/IEB-SPEC.md:283` is normative in the other direction: *"Readers MUST ignore fields
  they don't know."* The block is spec-mandated to be ignored.

### The identity fix overflows a budget that is already exactly full

`<parent_id>-s1` was supposed to keep one artifact per id.

`export.rs:547-549` caps a path segment at **100 bytes**. `crd.rs:211-213` caps `cluster_id`
at 83, and says why, verbatim: *"83 because the webhook composes `<cluster>-<16 hex>` and
`path_safe` caps the result at 100."* **83 + 1 + 16 = 100, exactly.** `-s1` needs three bytes
that do not exist. For any cluster id of 81–83 characters the sealed id is unrepresentable,
and `crd.rs:204-208` deliberately leaves `spec.incidentId` unconstrained, so any parent id
≥ 98 bytes has no `-s1` form at all.

Where it fails is worse than failing: `remote.rs:184`'s `path_safe` guard returns `None`, so
`verify_cmd.rs:137-151` leaves `expected_incident` unset with `source: "none"` and prints
*"identity: (not checked: the URL doesn't name one; use --cluster/--incident)"* — **no problem
code, no verdict change, and the message is false.** The object-key identity binding that this
document calls the anti-substitution defence **silently disappears on exactly the artifact the
fix invented.**

And the clause *"sealed bundles are never written to the controller's bundle root"* trades away
the mechanism round 17 was actually closed with: `reconcile.rs:849-850` — *"the incident-id
claim below is only exclusive within one directory."* The `O_EXCL` `.ieb.owner` claim is
per-directory, so barring the sealed bundle from that directory removes its exclusivity. A
second seal is undefined: `-s2`? `-s1-s1`? Two concurrent seals both compute `-s1`.

### The redaction fix traded one lie for another

`mode: "off"` was offered as an honest alternative. It is not: `verify_cmd.rs:652-658` prints
*"WARNING: captured with redaction OFF — `resources/` may contain credentials in plaintext"* —
naming the **one directory that was** passed through policy v1, while the actually-raw backfill
directories go unnamed. To an auditor that is a false compliance finding against the operator.

And `mode` is one value per bundle (`spec/IEB-SPEC.md:316-317`), so "core redacted, backfill
not" is **inexpressible**. Meanwhile `verify.rs:866-886` reads only `v["mode"]` — `not_redacted`,
`redacted_values` and `policy_version` are read by nothing in the repo — so the branch that *is*
honest is unverifiable. The critic's steelman of the honest branch **succeeded**, which is
precisely why offering the dishonest one as an equal was the defect.

### The notice fix has the same shape as the defect it replaced

- `spec/VERIFY-RESULT.md:107`: `notice` is *"Informational | nothing"*. `verify.rs:1025-1030`
  computes the verdict without it, so exit stays **0**.
- The `source` and `retrieved_at` live only in `message`, and `spec/VERIFY-RESULT.md:91` says
  *"`message` is for humans and is **not** stable."* In text mode notices go to **stderr**
  (`verify_cmd.rs:594-599`) while the verdict goes to stdout.
- `coverage_score: 1.0` / `partial: false` / exit 0 keep asserting a full capture — **three
  positive assertions** with the caveat in an advisory string.
- The discriminator "not an original `ieb/v1` name" is a **moving denylist**:
  `COMPATIBILITY.md:62-63` makes new collector names additive, so the day a legitimate
  contemporaneous collector is added, every bundle carrying it is labelled *"contains data
  retrieved after capture"*.

**And the phase-A notice fires on every released fixture.** All four v0.1.0 OK/partial fixtures
intend `logs` alone, so *"omits any `ieb/v1` table collector"* is true of all of them. Their
`expected.json` code sets are `[]`, and `COMPATIBILITY.md:242` pins those sets while `:155`
says *"Fixtures are not modified after their release ships."* Worse, the notice's own text —
*"deferred by profile `<name>`"* — is **false** about them: they have no profile and deferred
nothing. The document cites the independent Python producer's minimal bundle as proof that
minimal capture is legitimate, and its own notice would mislabel that exact bundle.
**Cheap fix, and it is kept:** gate the notice on the `deferred` manifest member, not on the
complement of the table.

### The credential fix hands over the key it exists to protect

`create jobs` in the release namespace is pod-spec authorship, including `serviceAccountName`.
`rbac.yaml:78-92` grants `get` on `.Values.signing.keySecret` and `:98-111` binds it to
`ServiceAccount {{ fullname }}`. So a principal with `create jobs` launches a Job as that
ServiceAccount and **reads the signing key** — voiding the document's own *"`seal` MUST NOT
hold a signing key."* Granting `create jobs` to the controller instead makes an HTTP webhook
receiver a pod-spawner in the namespace holding that key: strictly worse than either laptop
credential.

The `--dry-run` "zero-credential fallback" **emits no bundle** — only text. If a human
assembles one, the human is the producer, and every integrity rule this document just added
becomes a human attestation on a structurally unsigned bundle. Rule 6 checks that those files
*exist*; nothing checks they are *true*. And *"the exact LogQL"* is not derivable: `grep -rni
loki crates/` returns zero hits, and stream selectors are shipper-specific, so Lapilli can emit
a template, not a query.

### The name was already taken

`seal` means **sign** in this codebase and in the format: `sealing.rs` is the KMS signing
module, the CR phase is `Sealing`, and `spec/IEB-SPEC.md:293` defines
`timing = {capture_started, sealed_at, capture_to_seal_ms}`. A bundle that is unsigned by
construction cannot carry a `sealed_at` that means "when it was backfilled". **Rename to
`lapilli backfill`.**

## 2. Phase A — three fixes, all cheap

1. **Gate the deferred notice on the `deferred` manifest member** (above). No existing or
   third-party bundle acquires a code, and the wording becomes true wherever it fires.
2. **RBAC cannot be derived from Helm values.** `captureprofile.yaml:1` is behind
   `profile.create`, the profile is selected **per capture** (`crd.rs:196-202`), and the Role
   grants `update`/`patch` on `captureprofiles` (`rbac.yaml:77`) — so chart-rendered RBAC and
   the profile in use diverge with nothing re-rendering. Derivation must happen in the
   controller, which can see both, and a resolved profile naming a collector whose permission
   check is denied must be **refused with an Event**, not collected thinly.
3. **`perms.rs` must re-read the profile inside its loop.** `needs_from_cluster` is awaited
   once (`main.rs:397-404`) and `spawn` reuses the value forever (`perms.rs:506-512`). Its
   stated purpose is to catch RBAC drift; deriving from a startup-frozen profile gives it the
   mirror-image blind spot. Also, `Report`'s tri-state map (`perms.rs:317-319`) has no state
   for *deferred* — it is an absent key — and an unreadable profile currently `debug!`s and
   returns `(false, None)` (`perms.rs:473-477`), after which `check_once` logs *"every
   permission this install needs is held"* (`:395-398`). Post-fix that path drops **every**
   collector check. Needs a fourth `NotNeeded` state and a `warn`.

**Corrections to the phase-A prose**, both mine:

- **"p50 6.2 KB" is a floor restated as a median.** `values.yaml:164-165`: *"years at the
  6.2 KB measured for a crash-looping busybox pod whose whole log is one line. **Only the last
  of those is measured, and it is a floor** — a real capture carries a real log window."*
  Round 23 killed R5 for citing a memory figure as a disk figure; the replacement committed the
  same class of error in the other direction, in the paragraph written to fix it. The honest
  statement: *the only measured bundle is a 6.2 KB floor; no p50 exists.*
- **The rule-6 warrant was the wrong warrant.** I argued "no bundle that already exists uses
  these names". The real warrants are stronger and I missed both: `COMPATIBILITY.md:62-63`
  pre-authorizes *"new collector names (with their required files)"* as additive **verbatim**,
  and `git tag` returns nothing — the freeze *has not begun*. My reason would be false the day
  after v0.1.0 ships, so it teaches the wrong precedent. Also: rule 6 is **presence-only**
  (`verify.rs:841-843` calls `contains_key` and never opens the file), so `logs-backfill/index.json`
  containing `{}` satisfies the new row — R3 reproduced one level down. A bounded content peek
  already exists as precedent (`verify.rs:281-299`, `MAX_PEEK_METRICS`).

## 3. The shipped-code defect — ships on its own

**A `pods/log` permission denial produces a bundle that verifies OK at coverage 100% with zero
log bytes.**

`collector.rs:215-225`: the log fetch is soft — `Err(e) => entry["unavailable"] = json!(e.to_string())`
— and `collect_logs` then returns `Ok(())` at `:234`, so `:149` runs `run.push(name)` and `logs`
lands in `collectors_run`. Per `spec/IEB-SPEC.md:299` the bundle is therefore **not** PARTIAL:
verdict OK, coverage 100%, `logs/index.json` with every entry the string `"…403 Forbidden…"`.

`events` is the opposite: `collector.rs:327` `events.list(&lp).await?` is a hard `?`, so an
`events` denial **does** yield PARTIAL. The asymmetry is exact — the same class of failure is
loud on one collector and a false green on the other.

This exists today, independent of this design. The design merely makes it reachable in normal
operation, and it lands squarely on the one change an operator critic said would flip a
*would-install: no*. A legitimate absence ("kubelet kept no log", `is_kubelet_log_error`) must
stay soft; an *infrastructure* failure must not.

Adjacent, pre-existing, rated WEAK: `crd.rs:418-420` `default_collectors() -> vec!["logs"]` —
the CRD's default for an omitted `spec.collectors` is the single collector phase A drops.

## 4. Why phase B returns to premise rather than getting a round 3

Four mechanisms, four different authors' fixes, one underlying cause: **a human-run, unsigned,
off-cluster producer merging late data into a signed evidence bundle is fighting the format's
trust model, not a gap in it.** Each refusal is a design decision the format made on purpose:

- the signed payload is the literal manifest bytes, so a core signature cannot be carried by
  reference;
- `trusted_key.is_some() && !Trusted` is FAILED, so there is no "partly trusted" verdict to
  land in;
- `redaction.mode` is one value per bundle, so mixed provenance is inexpressible;
- `notice` changes no verdict, so an advisory cannot carry a material caveat.

A round 3 would be the fourth attempt to route around four deliberate decisions. The premise to
re-examine is not *how* to merge late data into a bundle, but **whether a backfill belongs in a
bundle at all** — as against, say, a separate artifact that *references* a signed phase-A bundle
and never claims to be one.

## 5. Independence — honest

All six critics across both rounds are Claude-family: **weak independence, calibration only.**
No human or non-Claude critic ran. What the rounds do demonstrate is that the critics were not
dead: round 2 found eleven blockers in fixes the arbiter believed were sound, three of them in
shipped code rather than in prose.

## 6. Carried forward

- Phase A's three fixes and two prose corrections → implement.
- The `pods/log` false green → its own ship, with a negative test.
- Phase B → premise re-examination, not a patch round.
- Still unresolved from round 23, untouched by this round: what happens to a phase-A bundle
  nobody backfills, and whether the profile choice belongs to the operator or the alert.
