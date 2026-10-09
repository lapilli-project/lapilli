# The case format (`lapilli.dev/case/v0`)

Version 0, pre-alpha: it will change, and when it does the format string in the manifest moves. This
is not the Incident Evidence Bundle (`spec/IEB-SPEC.md`) and carries none of its compatibility
commitments.

A case is a directory:

```
cases/<id>/
  case.yaml            the answer key
  kubernetes.tar.gz    a crust-gather snapshot of the Kubernetes API: objects, events, pod logs
  metrics.jsonl.gz     optional: the samples of a Prometheus, as stored
  metrics-metadata.json   optional, beside the samples: what kind of metric each is, its help and its unit
  freeze.json          when it was frozen, which stores, what was redacted, whether the evidence was found
  MANIFEST.json        a SHA-256 per file and a digest over the list
```

## `case.yaml`

| field | meaning |
|---|---|
| `id` | stable identifier; the directory name |
| `prompt` | the question, as an on-call engineer would ask it. It may contain the bait ("after today's rollout") |
| `expected` | statements a correct answer conveys. Each is graded separately. Put the specifics in: a value, a path, a name |
| `must_not` | what makes an answer wrong: a decoy named as the cause, or as the fix |
| `specificity` | the narrowing fact — the one population the problem is confined to |
| `decoys` | the plausible wrong causes planted in the scene |
| `evidence` | the decisive items. Each is a regular expression, or `{pattern, store, query}`: `store` is `kubernetes` (default) or `metrics`; `query`, for a metrics item, is the PromQL whose `promq` output the pattern must match at the freeze — the witness that the evidence can be reached |
| `metrics` | `true` when the case carries a metrics store |

`id`, `prompt`, `expected`, `must_not`, `specificity`, `decoys` and `evidence` are required. An
invalid pattern, an unknown store, a `query` on a Kubernetes item, or metrics evidence in a case that
says `metrics: false` is an error when the case is loaded, not a failed run.

Patterns are [RE2](https://github.com/google/re2/wiki/Syntax), the syntax of Go's `regexp`: no
backreferences and no lookaround. `.` does not match a newline; write `[\s\S]` where it must. `^` and
`$` are the beginning and end of the whole text, not of a line, unless the pattern starts with `(?m)`.

A pattern is matched against what an agent's tools *returned*, as text, whichever tool it was
([`case-grading.md`](case-grading.md)). Write it for what the tools print — `client="x"`,
`client=x` and `"client":"x"` are three tools' ways of saying one thing — and not so loosely that an
unrelated line satisfies it.

## `metrics.jsonl.gz`

One JSON object per line, one line per series, gzip-compressed:

```
{"labels":{"__name__":"up","job":"thumb-api"},"t":[1791227625164,1791227630168],"v":["1","stale"]}
```

- `t` is milliseconds since the epoch, ascending.
- `v` holds each value **as a string**, the shortest decimal that parses back to the same 64-bit
  float, so every number survives bit for bit. `+Inf`, `-Inf` and `NaN` are written as Go writes
  them; a NaN comes back as the one ordinary NaN, whatever payload it had.
- `"stale"` is a staleness marker: the series stopped being exposed at that instant. Dropping it makes
  a series that had disappeared look alive for five more minutes
  ([`design-case.md`](design-case.md) §3).
- Float samples only. Native histograms, exemplars and start timestamps are not carried.
- **The order of the lines is kept, and is part of the case.** Where `freeze.json` says
  `series_order: head`, it is the order the Prometheus's head held the series in, which is the
  order its engine is handed them in by a query that reaches no block — and what an engine does
  among equal series, such as which of them `topk` keeps, follows that order
  ([`design-case.md`](design-case.md) §3). Series the head did not hold, which ended before the
  blocks did, come after the others, by label. A query that does reach a block is handed its
  series by label, by a Prometheus and by a replay: `freeze.json` says where the blocks ended
  (`head_from_ms`). Where it does not say `series_order`, the file is in the order it was read in,
  which is by label if the reading reached a block, and one selector after another if `freeze`
  was given several; a file written before 2026-10-08 has its lines by label. The same series twice is refused.
- **It reaches back further than the window that was asked for**, by as much as an instant looks back
  for a sample: five minutes, unless the Prometheus was set otherwise. An instant at the window's
  beginning finds its sample just before the beginning, and a rate over five minutes there needs
  the five minutes; with them, an instant anywhere in the window, and a range no longer than that
  at its beginning, are answered as the Prometheus answered them. A query that looks further back
  still — a rate over ten minutes at the window's beginning — is not. `freeze.json` says where
  the file begins (`from_ms`).
- **The series are named as the Prometheus's own queries name them.** What it adds to every series
  it sends elsewhere — its external labels — a remote read returns and `freeze` takes off again
  (`freeze.json`: `external_labels`). A series that has a label of that name and value of its own
  keeps it: `freeze` asks the Prometheus which of its series do, and where it is not answered
  takes the label off every series that has it.

`lapilli case export-metrics --url <prometheus> -o metrics.jsonl.gz` writes one from a live
Prometheus over its remote-read endpoint (`/api/v1/read`; neither the admin API nor a shell in the
pod is needed). `--match` narrows it to selectors and `--window` sets how far back it reaches. A file
that decompresses to more than 2 GiB is refused when read: the store is held in memory.

## `metrics-metadata.json`

What the Prometheus knew of each metric family when the case was frozen — what `/api/v1/metadata`
answered — and what a replay answers to the same request:

```
{"thumb_requests_total": [{"type": "counter", "help": "Requests served.", "unit": ""}]}
```

A family has an entry for each thing its targets said of it, and they need not agree: two targets
with other words of help are two entries. The families are written by name and a family's entries
by type, help and unit, so the same metadata is the same bytes. A Prometheus sends them in the
order of a map, another each time, and under a `limit` keeps whichever came first; a replay keeps
the first by name, and of one family the first in the order of the file.

The file is there when the Prometheus could be asked and knew of at least one family, and
`freeze.json` then says how many (`metadata_families`): a replay reads the file when it says so,
and a case that says so without a file that can be read is not served. A case frozen before
`freeze` asked for this (it has since 2026-10-08), or made by `pack`, has none, and a replay of it
answers that it knows of no metric's kind — which is what round 38's agent was told five times.

One build of the tool writes the same store to the same bytes every time. Across Go releases only
the *uncompressed* bytes are stable — the compressor changed between Go 1.25 and 1.27, and the same
store came out as 3,151 and 3,077 bytes. A sealed case is identified by the bytes that were sealed,
not by being reproducible from its source.

## `freeze.json`

| field | meaning |
|---|---|
| `freeze_time` | the instant of the freeze, in Unix seconds: named first, the cluster is collected from then on, and the metrics are read up to it. A replayed case answers metrics queries as if this were now |
| `frozen_at` | the same, RFC 3339 |
| `secrets_redacted` | how many Secret objects had their values blanked or were removed |
| `evidence_in_snapshot` | per evidence pattern, whether the frozen copy contains it: a Kubernetes item in some file of the snapshot, a metrics item in what its query prints at the freeze |
| `stores` | `kubernetes`, and `metrics` when carried |
| `metrics` | when carried: series, samples, and the oldest and newest sample time; and, from a `freeze` since 2026-10-08, what the Prometheus said of itself: `prometheus_version`, and `evaluation_interval_ms`, its global evaluation interval, which is the step of a subquery that names none; and `lookback_delta_ms`, how far before an instant a sample still counts. Without the second and the third a replay uses Prometheus's defaults, one minute and five. And, since a later change of the same day: `from_ms`, the instant the metrics were read from — the window asked for, and before it what an instant looks back — which is where the case's metrics begin, whatever its oldest sample is; `head_from_ms`, where the Prometheus's blocks ended, if it had any; `series_order`, which is `head` when the metrics file is in the order the Prometheus's head had its series — `freeze` was given one selector, the Prometheus said where its blocks end, or that it has none, and its head could be listed — and is absent when the file is as it was read; `external_labels`, what the Prometheus adds to every series it sends elsewhere, which were taken off; and `metadata_families`, how many metric families `metrics-metadata.json` describes |
| `logs_added`, `logs_missing` | how many logs `freeze` fetched itself because the collector leaves them out, and the ones it asked for and did not get |

A replay evaluates "now" at `freeze_time` rounded to the millisecond, which is how Prometheus reads a
request's time. A query that looks further back than `from_ms` is answered from the nothing the case
holds there, and the replay says so among the warnings beside the answer; without `from_ms` it
says nothing, since a case's oldest sample is not where it begins when its Prometheus was younger
than the window. And a query that looks further back than `head_from_ms` is handed its series by
label, where one that does not is handed them in the order of the file; without it, always in the
order of the file.

## `MANIFEST.json`

```
{"format": "lapilli.dev/case/v0",
 "files": {"case.yaml": "<sha256>", "freeze.json": "<sha256>", "kubernetes.tar.gz": "<sha256>"},
 "digest": "<sha256>"}
```

`digest` is the SHA-256 of the file list in `sha256sum` form, sorted by path, so it can be recomputed
with nothing but a shell:

```
cd cases/s2-periodic-saturation
shasum -a 256 case.yaml freeze.json kubernetes.tar.gz metrics.jsonl.gz | shasum -a 256
```

`lapilli case seal <case>` writes the manifest; `lapilli case verify <case>...` reports altered,
missing and unlisted files and exits 1; `lapilli case run` refuses a case that does not verify. The
manifest is an integrity check: it says the case is what was sealed, not who sealed it.

## What a case has to be

1. **Hard for a reason you can state.** A case with no decoy and no narrowing fact is a lookup. Ten
   such scenarios were answered correctly 20 times out of 20 by a current model; they separate nothing.
2. **Solvable from what was frozen.** `freeze` looks for every evidence item where it lives — the
   snapshot's files, or the metrics as a query prints them — and exits 2 when one is missing; the
   `case-tool` CI job repeats both for every case under `cases/`. That shows the evidence exists, not
   that a command reaches it: for a Kubernetes item, serve the case, reach it with `kubectl`, and say
   in the pull request how.
3. **Rebuildable.** `scenarios/<id>/` holds `setup.sh`, `teardown.sh` and the manifests, and brings the
   incident up on a fresh kind cluster ([`scenarios/README.md`](../scenarios/README.md)). `setup.sh`
   must itself wait for the symptom and fail if it does not form. A case nobody can re-freeze cannot
   be corrected.
4. **Written before it was run.** `expected`, `must_not` and `evidence` are committed before any agent
   is pointed at the case. An answer key adjusted after seeing answers is a description of one agent.
5. **Safe to publish.** Secret values are blanked at freeze time. **Logs, environment values and
   ConfigMaps are not touched**, and with `--node-logs` each node's kubelet journal is in there too
   ([`design-case.md`](design-case.md) §7). A case frozen from a real cluster needs a person to read
   it first.

## Evidence that is not in the Kubernetes API

The incidents worth freezing are usually decided by something the API does not hold: a kernel table,
a node-local file, a trend. Two ways have been exercised:

- **Through an agent that already records it.** In `s3-node-local-drift` a DaemonSet prints node-local
  facts; the decisive value reaches the case as that pod's log. This is how such evidence exists in
  practice — an incident cannot be predicted, so the collection has to have been running.
- **Through the metrics store.** In `s2-periodic-saturation` the per-caller request count exists only
  in Prometheus.

What cannot be frozen is an action: `kubectl exec`, a packet capture started now, a request sent to
see what happens. A case whose only path to the answer is an action is not a case.

## The three cases here

| case | the narrowing fact | the decoy | where the decisive evidence lives | manifest digest |
|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | most connections come from one client | a NetworkPolicy; a recent rollout of the cache | the server's log, a Deployment's environment, the client's code in a ConfigMap | `78b6fe6f71010f54` |
| `s2-periodic-saturation` | bursts every two minutes from one caller | a recent rollout | **the metrics store** | `401599f4f44cad09` |
| `s3-node-local-drift` | every failing pod is on one node | today's rollout | **a node-local file**, seen only through a node agent's log | `16520131a78d93fc` |

All three were packed with `lapilli case pack` from the snapshot directories the recorded frozen runs
under `test/fixtures/case-runs/` had been served from. They were packed **after** those runs, and
packing is where Secret values are blanked: the runs were served the one Secret each cluster had (a
kubeadm bootstrap token of a kind cluster that no longer exists) with its values, and no recorded
transcript touches it. For `s1` and `s2` the unpacked archive was compared file by file with the
served directory and differs in that Secret and nothing else; the `s3` directory was not kept, so for
`s3` this rests on the procedure having been the same. `case.yaml` is the file the runs were asked
from, unchanged.

The metrics of `s2` are the same samples in a different container: the prototype sealed a TSDB block,
which only Prometheus's storage layer can open (`internal/metrics/testdata/reference/`).

All three were collected with crust-gather's defaults, before `freeze` turned node logs off: each
holds the kubelet journal of its three kind nodes and, in the `default` namespace, the three pods the
collector started to read them, with their events ([`design-case.md`](design-case.md) §7).
