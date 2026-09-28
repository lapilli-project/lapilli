# Data handling: what a bundle holds, where copies go, how long they live

Read this before you install. Lapilli seals a Kubernetes workload's state into a portable `.ieb`
file that is **designed to leave the cluster** — onto a PVC, into an S3 or GCS bucket, summarized
into a chat channel, served to an AI client over MCP. If you have to classify that artifact before
you may run it, this page is the input: it names every category of data a bundle carries and says,
for each one, whether redaction touches it.

Two framing statements, because the rest of the page is detail:

- **Redaction targets credentials, not personal data.** Its scope is env values, `command`/`args`,
  probe and lifecycle handler values, annotations and event messages — the places a password
  ends up. It is best-effort even there ([`../spec/IEB-SPEC.md`](../spec/IEB-SPEC.md), "Redaction
  and `redaction.json`"), and `DESIGN.md` §4 says so. What this page adds is the other half:
  **what is never redacted, in any mode, by design.** Labels, IP addresses, node and service
  account names, field-manager identities, and every line of every container log are in that half.
- **Retention is off by default.** `retention.maxBytes: 0` and `retention.days: 0` in
  [`../charts/lapilli/values.yaml`](../charts/lapilli/values.yaml) mean nothing ever deletes a
  sealed bundle. On a default install, bundles are kept forever. `DESIGN.md` §11 argues that
  "the retention default cannot be 'keep forever'"; the shipped default is keep forever, and
  nothing in the product closes that gap for you.

Everything below was read out of the code, not out of intent. The file and symbol are named so you
can check each row yourself.

## What lands in a bundle

`redact.rs`'s `redact_object` is the whole of what runs over a captured API object: it walks
`metadata.annotations`, `spec.template.metadata.annotations`, and the containers of a pod spec or
pod template. Nothing else in the object is visited. So the table splits into three groups, and the
third is the long one.

### Redacted, best-effort

The name and value rules are specified in [`../spec/IEB-SPEC.md`](../spec/IEB-SPEC.md). Best-effort
means a value in a shape no rule matches comes through: a missing pattern is a bug fixed by adding a
rule and a test vector, not a guarantee you can rely on.

| What | Where it lands | `strict` |
|---|---|---|
| `env[].value` | `resources/*.json`, `diffs/**` | every value redacted unless its name is in `redaction.plaintext` |
| `command[]`, `args[]`, lifecycle and probe `exec.command[]` | `resources/*.json`, `diffs/**` | unknown `name=value` tokens also redacted |
| probe / lifecycle `httpGet.httpHeaders[].value` | `resources/*.json`, `diffs/**` | every value redacted unless the header name is in `redaction.plaintext` |
| annotations, except the `*.kubernetes.io/*` and `*.k8s.io/*` domains | `resources/*.json`, `diffs/**` | every value redacted unless the key is in `redaction.plaintext`; **the Kubernetes-domain exemption still applies** |
| event `message` | `events.json`, `timeline.json` | tightened, still best-effort — arbitrary prose can hide a secret in a shape no rule matches, and `redact_text`'s own doc comment says so |
| ConfigMap `data` values, and always the whole of `binaryData` | `diffs/**`, only with `diffs.configMaps: true` | multi-line values redacted whole unless the key is in `redaction.plaintext` |

`kubectl.kubernetes.io/last-applied-configuration` is **dropped outright** rather than redacted, in
every mode but `off`, and the drop is recorded in `redaction.json`.

`redaction.mode: off` redacts nothing at all. It is recorded in `redaction.json` and
`lapilli verify` warns about it.

### Never redacted, by design

No mode changes any of these. `strict` does not reach them: it widens *which values under a name*
are redacted, not *which fields are visited*.

| What | Where it lands | Why it is there |
|---|---|---|
| **`metadata.labels`** | `resources/*.json` (pod, ReplicaSet, Deployment/StatefulSet/DaemonSet) | The string `labels` does not appear anywhere in `crates/lapilli-bundle/src/redact.rs`. Labels routinely carry `owner`, `team`, `cost-center`, and in real clusters sometimes an email address or a personal username. |
| **`metadata.managedFields`** | `resources/*.json` in full; `changes.json` as `{manager, operation, subresource, time}` per entry (`managed_field_actors` in `collector.rs`) | A field-manager name is a person or service identity (`kubectl-client-side-apply`, `argocd-controller`, `alice@example.com` on a cluster where kubectl is run with a per-user manager). `diffs/index.json`'s `actor` is the same string. It is client-asserted, not authenticated — a hint, not proof. |
| **`status.podIP`, `status.podIPs`, `status.hostIP`, `status.hostIPs`** | `resources/pod.json` | The whole Pod object is serialized and written (`Redactor::write_object`). Pod and node addresses are internal network topology, and in some regimes an IP is personal data. |
| **`spec.nodeName`, `spec.nodeSelector`, `spec.tolerations`, `spec.affinity`** | `resources/pod.json` | Same reason. Node names often encode account, region, zone and instance id. |
| **`spec.serviceAccountName`, `spec.imagePullSecrets[].name`, `env[].valueFrom` references** | `resources/*.json` | Identity and Secret/ConfigMap *names* (not their values) are part of the object and stay readable. |
| **Image references and digests** | `resources/*.json`, `changes.json`, `manifest.json` (`producer.image_digest`) | A private registry host and repository path name your organization and often your internal project names. |
| **`events.json`** beyond `message` | `events.json` | Only `message` passes through redaction (`collect_events`); the rest of each `Event` object is written verbatim. That includes `source.host` (the node), `involvedObject`, `reportingComponent`, `reportingInstance`, and the Event's own `metadata` and `managedFields`. |
| **Every log line** | `logs/<container>-current.log`, `logs/<container>-previous.log` | **Not redacted in any mode**, stated as such in the spec's redaction table and in `redact.rs`'s module note: "Container logs are **not** redacted: they are the evidence." Application logs carry user emails, user ids, session tokens, request bodies and stack traces containing all three, as a matter of course. If one category on this page decides your classification, it is this one. A tail is bounded at 4 MiB per container instance, and because the kubelet spends that budget forward from the start of the window, a cut tail is missing its *newest* lines — recorded as `truncated: {limit_bytes, bytes, cut}` in `logs/index.json`, and a `cut: "newest"` file MUST NOT be read as the container's last words. |
| **`metrics/index.json`** | `metrics/index.json` | Holds `prometheus_url` and the **rendered** PromQL of every query, i.e. the namespace and pod name substituted into the label matchers. |
| **`metrics/<name>.json`** | `metrics/*.json` | The raw Prometheus `query_range` response, kept verbatim so any tool can re-plot it. Its series labels carry namespace, pod, container, and usually node and image. |
| **`manifest.json`'s `incident`** | `manifest.json` | `{id, cluster_id, trigger: {rule, firing_ts}, window, target: {namespace, pod}}`. `incident.target` exists so a lookup by what an alert carries never has to unpack a bundle, which means it is **the field every reader parses** — and it is the namespace and pod in plain text. |
| **`redaction.json`'s `plaintext_names`** | `redaction.json` | The names you exempted in `redaction.plaintext` are recorded, so the bundle says which values were deliberately left readable. |

### Also on the bundle volume, outside any bundle

These sit beside the `.ieb` files under the bundle root. They are not part of the hash tree, they
are not signed, and `lapilli verify` says nothing about them — but they hold data, and an erasure
request covers them.

| File | Holds |
|---|---|
| `<incident>.summary.json` | The notification's source: container, termination reason and exit code, restart count, the change's kind/name/revisions/actor and its field path with **before/after values**, memory peak against limit. **No log line** — it is written through `Summary::without_log_line()`. |
| `<incident>.unsent` | A notification hand-off a shutdown flush failed to POST. A serialized group: cluster, namespace, the owning workload, **every member pod's name**, and every member's `Summary`. No log line. This is the one file under the bundle root that carries workload content, which is why it is age-limited rather than kept forever (see below). |
| `<incident>.notified` | A claim. Empty, or the seven bytes `history`. No incident content. |
| `<incident>.ieb.owner` | A claim. The `IncidentCapture` CR's uid, and nothing else. |
| `reclaimed.jsonl` | One line per reclaimed file: incident id, file name, bytes, the bundle's `sha256`, the export states the decision rested on, the reason, and the instant. No namespace, no pod, no workload content. |
| `keys/<key_id>.pub` | Archived signing **public** keys. No private material. |
| `.staging-<incident>-<uid>/`, `.<incident>-<uid>.ieb.tmp` | Work in flight: the same content as a bundle, **uncompressed**. Reclaimed once the capture is no longer live. |

## What `redaction.mode: strict` buys, and what it does not

`strict` changes one thing: inside the fields redaction already visits, every candidate value is
replaced with `<redacted>` unless its name is listed in `redaction.plaintext`. So it removes the
"did a rule match?" question for env values, probe headers, annotation values and multi-line
ConfigMap values, and it tightens unknown `name=value` tokens in argv and event messages.

It does **not**:

- touch any row in *Never redacted, by design*, above — not labels, not IPs, not `nodeName`, not
  `serviceAccountName`, not `managedFields`, not image references, and **not one line of any log**;
- make event messages safe. `redact_text` stays best-effort in `strict`, deliberately: prose can
  hide a secret in a shape no rule matches;
- redact `*.kubernetes.io/*` or `*.k8s.io/*` annotations. `redact_meta` skips those keys before the
  mode is consulted, because revision numbers and `restartedAt` are what a reader needs to follow a
  rollout;
- reach anything outside `metadata.annotations` and the pod spec's containers. `strict` is a wider
  net in the same water, not a larger pond.

It also costs you readability: `strict` redacts `CACHE_WARMUP=eager` along with the password, so the
one-line config change that caused the incident may arrive as `<redacted> → <redacted>`. Use
`redaction.plaintext` to name the values you need back.

Both tables above have a machine-readable form travelling inside every bundle: `redaction.json`'s
`not_redacted` lists the whole trees the policy never visits (the path prefixes), and
`not_redacted_fields` lists the fields that survive inside the files it does visit. They exist so a
tool does not have to infer from "this file was redacted" that any particular value in it was — which
is what `lapilli mcp` was doing until round 30. Both are advisory: `spec/IEB-SPEC.md` §7 fixes only
`mode`, a verifier checks neither, and anything reporting them must report them as what the capture
recorded rather than as something it verified.

## Where copies go

The bundle is the product, so every path out of the cluster is a copy of it. Each of these is off
unless you turn it on, except the first.

| Destination | On by default? | What leaves | Notes |
|---|---|---|---|
| **The PVC** (`persistence`) | yes | the whole bundle, plus every file in *Also on the bundle volume* | 1 GiB by default. This is inside the cluster, but it is a volume your platform backs up, snapshots and replicates like any other. |
| **Object store** (`export.destinations`: S3, S3-compatible, GCS) | no | the whole `.ieb`, byte for byte | Written at `<prefix>/<cluster_id>/<incident_id>.ieb` — the key itself carries the cluster id and a hash, **not** the namespace or pod. Conditional create, never overwritten. The archived signing public key goes to the same destination. |
| **A chat channel or webhook** (`notify.routes`) | no | the rendered summary, never the bundle | `detail: facts` (the default) sends counts, reasons, revisions and identities: namespace, pod names, the owning workload, the alert rule, the termination reason, restart counts, memory against limit, and the change's field manager. `detail: content` adds the changed field path and its (already-redacted) before/after values. **Never a log line, in any mode.** `lapilli demo` captures are excluded unless `notify.includeDemo: true`. |
| **An MCP client** (`mcp.enabled`, or `lapilli mcp` on a laptop) | no | on request: `verify`, `summary`, `postmortem`, and `read_file` over any path the verified hash tree names under `resources/*.json` and `diffs/**` | In practice the client is an AI client, so `resources/pod.json` — labels, IPs, `nodeName`, `serviceAccountName`, `managedFields` — goes into a model's context and wherever that provider keeps it. `logs/**` is served **only** with `mcp.allowLogs` / `--allow-logs` (off by default) and comes back flagged `untrusted: true`. See [`design-distribution-path.md`](design-distribution-path.md). |
| **`lapilli postmortem` output** | n/a | a Markdown document transcribing values from one bundle, each with the file it came from | Written to be pasted into a wiki, which is a broader and more permanent audience than a chat channel. The crashed container's last log line is off by default (`--include-log-line`). |
| **`lapilli demo --out <dir>`** | n/a | `<incident>.ieb` **and its unpacked directory** | `--out` defaults to `.`, so running it in a checkout drops both into your working tree. `.gitignore` covers them; a copy to a shared drive is on you. |

Outbound connections, and the fact that the chart ships no egress NetworkPolicy because
NetworkPolicy v1 cannot match a DNS name, are in [`egress.md`](egress.md).

## How long copies live

- **On the PVC: forever, by default.** `retention.maxBytes: 0` and `retention.days: 0`. Nothing
  reclaims a sealed bundle until you set a bound. When the volume fills, **every new capture
  fails** — so "keep forever" is not even a stable state.
- **With retention on, still not always.** A bundle is reclaimable only when **every referenced
  destination was observed `Uploaded`**. `Refused`, `Conflict` and `Failed` are refusals, on purpose:
  those mean the evidence never reached the destination. An install with **no** destinations — the
  default — therefore reclaims nothing but abandoned staging directories unless you also set
  `retention.allowUnexported: true`. Read [`design-retention.md`](design-retention.md) before you do.
- **`<incident>.notified`, `<incident>.ieb.owner`, `keys/`: never removed**, whatever the policy
  says. They are claims and key material, not sidecars; the next section says what deleting one
  does.
- **`<incident>.unsent`: 30 minutes past its last write**, then reclaimable like a bundle. Inside
  that window it is protected as hard as a claim, even on a full volume, because a message still
  going to be sent should not be destroyed to free bytes.
- **`reclaimed.jsonl`: never reclaimed.** It rotates once at 8 MiB to `reclaimed.jsonl.1`, so the
  oldest records are eventually overwritten by the rotation.
- **In an object store: Lapilli never deletes a remote object.** Lifecycle is entirely your
  bucket's. Under Object Lock it is deliberately undeletable for the lock period, which is the
  point of using it and also means an erasure request cannot be satisfied there until the lock
  expires. Decide that before you enable Object Lock, not after the first request arrives.
- **In a chat channel, a wiki, or a model's context: your provider's retention, not Lapilli's.**
  Lapilli cannot recall a posted message or an answered MCP call.

## Erasure: removing one incident

Suppose you must remove incident `<incident>` — say `kubernetes-1a2b3c4d5e6f7a8b`. The set is not
"everything with that prefix": two of those files are claims whose meaning outlives the evidence,
and deleting one of them causes a new, visible failure.

**Remove:**

- `<incident>.ieb` — the bundle.
- `<incident>.summary.json` — the notification's source.
- `<incident>.unsent`, if it exists — the only bundle-root file holding workload content.
- the remote object `<prefix>/<cluster_id>/<incident>.ieb` in **every** `export.destinations` entry,
  and, on a versioned bucket, its previous versions. Under Object Lock the store will refuse, and
  that refusal is the design working as intended.
- any unpacked copy anyone pulled: `lapilli demo` output, a `lapilli unpack` directory, a bundle
  attached to a ticket.
- the chat message and the postmortem document, in the tools that hold them.

**Leave in place:**

- **`<incident>.notified`.** It is a claim, not a sidecar. Deleting it re-arms notification: the
  claim check at the top of `enqueue_notification` stops returning early, the
  `created < started_at` history guard does not catch a capture newer than the current controller
  process, the 30-minute cooldown has long expired — and **a month-old incident is announced to
  Slack as news.** It is also non-convergent: a restart re-creates the file and the next sweep
  would delete it again, forever. It holds no incident content — it is empty, or the seven bytes `history`.
- **`<incident>.ieb.owner`.** It holds the `IncidentCapture` CR's uid and nothing else. Releasing it
  lets a resent webhook create a **new** bundle carrying the old incident's identity, so
  `lapilli verify --incident <incident>` would pass on bytes collected months later. That is the
  replay/substitution defence `DESIGN.md` §7 claims.
- **`reclaimed.jsonl`.** Its lines hold the incident id, the file name, a byte count, the bundle's
  `sha256`, export states, a reason and a timestamp. The incident id is itself
  `<cluster_id>-<16 hex of SHA-256(rule | cluster | namespace/pod | minute)>`, so it names no person
  and does not reverse to a namespace or pod without already knowing them. Keeping it is what lets a
  missing bundle years later be distinguished from a lost one.
- **`keys/<key_id>.pub`.** Public keys. On a local-only install a rotated key exists nowhere else,
  and deleting it makes every bundle it signed unverifiable — including bundles safely in an Object
  Lock bucket.

**The `IncidentCapture` CR** is a separate decision. It carries `spec.target` (the namespace and
pod) and `status`, so it is in scope for an erasure request — but nothing in Lapilli ever deletes a
CR, and deleting one turns its files into orphans. The orphan pass is off by default
(`retention.reclaimOrphans: false`) and refuses above 100 files or 5% of the population, so a bulk
`kubectl delete incidentcapture` will not quietly become a bulk bundle delete. Delete the files
first, then the CR.

### A manual deletion is not recorded

`reclaimed.jsonl` is written by the retention sweep and by nothing else. An `rm` on the volume, or a
`DeleteObject` in the bucket, leaves **no record anywhere** that the bundle existed.

That is in tension with `DESIGN.md` §11, which is explicit about why: "A delete feature in an
evidence tool is a destroy-evidence feature… deletion has to be at least as recorded as capture
is." Retention meets that bar. Hand deletion — which is the *only* erasure path the product offers
today — does not.

The shape that would fix it is one command, `lapilli erase <incident>`: remove exactly the set
above, leave exactly the claims above, reconcile each destination against what the store actually
permits, and append one journal line per file removed. That is a **roadmap item, not something you
can run today**, and until it exists the honest procedure is the list above plus a record you keep
yourself, outside Lapilli.

## See also

- [`../spec/IEB-SPEC.md`](../spec/IEB-SPEC.md) — the normative bundle layout and the redaction table.
- [`design-retention.md`](design-retention.md) — what is reclaimable by name, when the sweep
  refuses, and why the journal is the only durable record.
- [`design-notify.md`](design-notify.md) — what a notification may carry and why a log line never is.
- [`design-distribution-path.md`](design-distribution-path.md) — the MCP server's content policy.
- [`egress.md`](egress.md) — every outbound connection, and why the chart ships no egress policy.
- [`../DESIGN.md`](../DESIGN.md) §4 (redaction), §5 (the integrity model) and §11 (retention).
