# Design Review — Round 18 (the postmortem proposal, and the shipped code under it)

Constitution: **Loop Engineering Constitution v0.5.0**. Snapshot: `b51e7a0` plus
`docs/design-postmortem.md` as first written.

Two artifacts, because they are one question. The proposal's whole claim is *"it reuses
`lapilli-bundle`'s `Summary`, which the notification already renders from, so the facts in a message
and the facts in a draft cannot drift apart."* A lens was therefore pointed at that `Summary` — not
at the proposal — to check the premise before reviewing what rests on it. The premise was false in
three places, all of them shipped.

**This log was written after the code fixes were applied**, from the round's findings. It is a
record of dispositions, not a transcript.

## Round 0

- **Category:** design/spec, and — once the first lens returned — shipped-correctness.
- **Break-even:** cost ≈ 2 critic runs plus a live E2E. Downside: a command that launders facts into
  a document quoted in an incident review, built on a reader that was silently returning the wrong
  facts. Downside ≫ cost.
- **Sizing:** 2 lenses × 1 round on the proposal; the correctness lens then ran to exhaustion,
  because each finding it produced was a shipped defect rather than a design opinion.
- **Independence: not achieved (calibration only.)** Both lenses were Claude. One read the producer
  (`diffs.rs`, `collector.rs`) against the consumer (`summary.rs`); one attacked the proposal's
  claims. Calibration, not independence.

## Findings — the shipped code the proposal rests on

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| S1 | BLOCKER | **The summary reader looked for field names no producer has ever written.** `summary.rs` read `revision_from`, `revision_to` and `seconds_before_alert` from `diffs/index.json`. `diffs.rs` writes the revisions inside `before`/`after` and the timing as `seconds_relative_to_firing` — and so does `spec/IEB-SPEC.md` §diffs. So the reader disagreed with both the producer *and* the frozen spec. Two consequences, not one: every notification rendered `Kind/name changed` with no revision and no timing; and the "change nearest the alert" selector, keying every entry through `map_or(i64::MAX, …)`, silently ranked them all equal and returned **the first entry** instead of the nearest. | None. The spec and the producer agree with each other against the reader. | **APPLY.** Reads `before.revision` / `after.revision`, and `-seconds_relative_to_firing` — a sign flip, not a rename, since the producer's value is `changed_at - firing` and a change *before* the alert is negative there. Selector re-keyed. Verified live on kind 1.30: the message now reads `Deployment/notify-crash rev 1 → 2 changed, 19s before the alert, by ~lapilli-notify-e2e`. |
| S2 | BLOCKER | **Nothing bound the two sides but a fixture I wrote by hand** — from the reader's assumption. So the fixture agreed with the reader, the reader agreed with the fixture, and both were wrong. This is *generation and verification sharing a blind spot* — round 13's lesson (`design-review-round13.md:85`), which has recurred in every round since that touched a test double: round 14's gauge assertions that only ever proved `== 1`, round 15's fingerprint that left `group` invisible to 89 tests, and the retention ship's two vacuous guards. Fixing this fixture does not fix the class. | None; the recurrence is documented in the rounds themselves. | **APPLY.** Fixtures rewritten to the producer's shape, with the **far entry first** so "nearest" and "first" are distinguishable at all — the old fixture could not tell the two selectors apart. Plus a new gate that is not hand-written: `every_index_field_the_summary_reader_uses_exists_in_this_producer` extracts the key names the consumer looks up **out of the consumer's own source** and fails if this producer never writes one. Reinstating the original defect makes it fail with the name. |
| S3 | MAJOR | **The summary read a path straight out of the index**, against `COMPATIBILITY.md` §1's reader rule — *"read only files listed in a verified hash tree"*. `e["file"]` was joined to the bundle directory and read, so an index naming `../../etc/passwd` was followed. The bundle is attacker-influenced input the moment it arrives from a bucket or a colleague. | Tried: "verification runs first, so the tree is checked." It does in `lapilli verify` — but `Summary::from_dir` is also called on the freshly staged directory during capture, and the proposal adds a *third* caller that a user points at an arbitrary directory. The guard cannot live in the caller. | **APPLY.** `read_inner` validates through `hashtree::check_path` before reading, with a test that an index naming a file outside the bundle reads nothing. |
| S4 | MAJOR | **The timeline threw away the two fields that make a coalesced event readable, and sorted the unknown first.** `collector.rs` dropped `count` and `firstTimestamp`, so 200 restarts over an hour and one restart rendered identically; and the sort keyed on `ts` as a string, where an event with no timestamp yields `""` — which sorts **before** every real timestamp, putting the least-known event at the top of the document the proposal renders. | None. | **APPLY.** Entries carry `count` and `first_ts`; `sort_timeline` keys on `(ts.is_empty(), ts)` so unknowns sort last. Mutating the key back to `(false, ts)` fails the test. |

## Findings — the release gate, found by running it

Both of these came out of one false failure. The `notify` suite reported *"the disabled route was not
logged"* while its own failure handler printed the exact line it said was missing. Chasing that found
two defects in the gate, neither of which is the thing that caused it.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| G1 | MAJOR | **`producer \| grep -q X` is not a safe assertion under `set -o pipefail`,** and the gate had 68 of them. `grep -q` exits the instant it matches, so a producer still writing takes EPIPE and exits non-zero — and the pipeline then reports **failure with the assertion satisfied**. Worse in the negative direction: `if get events … \| grep -q .; then fail; fi` reads an EPIPE as "no events" and passes silently, and `! helm template … \| grep -q X` inverts it into a pass. The mechanism is not theoretical — the second gate run printed `test/e2e/run.sh: line 292: echo: write error: Broken pipe` and then failed on a series `/metrics` was serving. | Tried: "it has always worked." It has mostly worked, which is the property of a race. Measured: a 200,000-line producer fails this way 10 times out of 10; an 8-line one, 0 out of 10, because its writes fit in the pipe buffer. `/metrics` grew past that boundary when the retention and permission series were added. | **APPLY.** Every one of the 68 reads its producer as a value and matches with a herestring, which has no pipe and so cannot EPIPE. Three of the mechanical conversions were wrong in ways `bash -n` accepts — two consumed the second line of a continued command, one hung the herestring off a one-line `if … fi` — and were found by reading the diff, which is the only thing that finds them. |
| G2 | MAJOR | **All 27 negative checks in `scripts/helm-renders.sh` were vacuous**, which was the entire negative coverage of `charts/lapilli/values.schema.json`: "the schema must reject a prometheusUrl with credentials", "…a misspelled profile key", "…ConfigMap diffs over kube-system", "…a KMS alias instead of a key", "…a profile naming an undefined route", and 22 more. Bash's manual, on `set -e`: the shell does not exit "if the command's return value is being inverted with a `!`". So `! helm template …` never failed the script, whatever the chart did. | None. Proved by mutation, not argued: pointing a negative check at a string the default render plainly contains still exited 0. The file even carries a comment about `grep -qv` being vacuous — the shape was on our minds and the `!` was not. | **APPLY.** Two helpers, `refuses` (fails if the chart *accepts*) and `absent` (fails if the string *is* there), both mutation-checked. And enforcing them immediately found one of the 27 was also **false**: `pathSecret` does appear in the render — in the chart's own comment saying it is not a route field. The assertion now reads `routes.json` alone, with a positive companion so it cannot pass on an empty string. Had that check ever been able to fail, the likely repair would have been deleting the comment. |

**What is still not explained.** The failure that started this is not accounted for by either finding.
The pod had 8 log lines; the line was third; `--tail` had nothing to truncate. So the assertion now
prints what it read and the pods it read from, and the comment says the cause is unknown rather than
naming the nearest plausible mechanism. Reporting a fix for it would be the same error as S1 —
believing a story because it is the only one available.

## Findings — the proposal

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| P1 | MAJOR | **"The forty minutes an on-call engineer spends" is invented.** No measurement, no citation, in a document whose entire thesis is that it does not guess. A number with no source in a design doc becomes a number with no source in a README. | None attempted; I made it up. | **APPLY — deleted**, and the value restated as something checkable instead: the draft carries `input.sha256` and the `lapilli verify` line that reproduces its verdict, so a reader can re-derive every fact in it. That claim can be tested; "forty minutes" cannot. |
| P2 | MAJOR | **Refusing on FAILED is the wrong failure.** The proposal exits 1 and prints nothing. But a FAILED bundle is exactly when someone needs to see what it *claims* — and "no output" pushes them to `cat` the files by hand, without a verdict attached to anything. The blast radius the refusal is protecting against comes from a document that looks trustworthy, not from the facts being visible. | Tried: "a document saying the evidence failed integrity is pasted without reading." That is an argument about the **banner**, not about suppressing the content. | **APPLY.** It renders, with the verdict as the first line of the document and a banner no reader can skip, and still **exits 1**, so a pipeline stops even though a human can read. There is still no `--force`, because there is nothing to force. |
| P3 | MAJOR | **`CANNOT_EVALUATE` was folded in with `PARTIAL`, and they are opposites.** PARTIAL means collectors did not run — the bundle is intact and thin, so proceeding and saying which sections are thin is right. CANNOT_EVALUATE means *the verdict itself could not be reached* (an unreadable object, an unknown `alg`), so there is no verified tree to render from, and a document built on it would carry facts with no verdict behind them. | None; `spec/VERIFY-RESULT.md`'s precedence already separates them. | **APPLY.** PARTIAL proceeds with the missing collectors named. CANNOT_EVALUATE refuses, **exit 3** — matching the CLI's existing convention that 3 says nothing about the bundle. |
| P4 | MAJOR | **The `context` collector failing means "misfiled", not "tampered".** The draft's header would report a FAILED verdict as an integrity failure; but the most common way to get one is a bundle whose cluster or namespace context could not be re-derived, which makes the document's *attribution* wrong, not its bytes. Telling an incident review "the evidence failed integrity" when the true statement is "this may be the wrong cluster" is a worse error than silence. | None. | **APPLY.** The banner distinguishes the two and names what is in doubt. |
| P5 | MAJOR | **The last log line was included by default, with the reasoning backwards.** The argument was "the notification defaults the other way because a chat channel is a broadcast; this is run by somebody who already holds the bundle." But the *output* is a document pasted into a wiki — a broader audience than the Slack channel, and a permanent one. The bundle holder is not the document's audience. | Tried: "it is the most useful line in the document." Probably true, which is why it is one flag away, not why it should be the default. | **APPLY.** Off by default; `--include-log-line` turns it on and the draft marks it as workload content. Matches `facts_only()` / `without_log_line()`, which already exist for exactly this. |
| P6 | MINOR | **`s3://` and `gs://` in the synopsis is scope the command does not need.** `lapilli verify` grew remote reading with a byte limit, a redirect policy, a version-listing path and a distinct exit code. A rendering command inheriting all of that gains four failure modes for a convenience that `lapilli pull \| lapilli postmortem` already covers. | None. | **APPLY.** Local bundle or directory only, in v0.2. |
| P7 | MINOR | **The second reader was unpriced.** "It reuses `Summary`" is stated as free, but `Summary` currently exists to render a ~2 KB message; a document needs the timeline and the evidence inventory, which are reads the notification path never makes. Every field the draft adds is a field the message can then also drift on. | None. | **APPLY.** The doc now names the cost: what the draft needs beyond the message, and that both render from one reader on purpose, so the drift shows up in one place rather than two. |
| P8 | MINOR | **Markdown stability was never stated.** `COMPATIBILITY.md` pins the JSON surfaces and explicitly unpins the human ones; the draft is a human surface with no such statement, so a team will build a parser on it. | None. | **APPLY,** with the line landing *with the code*, not before it: a compatibility file that describes a command nobody can run is worse than one that is a release behind. The design records the exact sentence to add. |

**Attacks that failed.** CLI-not-controller is right, and for a stronger reason than the proposal
gave: `Summary::from_dir` is already called inside the capture path, so adding a *rendering* caller
there would put a second reader on the hot path. Empty headings for impact and root cause survived —
a team with its own template deletes four lines, which is cheaper than a team not noticing its
postmortem has no root cause. `--key` staying optional survived: requiring it would make the command
unusable on the majority of installs, where signing is off, and `signed:unpinned` is an honest
string. Refusing `--template` survived. And the proposal's own open question 4 was right — the
timeline's order is `lastTimestamp` order, which is now said once in the document rather than
implied.

## Carried out of this round, not fixed in it

- **`status.message` is unbounded and echoes `spec.clusterId`.** Verified: `crd.rs:245` is a bare
  `Option<String>`, and `refuse_capture` (`reconcile.rs:198`) formats the *caller's* `clusterId` into
  it. Unlike `trigger.rule`, which carries `^[^<>&]{1,200}$`, none of `clusterId`, `incidentId` or
  `profile` has any schema constraint at all — so the bound on what lands in the CR is etcd's object
  limit. `{:?}` escaping blunts the markup half; it does nothing about the size. Tracked, and it is
  not a one-line fix: adding `maxLength`/`pattern` to shipped fields *tightens* a CRD, which can
  reject objects that already exist, so it needs the migration thought that `COMPATIBILITY.md` §3's
  additive-only rule is there to force.
- Retention's `unreachable` and full-PVC journal paths have no live coverage.

**One item this round claimed and I withdrew.** I recorded "the chart ships no egress NetworkPolicy
while `DESIGN.md` §7 promises one" as an open gap. Reading the chart before committing it: both halves
are false. The NetworkPolicy §7 calls optional is an **ingress** restriction on the webhook port, and
it exists — `charts/lapilli/templates/webhook-auth.yaml:25`, behind `webhook.networkPolicy.enabled`.
The absent one is **egress**, whose absence is a decision round 13 took and wrote down:
`docs/egress.md` is titled *"why the chart ships no NetworkPolicy"*, because NetworkPolicy v1 cannot
match a DNS name and most of Lapilli's peers are cloud endpoints. Left here rather than deleted,
because the failure it shows is the round's own lesson turned on its author: I asserted a
contradiction between two files from memory of them instead of reading either.

## Verdict

**Not dry — four shipped defects (S1–S4) and two gate defects (G1–G2) applied and verified**, and the
proposal **RETURNED FOR REVISION** rather than approved: eight findings, none of which required new
information, all of which were available to the lens that read the code instead of the prose. The
proposal's premise had to be repaired before the proposal could be judged, which is the argument for
reviewing designs against the code they claim to reuse — not only against themselves.

The round's own shape is worth keeping. S1 was found by reading the producer beside the consumer;
G1 and G2 were found by *running* the gate and disbelieving what it said — first when it failed while
being right, then when it passed while being unable to fail. Both halves were needed: the reading
lens cannot see a vacuous guard, because a vacuous guard reads correctly.
