# Design — Lapilli cases (`lapilli case`)

Status: **pre-alpha, unreleased.** Built 2026-10-06 from a prototype that ran one set of experiments,
and taken apart again the next day ([`design-review-round37.md`](design-review-round37.md), §9 for
the second pass). One author wrote the tool, the three cases, the grader and this page.

This is a second tool in the repository, not a feature of the recorder. The recorder (`DESIGN.md`)
seals what a cluster looked like when an alert fired, for a person to read later. This freezes an
incident **together with its answer**, so that an agent that investigates incidents can be given the
same incident again and again, with no cluster, and be graded on *how* it investigated. The two share
a name, a repository and a habit — seal it, then verify it offline — and no code. `DESIGN.md`'s
identity block has had a sentence for each of them, and one above both, since 2026-10-07:
*cases freeze an incident together with its answer key, replay it with no cluster, and grade how an
agent investigated — shipping no agent, no model and no judge.* This page is checked against that.

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

A case whose decisive evidence is not in the frozen copy measures nothing about an agent, and without
a check a broken case and a failed agent look the same. So `lapilli case freeze` looks for every
evidence item where it lives, records the result in `freeze.json`, and exits 2 when one is missing:

- an item in the Kubernetes store, in the files of the snapshot;
- an item in the metrics store, in what `promq` prints at the freeze — for the query the item names
  as its witness, or for every series when it names none.

A test repeats both for every case under `cases/` (`internal/replay/cases_test.go`), in the
`case-tool` CI job, which runs on pull requests and on pushes to `main` and is a required check.

What this establishes is that the evidence *exists* in the copy. That an agent's tools can reach a
Kubernetes item — through which command — was checked by hand for the three cases and is not a
command yet (ROADMAP §7, item 2).

## 3. Replay uses the real tools, and a clock

- **Kubernetes.** [`crust-gather`](https://github.com/crust-gather/crust-gather) `serve` is an API
  server over the snapshot; `kubectl get`, `describe`, `logs`, `events` and label selectors work.
  Borrowed whole, as an external binary. It ignores **field selectors** and **`logs --tail`**, and
  those are not left as differences: a front stands before it (`internal/replay/fields.go`). A list
  asked for with a field selector is fetched whole, filtered, and returned in the shape that was asked
  for, table or objects; a log asked for by its tail is cut to its last lines. Measured on a kind
  cluster, live against frozen: six selector questions gave the same lines, and `kubectl describe
  pod` gave 2,738 bytes and five events both times — where without the filter the frozen one lists
  every event in the namespace. **Its tables are a third thing it does not do as a cluster does**,
  found in round 38 and not yet repaired: no wide columns, some kinds printed as a name and an age,
  a sorted listing that can come back empty. That and the rest of what a replay does differently
  are in §8.
- **Metrics.** Prometheus's own PromQL engine, linked in, over the case's samples
  (`internal/metrics`). No emulation of the query language: six queries against the frozen store
  return the same digits, to the last one, as Prometheus 3.5.0's own storage layer and engine over
  the block the samples came from, at the instant a replay evaluates them, and a test pins them. The
  block and that reader are kept under `internal/metrics/testdata/reference/`, so the comparison can
  be run again.
- **The frozen clock.** An agent asks for "the last ten minutes". Against samples that end an hour
  ago the honest answer is nothing, which is not what the incident looked like.

  *What is mapped is the question, never the data.* A time after the freeze does not exist in the
  case, so a request that reaches past it is asking about a present the case does not have: it is
  moved back by the distance from the freeze to the caller's now, and "the last ten minutes" becomes
  the last ten minutes of the incident. A request that ends at or before the freeze names the
  incident's own time — an agent read `19:21:05` in a pod log and asks the metrics about 19:21:05 —
  and is taken as written. Every answer carries the incident's own timestamps, whichever way it was
  asked, and `time()` in a query is the freeze.

  The first version moved the answers forward to the caller's clock as well. Replayed a day later,
  the metrics then said "just now" and the pod logs beside them said "yesterday": an agent given both
  reported a metric timestamp at which, in the incident, nothing existed. The Kubernetes half of a
  case has always kept its timestamps; now all of a case's data has one time.

**Staleness markers are kept, and that decided the export path.** When a series stops being exposed,
Prometheus writes a marker — a NaN with one particular payload — and an instant query stops returning
the series at once instead of five minutes later. A range selector drops markers by definition, so an
export through `/api/v1/query` loses them, and JSON cannot carry a NaN at all. `freeze` therefore
reads the remote-read endpoint, which returns what is stored, and the file writes a marker as the
token `stale`. Measured twice: an export from a Prometheus 3.5.0 serving a block gave 15 series,
2,064 samples and 3 markers, the same bytes once decompressed as a dump through the storage layer;
and frozen from a Prometheus running inside a kind cluster, five queries at the freeze instant
returned the same value strings from the live server and from the frozen store.

**The engine is linked without the storage layer.** Prometheus's top-level `tsdb` package would read
a block from disk, and at v0.315.0 importing it compiles 697 packages, 160 of them from the AWS, Azure
and Google SDKs. `lapilli-kms` signs with two clouds' KMS and links neither SDK; the Go half holds the
same line by giving the engine an in-memory store instead (412 packages for the same engine, no cloud
SDK), and CI fails if one enters the graph. What the lean path does carry, since Prometheus v0.311:
79 packages of the Kubernetes client, pulled in by a logging helper `promql` imports. It is dead
weight, not a violated constraint.

## 4. What stands between the agent and the rest of the machine

The machine that runs an evaluation usually also holds credentials to real clusters; the one these
cases were built on has a production cluster as the current context of its default kubeconfig. And a
case is something one downloads from a stranger: a log line in it can be written to talk an agent
into something. Three things are done about that, and none of them is a sandbox.

**The kubectl guard** (`internal/guard`). `run` puts a `kubectl` first on the agent's `PATH` that
hands every invocation to the guard, which

- runs **read-only verbs only**: `get`, `describe`, `logs`, `events`, `top`, `explain`,
  `api-resources`, `api-versions`, `version`, `cluster-info`, `auth can-i|whoami`,
  `rollout status|history`, `config current-context|get-contexts`. No `delete`, `apply`, `exec`,
  `edit`, no `config set` or `use-context`, and no plugin — a plugin is somebody else's program;
- **refuses** flags that name another cluster or identity (`--kubeconfig`, `--context`, `--cluster`,
  `--server` and `-s` in any group of short flags, `--user`, `--token`, `--as…`, the certificate and
  TLS flags) and flags that read or write local files (`-f` outside `logs`, `-k`,
  `--output-directory`, `--cache-dir`), saying why, so the agent can go on;
- **pins** what it does run: the kubeconfig, context, cluster, server and user of the case are
  appended to the command line. kubectl takes the last value of a flag, so a spelling the refusals
  missed is overridden rather than obeyed. The refusals are for the agent's benefit; this is the
  control.

The first guard was a shell script that compared `KUBECONFIG` and matched a list of flags. Two
reviews found three ways through it — `kubectl get pods -As https://…`, `kubectl config use-context`,
and, in the prototype, `--kubeconfig` itself — none of which a recorded run had tried.

**A built environment** (`internal/agent`). The agent does not inherit the operator's environment.
It gets locale, terminal, time zone, proxy and certificate settings, what the case adds
(`KUBECONFIG`, `PATH`, `PROM_URL`), the variables the operator names with `--pass-env` — the key for
its model, usually — and a home directory that is **empty**. HolmesGPT has toolsets for cloud CLIs
and observability backends and switches them on when it finds their credentials; with nothing but
its model key, it has Kubernetes and the case's metrics. The `claude-code` adapter is the exception:
it gets the real home directory, because that is where its login lives.

**The agent's own limits.** The `claude-code` adapter allows the Bash tool only, and in it only
kubectl's read verbs, `promq` and text filters to pipe them through. That list is Claude Code's to
enforce. Observed once, with 2.1.291: the filters were refused on a file outside the working
directory (`head -1 /etc/hosts`, `grep -c … /etc/hosts`, `/usr/bin/head …`), so they are for pipes;
but that is its behaviour, not something this tool guarantees.

An agent that may run arbitrary programs can call the real `kubectl` by its path, and can read what
its user can read. `freeze`, `run --live` and the scenario scripts all require the kubeconfig to be
named and never read the default one. A way around the guard is a vulnerability (`SECURITY.md`).

## 5. One transcript shape, so graders never know which agent they are reading

```
{"agent", "model", "answer",
 "steps": [{"tool", "input", "output", "error"}],
 "usage": {"llm_calls", "tokens", "cost_usd", "seconds"},
 "error"}
```

An adapter runs an agent and returns this (`internal/agent`). `claude-code`, `holmes` and a generic
`command` adapter exist. Everything downstream reads only this shape.

"One shape" has to mean one *content*, and that took a real run to find out. A step's `output` is
what the agent's model was shown, as text. HolmesGPT records each tool result inside an envelope, and
the first adapter stored the envelope — the model's text JSON-encoded inside JSON, where
`"client":"x"` reads `\"client\":\"x\"`. The evidence pattern of one case, written for what a tool
prints, then failed to match for HolmesGPT and for no other agent: the same query result, graded
differently by agent. The adapter unwraps it now, and a test holds the shapes of that run.

A run that ends without an answer — the agent's limit, or its provider's outage — is recorded with
its `error` and counted as such in the report, not hidden among the failures. HolmesGPT writes its
record at the end, so a run of it that dies midway leaves no steps.

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

**Secrets.** `freeze` blanks the value of every Secret and removes the `last-applied-configuration`
annotation that repeats it. A Secret is recognised by what a file contains — a document of kind
Secret, alone, in a multi-document file, or as an item of a list — not by the directory it lies in,
because `pack` accepts a snapshot from any collector. The collector writes Secret values to a
temporary directory on the machine that freezes, and they are blanked there before anything is
packed: they do not enter the case, and they do leave the cluster.

**Nothing else is redacted.** Pod logs, environment values in pod specs and ConfigMaps are copied as
they are. The recorder's redaction engine (`docs/data-handling.md`) is not applied here, and a case
has no equivalent of the recorder's `redaction.json`.

**Node logs are off unless asked for.** crust-gather, left to its defaults, also reads each node's
kubelet journal, and it does so by starting a pod on every node with the host's process namespace and
root filesystem. Measured on a three-node kind cluster: fifteen new events in `default` and three
kubelet journals in the snapshot; with `--disable-additional-logs`, no pod, no event, no journal.
`freeze` passes that flag, so by default it reads the API and changes nothing; `freeze --node-logs`
turns the collection on. Evidence that lives on a node has to have been recorded by something already
running there — which is how such evidence exists in a real incident anyway.

The three cases in this repository were collected before that default existed. They are synthetic,
built from `scenarios/` on kind clusters that no longer exist, and each carries the kubelet journal
of its three nodes and, in `default`, the three collector pods and their events. An agent sees those
pods and events as part of the cluster.

A case frozen from a real cluster is that cluster's data: it needs a person to read it before it is
shared. The cases worth most are redacted real incidents, and that is unsolved here (§9).

`MANIFEST.json` is an integrity check, not a signature. It says the case is what was sealed, not who
sealed it. Unpacking refuses links, devices, paths that climb out, and an archive that unpacks to
more than a fixed number of entries or bytes.

## 8. Known differences between a replayed case and a live cluster

- **Tables are the snapshot server's, not a cluster's — found in round 38, not yet repaired.**
  Reproduced on a served case, and seen in 33 steps of 19 of that round's 36 frozen runs and in no
  live one ([`design-review-round38.md`](design-review-round38.md), R2):
  - `kubectl get … --sort-by=<path>` answers "No resources found" unless the path is under
    `metadata`. kubectl asks for rows that carry the whole object (`includeObject=Object`) and
    sorts by a path into it; the rows come back with metadata only. `--sort-by=.lastTimestamp` on
    events — how both agents asked for recent events — is the common case.
  - `-o wide` adds nothing for pods and Deployments: no IP, no NODE, no images. `-o
    custom-columns`, `-o jsonpath`, `-o yaml` and `describe` do carry them.
  - ReplicaSets, Endpoints and EndpointSlices list as `NAME AGE`.
  - One object asked for by name — `kubectl get pod <name>`, and a Deployment, a Service or a node
    alike — lists as `NAME AGE`.
  - The event table is headed `LASTTIMESTAMP` where a cluster prints `LAST SEEN`, and its `OBJECT`
    column holds the event's own name where a cluster prints `pod/<name>`.
- **Field selectors are done by a filter, not by the snapshot server**, and the filter is more
  permissive than a real API server: it accepts any dotted path into an object, where a live cluster
  knows a short list per resource and refuses the rest. A selector that works here and not live is
  possible; the reverse should not be. With a selector, `limit` is not honoured: the filtered list
  comes back whole. A watch is passed through unfiltered.
- **The recorded runs were made before that front existed.** Then, measured on a served case,
  `kubectl get events --field-selector involvedObject.name=<pod>` returned all 48 events of the
  namespace, of which 5 concerned the pod, and `--field-selector spec.nodeName=…` returned all 23
  pods. `kubectl describe` asks for its Events section that way, so a frozen `describe pod` in those
  runs averages 10.5 KB against 2.7 KB live, and the four steps the agent's harness truncated were
  all `describe`. No recorded frozen run passed `--field-selector` itself. `--tail` was ignored too:
  of the 49 frozen steps that asked for a tail, 3 were given more lines than they asked for.
- **kubectl computes ages from the wall clock.** The timestamps in a case stand still; the `AGE`
  column and "5m ago" in `describe` keep counting. A case replayed a week later shows pods a week
  old. Metrics answers carry the incident's own time (§3), and so do log lines and event timestamps.
- `kubectl logs --since` and `--since-time` are ignored: honouring them needs a time for every line,
  and a snapshot has only the text. `--tail` is honoured.
- `kubectl logs --previous` for a container with no previous instance, and a log request for a
  container that never started, return a generic error where a live cluster explains.
- `kubectl top` needs a metrics API the snapshot does not have.
- An action cannot be frozen: `kubectl exec`, a packet capture started now, a request sent to see what
  happens. A case whose only path to the answer is an action is not a case. (The guard refuses those
  verbs in both conditions.)
- Of the Prometheus HTTP API: `query`, `query_range`, `labels`, `label/<name>/values`, `series` (the
  last three honour `match[]` and ignore `start`/`end`) are served; `rules`, `alerts`, `targets`,
  `metadata` and `query_exemplars` answer empty; anything else is refused in the API's error shape.
- Native histograms are not carried; `freeze` refuses a series that has them. Start timestamps and
  exemplars are not carried.

## 9. Neutrality, and what is not built

A benchmark controlled by a party it grades is not trusted by the others, so it is not used, so it is
not a benchmark. Lapilli ships no agent, no model and no judge; the recorder's own position —
depending on no AI tool — is what lets it stand between the tools that are graded. The rules that
follow are written down rather than left as intentions: in `case-grading.md`, that a change to
grading is versioned and public and that a run record and a report state the version that produced
them; in `GOVERNANCE.md`, that nobody decides alone how their own agent is graded.

Open, in the order they threaten the idea:

- **Independence.** Every case, the grader and the rubric share one author. A case written by someone
  else is worth more than anything on this page.
- **Fidelity.** The differences in §8 are the ones that were looked for and found. The largest of
  the first set was found a day late, by a reviewer and not by the experiment built to find it. The
  comparison was then run again with the instrument repaired (round 38): outcomes were not
  distinguished at a resolution of 28 points, and the transcripts held a second set — the tables —
  that neither its rules nor its committed search could see. Twice now a difference has been found
  by someone reading for another reason. What replaces that is the same command run against a
  cluster and against its frozen copy, output compared, over the commands agents have actually
  typed; it does not exist yet (`ROADMAP.md` §7).
- **Contamination.** Public cases will be trained on. A held-out set needs someone other than the
  author to hold it.
- **Real incidents.** See §7.
- **The judge.** Blind is not the same as independent. Agreement between two judges has been
  measured once, between two models of two families: Cohen's κ 0.81 over the 36 packets on which a
  pass was possible, three disagreements, all on one statement of one key, the process check siding
  with the stricter judge each time (round 38). No person has judged.
- **What "retrieved" means.** A pattern is looked for in everything the agent's tools returned,
  whichever store it came from: one enormous dump earns the credit without the agent having
  localised anything, and output its harness truncated earns none for what was cut.
- **Remediation** is out of scope while a case is frozen. AIOpsLab, ITBench and coroot/rca-lab grade
  on live environments, which is what grading a fix requires; this trades that for a run that needs
  no cluster and repeats.
- **More stores.** A log store and traces are not frozen; pod logs from the API are.

## 10. Layout, and why two languages

| path | what |
|---|---|
| `cmd/lapilli-case/` | the CLI: `verify`, `seal`, `freeze`, `pack`, `export-metrics`, `serve`, `run`, `packets`, `report`, `promq` |
| `internal/casefile/` | the answer key, `freeze.json`, the manifest |
| `internal/freeze/` | Secret redaction, the solvability checks, packing |
| `internal/metrics/` | the sample file, the engine over it, the frozen clock, the remote-read export, `promq` |
| `internal/replay/` | bounded extraction, the field-selector filter, serving a case |
| `internal/guard/` | what the agent's `kubectl` is allowed to be |
| `internal/agent/` | the transcript shape, the built environment, the three adapters |
| `internal/grade/` | process checks, blind packets, the report |
| `cases/` | sealed cases |
| `scenarios/` | what rebuilds each incident on a kind cluster |
| `test/fixtures/case-runs/` | the 100 recorded runs: 28 from the first instrument, 72 from round 38 with the three cases frozen for it |
| `test/round38/` | what ran round 38 and computes its numbers |

What CI recomputes from those records, and so what cannot drift: every run's process grade, all 100.
For the first 18 judged: 3 of 9 passed in each condition; of the 9 runs that retrieved all decisive
evidence 6 passed, and of the 9 that did not, none. For round 38's 72: 5 of 18 passed in each
condition for one agent and none for the other; the judges passed 10 and 13 and disagreed on 3; of
the 15 runs that retrieved all decisive evidence 10 passed, and of the 57 that did not, none. Other
numbers on these pages — costs, steps, intervals, the per-case tables — can be read off the same
files with `lapilli case report` and the scripts under `test/round38/`, and are not asserted by a
test.

`lapilli case …` hands over to `lapilli-case` (`crates/lapilli-cli/src/main.rs`): the copy beside
`lapilli` first, then `PATH`, a fixed name and nothing else, with the arguments and the exit code
untouched.

The recorder stays Rust. This is Go because of what it has to link: every store a case freezes or
will freeze is written in Go, and for the one that matters today — the PromQL engine — no second
implementation to link from Rust was found. Round 37 §5 has the comparison, including the two
premises of the first recommendation (Rust) that turned out to be wrong. Mixed Go-and-Rust
repositories are ordinary in this ecosystem (`linkerd/linkerd2`,
`open-telemetry/opentelemetry-ebpf-profiler`).

`lapilli-case` is **not released**: `release.yml` does not build it, `THIRD-PARTY-LICENSES.md` does
not cover its dependencies, and it has no SBOM or provenance. Those gates come before a tag carries
it (ROADMAP §7). It builds and its tests pass on Linux and macOS; nothing about it has been tried on
Windows, and its guard needs `/bin/sh`.
