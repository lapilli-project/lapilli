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
| `evidence` | the decisive items. Each is a regular expression, or `{pattern, store}` with `store` one of `kubernetes` (default) or `metrics` |
| `metrics` | `true` when the case carries a metrics store |

`id`, `prompt`, `expected`, `must_not`, `specificity`, `decoys` and `evidence` are required. An
invalid pattern or an unknown store is an error when the case is loaded, not a failed run.

Patterns are [RE2](https://github.com/google/re2/wiki/Syntax), the syntax of Go's `regexp`: no
backreferences and no lookaround. `.` does not match a newline; write `[\s\S]` where it must.

## `metrics.jsonl.gz`

One JSON object per line, one line per series, gzip-compressed, sorted by label set:

```
{"labels":{"__name__":"up","job":"thumb-api"},"t":[1791227625164,1791227630168],"v":["1","stale"]}
```

- `t` is milliseconds since the epoch, ascending.
- `v` holds each value **as a string**, the shortest decimal that parses back to the same 64-bit
  float, so every sample survives bit for bit. `+Inf`, `-Inf` and `NaN` are written as Go writes
  them.
- `"stale"` is a staleness marker: the series stopped being exposed at that instant. Dropping it makes
  a series that had disappeared look alive for five more minutes
  ([`design-case.md`](design-case.md) §3).
- Float samples only. Native histograms, exemplars and start timestamps are not carried.

`lapilli case export-metrics --url <prometheus> -o metrics.jsonl.gz` writes one from a live
Prometheus over its remote-read endpoint (`/api/v1/read`, on by default; neither the admin API nor a
shell in the pod is needed). `--match` narrows it to selectors and `--window` sets how far back it
reaches.

One build of the tool writes the same samples to the same bytes every time. Across Go releases only
the *uncompressed* bytes are stable — the compressor changed between Go 1.25 and 1.27, and the same
store came out as 3,151 and 3,077 bytes. A sealed case is identified by the bytes that were sealed,
not by being reproducible from its source.

## `freeze.json`

| field | meaning |
|---|---|
| `freeze_time` | the instant the stores were read, in Unix seconds. A replayed case answers metrics queries as if this were now |
| `frozen_at` | the same, RFC 3339 |
| `secrets_redacted` | how many Secret objects had their values blanked or were removed |
| `evidence_in_snapshot` | per `kubernetes` evidence pattern, whether the frozen copy contains it |
| `stores` | `kubernetes`, and `metrics` when carried |
| `metrics` | when carried: series, samples, and the oldest and newest sample time |

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
2. **Solvable from what was frozen.** `freeze` checks every `kubernetes` evidence pattern against the
   snapshot and exits 2 when one is missing, and CI repeats the check for every case under `cases/`.
   Evidence in the metrics store is not checked automatically yet: check it by hand with
   `lapilli case serve` and say in the pull request how.
3. **Rebuildable.** `scenarios/<id>/` holds `setup.sh`, `teardown.sh` and the manifests, and brings the
   incident up on a fresh kind cluster ([`scenarios/README.md`](../scenarios/README.md)). `setup.sh`
   must itself wait for the symptom and fail if it does not form. A case nobody can re-freeze cannot
   be corrected.
4. **Written before it was run.** `expected`, `must_not` and `evidence` are committed before any agent
   is pointed at the case. An answer key adjusted after seeing answers is a description of one agent.
5. **Safe to publish.** Secret values are blanked at freeze time. **Logs, environment values and
   ConfigMaps are not touched** ([`design-case.md`](design-case.md) §7). A case frozen from a real
   cluster needs a person to read it first.

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
| `s1-shared-cache-exhaustion` | most connections come from one client | a NetworkPolicy; a recent rollout of the cache | server log, pod IPs, a ConfigMap | `78b6fe6f71010f54` |
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
