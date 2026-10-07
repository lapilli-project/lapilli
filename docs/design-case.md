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
  server over the snapshot. Borrowed whole, as an external binary — and not trusted whole: it
  accepts requests it does not implement and answers them well formed and wrong, and each time that
  was found it was found late. So a front stands before it (`internal/replay/fields.go`,
  `tables.go`, `printers.go`, `printers_more.go`) and answers itself what the snapshot server does
  not answer as a cluster does:
  - **a field selector**: the list is fetched whole and filtered, and so is a watch for objects
    that names what it watches, which is how `kubectl rollout status` asks;
  - **the order of a list**, which a cluster returns by key and the snapshot server in the order it
    read its files;
  - **what a log is asked by**: a snapshot keeps every line with the time the kubelet stamped on
    it, and the snapshot server gives those times back or drops them and reads none of them. So
    `--tail`, `--since`, `--since-time`, `--limit-bytes` and `--timestamps` are done here — "the
    last minute" counted back from the freeze — and where there is no log, why not is said in a
    cluster's words: no previous container, or what the container is waiting for. One kind of line
    has no time: what the kubelet says where it has no log to give. The snapshot server takes such
    a line's first word for a time and cuts it off, or, asked for times, gives it the moment it
    took the file up. No line of the cluster's was stamped after its case began to be served, so a
    time that late is the snapshot server's, and is taken off again;
  - **a table**, which is everything `kubectl get` prints: built from the objects the way the API
    server builds it, for the kinds in the two printer files — with the wide columns, with the whole
    object in each row when a sort asks for it, for one object asked for by name as for a list, in
    the order a cluster lists, and as the version of Kubernetes the case was frozen from wrote it
    (§8). A custom resource is printed from its definition: the columns of the version asked for,
    read with the JSONPath package the API server reads them with, and in any column but a string
    one a value printed only if it is of the column's type. The snapshot server prints them too — with a JSONPath of its own,
    a date as a date, and a custom resource whose kind is called `Service` as a Service;
  - **an object the snapshot server lists and cannot find by name** — every one with a colon in its
    name, so every `system:` role and binding — and, for what is really not there, a cluster's own
    words: `pods "x" not found`;
  - **a Secret that `freeze` blanked**, sent so that it decodes.

  How far that goes is measured, not argued: `test/replay-diff` builds a case on a kind cluster,
  asks the live cluster five to eight hundred commands — a fixed set about every kind the cluster
  has, and the `kubectl` reads the recorded agents typed — freezes it, asks the frozen copy the same, and
  asks the cluster again. The cases are the three scenarios and a fixture that is no incident: the
  kinds an investigation is likely to list that the scenarios lack, and a pod in each state it is
  likely to meet. On 2026-10-08, on Kubernetes v1.37: 2,469 commands, 2,440 answered the same, 12
  where the cluster itself moved between two askings, and 17 that differ, all of the three kinds §8
  lists. Of the 276 a recorded agent had typed, none differs. The same build swept v1.33 whole — 2,340 commands, the same 17 —
  and the fixture alone on v1.31, v1.32, v1.34, v1.35 and v1.36: 740 to 776 commands each, and
  nothing differing but those three kinds. Each cluster was asked by the `kubectl` of its own
  version; a client of another version than its server was asked of none. A command that differs
  without being excused by name fails the sweep.

  The same measure, one tool back each time: with the replay as it was when round 38 ran, 635 of
  1,577 commands over the three scenarios differed or were refused in other words, 45 of them among
  the 268 an agent had typed; with the replay repaired for those and the fixture then added,
  148 of the fixture's own 791 did. (Both counts include the three known kinds: 9 of the 635, 5 of
  the 148.)
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
79 packages of the Kubernetes client, pulled in by a logging helper `promql` imports. That was dead
weight, and is. Two more are now linked on purpose — `client-go/util/jsonpath`, which is what the
API server reads a custom resource's printer columns with, so that the front reads them the same
way (§3), and the copy of some of `text/template`'s helpers that it needs.

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
- makes **`rollout status` a read and not a wait**, by giving it `--watch=false`: it says where the
  rollout stands and returns. Left alone it waits for the rollout to finish, and the rollout an
  investigation asks about is the one that will not — live or frozen. The first agent allowed to
  type it was still there six minutes later;
- **refuses** flags that name another cluster or identity (`--kubeconfig`, `--context`, `--cluster`,
  `--server` and `-s` in any group of short flags, `--user`, `--token`, `--as…`, the certificate and
  TLS flags) and flags that read or write local files (`-f` outside `logs`, `-k`,
  `--output-directory`, `--cache-dir`), saying why, so the agent can go on;
- **refuses an output format that takes its template from a file** — `go-template-file`,
  `jsonpath-file`, `custom-columns-file`, in every spelling of `-o`. A template with nothing to
  fill in is printed as it stands, so `kubectl get ns -o go-template-file=<a file>` printed the file;
- **refuses any flag before the verb but kubectl's own** (`-n`, `--request-timeout`, `-v`), and any
  flag between a verb and the word that says what it does. kubectl takes the word after a flag it
  does not know yet for that flag's value: in `kubectl -l version delete pods` the command is
  `delete`, and in `kubectl rollout --field-manager history restart deploy/x` it is `restart`, while
  the first word of each is a read. The verb the guard reads has to be the verb kubectl runs;
- **refuses `get --raw`**: it asks for a path, and a path can be a proxy to a node, a pod or a
  service — a request sent to see what comes back, which is not a read and cannot be frozen (§8).
  One recorded live run had read a kubelet's metrics that way;
- **holds whatever stands where the verb stands to be a read** — an empty word too. `kubectl ""
  delete pod x` has nothing there, and was passed on with `delete` in it for as long as the check
  was made only of a verb that was not empty;
- **refuses what a run does not offer**: verbs that are reads and that whoever started the agent
  has named (`LAPILLI_KUBECTL_NOT_OFFERED`), found where `kubectl` finds a verb and not by matching
  the command's text. The Claude Code adapter withholds four that way (below);
- **pins** what it does run: the kubeconfig, context, cluster, server and user of the case are
  appended to the command line. kubectl takes the last value of a flag, so a spelling the refusals
  missed is overridden rather than obeyed. The refusals are for the agent's benefit; this is the
  control.

The first guard was a shell script that compared `KUBECONFIG` and matched a list of flags. Two
reviews found three ways through it — `kubectl get pods -As https://…`, `kubectl config use-context`,
and, in the prototype, `--kubeconfig` itself. A third review, of the change that made this guard's
list the source of what an agent's harness may run, found two more in the guard as rewritten: the
file-reading output formats and the flag before the verb. Both were confirmed against a served case
— a local file printed, a `delete` reached under a client-side dry run — and neither had been tried
by a recorded run. A fourth review, of the change that gave an agent's harness `kubectl` whole and
left the reading of it to this guard, found the empty word. Six ways through in four reviews is the
rate at which this kind of code is wrong; the pin, which does not depend on the refusals being
complete, is why the first three could not have reached another cluster, and it would not have
stopped the others. The sixth was not shown to do anything — `kubectl` most likely runs nothing
when its first word is empty, and the reviewer was not given the real one to try — but that would
be `kubectl`'s reading of an edge, and this is supposed to be a refusal.

**A built environment** (`internal/agent`). The agent does not inherit the operator's environment.
It gets locale, terminal, time zone, proxy and certificate settings, what the case adds
(`KUBECONFIG`, `PATH`, `PROM_URL`), the variables the operator names with `--pass-env` — the key for
its model, usually — and a home directory that is **empty**. HolmesGPT has toolsets for cloud CLIs
and observability backends and switches them on when it finds their credentials; with nothing but
its model key, it has Kubernetes and the case's metrics. The `claude-code` adapter is the exception:
it gets the real home directory, because that is where its login lives.

**The agent's own limits.** The `claude-code` adapter allows the Bash tool only, and in it only
`kubectl`, `promq` and text filters to pipe them through. `kubectl` whole: Claude Code matches a
command against its list as text, and a list of texts is not a reading of a command. Written by
hand, with five verbs, the list refused `kubectl rollout history` twelve times in round 38. Built
from the guard's verbs, it refused every `kubectl -n <namespace> get` in round 39, where an agent
that wrote the namespace first was turned away one to three times in eight runs of eighteen and
answered that it could not investigate. With a pattern for each place a namespace can stand,
it still refused `kubectl -nshop get pods`, and let through what it was meant to withhold. So what
a `kubectl` command is, the guard decides, which reads it as `kubectl` does; and every `kubectl`
the agent runs is the guard.

Four of the guard's verbs are withheld from this adapter all the same — `config`, `auth`,
`explain`, `cluster-info` — because they are about the client and the server and not about the
incident, and a frozen case answers them otherwise than a cluster does (§8): to offer them would
give the live condition answers the frozen one cannot give. The adapter names them in the agent's
environment and the guard refuses a command whose verb is one of them, wherever its namespace
stands. No way was found for an agent to take the name back: every line tried that would change
its environment begins with something other than `kubectl`, and Claude Code refused each. That
rests on Claude Code's behaviour, described next, and is not something this tool guarantees.

The rest of the list is Claude Code's to enforce, and Claude Code both refuses more than the list
says and runs more. More refused: a filter inside `-o custom-columns`, `[?(@.type=="Ready")]`, five
times in round 38; a `for` loop; one pipe into `sed` of the four in round 39. More run: **some commands it runs unasked,
whatever it is allowed** — `id`, `whoami`, `pwd`, `hostname`, `uname -a`, `date`, `which` and
`ps aux` all ran with 2.1.293 under a list that named none of them. So an agent run through this
adapter can learn whose machine it is on and see its process table, command lines and all. It was
refused a file outside the working directory (`cat`, `head`, `grep` on one, `ls ~`, `find ..`) and
the environment (`env`, `printenv`), and a redirection to a file. All of that is its behaviour,
observed, and not something this tool guarantees: where it matters, run the agent on a machine
that has nothing to show.

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

**Init containers' logs are fetched by `kubectl`.** The collector takes the log of each of a pod's
containers and of none of its init containers — and a pod stuck in `Init:CrashLoopBackOff` says why
nowhere else. After the collector has run, `freeze` reads the pods it took and fetches what is
missing with `kubectl logs --timestamps`, through the same kubeconfig: of every init and ephemeral
container that has run, and of its previous run if it had one. It is a read, and it needs `kubectl`
on the path only when there is such a container. A log the kubelet no longer has is left out and the
case is sealed without it — and not in silence: `freeze.json` counts the logs that were added and
names the ones that were asked for and did not come, and `freeze` warns of them. One failure is not
left to look like that: where the first `kubectl` on the path is the guard of a case being served
from the same shell, which lets through reads of that case and nothing else, `freeze` stops and
says so, rather than seal a case with no init container's log in it.

`MANIFEST.json` is an integrity check, not a signature. It says the case is what was sealed, not who
sealed it. Unpacking refuses links, devices, paths that climb out, and an archive that unpacks to
more than a fixed number of entries or bytes.

## 8. Known differences between a replayed case and a live cluster

Asked the same 2,469 commands over three scenarios and a fixture with the kinds they lack that an
investigation is likely to list, a cluster and its frozen copy differ on seventeen, of three kinds — `explain`, `cluster-info`,
`describe secret` — which are below and in `test/replay-diff/known.txt` (§3). The rest of this list
is what that comparison cannot see or did not ask: what depends on when a case is replayed, on
kinds and versions that were not swept, or on the agent.

- **A table is written as the cluster's own version of Kubernetes wrote it, between v1.31 and
  v1.37.** The front builds it for pods, Deployments, ReplicaSets, DaemonSets, StatefulSets,
  replication controllers, Jobs, CronJobs, Services, Endpoints, EndpointSlices, Ingresses and their
  classes, claims and volumes, autoscalers, quotas, limit ranges, events of either API group, nodes,
  and the kinds a cluster is made of (roles and bindings, service accounts, storage, priority and
  runtime classes, API services, custom resource definitions and the like). It writes them as v1.37
  does, and seven of them otherwise for an older cluster — six differences found by asking a
  cluster of that version, one read in Kubernetes' source and then asked: before v1.37 a custom resource definition was listed by name and date alone and
  the default storage class was marked wherever it was printed; before v1.36 a node's kernel had no
  architecture beside it and the default ingress class was not marked; before v1.35 a service
  account had a count of its Secrets; before v1.33 a quota's age stood before its amounts; before
  v1.32 a priority class had no preemption policy. **A cluster older than v1.31 was never asked**,
  and a case from one is printed as v1.31 printed; one that does not say its version, as v1.37.
- **A custom resource is printed as its definition says**, by the columns of the version asked for
  and with the JSONPath the API server reads them with, if the definition is in the case — it is,
  unless the case was packed from a snapshot that left definitions out. **Any other kind keeps the
  snapshot server's columns**, which may be a name and an age where a cluster prints more: what an
  aggregated API serves, and whatever Kubernetes has that is not in the list above.
- **An age in a table is counted to the freeze**, because a table is the server's answer and the
  case's server stopped then: a pod twelve minutes old at the freeze is twelve minutes old a month
  later. **`kubectl describe` and `kubectl events` count their own from the wall clock**, and
  nothing here can change that: the same case, a month later, describes that pod as a month old.
  The timestamps in a case stand still — log lines, event times and metrics answers carry the
  incident's own time (§3).
- **Seventeen kinds a v1.37 cluster can list have no object in any swept case**, and what a case
  prints for one of them was compared with no cluster: pod templates, volume attachments, CSI
  drivers, mutating webhook configurations, admission policies, resource claims and device classes
  among them. An empty listing of each was compared, and agrees.
- **Of an autoscaler's targets, only resource metrics were compared with a cluster.** A kind cluster
  has no metrics API, so every current value there is `<unknown>`; pod, object and external metrics
  follow the API server's code and no cluster's answer.
- **`kubectl explain`** fails: it reads the cluster's OpenAPI document, which a case does not carry.
- **`kubectl cluster-info`** prints the address of the server, and a case is served from another.
- **`kubectl describe secret`** counts the bytes of the marker `freeze` wrote, not of the value.
- **`kubectl auth can-i`** is answered yes, whatever is asked: a snapshot has no one to authorise.
  **`kubectl config current-context`** names the case's own context, which is not the cluster's.
- **A watch** for objects that names what it watches is answered with what is there, and then
  nothing, since nothing more happens in a case. Any other watch is the snapshot server's, which
  sends one object and ignores a selector: a watch of a whole list, and every watch that asks for a
  table, which is what `kubectl get -w` sends. `kubectl get -w` is not something to trust on a case.
- **Field selectors are done by a filter**, and the filter is more permissive than a real API
  server: it accepts any dotted path into an object, where a live cluster knows a short list per
  resource and refuses the rest. A selector that works here and not live is possible; the reverse
  should not be. With a selector, or for a table, `limit` is not honoured: the list comes back whole.
- **`kubectl logs --since` counts back from the freeze**, not from now: "the last five minutes" is
  the last five minutes before the case was frozen, whenever it is asked. It and `--since-time` and
  `--timestamps` go by the times the kubelet stamped on each line; `--tail` and `--limit-bytes`
  count lines and bytes. `logs -f` prints what there is and ends, since nothing more is written in
  a case.
- **A log is as long as it was when the collector read it**, which is some seconds after the instant
  the case calls its freeze: a line stamped in those seconds is in the case.
- **An init container's log is in a case only if `freeze` could fetch it** (§7), and in no case
  frozen before 2026-10-08. `freeze.json` names the ones it asked for and did not get.
- `kubectl logs --previous` for a container that has run only once is refused in a cluster's words,
  and so is a log asked of a container that has not started: `is waiting to start:` and the reason
  the kubelet gave. Two reasons are reworded as a cluster rewords them — an image that cannot be
  pulled — and any other is passed on as it stands.
- **A node whose clock ran ahead of the replaying machine's** by more than the time between freeze
  and replay could have the last lines of a log read as lines with no time on them: they would lose
  their time under `--timestamps` and escape `--since`. Not seen; it follows from how a line without
  a time is told from one with (§3).
- Of the warnings an API server sends beside an answer, one is replayed: that `v1 Endpoints` is
  deprecated, on a case frozen from v1.33 or later.
- `kubectl top` needs a metrics API the snapshot does not have.
- **An agent's own tools carry their own clocks.** HolmesGPT's log tool heads what it returns with
  the wall-clock time of the query, and one frozen answer of round 38 reports a log window that
  ends after the case was frozen. A case cannot freeze that.
- An action cannot be frozen: `kubectl exec`, a packet capture started now, a request sent to see what
  happens — `kubectl get --raw` on a path that is a proxy to a node or a pod among them. A case whose
  only path to the answer is an action is not a case. (The guard refuses those in both conditions.)
- Of the Prometheus HTTP API: `query`, `query_range`, `labels`, `label/<name>/values`, `series` (the
  last three honour `match[]` and ignore `start`/`end`) are served; `rules`, `alerts`, `targets`,
  `metadata` and `query_exemplars` answer empty; anything else is refused in the API's error shape.
- Native histograms are not carried; `freeze` refuses a series that has them. Start timestamps and
  exemplars are not carried.

**The recorded runs were made before most of this.** The 28 of 2026-10-06 before the front
existed: `kubectl describe pod` in their frozen condition averages 10.5 KB against 2.7 KB live, and
`--tail` was ignored. The 72 of round 38 with the selector and the tail repaired and the tables not:
a sorted event list came back empty, a pod listing asked for `-o wide` had no IP and no NODE,
ReplicaSets, Endpoints and a pod asked for by name listed as `NAME AGE`, and `kubectl get all`
printed bare names — at least 33 steps in 19 of that round's 36 frozen runs
([`design-review-round38.md`](design-review-round38.md), *Outside the rule*). The 39 of round 39
are the only ones made on the replay as it is: none shows any of those, and the 233 `kubectl`
reads they typed were put to a cluster as well, none differing — the first time through a reading
of their command lines that was wrong in 22 places, and then again
([`design-review-round39.md`](design-review-round39.md)). What they asked of `promq`, 66 times,
was compared with nothing (`ROADMAP.md` §7, 1e).

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
- **Fidelity.** Twice a difference between a replayed case and a cluster was found late, by
  someone reading for another reason: field selectors and `--tail` by a reviewer reading code, a
  day after the first experiment had compared live with frozen and seen nothing (round 37 §9); the
  tables by reading transcripts, after the second had (round 38). What replaced reading is
  `test/replay-diff` — the same commands asked of a cluster and of its frozen copy, output compared —
  and on its first runs it found five more that no one had read their way to: a `rollout status`
  that reported another Deployment's rollout, objects with a colon in their name that could be
  listed and not fetched, a blanked Secret that `describe` could not decode, a node's roles left
  blank, a list that could come back in another order the second time it was asked. It is as good
  as what it asks. It asks about the kinds
  three small scenarios have, through `kubectl`, within a minute of the freeze; §8 says what that
  leaves out.
- **Contamination.** Public cases will be trained on. A held-out set needs someone other than the
  author to hold it.
- **Real incidents.** See §7.
- **The judge.** Blind is not the same as independent. Agreement between two judges has been
  measured once, between two models of two families: Cohen's κ 0.81 over the 36 packets on which a
  pass was possible, three verdicts apart, all on one statement of one key, the process check siding
  with the stricter judge each time (round 38). The two were not instructed word for word alike, so
  the difference is not the models' alone. No person has judged.
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
| `internal/freeze/` | Secret redaction, the solvability checks, the logs the collector leaves out, packing |
| `internal/metrics/` | the sample file, the engine over it, the frozen clock, the remote-read export, `promq` |
| `internal/replay/` | bounded extraction, serving a case, and the front that answers what the snapshot server does not: field selectors, a log read by its times, tables — of the kinds §8 lists and of the ones a cluster defines for itself |
| `internal/guard/` | what the agent's `kubectl` is allowed to be |
| `internal/agent/` | the transcript shape, the built environment, the three adapters |
| `internal/grade/` | process checks, blind packets, the report |
| `cases/` | sealed cases |
| `scenarios/` | what rebuilds each incident on a kind cluster |
| `test/fixtures/case-runs/` | the 139 recorded runs: 28 from the first instrument, 72 from round 38 with the three cases frozen for it, 39 from round 39 on those same frozen cases |
| `test/round38/` | what ran round 38 and computes its numbers |
| `test/replay-diff/` | the same commands asked of a cluster and of its frozen copy, output compared |

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
