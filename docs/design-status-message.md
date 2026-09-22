# Design — bounding `status.message`

Status: **implemented.** Designed here, returned by `docs/design-review-round19.md` with three
blockers, revised, and shipped — `MESSAGE_MAX = 1024` with a compile-time assert naming the
`events.k8s.io` note limit, `DETAIL_MAX = 512`, a `StatusMessage` whose only constructor bounds,
`maxLength: 1024` on `status.message` in the CRD, and two live E2E assertions. Raised by round 18
as a carried item and done before the first release on purpose: see *Why now*.

Round 19 returned the first draft rather than approving it, and the reason is worth keeping at the
top: **the first draft bounded the wrong end.** It proposed constraining the API inputs, on the
theory that "the unbounded string never becomes an object in the first place". That sentence was
false. The sharpest sources are a file on the PVC and an error string from AWS, and no API-server
schema reaches either.

## What is actually wrong

`IncidentCaptureStatus::message` is a bare `Option<String>` (`crd.rs:245`). Five sources reach it,
none bounded:

| Source | Where | Reachable by |
|---|---|---|
| `spec.clusterId`, echoed by the `cluster-mismatch` refusal | `reconcile.rs:198` | anyone who can create an `IncidentCapture` |
| `incident.cluster_id` / `incident.id`, echoed by `staging-mismatch` | `sealing.rs:141` | **anyone who can write the PVC** — the values come from `seal_file(stage)`, not from any API |
| `staging-lost: {e}` | `sealing.rs:134`, `:137`, `:147` | io and serde error text |
| KMS error text, `SealError::Kms(k) => k.message` | `reconcile.rs:988` | AWS and GCP |
| `e.to_string()` for any other error | `reconcile.rs:182`, `:958` | includes `kube::Error`, which carries API-server text |

So the bound on what one `IncidentCapture` can carry is etcd's object limit, and the bound on the
cluster is that times the number of captures a caller can create.

The `staging-mismatch` case is the one that settles the design. That check runs *before* the
hash-tree check (`sealing.rs:141` vs `:148`), so someone who edits the seal file on the volume
gets their own strings echoed into the status, and the E2E already models exactly that adversary
(`test/e2e/kms.sh:236`).

## What is not wrong, stated so this document does not overclaim

- **It does not reach a notification.** `notify.rs` never reads `status`; messages render from
  `Summary` over the bundle. Checked.
- **It is not in `kubectl get` output.** The printer columns are Phase, Bundle, Export, Notify,
  Age (`crd.rs:186`).
- **`incidentId` is already bounded before it is echoed** — `path_safe` (`export.rs:547`): ≤100
  bytes, `[A-Za-z0-9._-]`, not `.` or `..`.
- **`{:?}` already escapes** quotes and control characters. That is the markup half; this document
  is about the size half.
- **No other status field is unbounded.** `status.notification.reason` is a fixed-code enum
  (`notify.rs:1506`), `status.local.reason` is `Reason::label()` (`retention.rs:604`), and every
  `status.exports[*].reason` is a `&'static str`.

So this is **resource exhaustion plus noise**, not evidence forgery: `export.rs:236` refuses a
capture whose cluster id is not the controller's, the object key is built from the controller's
own id, and `test/e2e/export.sh` already proves a forged status cannot redirect an upload.

**And this design does not close spec bloat.** A caller can still put a megabyte in
`spec.incidentId`; it is refused within one reconcile and never echoed. Saying so plainly is
better than a constraint that implies more coverage than it has — see *What was dropped*.

## The fix

### 1. Bound the message where it is built, not where it is stored

Truncating the assembled string cuts the wrong end. The give-up message reads

```
{reason}: gave up after {n} attempts ({detail}); the collected data is kept: set the … annotation to retry
```

— the unbounded `{detail}` sits in front of the only actionable sentence, so a cut at the end
removes the instruction and keeps the noise. So: **bound the component, and put the instruction
first.** `detail` is capped at 512 bytes at construction; `sealing.rs`'s echoed ids are capped
before they enter `SealError`.

### 2. Make the bound bind writers that are not this codebase

A newtype binds callers that go through the helpers. It does not bind the four call sites that
reach `api.patch_status` directly (`reconcile.rs:322`, `:574`, and the two helpers), and it cannot
bind a holder of `incidentcaptures/status` at all, who never runs this code.

So **both guards, for different populations**:

- `maxLength: 1024` on `status.message` in the CRD — binds every writer, including a hostile one.
- Truncation in code — so the controller never has its own patch **rejected** and loses the whole
  status object, which is what a schema cap alone would do on the day it matters.

`publish()` takes the bounded type too, so an Event cannot be built from a raw string
(`reconcile.rs:1021` builds the `SealDelayed` note from raw `detail` today).

### 3. `MAX = 1024`, and why it is a ceiling rather than a round number

Measured, not chosen:

| | bytes |
|---|---|
| static skeleton of the give-up message | 108 |
| worst reason, empty detail | 132 |
| realistic AWS `AccessDeniedException` (IRSA role ARN + key ARN) | 480 |
| realistic GCP `cryptoKeyVersions…asymmetricSign` denial | 561 |
| `staging-mismatch` with a maxed cluster id and incident id | 411 |

`events.k8s.io/v1` caps `note` at **1024**, and the API server rejects a longer one — while
`reconcile.rs:1035` discards the publish result, so the `SealFailed` Event an operator alerts on
would vanish with no trace. 1024 is therefore ~1.8× the measured realistic worst case **and**
exactly the Event ceiling. A compile-time assert records that it is only wrong if raised.

### 4. Two input constraints that earn their place

| Field | Constraint | Why |
|---|---|---|
| `clusterId` | `^[A-Za-z0-9._-]{1,83}$` | Exactly the chart's pattern (`values.schema.json`) and exactly what `main.rs:171-180` already enforces on the controller's own id. 83 because `webhook.rs:247` composes `<cluster>-<16 hex>` and `path_safe` caps at 100: 83 + 1 + 16 = 100 |
| `profile` | RFC 1123 **subdomain**, `maxLength: 253` | It names a `CaptureProfile`, so it is a Kubernetes object name. The first draft proposed a DNS *label* regex with a *subdomain* length — a length no label can reach — and it would have broken a legal install: `values.schema.json`'s `profile.name` has **no pattern** (while `notifyRoute` beside it does), so `--set profile.name=prod.default` installs today and every capture it produced would be rejected at admission |

The same `profile` pattern is mirrored into `values.schema.json`, with a `refuses` case in
`scripts/helm-renders.sh` beside the `clusterId` one, so the chart and the CRD cannot drift apart
again.

## What was dropped, and why

**The `incidentId` CRD pattern is not part of this design.** It buys nothing for the problem —
`incidentId` is already bounded by `path_safe` before it is echoed — and it costs two real things:

- `lapilli_captures_total{result="refused"}` is incremented only at `reconcile.rs:149` and `:908`,
  after the object exists. An admission rejection reaches neither, so `docs/metrics.md:23`'s
  "refused means … an unsafe or claimed incident id" would become false, and the shipped alert's
  annotation — *"check the capture's `status.message`"* — would point at an object that was never
  created. `alert-rules-check.sh` would stay green on synthetic series, which is the blind spot it
  exists to catch.
- `test/e2e/run.sh:572` (`refused ref-traversal … "/../../x"`) is the only live coverage of the
  traversal defence. With the pattern, the create is rejected, no status exists, and the step fails
  with the misleading `"an unsafe incident id was not refused"`.

`path_safe` stays and remains the guard. It also covers bytes that arrived from a bucket, which no
API server validated.

## Why now, and not after the first release

`COMPATIBILITY.md` §3 says the CRDs are alpha and additive-only. Adding `pattern` and `maxLength`
to existing fields is a **tightening**, not an addition, and a tightening can reject an object a
cluster already stores when something next writes to it.

That rule is a promise to users, and there are none: v0.1.0 is not tagged and the repository is
not public. The same shape as the rename — the cheap moment is the one before anybody depends on
it, and it is now.

## What this breaks, corrected

- `test/e2e/run.sh:570` (`refused ref-cluster other-cluster`) — **unaffected.** `other-cluster`
  matches the pattern, so the object is still created and still refused by `refuse_capture`. The
  first draft named this as the casualty; it was wrong.
- `test/e2e/run.sh:572` (`ref-traversal`) — **unaffected**, because the `incidentId` pattern was
  dropped. It was the real casualty of the first draft.
- An install with a dotted `profile.name` now fails at **install** rather than silently producing
  captures that cannot be admitted. That is the point, and it needs a CHANGELOG Migration bullet.
- The CRD manifests change, so both copies are regenerated.

**The CRD drift check does not cover this.** `release-check.sh:44-49` diffs the two committed
manifests against the *current* generator; it has no previous version as input, so a tightening is
green. `COMPATIBILITY.md:45` currently lists that same check as the enforcement of the
additive-only promise, which it cannot be — that cell is corrected to say nothing mechanical
checks §3. And `COMPATIBILITY.md:206` tells operators `kubectl apply --server-side -f crds.json`,
which conflicts with Helm's field manager on a chart-installed CRD; it gains `--force-conflicts`.

## Carried out, not fixed here

- `main.rs:171-180` already enforces `(1..=83)` on the controller's own cluster id, which makes
  `export.rs:142-149`'s `invalid-cluster-id` degradation unreachable.
- `CHANGELOG.md`'s existing `clusterId` migration bullet overclaims: "the chart schema and the
  controller refuse others", when `export.rs:142` only logs and disables object-store export, at
  ≤100 rather than ≤83, and never refuses the capture.

## Open question left after review

**Should `cluster-mismatch` echo the offending `clusterId` at all?** With the pattern it is
bounded to 83 characters, so echoing is cheap — but "the value you sent" is useful to an operator
and to nobody else, and the message could say the ids differ and leave the value to the log. Left
open because it is a product judgement, not a correctness one.
