# Design — record the perishable, seal the rest on demand

Status: **proposal, round 1 applied.** Owner resolved a fork on 2026-09-23; the first draft
of this document was attacked by three critics and **five BLOCKERs survived**, one of which
reopened a defect round 17 had closed. This is the revision. Round-1 log:
`docs/design-review-round23.md`.

## Why this exists

A market review (vault `17 Reviews/Lapilli 시장 분석 — Review Round 1`) ran an incumbent
lens — *a team already running Grafana + Loki + Prometheus + kube-state-metrics + API audit
logs* — which refused the install and named an alternative:

> The single version of Lapilli I'd take is the opposite of the current architecture: a
> stateless `lapilli seal` binary I run on demand during a postmortem. Ship the format and
> the verifier; drop the recorder.

The owner chose neither pole. Dropping the recorder loses the one claim that survived the
whole review — the point-in-time **object body**; three critics independently tried to find
a later reconstruction path for env, args, limits and probes of the object that was
*running*, and none found one. Keeping the recorder as is keeps collecting what a retention
stack already holds.

## The split

One axis decides it: **can this be reconstructed later from something that already stores
it?**

| Collector (`ieb/v1` name) | Reconstructible later? | Phase |
|---|---|---|
| `resources` (object body) | **No** — overwritten by the next apply | **A — record now** |
| `changes` (`changes.json` + `diffs/`) | **No, on a clock** — `revisionHistoryLimit` default 10 | **A — record now** |
| `logs` | Only if a log shipper runs, and only inside *its* retention | B — seal later |
| `events` | Only if an event exporter runs; TTL 1h kills it either way | B — seal later |
| `metrics` | Prometheus is the store, inside *its* retention | B — seal later |

The perishable half is `resources` + **`changes`**. The first draft wrote `diffs`, which is
not a collector name — `collector.rs:146` would reject it as `unknown collector`, leaving it
out of `collectors_run` and making phase A **PARTIAL**, contradicting this document's own
claim. The collector is `changes` and rule 6 then also requires `changes.json`
(`spec/IEB-SPEC.md:311`, `manifest.rs:24`).

**The discriminator is the log shipper, not Prometheus.** The first draft framed the
fallback segment as "clusters with no retention stack", and an operator critic showed that
is confused: Grafana Alerting, `vmalert`, Thanos Ruler and the Mimir ruler all POST to
Alertmanager, so a trigger implies no particular retention at all. A 6h-local-retention
Prometheus with no log shipper is an ordinary small-cluster shape, and the chart already
expresses it (`charts/lapilli/templates/captureprofile.yaml:12` adds `metrics` only when
`metrics.prometheusUrl` is set).

## Phase A — capture (always on, small)

`collectors_intended = ["resources", "changes"]`. Both run. The bundle verifies **OK (0)**,
not PARTIAL, and that is correct on the letter of the spec: *"The bundle is PARTIAL if and
only if some name in `collectors_intended` is not in `collectors_run`. Nothing else decides
PARTIAL"* (`spec/IEB-SPEC.md:299`). PARTIAL would be a lie about a collector that never
failed. The minimal-collector shape is already exercised in CI — the independent Python
producer ships a `logs`-only bundle that verifies OK (`test/spec/build_from_spec.py:46`).

But **OK must stop meaning what it meant.** A consumer seeing exit 0 and `coverage=100%`
today believes a full capture succeeded. Three things close that, and none needs a bundle
format change:

- The manifest carries `deferred: ["logs", "events", "metrics"]`. An unknown manifest member
  is proven tolerated — `build_from_spec.py:52` ships `x_future_field` deliberately.
- `verify` emits a **notice** whenever `collectors_intended` omits any `ieb/v1` table
  collector: *"deferred by profile `<name>`: logs, events, metrics — this bundle is not a
  full capture."* This mirrors the notice `verify.rs:280-308` already carries for a
  collector that ran and returned nothing, whose own comment reads *"a reader who sees 100%
  will not assume the difference."* The identical reasoning applies to a bundle that
  intended nothing.
- `collectors_run` and `collectors_intended` join `verify-result/v1`. `COMPATIBILITY.md`
  makes that document **additive-only within v1**, so this is permitted outright.

`lapilli postmortem` prints the same fact in its header, and `lapilli_bundles_unsealed`
plus an alert rule make it visible without a human opening a file.

## Phase B — `lapilli seal` (on demand)

### It gets its own incident id

The first draft claimed *"two bundles sharing an incident id is therefore normal, and
`verify` must not read it as a forgery."* **That was the worst error in the draft.** All
three critics killed it, by different routes, and it reopens `docs/design-review-round17.md:31`
— a BLOCKER closed by the `O_EXCL` owner file of round 9 (N1) and the written-twice custody
rule of round 7 (A1):

> Deleting `.ieb.owner` frees the id claim, so a resent webhook creates a **new** bundle
> carrying the old incident's identity and `lapilli verify --incident X` passes on bytes
> collected months later, which is the replay/substitution defence `DESIGN.md` §7 claims.

Four separate places enforce one-bundle-per-id: the export key is
`<prefix>/<cluster_id>/<incident_id>.ieb` written with `PutMode::Create`
(`export.rs:183`, `:327`); `verify_cmd.rs:136-142` derives the expected incident *from the
key* and enforces it as bound context; `remote.rs:337-341` returns FAILED when a versioned
key was written more than once; and `retention.rs:222-230` reaps any `.ieb` whose stem is
not a live incident id.

So: **the sealed bundle is a separate artifact with its own id**, `<parent_id>-s1`, at its
own key. `verify --incident` keeps naming exactly one artifact and every invariant above
stays intact. The parent is carried as a reference, not by reusing the identity:

```json
"parent": { "incident_id": "…", "hash_tree_root": "…",
            "signing": { "key_id": "…" }, "signature": "…" }
```

**Sealed bundles are never written to the controller's bundle root**, because
`retention.rs:222-230` would reap them as orphans.

### It never holds a signing key

The draft never mentioned signing at all, and both available options break the trust root
`DESIGN.md:160-161` documents (*"Lapilli signs its own output, so the trust root is an
unmodified Lapilli controller"*). If `seal` signs, the key leaves the controller Secret and
the postmortem author can mint any bundle that passes `verify --key` — destroying the one
row in `spec/IEB-SPEC.md:421` that grants authenticity. If it does not sign, the only
*complete* bundle is permanently unsigned, and `spec/IEB-SPEC.md:376` applies to it:
*"Without `--key`, a bundle proves nothing against anyone who could write to it."* That
qualifier assumed the writer was the adversary; here the writer is the expected workflow.

Normative resolution: **`seal` MUST NOT hold a signing key.** The sealed bundle is always
`signing: null`. Its `parent` block carries the phase-A bundle's root, `key_id` **and
signature bytes**, so the signed-at-capture core remains the trust anchor and the backfill
sits explicitly outside it. A consumer who trusts only the controller's key can still
verify the core and read the backfill as unattested context — which is what it is.

### The backfill names must be load-bearing, and that is a spec amendment

The draft claimed `*-backfill` collector names need no spec change, citing *"collector names
not in this table have no requirement."* The citation is correct and the conclusion was
backwards. That rule cuts both ways: nothing rejects the names **and nothing checks them**.
`verify.rs:840-850` consults only `required_files()`, which returns `&[]` for unknown names
(`manifest.rs:26`), so `logs-backfill` in `collectors_run` obliges the producer to write
nothing while `verify` still reports `coverage=100%`. No reader looks either —
`summary.rs:114,144` and `postmortem.rs:392` read fixed paths only. **Legal but inert**, and
inert is worse than absent because it reads as coverage.

Two consequences, both admitted rather than denied:

1. **Rule 6's table gains rows** for `logs-backfill`, `events-backfill`, `metrics-backfill`,
   each requiring its own `index.json` carrying `source`, `retrieved_at` and the query
   issued. This *is* an amendment to `spec/IEB-SPEC.md:305-311`. `COMPATIBILITY.md` permits
   it: no bundle that already exists uses these names, so no existing bundle's verdict
   changes, and the commitment is that released majors stay *readable*.
2. `verify` emits a notice naming every collector in `collectors_run` that is not an
   original `ieb/v1` name: *"contains data retrieved after capture: logs-backfill (source
   …, retrieved_at …)."*

And the one file every human and every LLM consumer actually reads gets a normative rule:
**a backfilled record MUST NOT enter `timeline.json` without a per-entry `source` and
`retrieved_at`.** `merge_timeline` (`collector.rs:458-475`) appends whatever it is handed
and the existing `"source": "k8s-event"` tag is a producer convention with no rule behind
it — so without this, the timeline silently mixes contemporaneous and late records.

### It must rewrite `redaction.json`

`not_redacted` is a hardcoded `["logs/", "metrics/"]` (`collector.rs:89`,
`spec/IEB-SPEC.md:149`). A sealed bundle whose unredacted content lives in `logs-backfill/`
would declare a list that names two paths that may not exist and omits the two that do.
Worse, `spec/IEB-SPEC.md:124` makes event `message` redaction mandatory in `events.json`
**and `timeline.json`** — text phase A passes through `redactor.text()`
(`collector.rs:330-332`) and text pulled from Loki never touches, while `redaction.json`
still reads `mode: "default"`.

So `seal` MUST either run policy v1 over every backfilled record, list the backfill
directories in `not_redacted`, and write per-file counts under the new paths — or declare
`mode: "off"` so `verify`'s existing warning fires. **It may not inherit phase A's
redaction record.**

### Credentials: an in-cluster Job, not a laptop binary

Today's read path is a namespaced `Role`+`RoleBinding` per watched namespace
(`rbac.yaml:38-63`) and every read lands in the Kubernetes audit log the operator already
retains. A laptop `seal` replaces that with a human-held Loki token — in multi-tenant Loki a
tenant-scoped `X-Scope-OrgID` credential that reads *every* namespace for that tenant, whose
use appears in Loki's query log rather than the cluster audit trail, living in an engineer's
`~/.config`. A change-approval board rates that **worse** than what it just refused.

`seal` is therefore specified as an **in-cluster Job** reading a named Secret with a
namespace-scoped Loki tenant. A `--dry-run` mode that emits the exact LogQL/PromQL for a
human to run in Grafana is the zero-credential fallback.

## What actually changes in the install — and what does not

**The RBAC must shrink, or this design fails its own purpose.** The operator critic's
verdict on the draft was *would-install: **no***, because the compromise kept every cost
they refused and subtracted capability. `charts/lapilli/templates/rbac.yaml:2-4` grants
`pods`, `pods/log` and `events` **unconditionally**; the only conditional grant in the file
is `configmaps`. Phase A needs `pods get/list` and the `apps` resources. It does **not** need
`pods/log` or `events`.

- `$collectorRules` is derived from the profile's collector classes, and a chart test asserts
  that a perishable render contains **no `pods/log` rule and no `events` rule**. That single
  change is what the critic named as flipping their decision.
- `perms.rs` `Needs`/`checks()` is derived from the profile too. Today it has one
  collector-shaped field (`config_maps`, `:65-75`) and emits the `pod-logs` and `events`
  checks unconditionally (`:95-114`), so an operator who correctly drops `pods/log` gets a
  permanent false alarm every 600 s — the exact noise `:77-80` says the module exists to
  avoid.

**And the retirement clock does not move at all.** The draft claimed *"bundle size falls well
below the 19.4 KB × 62-day disk clock."* Wrong twice. That figure is **controller memory per
`IncidentCapture` CR** (`docs/design-trigger-and-load.md:214-222`), not bundle bytes, and the
CR is created in the webhook handler *before any collector runs* — so the perishable profile
creates the same ~120 CRs/day and reaches ~7,400 on the same day. Disk was never the
constraint: measured bundles are p50 6.2 KB against a default `persistence.size: 1Gi`.
Retirement and retention machinery stay in full.

## Open questions this design still does not answer

- **What happens to a phase-A bundle nobody seals?** The clocks were sized for evidence
  nobody revisits; this design expects a human to come back days later. `lapilli_bundles_unsealed`
  makes it visible but does not decide the retention policy.
- **A backfill that returns zero rows** — because Loki's retention window already closed —
  must record the queried window, not an empty index. Where does that live in the verdict?
- **Does the profile choice belong to the operator or to the alert?** A cluster can have both
  kinds of workload.
- **`capture_started` in phase B.** `verify` never reads `manifest.timing`, and nothing
  constrains it, so a day-late seal writes a `capture_to_seal_ms` of ~86,400,000 that is
  byte-identical to a controller that hung for a day. Phase B writes *its own* start; the
  phase-A→seal gap belongs in `parent`, not in `capture_to_seal_ms`.
