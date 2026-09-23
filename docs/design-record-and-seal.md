# Design — record the perishable, seal the rest on demand

Status: **proposal, pre-loop.** Owner resolved the fork on 2026-09-23; this document
reifies the resolution so it can be attacked before any code moves.

## Why this exists

A market review (vault `17 Reviews/Lapilli 시장 분석 — Review Round 1`) ran two lenses
against the project's own positioning. The incumbent lens — *a team already running
Grafana + Loki + Prometheus + kube-state-metrics + API audit logs* — refused the install
and named an alternative:

> The single version of Lapilli I'd take is the opposite of the current architecture: a
> stateless `lapilli seal` binary I run on demand during a postmortem. Ship the format and
> the verifier; drop the recorder.

That is a fork, not a finding, so it went to the owner. The owner chose **neither pole**:

- **Drop the recorder entirely** loses the one claim that survived the whole review — the
  point-in-time **object body**. `kube_pod_owner` gives the owner *graph*; the audit log
  gives the *submitted* object; nothing gives env, args, limits, probes and annotations of
  the object that was actually running. It cannot be pulled later because the next change
  overwrites it.
- **Keep the recorder as is** keeps collecting logs, events and metrics that a retention
  stack already holds, which is exactly what the incumbent refused to pay for — in pod
  count, RBAC surface, and the 19.4 KB × 62-day disk clock that round 22 had to fix.

## The split

One axis decides everything: **can this be reconstructed later from something else that
already stores it?**

| Collector | Reconstructible later? | Phase |
|---|---|---|
| `resources` (object body) | **No** — overwritten by the next apply | **A — record now** |
| `diffs` (revision history) | **No, on a clock** — `revisionHistoryLimit` default 10 | **A — record now** |
| `logs` | Yes, if a log shipper runs (Promtail tails `/var/log/pods/…/<restart>.log`) | B — seal later |
| `events` | Yes, if an event exporter runs; otherwise TTL 1h kills it either way | B — seal later |
| `metrics` | Yes — Prometheus is the store; Lapilli only re-queries it | B — seal later |

Two phases follow:

**Phase A — capture (always on, small).** On alert, write only the perishable core.
Everything else is declared *deferred*, not *failed*.

**Phase B — `lapilli seal` (on demand, stateless).** At postmortem time, pull the
reconstructible parts from wherever they are kept, merge them with the phase-A core, and
seal one complete `.ieb`.

## What this does to the frozen format

`ieb/v1` is frozen. This design must fit inside it or it is dead. Three claims:

1. **Phase A is a valid, complete bundle.** `collectors_intended` lists only `resources`
   and `diffs`; both run; the bundle verifies **OK (0)**, not PARTIAL. That is honest —
   it collected everything it intended to. A bundle that nobody ever seals is still
   evidence on its own, which is a hard requirement: a design whose output is useless
   until a human runs a second command has just moved the failure to the human.

2. **Phase B must not launder late data as contemporaneous.** Logs pulled from Loki a day
   later are not the bytes the kubelet held at firing time. Writing them into
   `logs/index.json` would make the bundle assert a provenance it does not have. Instead
   phase B writes **separate collector names** — `logs-backfill`, `events-backfill`,
   `metrics-backfill` — each with its own index carrying `source` (the system queried),
   `retrieved_at`, and the query used. The spec permits this: *"collector names not in
   this table have no requirement"*, and readers MUST ignore fields they don't know. No
   spec change, no version bump.

3. **Sealing produces a new bundle, not a mutation.** The phase-A bundle is immutable and
   its hash tree stays valid. Phase B emits a second file that carries the same
   `incident.id` and records the phase-A bundle's hash-tree root as its parent. Two
   bundles sharing an incident id is therefore normal, and `verify` must not read it as a
   forgery.

## What changes in the controller

- Collectors gain a declared class (`perishable` / `deferrable`) rather than a hardcoded
  list, so the split is one property per collector instead of a branch in `collect_all`.
- The default capture profile intends only the perishable set. The current always-collect
  behaviour stays reachable as a profile for **clusters with no retention stack** — the
  one segment where the original thesis was true, and the segment the review could not
  size.
- Bundle size falls well below the 19.4 KB that drove round 22's retirement clock. The
  62-day math in `design-capture-retirement.md` needs re-deriving, not deleting.

## Open questions this design does not answer

- **Where does `seal` get its endpoints?** A postmortem-time binary needs Loki and
  Prometheus URLs and credentials. `lapilli postmortem` already talks to a cluster; `seal`
  may need a config file, which is a new surface.
- **What if the phase-A bundle is gone?** Retirement and retention now bound something a
  human is expected to come back to days later. The clocks were sized for evidence nobody
  revisits.
- **Does the profile choice belong to the operator or to the alert?** A cluster can have
  both kinds of workload.
- **Does `seal` need the cluster at all?** If not, it is a pure client and the RBAC
  argument gets much stronger. If it does (to resolve namespaces, say), the incumbent's
  objection partly survives.
