# Design — Lapilli cases (`lapilli case`)

Status: **pre-alpha, unreleased, on a branch.** Built 2026-10-06 from a prototype that ran one set
of experiments ([`design-review-round37.md`](design-review-round37.md)). One author wrote the tool,
the three cases, the grader and this page.

This is a second tool in the repository, not a feature of the recorder. The recorder (`DESIGN.md`)
seals what a cluster looked like when an alert fired, for a person to read later. This freezes an
incident **together with its answer**, so that an agent that investigates incidents can be given the
same incident again and again, with no cluster, and be graded on *how* it investigated. The two share
a name, a repository and a habit — seal it, then verify it offline — and no code. `DESIGN.md`'s
identity sentence does not describe this tool and was not edited for it; that is the maintainer's
decision and round 37 §7 holds the drafts.

## 1. The unit is a case

A case is three things sealed together ([`case-format.md`](case-format.md)):

1. **Frozen stores.** The Kubernetes API as it was — objects, events, pod logs
   (`kubernetes.tar.gz`) — and, when the incident needs it, the metrics as they were
   (`metrics.jsonl.gz`).
2. **An answer key written before any agent ran** (`case.yaml`): the question, the statements a
   correct answer conveys (`expected`), what makes an answer wrong (`must_not`), the narrowing fact
   (`specificity`), the planted wrong causes (`decoys`), and the decisive evidence (`evidence`).
3. **A manifest** (`MANIFEST.json`) with a digest per file, so a case edited after it was sealed is
   detectable, and `lapilli case run` refuses it.

The answer key has the shape of an investigation rather than of an answer, on purpose. An incident
review that goes well runs: notice, collect, find what is specific to the failing population, form a
hypothesis that names the evidence it needs, check it — and usually discard the first one.
`specificity`, `decoys` and `evidence` are those steps written down, which is what makes the process
gradable at all. The structure follows a talk by the Toss Securities SRE team (*서버 개발자가 서비스
에러 원인을 탐지하는 방법*, Toss Challengers, 2026-10-05).

## 2. A case is solvable or it is not, and that is checked before any agent runs

`lapilli case freeze` searches the snapshot for every Kubernetes-store evidence pattern, records the
result in `freeze.json`, and exits 2 when one is missing. A case whose decisive evidence is not in the
frozen copy measures nothing about an agent, and without the check a broken case and a failed agent
look the same.

The check is repeated on every push: a test extracts every case under `cases/` and looks for its
evidence again (`internal/replay/cases_test.go`). Evidence that lives in the metrics store is not
checked mechanically yet (ROADMAP §7, item 1).

## 3. Replay uses the real tools, and a clock

- **Kubernetes.** [`crust-gather`](https://github.com/crust-gather/crust-gather) `serve` is an API
  server over the snapshot; `kubectl get`, `describe`, `logs`, `events`, label and field selectors
  work. Borrowed whole, as an external binary.
- **Metrics.** Prometheus's own PromQL engine, linked in, over the case's samples
  (`internal/metrics`). No emulation of the query language: six queries against the frozen store
  return the same digits, to the last one, as Prometheus 3.5.0 reading the block the samples came
  from, and a test pins them (the block and the reference reader are kept under
  `internal/metrics/testdata/reference/`).
- **The frozen clock.** An agent asks for "the last ten minutes". Against samples that end an hour
  ago the honest answer is nothing, which is not what the incident looked like. Every request is
  evaluated as if now were the moment of the freeze, and the timestamps in the answer are moved
  forward again to the caller's clock. The clock is frozen for metrics only (§8).

**Staleness markers are kept, and that decided the export path.** When a series stops being exposed,
Prometheus writes a marker — a NaN with one particular payload — and an instant query stops returning
the series at once instead of five minutes later. A range selector drops markers by definition, so an
export through `/api/v1/query` loses them, and JSON cannot carry a NaN at all. `freeze` therefore
reads the remote-read endpoint, which returns what is stored, and the file writes a marker as the
token `stale`. Measured: an export from Prometheus 3.5.0 gave 15 series, 2,064 samples and 3 markers,
byte for byte what the TSDB reader gives for the same block.

**The engine is linked without the storage layer.** Prometheus's top-level `tsdb` package would read
a block from disk, and at v0.315.0 importing it compiles 697 packages, 160 of them from the AWS, Azure
and Google SDKs. `lapilli-kms` signs with three clouds' KMS and links none of their SDKs; the Go half
holds the same line by giving the engine an in-memory store instead (412 packages for the same
engine, no cloud SDK), and CI fails if one enters the graph. What the lean path does carry, since
Prometheus v0.311: 79 packages of the Kubernetes client, pulled in by a logging helper `promql`
imports. It is dead weight, not a violated constraint.

## 4. The guard

The machine that runs an evaluation usually also holds credentials to real clusters; the one these
cases were built on has a production cluster as the current context of its default kubeconfig. So
`run` puts a `kubectl` wrapper first on the agent's `PATH`. It runs the real `kubectl` only when
`KUBECONFIG` is exactly the file the run was given, and refuses `--kubeconfig`, `--context`,
`--cluster`, `--server`/`-s`, `--user`, `--token`, `--as` and `--as-group` — an agent that was allowed
`kubectl get *` must not be able to read another cluster by adding a flag. (The prototype compared
`KUBECONFIG` only; the flags were added in the port.) The agent starts in an empty directory, a time
limit kills its whole process group, and the `claude-code` adapter allows only
`kubectl get|describe|logs|events|top`, `promq` and text filters.

It is a guard, not a sandbox: an agent that may run arbitrary programs can call the real `kubectl` by
its path. `freeze`, `run --live` and the scenario scripts all require the kubeconfig to be named and
never read the default one. A way around the guard is a vulnerability (`SECURITY.md`).

## 5. One transcript shape, so graders never know which agent they are reading

```
{"agent", "model", "answer",
 "steps": [{"tool", "input", "output", "error"}],
 "usage": {"llm_calls", "tokens", "cost_usd", "seconds"}}
```

An adapter runs an agent and returns this (`internal/agent`). `claude-code`, `holmes` and a generic
`command` adapter exist. Everything downstream reads only this shape.

## 6. Grading is two things ([`case-grading.md`](case-grading.md))

**Process** is deterministic and model-free. From the transcript alone: for each decisive evidence
pattern, did any tool *return* it; which pod names and addresses does the answer cite that no tool
returned; is the narrowing fact named; which decoys are mentioned.

**Outcome** needs reading, and no reader is embedded. `packets` writes shuffled packets (question,
expected, must_not, answer) with the condition and the run hidden, and a separate key. A judge — a
person, a model, several — returns verdicts; `report` joins them back.

Why they are kept apart: in the 18 judged runs, no run that failed to retrieve the decisive evidence
passed (0 of 9), and three of the nine that did retrieve it still failed. The process check is
necessary and not sufficient, and it is the half that costs nothing and cannot be argued with.

There is no single score. Two numbers that mean different things are not improved by adding them.

## 7. What a case contains, and what that means for sharing one

`freeze` blanks the value of every Secret and removes the `last-applied-configuration` annotation
that repeats it; a Secret file it cannot parse is deleted rather than kept. **Nothing else is
redacted.** Pod logs, environment values in pod specs and ConfigMaps are copied as they are. The
recorder's redaction engine (`docs/data-handling.md`) is not applied here, and a case has no
equivalent of `redaction_report.json`.

So the three cases in this repository are synthetic, built from `scenarios/`, and a case frozen from
a real cluster is that cluster's data: it needs a person to read it before it is shared. The cases
worth most are redacted real incidents, and that is unsolved here (§9).

`MANIFEST.json` is an integrity check, not a signature. It says the case is what was sealed, not who
sealed it.

## 8. Known differences between a replayed case and a live cluster

- `kubectl logs --tail` and `--since` are ignored by the snapshot API; the whole captured log comes
  back. Four frozen steps in the recorded runs were long enough for the agent's harness to truncate.
- `kubectl logs --previous` for a container with no previous instance, and a log request for a
  container that never started, return a generic error where a live cluster explains.
- `kubectl top` needs a metrics API the snapshot does not have.
- An action cannot be frozen: `kubectl exec`, a packet capture started now, a request sent to see what
  happens. A case whose only path to the answer is an action is not a case.
- Native histograms are not carried; `freeze` refuses a series that has them.
- Start timestamps and exemplars are not carried.

## 9. Neutrality, and what is not built

A benchmark controlled by a party it grades is not trusted by the others, so it is not used, so it is
not a benchmark. Lapilli ships no agent, no model and no judge; the recorder's own position —
depending on no AI tool — is what lets it stand between the tools that are graded. Two rules follow,
and they are written in `case-grading.md` rather than left as intentions: a change to grading is
versioned and public, and results state the version that produced them.

Open, in the order they threaten the idea:

- **Independence.** Every case, the grader and the rubric share one author. A case written by someone
  else is worth more than anything on this page.
- **Contamination.** Public cases will be trained on. A held-out set needs someone other than the
  author to hold it.
- **Real incidents.** See §7.
- **The judge.** Blind is not the same as independent, and agreement between judges has not been
  measured.
- **Remediation** is out of scope while a case is frozen. AIOpsLab, ITBench and coroot/rca-lab grade
  on live environments, which is what grading a fix requires; this trades that for a run that needs
  no cluster and repeats.
- **More stores.** A log store and traces are not frozen; pod logs from the API are.

## 10. Layout, and why two languages

| path | what |
|---|---|
| `cmd/lapilli-case/` | the CLI: `verify`, `seal`, `freeze`, `pack`, `export-metrics`, `serve`, `run`, `packets`, `report`, `promq` |
| `internal/casefile/` | the answer key, `freeze.json`, the manifest |
| `internal/freeze/` | Secret redaction, the solvability check, deterministic packing |
| `internal/metrics/` | the sample file, the engine over it, the frozen clock, the remote-read export, `promq` |
| `internal/replay/` | safe extraction, the guard, serving a case |
| `internal/agent/` | the transcript shape and the three adapters |
| `internal/grade/` | process checks, blind packets, the report |
| `cases/` | sealed cases |
| `scenarios/` | what rebuilds each incident on a kind cluster |
| `test/fixtures/case-runs/` | the recorded runs every number on these pages is recomputed from |

`lapilli case …` hands over to `lapilli-case` (`crates/lapilli-cli/src/main.rs`): the copy beside
`lapilli` first, then `PATH`, a fixed name and nothing else, with the arguments and the exit code
untouched.

The recorder stays Rust. This is Go because of what it has to link: every store a case freezes or
will freeze is written in Go, and the one that matters today — the PromQL engine — has no second
implementation to link from Rust. Round 37 §5 has the comparison, including the two premises of the
first recommendation (Rust) that turned out to be wrong. Mixed Go-and-Rust repositories are ordinary
in this ecosystem (`linkerd/linkerd2`, `open-telemetry/opentelemetry-ebpf-profiler`).

`lapilli-case` is **not released**: `release.yml` does not build it, `THIRD-PARTY-LICENSES.md` does
not cover its dependencies, and it has no SBOM or provenance. Those gates come before a tag carries
it (ROADMAP §7).
