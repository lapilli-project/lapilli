# Design — `lapilli postmortem`: the draft a human then writes

Status: **implemented.** Designed here, returned by `docs/design-review-round18.md`, revised, and
built as `crates/lapilli-cli/src/postmortem.rs`. Roadmap: it was the last item of the v0.2 row in
`DESIGN.md` §11 when this was written; §11 was re-baselined on 2026-09-25 and it now sits in the
v0.1.0 row, shipped. Round 11's product lens called this "the stronger feature".

Implementation found three things the review did not, all by running the command over every
released fixture (43 bundles, 47 pinned cases) rather than one:

- **The banner made the wrong accusation for four of the six FAILED codes.** It had two branches
  — "wrong place" and "the bytes do not match what was sealed" — so a malformed tar archive was
  reported as tampering. A truncated download and a hand-edited bundle look identical from the
  verifier's side, and saying otherwise in a document quoted at an incident review is round 18's
  own P4 in a new place. It now branches on the real codes, and says so when it cannot tell.
- **A structurally malformed bundle lost its verdict.** `unpack` refuses a traversal path or a
  link entry, correctly — but the rendering path died with it and exited 3, "says nothing about
  the bundle", for twelve bundles `lapilli verify` had already judged FAILED with exit 1. Two
  commands contradicting each other on the same bytes is worse than either being wrong alone. The
  document now renders what the verdict knows, says plainly that nothing could be transcribed, and
  exits with the verdict's code. A test pins the agreement across every fixture.
- **The PARTIAL header named nothing.** It was meant to name the collectors that did not run, and
  it filtered the verdict's problems on a code called `coverage` — which does not exist; the
  PARTIAL problem's code is `partial` (`spec/VERIFY-RESULT.md`). So every partial bundle read
  "PARTIAL: none did not run", the opposite of the sentence below that promises the header says
  which sections are thin. Found in round 25 (`design-review-round25.md`), fixed the same round;
  `report_missing` in `postmortem.rs` carries the note.

Round 18 reviewed the first draft of this document *and* the code it claimed to reuse. The code lost:
the reader this command renders from was returning the wrong facts, in three separate ways, on every
install (`design-review-round18.md` S1–S4). Those are fixed and shipped. What follows is the
proposal as that round left it.

## The line this must not cross

`DESIGN.md` §2, non-goals: *"❌ Not an AI 'auto-RCA' narrative (HolmesGPT, k8sgpt, Robusta) — Lapilli
produces a bundle those tools can consume."*

**So this command does not say why anything happened.** It transcribes. Every line it emits is a
value that exists in the bundle, followed by the file it came from, and the sections a postmortem
needs a human for — impact, root cause, contributing factors, action items — are emitted **empty**,
with their headings, because a blank heading is an honest prompt and a filled one would be a guess.

## What it is for, stated so it can be checked

The first draft of this document claimed the command saves "the forty minutes an on-call engineer
spends after an incident". Nobody measured that; I made the number up, in a document whose thesis is
that it does not guess. It is gone.

The claim that replaces it is testable: **the draft is a self-checkable artifact.** Its header
carries the bundle's `input.sha256` and the exact `lapilli verify` invocation that reproduces its
verdict, so any reader — a week later, in a review, without access to the cluster — can re-derive
every fact in the document from bytes whose integrity they check themselves. A wiki page somebody
typed cannot be re-derived. That is the difference, and unlike a duration it is either true of the
output or not.

If a team wants a narrative, the bundle is what they feed to a tool that writes one.

## Shape

```
lapilli postmortem <bundle.ieb | directory> [--key <pub>] [--include-log-line] > incident.md
```

**Local bundle or directory only.** `lapilli verify` grew remote reading with a byte limit, a redirect
policy, a version-listing path and its own exit code; a rendering command that inherits all of it
gains four failure modes for a convenience that fetching the file first — `lapilli cat-bundle` out
of the pod, or a download from the bucket — and then `lapilli postmortem` already provides. No
`s3://` in v0.2.

**In the CLI, not the controller.** The bundle is portable and so is the draft; nothing here needs a
cluster. The stronger reason is one round 18 found: `Summary::from_dir` is *already* called inside
the capture path, on the freshly staged directory, so putting a second — larger — reader there would
add work to the one path that must not gain failure modes. It also means the command works months
later, on a laptop, on a bundle pulled from a bucket, which is when postmortems actually get written.

Output is Markdown on stdout. No `--format`: one output, pasteable into every wiki, and
`lapilli verify --output json` already exists for anything that wants to parse instead.

## It verifies first, and the verdict is the first thing in the document

**A postmortem built on evidence nobody checked is worse than no postmortem**, because it launders
unverified bytes into a document that will be quoted in a review.

So the command runs the same verification `lapilli verify` runs, and the verdict decides how it
renders — not *whether* it renders, which is where the first draft was wrong:

- **OK** → the document, header first.
- **PARTIAL** → it renders, and the header names the collectors that did not run and therefore which
  sections are thin. The bundle is intact; it is thin. Exit 0, as `lapilli verify` does.
- **FAILED** → it **still renders**, with a banner as the first block and **exit 1**. The first draft
  refused and printed nothing, which is the wrong failure: a FAILED bundle is exactly when someone
  needs to see what it *claims*, and printing nothing sends them to `cat` the files by hand with no
  verdict attached to anything. The blast radius comes from a document that looks trustworthy, not
  from the facts being visible. So: visible, banner, non-zero exit — a pipeline stops, a human reads.
  There is no `--force`, because there is nothing left to force.
- **CANNOT_EVALUATE** → it **refuses**, exit **3**. This is not a milder FAILED, it is the opposite
  of PARTIAL: the verdict itself could not be reached, so there is no verified hash tree to render
  from, and every line would be a fact with nothing behind it. Exit 3 is the code the CLI already
  uses for "this says nothing about the bundle" (`COMPATIBILITY.md` §2).

The banner says which kind of failure it is, because they are not the same accusation.
*Update (2026-09-25):* as designed this table had two rows — "the bytes do not match" and "the wrong
place" — and the implementation found (above) that two rows made the wrong accusation for four of
the six FAILED codes. What ships branches on the verdict's problem codes (`banner` in
`postmortem.rs`), in this order of precedence:

| Why FAILED (problem code) | What the banner says |
|---|---|
| `structure`, `not-a-bundle`, `manifest` | **this is not a well-formed bundle** — nothing below was read from a structure Lapilli recognises, and this is *not* a statement that someone tampered with it: a truncated download and a hand-edited tar look the same from here. Get the bundle again |
| `integrity` | **the evidence did not verify** — the files do not match the hash tree that was sealed with them; treat every line below as unverified |
| `signature` | **the files are intact, but the signature is not** — missing, undeclared, invalid, or by a key other than the one supplied; the facts are internally consistent and who sealed them is not established |
| `context` | **the bundle is intact, but it may be about the wrong place** — which cluster and incident this belongs to could not be re-derived; the facts may be correctly recorded about the wrong incident |
| anything else | it did not verify, and the document does not know how to describe why: run `lapilli verify --output json` and read the problems. Unreachable today — a FAILED verdict always carries a code above — and it says so rather than guessing, because guessing a cause is the defect the other rows exist to avoid |

Every row is followed by the verdict's problem lines verbatim. The `context` row is round 18's P4:
the commonest FAILED is a *misfiling*, and reporting it to an incident review as "the evidence
failed integrity" is a worse error than silence.

The header carries the verdict, the incident id, the cluster id, the capture window, the signing
status in the same words `lapilli verify` uses — `signed:<key_id>`, `signed:unpinned` (no `--key`
given, so authenticity is not established) or `unsigned` — the bundle's `input.sha256`, and the
`lapilli verify` line that reproduces all of it.

`--key` stays optional. Requiring it would make the command unusable on the majority of installs,
where signing is off by default, and `signed:unpinned` is an honest string that says authenticity was
not established.

## What it renders, and from where

Every row cites its file, so a reader can check any line against the bundle they hold.

| Section | Content | Source |
|---|---|---|
| Header | verdict, banner if any, incident id, cluster, window, signing, `input.sha256`, the reproducing `lapilli verify` line | `manifest.json`, verification |
| What was captured | the container, restart count, termination reason and exit code, memory peak against the limit | `Summary` over `resources/pod.json`, `metrics/` |
| Timeline | events in time order, with the alert's firing time marked, each with its `count` and first occurrence | `timeline.json`, `events.json` |
| What changed before it | the rollout: revision, when, by whom, and the fields that differ | `diffs/index.json`, `changes.json` |
| Evidence inventory | collectors run and missing, coverage, redaction mode | `manifest.json`, `redaction.json` |
| Impact · Root cause · Contributing factors · Action items | **empty headings** | — |

The timeline says once, in the document, that its order is the order of `lastTimestamp`: Kubernetes
coalesced these events before Lapilli read them, so it is the order events were last *seen*, not the
order they occurred. `count` and the first occurrence are rendered for the same reason — without them
one restart and two hundred restarts read identically, which is what the collector was in fact doing
until round 18 (S4).

**The last log line — the log the container died on — is off by default.** `--include-log-line`
includes it, marked as workload content. The first draft had this backwards and argued that the
notification defaults the other way "because a chat channel is a broadcast, and this command is run
by somebody who already holds the bundle". The bundle holder is not the audience: the *output* is a
document pasted into a wiki, which is a broader audience than the Slack channel and a permanent one.
`Summary::without_log_line()` already exists for exactly this.

## The cost of a second reader, named

"It reuses `Summary`" is not free, and the first draft priced it at zero.

`Summary` exists today to render a ~2 KB notification. This draft needs the timeline and the evidence
inventory, which the notification path never reads. So the command grows `Summary` — and every field
it adds is a field the *message* can then drift on too.

That is still the right trade, and it is why there is one reader rather than two: a defect in it is
one defect, in one place, visible in both outputs. Round 18 is the evidence — S1 was a single wrong
assumption in `summary.rs`, and it was wrong in the notification and would have been wrong in every
draft this command ever rendered. Two readers would have made it two bugs found at two different
times. The mitigation is the gate that round added: the field names the reader looks up are extracted
from the reader's own source and checked against the producer, so a name only one side knows fails a
test instead of rendering as absent.

## What it is not

- **Not a template engine.** No `--template`: a rendering people can change is a rendering whose
  provenance claims stop being true.
- **Not a second summary format.** It reuses `lapilli-bundle`'s `Summary`, so the facts in a message
  and the facts in a draft cannot drift apart.
- **Not an export target.** It writes to stdout, or hands the same rendering back as an MCP tool
  result — `document()` returns a `Rendered`, and `lapilli mcp`'s `postmortem` tool serves it to a
  caller that asked (`mcp.rs`). Either way the document is *pulled* by whoever runs the command; it
  is never pushed anywhere. Lapilli does not gain the ability to post documents anywhere.
- **Not a stable interface.** The Markdown is a human surface, like `lapilli verify`'s human output,
  and a team will otherwise build a parser on it. With the implementation, `COMPATIBILITY.md` §2
  gained: *"`lapilli postmortem` renders Markdown for people. Its structure, headings and wording are
  not stable and may change in any release; `lapilli verify --output json` is the parseable
  surface."* — and one thing this design did not foresee: *its exit code is stable*, and is the
  verdict's, pinned across every released fixture (the second finding above). That line landed with
  the code and not before — a compatibility file describing a command nobody can run is worse than
  one that is a release behind.

## Open questions left after review

1. **Are the empty headings right, or patronising?** Survived round 18 — a team with its own template
   deletes four lines, which is cheaper than a team not noticing its postmortem has no root cause —
   but it is a product judgement, not a proof, and the first real user should be asked.
2. **Should the timeline be trimmed?** A capture can hold hundreds of events. The notification cannot
   show them, so this is the first output where the question exists, and "all of them" makes a
   document nobody scrolls. A window around the firing time is the obvious answer and needs a number
   that should come from a real bundle, not from this document.
   *Resolved (2026-09-25), with a caveat:* `TIMELINE_MAX = 60` (`postmortem.rs`). The table shows
   the first 60 events in `lastTimestamp` order and then says how many more are in `timeline.json`,
   so nothing is hidden, only deferred to the file. It is a first-N cut, not the window around the
   firing time proposed here, and 60 came from the code rather than from a real bundle — so the
   number is settled for now and the shape is still open to the first real user.
