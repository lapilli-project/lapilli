# Design — incident notification with a summary (v0.2)

Status: **v1, after loop-engineering round 11 (security and on-call lenses)** — log:
[`design-review-round11.md`](design-review-round11.md).

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

- **Exported bundles:** `kairn verify s3://…/<incident>.ieb --cluster <c> --incident <i>`.
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

- Captures are grouped by `(rule, namespace, owner)` — the owner from the capture's target,
  the same key the diffs already resolve.
- A coalescing window (default 30 s, reset by each new capture in the group, capped at 2
  minutes) collects members, then one message is sent: the group's verdict, how many pods,
  how many share the same termination reason, the change (identical across the group), and
  the first three incident ids plus `+N more`.
- A hard cap (`notify.maxPerWindow`, default 10 messages per 5 minutes per route) with one
  `N notifications suppressed` tail message, so a cluster-wide event can never outpace the
  channel.

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
- **URL handling reuses the CLI's strictest code** (`crates/kairn-cli/src/remote.rs`
  `parse_https`/`open_https`): HTTPS only, no userinfo, no control characters or escapes, no
  redirects (a 3xx is an error), and errors never print the URL. The assembled URL's host
  must equal the configured host. Plain HTTP is allowed only to loopback and cluster-local
  names **behind a test-only flag**, and resolved addresses in non-global ranges (link-local
  169.254.0.0/16, loopback, private ranges outside the cluster CIDR) are refused.
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

- **Unit:** summary from a staged directory; `facts_only()` drops content; truncation;
  grouping and coalescing; the rate cap; Block Kit escaping of `<!channel>`, `<url|label>`
  and `&`; URL validation vectors (`http://127.0.0.1@evil.example`, `[::1]`, a redirect);
  the `O_EXCL` claim (two dispatchers, one message).
- **kind E2E** with a receiver pod:
  - a bad rollout across 5 pods produces **one** message naming 5 captures;
  - the default install's message contains no workload string: the canary planted in the
    demo app's **log stream** (new) and in its env are both absent, and so is the last log
    line;
  - `detail: content` shows the changed image tag and still no canary;
  - an alert whose `alertname` contains `<!channel>` is refused by the CRD pattern, and a
    crafted `pod` label appears escaped, not as a link;
  - receiver down: the capture still reaches `Exported`, status says `failed`, and the
    counter moves.
