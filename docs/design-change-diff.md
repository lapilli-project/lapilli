# Design — real spec change-diff (v0.2)

Status: **v4 — hardened by loop engineering (time-boxed, not dry); phases 1–3 implemented**
(redactor v1; Deployment, StatefulSet and DaemonSet diffs, with the ⚑ canaries and scenarios in `test/e2e/diffs.sh`
passing on kind). See
[`design-review-round4.md`](design-review-round4.md). Two last fixes carry required kind E2E
canaries (marked ⚑) because no critic round attacked them after they were folded.

## Problem

`changes.json` (v0.1) answers *"was it changed recently, by whom, roughly when"* from free
metadata (generation, managedFields manager/time, revision annotation). It cannot answer the
question an on-call engineer actually asks: **what changed, from what to what, how long
before the alert?** In the `kairn demo` bad rollout the bundle says "Deployment/checkout →
revision 2 by demo-deployer" but not "`CACHE_WARMUP: lazy → eager`, 94 s before firing".

## Positioning (honest)

Revision history and diffs are **not new**: `kubectl rollout history --revision`, Argo CD
history/diff, Komodor and Robusta change tracking all show them. What Kairn adds is only
its §3 edge: the diff is **captured at alert time, next to the logs and events, into one
sealed portable file**. So v0.2 builds the smallest diff that serves that, not a change-tracking
product.

## Goals / non-goals

- **Goal:** for the target's owner chain, put into the bundle every spec change between the
  window start and capture, as before/after, each tied to *when* (relative to firing) and
  *who* (as the API recorded it).
- **Goal:** say honestly where each "before" came from, and when it is unknown or failed.
- **Goal:** never let a diff be the thing that leaks a credential.
- **Non-goal:** a cluster audit log or GitOps history (audit logs, Argo CD, Flux own that).
- **Non-goal:** any always-on recorder or watch in v0.2 (see "Deferred: recorder").
- **Non-goal:** reading Secret values (DESIGN §7 stands).

## Design

### Prerequisite — the redactor ships first

v0.1 already writes `resources/*.json` with env literal values, and DESIGN §4/§6 describe a
"deterministic redactor" that **does not exist yet**. A diff makes the exposure worse (it
adds the *previous* value, often the credential that was just rotated because it leaked).
So a redactor ships **before or with** `diffs/`, and applies to `resources/` and `diffs/`.

Redaction policy v1 (best-effort; DESIGN §5 already disclaims completeness). It is applied
**at the source**, when a value is extracted from an API object, so every sink
(`resources/`, `diffs/`, `timeline.json`, `kairn demo` output) receives redacted data. A test
plants a canary credential in every candidate path and greps every file of the bundle.

**Candidate paths (normative table, in IEB-SPEC):**

| Path (pod template / ConfigMap) | Name rule | Value rule | Nested parse |
|---|:--:|:--:|:--:|
| `env[].value` (name = `env[].name`) | ✅ | ✅ | tokens (below) |
| `args[]`, `command[]`, `lifecycle.*.exec.command[]`, probe `exec.command[]` | ✅ per token | ✅ | tokens (below) |
| probe/lifecycle `httpGet.httpHeaders[].value` (name = header name) | ✅ | ✅ | — |
| `metadata.annotations` (object + template) | ✅ | ✅ | JSON values |
| ConfigMap `data.<key>` | ✅ | ✅ | JSON / YAML / `.properties` / `.env`: per inner key; other text: per `key[:=]value` line |
| ConfigMap `binaryData.<key>` | — | always redacted, in every mode | — | ⚑ canary: a `.p12` in `binaryData` never appears |
| everything else (`image`, `uid`, IDs, names, `resourceVersion`, ownerRefs, status) | — | — | — |

**Tokens:** every candidate string (env values, each args/command element, ConfigMap
free-text lines) is split on whitespace and `;`/`&&`, so a `sh -c "export PGPASSWORD=x && …"`
script or `JAVA_OPTS="-Xmx1g -Dspring.datasource.password=x"` is covered. Tokens of the form
`--name=value`, `-Dname=value`, `name=value`, `export NAME=value`, and `-name value` apply
the name rule to `name` (split on `.` too) and redact only `value`. URLs inside tokens have
userinfo redacted, and query parameters named `password pwd passwd token access_token secret
sig signature X-Amz-Signature X-Amz-Credential api_key apikey` redacted
(`jdbc:postgresql://db/app?user=a&password=<redacted>`).

`kubectl.kubernetes.io/last-applied-configuration` is **dropped** from captured objects (it
duplicates the spec, raw, env included); `redaction.json` records that it was dropped.

**Name rule:** the name is split into tokens on `_ - . /` and case changes; it matches when
any token equals one of `password passwd pass pwd secret token apikey key credential
credentials private privatekey auth authorization bearer cert dsn` or the name ends with
`_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, `CONNECTION_STRING`. Token equality, not
substring: `BYPASS_CACHE`, `AUTHOR`, `MONKEY` do not match. **Exception:** a value under a
matching name is still shown when it is a boolean, an integer, a duration (`30s`, `5m`),
≤ 12 chars of `[a-z0-9_-]`, an absolute file path, or a URL with no userinfo and no
credential query parameter — so `TOKEN_TTL_SECONDS: 300 → 30`, `AUTH_ENABLED: true →
false`, `AUTH_SERVICE_URL: http://auth.svc:8080` and `PUBLIC_KEY_PATH: /etc/tls/tls.crt`
stay visible (a changed upstream URL is a classic incident cause).

**Amendment during implementation (redactor v1).** The "≤ 12 chars of `[a-z0-9_-]` stays
visible" exception, applied to *every* matching name, would have shown `DB_PASSWORD=hunter2`:
a short enum and a short password are indistinguishable by syntax. Names therefore split
into **strong** (`password`, `secret`, `credential`, `private`, `dsn`, `authorization`,
`bearer`, glued forms like `PGPASSWORD`, pairs like `api key`/`client secret`), where only
booleans and durations stay visible, and **weak** (`key`, `token`, `auth`, `cert`), where the
integer/enum/path/URL exceptions apply. Canonical UUIDs are excluded from the value rule
(their entropy, 4.04, crosses the base64 threshold; they are ids far more often than keys).

**Value rule** (any name, per token): redacted when it
- is hex, ≥ 32 chars, Shannon entropy ≥ 3.0 bits/char; or base64/base64url, ≥ 24 chars,
  entropy ≥ 4.0 (per-charset thresholds: entropy per char can't exceed log2 of the alphabet,
  so one threshold can't serve both);
- is a JWT (`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`);
- starts with a known credential prefix (`AKIA`, `ASIA`, `ghp_`, `gho_`, `github_pat_`,
  `glpat-`, `xox[abpr]-`, `sk-`, `-----BEGIN`);
- or is a URL with userinfo or a credential query parameter (those parts only); each URL
  path segment is also checked by the rules above (webhook URLs carry tokens in the path).

The canary test carries real-format vectors for each rule (64-hex, 40-char base64, JWT,
AKIA key, JDBC URL with `password=`, `JAVA_OPTS -D…password=`, `sh -c` export, SAS `sig=`)
and negative vectors that must stay visible (image refs, FQDN service URLs, JVM flags,
paths, UIDs).

**Scope of "best-effort":** the owner chose this over strict-by-default knowingly. A newly
found pattern gap is a MINOR fix (add a rule + a canary vector), not a redesign; deployments
that need a guarantee use `strict`.

**Output:** a redacted value becomes `"<redacted>"`; diffs keep `changed: true|false`. No
hash, no length. The `changed` flag does reveal whether a secret was rotated; that is
accepted and documented.

**Modes:** `default` (above); `strict` (redact every candidate value except names in
`redaction.plaintext: [...]`); `off` (recorded in the bundle; `kairn verify` prints a loud
warning for `off` bundles). Redaction never touches `imagePullSecrets` names or other
object references.

`redaction.json` records policy version, mode, dropped fields, and per-file counts.

### Layer 1 — history the cluster already keeps (default on)

| Kind | History | Pod → revision |
|---|---|---|
| Deployment | ReplicaSets (full pod template each) | pod's ownerRef RS |
| StatefulSet, DaemonSet (phase 2) | `ControllerRevision.data` = strategic-merge wrapper `{"spec":{"template":{…,"$patch":"replace"}}}`; unwrap it to get the template | pod label `controller-revision-hash` |

Which revisions to diff. Every rule below was checked against a live kind cluster (v1.37),
because two plausible signals turned out to be wrong:
- the RS `creationTimestamp` stays at the original creation when `rollout undo` reuses an
  old RS;
- a managedFields `time` is per manager, not per field: the deployment controller's single
  entry on an RS moves on every scale, including HPA-driven ones, so it can make a change
  from days ago look seconds old.

1. **Anchor on the pod's own revision**: its RS via ownerRef, or its ControllerRevision via
   `controller-revision-hash`. Record `pod_revision_is_current: bool`.
2. **Reconstruct the revision sequence** from each retained RS's `revision` plus its
   `revision-history` annotation, which lists the numbers it held before a rollback reused it.
   A pair is only diffed between **adjacent** revisions; a missing intermediate revision gives
   `status: "before_unknown"`, `reason: "intermediate revision pruned"`, never a diff across
   the gap.
3. **Activation time** of a revision (when it became the live template), with
   `changed_at_source` recording which rule produced it:
   a. `creationTimestamp`, when the RS has an empty `revision-history` (never reused). It is
      exact: the controller creates the RS right after the template write, and later scale
      events (HPA, KEDA scale-to-zero and back) cannot move it. ⚑ canary: scale a Deployment
      to 0 and back up; no in-range entry may appear.
   b. `event`, for a **reused** RS (rollback): among the Deployment's `ScalingReplicaSet`
      events (core/v1 or events.k8s.io/v1), those whose message matches
      `^(\(combined from similar events\): )?Scaled up replica set <rs> from 0 to \d+`; the
      time is the latest of `lastTimestamp`, `series.lastObservedTime`, `eventTime`. Take the
      **latest**, not the earliest: identical messages are coalesced, so the rollback appears
      only as the latest timestamp. The event is accepted only if a matching
      `Scaled down replica set <predecessor> from \d+ to 0` event exists at or after it,
      i.e. it really was a switch of templates and not a scale-from-zero of the same one.
   c. otherwise `unknown`: `changed_at: null`, `in_range: null`.
   **Event time applies only to an RS's *current* `revision`.** Numbers listed in its
   `revision-history` are earlier activations of the same RS; coalescing kept no time for
   them, so they get `unknown`.
   **Sanity check** (the recorder can drop or aggregate events under bursts, e.g. a rollout
   and rollback of a large Deployment within 10 minutes): an `event` time is rejected (→
   `unknown`) if it is later than the earliest `creationTimestamp` among that RS's live
   pods (live = no `deletionTimestamp`), or earlier than the predecessor revision's activation time.
4. **Emit every adjacent pair** whose activation time lies in `[window.start, capture
   time]`, in order, each with `seconds_relative_to_firing`. `after_firing` is set only for
   `event` / `creationTimestamp` sources, and is `null` otherwise, so a remediation rollback
   after the alert can't be mis-signed by a noisy clock. If nothing activated in range,
   emit the pair (previous → pod's revision) with `in_range: false`.
5. **Pending edits:** diff `Deployment.spec.template` against the **newest** RS (not the
   pod's). Only a difference there, or `status.observedGeneration < metadata.generation`, is
   `source: "deployment-spec-pending"` (paused rollout, or controller not yet synced).
6. **Actor:** the `manager` of the Deployment's managedFields entry owning
   `f:spec.f:template`, accepted only if exactly one such entry has a `time` inside the
   match window, otherwise `actor: null` with `actor_reason`. Entries without `time` never
   match. The match window is:
   - for an RS that was never reused: `[creationTimestamp − 5 s, creationTimestamp + 5 s]`
     (the RS is created right after the user's write, whatever the rollout strategy);
   - for a reused RS (rollback), with `event` activation `t`: `[t − (terminationGracePeriodSeconds
     of the old pods + 5 s), t + 1 s]`, since a Recreate rollout scales the new RS up only
     after the old pods have terminated.
   *Implementation amendment (measured):* with the **Recreate** strategy the new ReplicaSet
   is created only after the old pods have stopped, so for a never-reused RS the window is
   `[creationTimestamp − (grace + 5 s), creationTimestamp + 1 s]` under Recreate (the ±5 s
   window missed it). managedFields times have 1 s resolution, so two template writes in
   the same second tie and give `null`.
   *Implementation amendment:* "exactly one owner in the window" gave `null` whenever two
   writes landed seconds apart (a create followed by a quick fix, measured in the E2E). The
   ReplicaSet is created right after the write that triggered it, so the **latest** template
   owner inside the window is taken; a tie in time yields `null`. The window's lower bound
   still keeps old owners out.
   (Measured: after a rollback, the only template owner left can be the original
   `kubectl-create` entry, because the removed fields took the other managers' entries with
   them; the window rule turns that into `null` rather than a wrong name.) Always labeled
   `actor_kind: "fieldManager (client-asserted)"`; the authenticated identity is in the API
   server audit log. Expect `null` often on loaded clusters or slow rollouts; that is the
   intended failure mode (no name rather than a wrong name). Under GitOps the actor is the
   GitOps controller (e.g. `argocd-controller`), which is what the API recorded.
7. **Tamper hint:** if an RS's managedFields contain an entry owning `f:spec.f:template`
   from a manager other than `kube-controller-manager`, the entry carries
   `warnings: ["RS template written by <manager> at <time> (hint; manager names are
   spoofable)"]`. It is not an integrity control. It catches honest hotfix edits that would
   otherwise make the diff silently wrong.

Honest limits, recorded per entry: `revisionHistoryLimit: 0` or deleted history →
`before_unknown` with `reason: "no prior revision retained"`. Retained history is mutable by
anyone with write access to it (§5).

**Restart-only revisions** (only `kubectl.kubernetes.io/restartedAt` changed) are
`kind_of_change: "restart-only-template"`, with `may_apply_out_of_band: ["ConfigMap/Secret
contents read via env or subPath", "mutable image tags (imagePullPolicy: Always)"]`. Where old
and new pods are visible, `containerStatuses[].imageID` is compared and a changed digest is
reported.

StatefulSet/DaemonSet: `ControllerRevision.data` is stored raw and never re-defaulted, so after
an API server upgrade a field present only on the newer side that equals a known default is
flagged `"probable-default"`.

New RBAC: `get/list` on `controllerrevisions` (phase 2). Nothing else.

### Layer 1.5 — ConfigMaps whose *name* changed in the template (opt-in)

Kustomize `configMapGenerator` (hash suffix), immutable ConfigMaps, and similar patterns
deliver a content change as a **new ConfigMap name** in the pod template, which Layer 1
already sees (`…/configMap/name: app-cfg-abc → app-cfg-def`). When that happens and
`diffs.configMaps: true`:
- GET both ConfigMaps at capture time (no list, no watch); diff key by key; redact per the
  policy above.
- If the old one is gone → `status: "before_unknown"`, `reason: "old ConfigMap pruned"`.
- **Helm `checksum/config` annotation changed** with the same ConfigMap name → the content
  was overwritten in place; emit `status: "before_unknown"`, `reason: "overwritten in place
  (checksum annotation changed)"`. Honest, not silent.

New RBAC only when enabled: `get` on `configmaps`, as namespaced Roles only. The chart
**refuses** `diffs.configMaps: true` unless `watchNamespaces` is non-empty (a cluster-wide
ConfigMap `get` reaches `kube-system/aws-auth` and other tenants' configs by guessable name),
and refuses a `watchNamespaces` list containing `kube-system`. The Role necessarily grants
`get` on every ConfigMap in those namespaces (RBAC can't express "referenced by this pod"),
so "only ConfigMaps referenced by the target pod's own template are read" is enforced in
code, not by RBAC; the docs say so, and warn that with `HELM_DRIVER=configmap` Helm keeps
release values (gzip+base64, i.e. readable) in ConfigMaps of the release namespace. Off by default, since the current role grants no ConfigMap access.

### Deferred: the rolling recorder (was "Layer 2")

Cut from v0.2. An always-on ConfigMap watcher is the only way to diff in-place edits, but
R1 showed it costs more than it returns now. Requirements if adopters later report in-place
edits as a real gap:
- plain `watcher` (not a reflector store); hash-and-drop per event; record only when the
  data digest changes; `streaming_lists` where supported;
- HMAC (process-local key), never plain SHA-256; or `changed` only;
- `watchNamespaces` or a label selector required; `kube-system` excluded by default;
- per-namespace quotas, object-size cap, pin ≥ 1 prior version for referenced ConfigMaps,
  evictions recorded per object;
- compare against the version **live when the container started** (`state.running.startedAt`),
  with per-consumer `env | subPath | volume` applied-semantics.

## Bundle format

```
diffs/
  index.json
  <namespace>/<Kind>/<name>/<n>.json     # one file per emitted revision pair, n = 0,1,…
redaction.json
```

Path components are validated with the same rules `kairn verify` applies on unpack (no
`/`, `..`, NUL); names that fail are replaced by their sha256 prefix and the original is
kept in the index.

`diffs/` is owned by the `changes` collector (IEB-SPEC layout gains `diffs/` and
`redaction.json` under their collectors).

`diffs/index.json`:

```json
{ "normalization": "v1",
  "expected": [ { "namespace": "shop", "kind": "Deployment", "name": "checkout" } ],
  "entries": [
    { "namespace": "shop", "kind": "Deployment", "name": "checkout",
      "status": "ok", "source": "replicaset-history",
      "before": { "revision": "1", "object": "ReplicaSet/checkout-5c8f4588f5" },
      "after":  { "revision": "2", "object": "ReplicaSet/checkout-7f8b6b66f8" },
      "changed_at": "2026-09-19T03:50:25Z", "changed_at_source": "event",
      "seconds_relative_to_firing": -94, "after_firing": false, "in_range": true,
      "actor": "demo-deployer", "actor_kind": "fieldManager (client-asserted)",
      "pod_revision_is_current": true, "kind_of_change": "spec", "warnings": [],
      "summary": ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"],
      "file": "diffs/shop/Deployment/checkout/0.json" } ] }
```

`status` ∈ `ok` | `no_change` | `before_unknown` | `unsupported_kind` | `error`, with `reason`
for all but `ok`/`no_change`. Supported owner kinds are listed in IEB-SPEC (v0.2: Deployment →
ReplicaSet; phase 2 adds StatefulSet, DaemonSet). Any other owner (Job, CronJob, Argo
Rollout, custom controllers) gets `unsupported_kind`, which, like `before_unknown`, does not
affect coverage. Classification is by the **top** controller of the chain, so pod → RS →
Argo Rollout is `unsupported_kind`, not a ReplicaSet diff.

**`expected`** is built from references the collector already holds before any fetch (the
pod's ownerRefs, then each fetched owner's ownerRefs), so a failed GET still leaves an
`expected` object with an `error` entry. A bare pod has `expected: []`.

**Coverage rule:** any `error` entry fails the `changes` collector → bundle `PARTIAL` (same
rule as `metrics/`), after `changes.json` and `diffs/` have been written, so the indicators
that succeeded are kept. `before_unknown` does not make the bundle PARTIAL: it is a true
statement about the cluster, like `logs/index.json`'s `unavailable`. The existing silent
`if let Ok(...)` owner-chain lookups become `error` entries.

`<n>.json` is a list of field changes:

```json
[ { "op": "replace",
    "display": "containers[name=app].env[name=CACHE_WARMUP].value",
    "path_before": "/spec/template/spec/containers/0/env/0/value",
    "path_after":  "/spec/template/spec/containers/0/env/0/value",
    "before": "lazy", "after": "eager", "changed": true } ]
```

- `op` ∈ `add` | `remove` | `replace`. It is **not** RFC 6902: each change is independent,
  and nothing is meant to be applied as a patch.
- **`display` is the normative identity.** Keyed list elements are addressed by merge key
  (`containers[name=app]`, `volumeMounts[mountPath=/etc/x]`, composite keys comma-joined in
  a fixed order: `ports[containerPort=8080,protocol=TCP]`), positional lists by index
  (`args[2]`). Inside `[...]`, the characters `\ ] = ,` in key values are escaped with `\`.
  A reordered env list therefore yields no change.
- `path_before` / `path_after` are optional RFC 6901 JSON Pointers into each side: absent on
  the side where the element doesn't exist (`add` has no `path_before`, `remove` has no
  `path_after`).
- Redacted values appear as `"<redacted>"` with `changed` still set.

Humans read the one-line `summary`, which is also merged into `timeline.json` as a `source:
"change"` event and printed by `kairn demo`.

### Normalization v1 (written into IEB-SPEC as a numbered algorithm)

1. Take the pod template (`spec.template`); for ControllerRevision, unwrap `.data` first.
2. Remove `metadata.labels["pod-template-hash"]`, `metadata.labels["controller-revision-hash"]`.
3. Match list elements **by merge key** where Kubernetes defines one (containers,
   initContainers, ephemeralContainers by `name`; env by `name`; volumes by `name`;
   volumeMounts by `mountPath`; ports by `containerPort`+`protocol`); other lists are
   compared positionally. If a key is duplicated within a list (the API allows duplicate
   env names), that list falls back to positional comparison.
4. Treat absent and default-equal as distinct (no defaulting). For Deployments this is
   safe: the API server re-applies defaults when it reads each RS from etcd, so both sides
   carry current defaults. For ControllerRevisions see "probable-default" above.
5. Redact per policy v1 at extraction; raw values are compared only to compute `changed` and
   never leave the process.

## Integrity & honesty

- Layer 1/1.5 "before" is the cluster's own retained object read at capture time: the same
  trust as every other collector, with the explicit caveat that retained history is mutable
  by anyone with write access to it (§5).
- The actor is client-asserted; the bundle says so.
- Diffs and `redaction.json` are sealed like every other file.

## Cost and phasing

1. **Redactor v1** over `resources/` (+ `redaction.json`, canary test): ≈ 1 week (the
   field-scope table and nested parsing make it larger than R1 assumed).
2. **Layer 1, Deployments** + `diffs/` format + timeline line + demo output: ≈ 1 week. This
   alone delivers the demo aha (`CACHE_WARMUP` is a template env literal).
3. **Layer 1, StatefulSet/DaemonSet**: ≈ 2–3 days.
4. **Layer 1.5** (opt-in ConfigMap follow): ≈ 2–3 days.

No new watches, no new memory state, no new default RBAC beyond `controllerrevisions`.

## Also fixed in DESIGN (v0.1 overclaims found in R1)

- §4 said `changes.json` carries "ConfigMap content hashes" — it does not.
- §4/§6 described a deterministic redactor and `redaction.json` — they did not exist.
- §3 table: Robusta *does* track changes (in chat); it produces no portable file.
