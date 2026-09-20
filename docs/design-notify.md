# Design — incident notification with a summary (v1.1)

Status: **implemented**. v1 came out of loop-engineering round 11 (security and on-call
lenses) — log: [`design-review-round11.md`](design-review-round11.md). v1.1 records what the
implementation does **differently** from v1, in "[As built](#as-built)" — written after the
code, so the document describes the product rather than the intention.

## Problem

Kairn captures evidence and nobody sees it. The bundle holds what an on-call engineer wants
in the first ten minutes — whether the container was OOM-killed, whether its last log
survived, and what the last rollout changed, when, by whom — but that value sits in a `.ieb`
on a PVC. Somebody has to know it exists, pull it and unpack it.

That makes the product cold: opened only after an incident, during an audit, or in a
dispute. A tool nobody looks at is the first thing removed, and it never gains adopters.

## Goals / non-goals

- **Goal:** when a capture is sealed, post a one-screen summary where the team already looks,
  with a command that actually retrieves the bundle.
- **Goal:** **one message per incident**, not per pod. A bad rollout across 50 replicas is one
  message.
- **Goal:** the message is useful without carrying workload content, and it never becomes a
  channel for text an alert author chose.
- **Goal:** notification never affects the capture. It runs off the capture path, is bounded,
  and is sent at most once per incident.
- **Non-goals:** threading or editing existing messages, PagerDuty/Jira-specific payloads
  (generic JSON covers them), incident-management workflow, a postmortem draft (that is its
  own feature, and the stronger one — see the roadmap).

## What the message says

Built from the bundle's own files by `kairn_bundle::summary` (no extra API calls), which the
CLI shares. The **facts** are all numbers, enum-like reasons and cluster identifiers:

| Field | Why an on-call engineer cares |
|---|---|
| `OOMKilled (exit 137), restart 2` | what kind of failure this is |
| `last words: captured` / `discarded by kubelet` | whether opening the bundle is worth 90 seconds |
| `Deployment/checkout rev 1 → 2, 94s before the alert, by argocd-application-controller` | the single most actionable fact: what changed, when, who |

**Honest limit on that row.** The revisions and the timing come from `diffs/`, and the collector
cannot always establish them: a Deployment whose pods never become Ready stays *progressing*, so
the previous revision is unknown and the entry is recorded as `before_unknown`. The message then
says `Deployment/checkout changed, by ~someone` with no `rev N → M` and no timing — correct, but
thinner than this table suggests, and it happens in exactly the crashlooping case notification
exists for. Improving that belongs to `diffs/`, not here; the kind E2E asserts the change and the
actor for such a workload, and `diffs.sh` covers the workloads where the revisions do resolve.
The changed **field and values** are independent of that and do resolve: they are read from the
pair's own diff file.
| `peak 61.2 MiB of 64 MiB limit (8 samples)` | leak or spike, so: raise the limit or read the code |
| `collectors: logs, resources; missing: metrics` | how complete the evidence is |
| the retrieval commands (below) | how to act on it |

Cut deliberately: the cluster id (the channel implies it), the firing timestamp (the message
carries its own), and the signing line (`kairn verify` prints it, where it matters).

**Workload content is opt-in and narrow.** With `detail: content` the message also carries
the changed field and its before/after values — and **nothing else**:

- Those values pass through the capture's redactor, because Kairn redacts structured
  API-object values (`spec/IEB-SPEC.md`).
- **The last log line is never sent, in any mode.** Bundles deliberately do not redact logs
  ("they are the evidence"), and free-text redaction is heuristic: a password inside a panic
  message, a bearer token in a header dump and PII are all shapes it can miss. A chat channel
  is usually less protected than the evidence store, so the log line stays in the bundle.
  The summary says whether it exists; reading it means opening the bundle.

### Untrusted strings, escaped

`rule`, `namespace`, `pod` and the diff's `actor` come from whoever sent the alert or patched
the workload, not from the operator:

- `spec.trigger.rule` is pattern-constrained in the CRD and at the webhook, as `incidentId`
  already is.
- Every such string is rendered as Block Kit `plain_text` (never mrkdwn), with `&`, `<`, `>`
  escaped and a per-field cap, so `<!channel>` and `<url|label>` cannot be injected.
- The actor is labelled as client-asserted, as the bundle labels it.

## Retrieval: the message must be actionable

A path inside a pod is useless to a human, so the message prints what works:

- **Exported bundles:** `kairn verify s3://…/<incident>.ieb --cluster <c> --incident <i>`, as
  a loop over every capture in the group when there is more than one.
- **Otherwise (the default):** two copy-pasteable lines using the same mechanism the demo
  uses, which means promoting today's hidden `cat-bundle` to a documented
  `kairn pull <incident>`:
  ```
  kubectl -n kairn-system exec deploy/kairn -c controller -- \
    kairn cat-bundle /var/lib/kairn/bundles/<incident>.ieb > <incident>.ieb
  kairn verify <incident>.ieb --cluster <c> --incident <i>
  ```

Both are **recomputed** from the controller's own configuration and the capture's spec, never
read from `status` (see below).

## Grouping

One capture per pod is right; one message per pod is not.

- Captures are grouped by `(route, rule, namespace, owner)` — the owner as the capture's own
  diff resolved it (`Deployment/checkout`), which is the same workload identity the message
  already reports.
- A coalescing window (default 30 s, reset by each new capture in the group, capped at 2
  minutes) collects members, then one message is sent: the group's verdict, how many pods,
  how many share the same termination reason, the change (identical across the group), and
  the first three incident ids plus `+N more`.
- A hard cap (`notify.maxPerWindow`, default 10 messages per 5 minutes per route). The
  **first** group the cap turns away posts an immediate notice naming the count and the
  `kubectl get incidentcapture` that lists the rest; further suppressed groups are counted and
  reported by the next message that gets through. A cap still means a human does not see every
  incident in the channel — it bounds the channel, it does not preserve every message.
- **Repeats are counted, not posted.** A crashlooping pod re-fires the same alert for hours,
  and each re-fire is a new capture with a new id, so neither the per-incident claim nor the
  rate cap can see it — only the group key can. After a group has been **delivered**, further
  firings for the same **rollout** stay quiet for `COOLDOWN` (30 minutes) and the repeats ride
  on the next message as `×N more since 14:05`.
  - **Only a new rollout is news.** A changed termination reason deliberately is *not*: a
    container that dies `OOMKilled` on one restart and `Error` on the next is one incident, and
    treating each as news posts every single firing — measured at ~30 messages an hour, worse
    than the bug the cooldown exists to fix. The reasons seen while quiet are reported instead,
    as `also seen: Error, OOMKilled`, so damping never hides how it failed.
  - "A new rollout" means the revision when `diffs/` could establish one, and otherwise a
    **non-reversible fingerprint of what changed**. That fallback is not a detail: a Deployment
    whose pods never become Ready has no known revisions, which is the crashlooping case this
    feature exists for — keyed on the revision alone, a rollback *during* such an incident would
    look like the same incident continuing and stay unreported for the whole cooldown. The
    fingerprint is hashed rather than kept, because the key outlives the message and the values
    it derives from are workload content.
  - The rollout is taken over the **whole member set**, not the first capture sealed, or
    admission order would flip the key and re-announce.
  - **A send that never landed does not arm the cooldown.** A failed POST, or one the rate cap
    turned away, leaves the group undamped and gives its carried count back — otherwise the
    incident is muted for half an hour having never been announced at all, which is the same
    failure as a message the endpoint rejected.

## Routes (admin-defined, referenced by name)

The export pattern, which round 6 settled: the admin defines destinations; profiles name one.

```yaml
notify:
  routes:
    - name: platform
      # The host lives here (a ConfigMap; a change rolls the pod) and only the secret path
      # segment comes from the Secret, so the endpoint can't be silently repointed.
      host: hooks.slack.com
      pathSecret: kairn-slack-hook      # Secret with `path: /services/T000/B000/xxxx`
      format: slack                      # slack | json
      detail: facts                      # facts | content
      maxPerWindow: 10
```

- A `CaptureProfile` may only name a route (`notify.route: platform`) or opt out
  (`notify.route: ""`). It can neither define one nor change `detail`.
- Per-team routing falls out of this: one route per namespace group, each with its own
  channel and detail level.
- **URL handling lives in one crate, `kairn-net`**, shared with remote verify, the KMS client
  and the Prometheus collector: HTTPS only, no userinfo, no control characters, backslashes or
  escapes, no redirects (a 3xx is an error), and errors never print the query string. The
  assembled URL's host must equal the configured host, and the Secret's path must begin with a
  single `/` (so `//evil.example/x`, which some proxies renormalize into another origin, is
  refused). Plain HTTP needs **both** a per-route `insecureHttp` and the process-wide
  `KAIRN_NOTIFY_ALLOW_HTTP`, and even then only for a loopback or cluster-local host. Every
  resolved address is checked and then **pinned** into the client, so a name that answers with
  a public address during the check cannot answer with `169.254.169.254` at connect time.
- **Demo captures do not notify** unless `notify.includeDemo: true`. `kairn.dev/export: local`
  keeps its single meaning — "never leaves the cluster" — and a synthetic demo alert can't be
  used to push chosen text into a channel.

## Order, once-only and failure policy

1. Seal, sign, pack, and — this is new — **write the rendered summary next to the bundle**
   (`<incident>.summary.json`) before the staging directory is removed. Retries and restarts
   then have a source; the staged files are gone by the time the message is sent.
2. Notification runs **off the reconcile path**, in a small dispatcher task with the
   coalescing windows, so a slow webhook never delays another capture's collection.
3. **Once-only is a filesystem claim**, not a status field: `<incident>.notified` is created
   with `O_EXCL` before the POST, the same mechanism as the incident-id claim. Anyone who can
   patch `status` can therefore neither replay the message nor suppress it.
4. Sending: 5 s timeout, 2 retries over ~10 s, `429`/`Retry-After` respected within that
   budget. Failure is logged, counted (`kairn_notifications_total{result}`) and recorded in
   `status.notification = {state, at, reason}` — reporting only, with fixed reason codes and
   never a webhook response body (a 4xx body can quote the request).
5. Overflow: the payload is capped at 16 KiB; fields are truncated before assembly (a value
   to 100 characters) and the group list to three ids, so the cap is never reached in
   practice. If it still is, the message is sent without the change detail.

## As built

Six places where the implementation differs from v1. Each is a decision the code forced, not
a corner cut.

| # | v1 said | As built | Why |
|---|---|---|---|
| 1 | The owner comes from "the capture's target" | It comes from the capture's **resolved diff** (`change.kind/name`), falling back to `Pod/<pod>` | The owner chain is resolved during collection and written into the bundle; re-resolving it at notification time would be a second set of API calls for an answer already on disk. A capture whose diff found nothing is its own incident, which is the honest grouping. |
| 2 | Notify "when a capture is sealed" | Notify when the capture is sealed **and every export has settled** | Only then can the message say truthfully where the bundle is. Announcing at seal would print an `s3://` line for an object that may still fail to upload, or a pod-local path for one that is about to be in a bucket. Captures with no destinations settle immediately, so the common case is unchanged. |
| 3 | Plain HTTP behind "a test-only flag" | Behind **two**: the route's `insecureHttp` and `KAIRN_NOTIFY_ALLOW_HTTP` | One flag would be a single chart value away from downgrading a production endpoint. Two means a values change alone cannot do it, and the chart refuses `insecureHttp` on a non-local host at install time. |
| 4 | "`429`/`Retry-After` respected within that budget" | `429` and `5xx` are retried twice over ~6 s; `Retry-After` is **not** read | The whole budget is 10 s, and a `Retry-After` is usually longer than that. Honouring it would mean holding the group open past its own coalescing cap. A rate-limited route is better served by `maxPerWindow`. |
| 5 | "private ranges outside the cluster CIDR" are refused | An endpoint that is cluster-local by name gets `Reach::Cluster` (private ranges allowed); anything else gets `Reach::Internet` (every private range refused). Link-local, multicast, broadcast and unspecified are refused for both | The controller has no way to learn the cluster CIDR — nothing in the API exposes it portably — so v1's rule was unimplementable as written. The name/reach split gets the same protection without a value an admin would have to keep in sync. |
| 6 | (not stated) | A capture whose summary says nothing (no termination, no change, no memory, no last words) is **not** announced | A "we captured something" ping is the message that gets the channel muted, which is the failure P1 was about. |

**Shutdown is part of the contract, not an edge case.** Kubernetes sends SIGTERM on every
rollout, so a dispatcher that abandons its open groups loses a message on every upgrade — and
because a group is claimed *before* it is posted, those captures are left marked notified and
never announced. On SIGTERM (and SIGINT) the dispatcher therefore flushes the groups still
coalescing immediately rather than waiting out their windows, and the controller waits up to 10 s
for the sends in flight. What is still lost if the grace period runs out is a message, never
evidence.

Two numbers in that sentence used to be assertions rather than facts, and both are now enforced.

The 10 s has to cover **one whole flush attempt**, and an attempt is not just the request: the
address is resolved and vetted first, under `kairn_net::RESOLVE_TIMEOUT`. With the ordinary 5 s
request budget that made the worst case 5 + 5 = 10 s — exactly the drain window, so a slow resolver
plus a slow endpoint raced the timeout and lost the claim. The flush now uses a 3 s request budget
(`POST_BUDGET_DRAINING`), and `main.rs` carries a **compile-time assertion** that the drain window
exceeds `RESOLVE_TIMEOUT + POST_BUDGET_DRAINING` with slack. Putting the old number back does not
fail a test; it fails the build.

"Well inside the default 30 s grace period" was true only by default. The chart did not set
`terminationGracePeriodSeconds` at all, so lowering it silently truncated the flush **and** the
captures in flight, with the kubelet's SIGKILL and nothing in the log. The chart now sets it,
`values.schema.json` refuses anything below 25 s, the template refuses to render below it as a
backstop, and a unit test reads the schema back and fails if `RECONCILE_GRACE + NOTIFY_DRAIN` ever
outgrows the floor.

What remains lost at shutdown, and is documented rather than fixed: the **tally** a rate-capped
route carries on its next message. The storm itself is still announced — the first group a window
turns away gets a standalone notice — and every suppression is counted in
`kairn_notifications_total{result="suppressed"}`, so the loss is the "×N more" line in the channel,
not the knowledge. The dispatcher logs the outstanding count on its way out.

One thing v1 left implicit that matters: **the claim is never released**, and it is taken for
**every member of a group**, not only the one the message names. A notification that failed to
send, or that the rate cap turned away, is not retried by a later reconcile of the same
capture — the claim file is the once-only guarantee, and making it releasable would make replay
possible.

### Round 12 changed six more things

Round 12 reviewed the implementation against v1 with three lenses
([`design-review-round12.md`](design-review-round12.md)). Beyond the defects it found, these
are decisions the code now records:

1. **A group is claimed member by member, unconditionally, before the POST.** Claiming only
   the leader left the other N−1 captures of a grouped incident unclaimed, so each re-announced
   the incident on its next reconcile — and a kube watcher relist is enough to trigger that.
   Claiming before the POST, rather than after, means a controller killed mid-send cannot leave
   the members unclaimed; claiming unconditionally means a PVC written by an older build, which
   carries exactly that half-claimed state, does not re-announce a closed incident on upgrade.
2. **Sending happens off the coalescing task.** One stuck route otherwise held the single
   dispatcher task for its whole retry budget (measured: 21 s per group, serially), stalling
   every other group's window past the 2-minute cap. The claim file makes concurrent sends
   safe, which is what makes this cheap.
3. **Untrusted strings are escaped exactly once, at the leaf**, and every cap counts the
   **rendered** characters. Escaping at the block level too turned a real `&` into `&amp;amp;`,
   and capping the input could not keep a block under Slack's limit, because `&` expands to
   five characters. Escaping also flattens the characters that are *invisible* rather than
   markup — U+2028/2029, the bidi overrides and isolates, the zero-width marks and the BOM —
   because `char::is_control` is only `Cc`, and the CRD's boundary pattern on `rule` refuses
   only `<`, `>` and `&`. Without that, an alert author can write their own line under Kairn's
   text without using a single character the pattern blocks.
8. **The command block names at most `MAX_IDS` (40) captures**, then points at
   `kubectl get incidentcapture` for the rest, and the fence is applied *after* the text is
   fitted. Fitting the fenced string as a whole silently ate the closing `done` and the fence
   past ~96 members and dropped ids past ~105 — at 200 pods half the bundles were unretrievable
   from the message and the copy button yielded an unterminated `for … do`.
4. **Errors never reach `status` as transport text.** `status.notification.reason` is one of a
   fixed set of codes (`unreachable`, `timeout`, `endpoint-refused`, `endpoint-error`,
   `rate-limited-by-endpoint`, `endpoint-redirected`, `route-unusable`, `rate-capped`,
   `claim-failed`, `already-notified`, `in-cooldown`); the detail is logged. An endpoint's scheme and host are
   printable, its path is not — for a chat webhook the path **is** the credential, so
   `kairn-net` elides it everywhere.
5. **The retrieval command names this release's Deployment** (`KAIRN_DEPLOYMENT` from the
   chart, because the name is `<release>-kairn`), uses the absolute binary path, and loops over
   **every** capture in the group rather than the leader's alone.
6. **Only captures this controller process has seen from the start are announced.** A watcher
   relist re-reconciles every `Exported` capture on the PVC, so the moment an admin first
   configures a route — which rolls the controller — every incident the recorder has ever held
   would land in the channel at once. A capture declined this way is *claimed*, so a relist does
   not keep re-deciding it.
   The gate is the process start, not a wall-clock window: a window is either too wide to stop
   the replay or too narrow to survive a long `Sealing` wait during a KMS outage, where the
   capture is hours old and its message is still wanted. The cost, stated plainly: a capture
   created before a restart is never announced, even if it seals afterwards. The evidence is safe
   either way; only the message is lost.
7. **`<incident>.summary.json` never holds the log line.** The sidecar sits outside the hash
   tree and outside the signature, nothing reads it, and nothing prunes it, so a `.ieb` deleted
   for retention would have left the container's last words behind it in plaintext.

## Security summary

| Threat | Control |
|---|---|
| A profile editor redirects summaries | routes are admin-defined; profiles name one |
| The Secret is patched to repoint the endpoint | the host is in the ConfigMap and checked per send |
| SSRF, redirects, metadata endpoints | strict URL parse, no redirects, non-global IPs refused |
| Alert author injects text, pings or links | pattern-constrained rule, `plain_text` rendering, escaping, caps |
| Secrets in the message | no log text ever; structured values redacted; fixed field list |
| A status patcher replays or suppresses | `O_EXCL` claim; path and URL recomputed, never read from status |
| Evidence link forged in the channel | the bundle location is recomputed from config, not status |
| An alert storm floods the channel | grouping plus a per-route rate cap with one suppression notice |

Residual, stated in the docs: the chat vendor sees the bucket and prefix through link
unfurling; a route's channel is only as private as the team makes it; and the actor is
client-asserted.

## Testing

- **Unit** (`crates/kairn-controller/src/notify.rs`, 21 tests; `crates/kairn-net/src/lib.rs`, 6;
  `crates/kairn-bundle/src/summary.rs` for the summary itself): facts mode carries no workload
  value and content mode adds only the changed field; grouping, the coalescing window and its
  hard cap; the `O_EXCL` claim, including that it is never releasable and that a **counted
  repeat** is claimed too; the rate cap, its immediate notice, and the debt being restored when
  a send fails; `verdict_key` — what counts as "the same incident continuing"; escaping
  `<!channel>` and `<url|label>` exactly once, with every Slack block inside Slack's own limits
  for a 63-character namespace and a value of pure ampersands; the retrieval command naming
  this release's Deployment and every capture in the group; `safe_commands` degrading the
  fenced block to `plain_text` rather than trusting itself; route-name and unknown-field
  rejection; and a flapping **running** pod still reporting its restart count.
  A pin test over `Summary`'s serialized fields fails closed when a field is added without being
  classified `fact` or `content`, because the split is a deny-list.
- **kind E2E** with a receiver pod (`test/e2e/notify.sh`, run by `test/e2e/run.sh`):
  - a bad rollout across 5 pods produces **one** message naming 5 captures, with three ids plus
    `+2 more` in the context line and **all five** in the command block;
  - the default install's message contains no workload string: the canary planted in the demo
    app's **log stream** and in its env are both absent, and so is the last log line — and every
    `<incident>.summary.json` on the PVC is read back and checked for the same;
  - **exactly five** `.notified` claims for the five-member group;
  - `detail: content` shows the changed image tag and still no canary;
  - **repeats**: the same verdict on the same workload produces no second POST for 150 s; after
    a new rollout the next message carries `×N more since HH:MM UTC`;
  - the block shape: `header, section, context, section`, every text object `plain_text` except
    exactly one fenced `mrkdwn` block, and no raw `<!channel>`, `<!here>` or `<url|label>`
    anywhere in the request log;
  - a route whose path Secret does not exist: the pod still becomes Ready (the volume is
    `optional`), `kairn_notify_routes{state="error"}` is 1, and the log says which route and why;
  - **a rollout mid-window**: the controller pod is deleted while a group is still coalescing,
    and the message still arrives — the flush, not the window, is what delivers it;
  - receiver down: the capture still reaches `Exported`, `status.notification.state` is `failed`
    with a reason from the fixed code set, the `NOTIFY` column shows it, and
    `kairn_notifications_total{result="failed"}` moves;
  - after cleanup, `kairn_notify_routes` is absent while `kairn_notifications_total` remains —
    so "off" and "broken" never read the same.
- **Deliberately not covered yet**, listed in the script rather than left implied: the rate cap
  and its notice (it would need 11 groups inside one 5-minute window), `includeDemo`,
  `format: json`, the `dropped`/`already-notified`/`suppressed` results, and the exported-bundle
  retrieval line (it needs export destinations that `export.sh` tears down).
