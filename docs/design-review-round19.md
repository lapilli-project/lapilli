# Design Review — Round 19 (`status.message`, reviewed before any code)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact: `docs/design-status-message.md`,
a proposal, attacked before implementation as rounds 16–18 established is the cheap place.

## Round 0

- **Category:** a change to a **shipped CRD** plus a path an attacker can influence. Irreversible
  in the sense that matters: a tightening applied after the first tag cannot be walked back
  without breaking the additive-only promise in `COMPATIBILITY.md` §3.
- **Break-even:** cost ≈ 2 critic runs. Downside: constraining the wrong fields, or bounding the
  wrong string, and finding out after v0.1.0 is public. Downside ≫ cost.
- **Sizing:** 2 lenses × 1 round, at the design stage.
- **Independence: not achieved (calibration only).** Both critics were Claude. One read the code
  for sinks and bypasses; one read the chart, the E2E and the docs for what breaks. They found the
  same two things independently, which is calibration, not independence.

## Findings

Merged where both lenses found the same thing, which they did twice (R1, R5).

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| R1 | BLOCKER | **"What this breaks" was wrong in both directions.** The proposal named `run.sh:570` (`refused ref-cluster other-cluster`) as the casualty: `other-cluster` matches the proposed pattern, so that assertion is untouched. The one that actually goes red is `run.sh:572`, `refused ref-traversal "$CID" "/../../x"` — `/` fails the proposed `incidentId` pattern, so `kubectl apply` is rejected, no object exists, `{.status.phase}` is empty, and the step fails with the misleading `"an unsafe incident id was not refused"`. The proposal never mentions it. | None. One lens measured the shell behaviour rather than assuming it, showing `set -euo pipefail` does **not** abort the script — it reaches the wrong `fail`. | **APPLY, by removing the cause.** See R4: the `incidentId` pattern is dropped, so this assertion keeps working and keeps exercising `path_safe`'s traversal defence. |
| R2 | BLOCKER | **The sink inventory was incomplete, and its headline claim false.** The proposal said "the unbounded string never becomes an object in the first place". `sealing.rs:141` formats `incident.cluster_id` and `incident.id` — read from `seal_file(stage)`, **a file on the PVC** — into `staging-mismatch`. No CRD pattern can reach a file. `staging-lost: {e}` embeds io/serde text, and `reconcile.rs:958` embeds `e.to_string()`. | None. Verified by reading `sealing.rs:133-145`: the mismatch check runs *before* the hash-tree check, so a tamperer who edits the seal file gets the echo, not `staging-modified`. | **APPLY.** The bound moves to message **construction**, not only to input. Input constraints stay for what they do cover. |
| R3 | BLOCKER | **The newtype does not bind.** Four call sites reach `api.patch_status` directly, bypassing the two helpers — `reconcile.rs:560` writes `"message": null` itself. And a hostile holder of `incidentcaptures/status` never executes controller code at all, so no Rust type can reach them. | Tried: "the helpers are the only writers today." They are not; `grep -n patch_status` returns six call sites, four of them direct. | **APPLY, both guards.** `maxLength: 1024` on `status.message` in the CRD, which binds every writer including a hostile one; **and** truncation in code, so the controller never has its own patch rejected and loses the whole status. One lens cited `notify.rs` and `retention.rs` as bypassing writers — checked: they write `status.notification` and `status.local`, never `message`. That part of the evidence was overstated; the finding survives on the reconcile.rs sites. |
| R4 | MAJOR | **The `incidentId` pattern deletes a signal and buys nothing.** `lapilli_captures_total{result="refused"}` is incremented only at `reconcile.rs:149` and `:908`, after the object exists. An admission rejection reaches neither, so `docs/metrics.md:23`'s "refused means … an unsafe or claimed incident id" becomes false, and the shipped alert's annotation — "check the capture's `status.message`" — points at an object that was never created. `alert-rules-check.sh` stays green on synthetic series, which is the exact blind spot it was written to catch. | Tried: "the pattern stops spec bloat." It does — but `incidentId` is **already bounded before it is echoed** (`path_safe`, ≤100, `[A-Za-z0-9._-]`), which the proposal itself says. So the pattern buys nothing for the problem this design exists to solve, and costs a live refusal path plus the only E2E coverage of the traversal defence. | **APPLY: drop the `incidentId` pattern.** `path_safe` stays and remains the guard — it also covers bytes that arrived from a bucket, which no API server validated. The doc now says plainly that spec bloat is *not* closed by this design, rather than implying it is. |
| R5 | MAJOR | **The `profile` constraint was self-contradictory and would break a legal install.** A DNS-*label* regex with a DNS-*subdomain* length: no label can reach 253. Worse, `charts/lapilli/values.schema.json`'s `profile.name` is `{"type":"string","minLength":1}` with **no pattern** — while `notifyRoute` in the same object has one — so `--set profile.name=prod.default` installs today, renders a valid `CaptureProfile`, and would then have every webhook-created capture rejected at admission. | None. Both lenses found it; the second found the install case, which is the part that matters. | **APPLY.** RFC 1123 *subdomain* pattern with `maxLength: 253`, which is what a `CaptureProfile` object name actually is; the same pattern mirrored into `values.schema.json`; and a `refuses` case in `scripts/helm-renders.sh` beside the `clusterId` one, so the chart and the CRD cannot drift again. |
| R6 | MAJOR | **Truncating the assembled message cuts the only actionable sentence, and the Event is not bounded at all.** The give-up message puts the unbounded `{detail}` *before* "the collected data is kept: set the … annotation to retry". And `reconcile.rs:1021` builds the `SealDelayed` Event note from raw `detail`, not from the message — so "bounding the message bounds both" was false. | None. | **APPLY.** Bound the **component** (`detail`) at construction, put the static instruction first, and make `publish()` take the bounded type so an Event cannot be built from a raw string. |
| R7 | MAJOR | **1024 was a round number; it is now a measured ceiling.** Measured: static skeleton 108 B; worst reason with empty detail 132 B; realistic AWS `AccessDeniedException` with IRSA and key ARNs → 480 B; realistic GCP `cryptoKeyVersions…asymmetricSign` denial → 561 B; `staging-mismatch` with a maxed cluster and incident id → 411 B. And `events.k8s.io/v1` caps `note` at **1024**, above which the API server rejects it — and `reconcile.rs:1035` discards the publish result, so the `SealFailed` Event an operator alerts on would vanish silently. | None; this answers the proposal's own open question 1 with numbers instead of leaving it open. | **APPLY.** `MAX = 1024`, justified as ~1.8× the measured realistic worst case **and** exactly the Event ceiling, with a compile-time assert and a comment naming the reason. It is only wrong if raised. |
| R8 | MAJOR | **Both migration claims were wrong.** "The drift check covers it" — `release-check.sh:44-49` diffs the two committed manifests against the *current* generator; it has no previous version as input, so a tightening is green. Worse, `COMPATIBILITY.md:45` lists that same check as the **enforcement** of the additive-only promise this change breaks. And `COMPATIBILITY.md:206` tells operators `kubectl apply --server-side -f crds.json`, which conflicts with Helm's field manager on a chart-installed CRD without `--force-conflicts` — a string that appears nowhere in the repo. | None. | **APPLY.** The sentence is deleted; `COMPATIBILITY.md:45`'s enforcement cell becomes "reviewer judgement (nothing mechanical checks §3)"; `:206` gains `--force-conflicts`; and `CHANGELOG.md`'s Migration section gains the `profile` bullet. |

**Attacks that failed, reported because they are what makes the rest worth anything.** The
`clusterId` pattern and the number 83 survived: `webhook.rs:247` composes `<cluster>-<16 hex>` and
`path_safe` caps at 100, so 83 + 1 + 16 = 100 exactly, and `values.schema.json` already carries
the same pattern. The "resource exhaustion plus noise, not forgery" framing survived: `export.rs`
refuses a capture whose cluster id is not the controller's, builds the object key from the
controller's own id, and re-applies `path_safe`; the forged-status bypass is already covered by
`test/e2e/export.sh`. `status.notification.reason`, `status.local.reason` and
`status.exports[*].reason` are all fixed-code enums or `&'static str`, so `message` really is the
only unbounded status field.

## Two things this round found that are not about this design

- `main.rs:171-180` **already** enforces `(1..=83)` and `[A-Za-z0-9._-]` on the controller's own
  `--cluster-id`. So `export.rs:142-149`'s graceful `invalid-cluster-id` degradation is
  unreachable, and the proposal's derivation of 83 reinvented a constraint the binary had.
- `CHANGELOG.md`'s existing `clusterId` migration bullet overclaims: it says "the chart schema and
  the controller refuse others", but `export.rs:142-144` only logs and disables object-store
  export, at ≤100 rather than ≤83, and never refuses the capture.

## Verdict

**RETURNED FOR REVISION, not approved.** Eight findings, three of them blockers, none needing
information the author did not have — every one was reachable by reading the code, the chart, the
E2E and the docs the proposal cites. Two of the three blockers say the same thing in different
places: **the design bounded the wrong end.** Constraining the input is not the fix, because the
sharpest sources are a file on the PVC and an error string from AWS; bounding the message is, and
it has to bind writers that are not this codebase.

The revised proposal is `docs/design-status-message.md` as it now stands. Implementation follows
that, not this log.
