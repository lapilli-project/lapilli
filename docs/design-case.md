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

  *What is mapped is the question, never the data.* A case ends at the freeze, and a time after it
  does not exist in the case. A request that ends at or before the freeze names the incident's own
  time — an agent read `19:21:05` in a pod log and asks the metrics about 19:21:05 — and is taken
  as written. **One that reaches past the end is moved back, whole, so that it ends there**: a
  window keeps its length and its step and begins that length before the freeze, and an instant is
  answered at the freeze. "The last ten minutes" is the last ten minutes of the incident; 09:18:00
  to 09:43:00, asked of a case frozen at 09:42:36, is the twenty-five minutes up to 09:42:36.

  Nothing else decides it: not how long ago the case was frozen, not where the window begins. The
  server has no clock of its own. So what a request is answered with, and what is said beside the
  answer, go by the request and the case, and the same request sent a month later is answered the
  same; and **no step of a request is evaluated past the freeze**. The engine does not know that a
  case ends. Asked about the minute after, it carries the last sample forward for as long as it
  looks back and lets a `rate` run out of samples, and the answer is a traffic that fell to
  nothing after the freeze, which nobody measured. (A query can reach there itself, with a
  negative offset or an `@` later than the freeze, as it can past the now of a Prometheus. It is
  then evaluated there, and what it returns can be stamped there.)

  **What that costs.** A clock time and "ten minutes ago" are the same number, and no rule tells
  them apart; this one does not try. So:

  - A request that means *some time ago* is not answered as that. While it still lies before the
    freeze it is taken as written: "ten minutes ago", to a replay seven minutes old, is three
    minutes before the freeze, and the hour up to a now ninety seconds stale, to a replay a second
    old, stops eighty-nine seconds short of the freeze. Nothing is said of either, since nothing
    can tell them from a time read in a log. Once it lies past the freeze it is the end of the
    case: "five minutes ago", to a replay older than that, is the freeze, and the window from
    twenty minutes ago to ten, to a replay older than ten, is the last ten minutes of the
    incident. (In a query, `offset 5m` says "five minutes before", and is answered as it says.)
  - A clock that runs behind the freeze asks about the incident's own time, as far as this can
    tell: its now lies before the end, and is taken as written.
  - A window is moved by where it ends, not by where its last step falls. One whose steps all lie
    inside the case and whose end does not — 18:43:18 to 19:42:18 in steps of half an hour, of a
    case frozen at 19:25:54 — is moved all the same, and its two points are sixteen minutes
    earlier than the two it named.
  - A window of the incident's own clock that lies wholly after the freeze — the half hour from
    10:00, of the case frozen at 09:42:36 — is answered with the half hour before the freeze, of
    a time the case holds nothing of.

  It says what it did, every time it moves a request. The answer carries a line among the `infos`,
  where a Prometheus sends its own remarks: `frozen case: it ends at 2026-10-07T09:42:36.177Z; the
  window asked for ends 23.823s after that, and was moved back by that much, whole: each point is
  that much earlier than the one asked for — name an end at or before the case's to be answered
  about a window as it is written`. It cannot know that a caller meant "now" by the time it named.
  A caller that did knows it: `promq`, asked about no time, means now, and prints nothing of this;
  given a time with `--at`, it prints the line under the answer. HolmesGPT's own tool, it seems,
  does not pass it on: each of the 34 outputs of its two query tools that round 38 recorded
  carries the `data` of an answer and the request as the model made it, and nothing else of what
  the API sent — which is the shape of the records, and no run of that agent on this rule. An
  agent on that tool can see only that the timestamps it was given are not the ones it asked for.

  This is the fifth rule. The second, the third and the fourth were each written to put right the
  one before, as the comparison below was reviewed:

  1. *Moved back by the age of the replay*, whatever it asked (until 2026-10-08). A run recorded
     in round 38 asked for 09:18:00 to 09:43:00 seven minutes after its case was frozen and was
     answered up to 09:35:44, the last seven minutes of the incident missing from what it was
     shown; a day later it would have been answered with nothing at all. And "now" landed some
     milliseconds before the freeze, a different few each time, so that the same window of `rate`
     asked for twice was two numbers from the fourth or fifth digit on.
  2. *Taken as written when it began before the freeze.* Evaluated past it, and the traffic fell
     away.
  3. *Cut at the freeze, and told for the incident's time or the caller's present by which of the
     two its end lay nearer.* The incident's own hour, running ten minutes past the freeze, was
     cut there by a replay more than twenty minutes old and moved back by a younger one: one
     request, two answers.
  4. *Three readings — about now, the incident's time overshot, a present the case does not have —
     told apart by where the window began.* The hour up to a now ninety seconds stale — three of
     the four requests round 38 recorded past a freeze ended a minute or two behind the server's
     now — was cut at the freeze by a replay young enough for the hour to begin before the freeze,
     and by an older one moved back to end ninety seconds short of it, the burst under way at the
     freeze gone from the answer; and a caller 2.1 seconds behind the server was answered with
     another window than one 2.0 seconds behind.

  Each of the four went by the server's clock to tell what a request meant. The fifth has no
  clock.

  An answer carries the incident's own timestamps, whichever way it was asked, and `time()` in a
  query is not past the freeze — unless the query reaches forward itself.

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

**What `promq` prints, and what the API sends, are compared with the Prometheus as well.** Those six
queries and those five were what somebody thought to ask. `test/replay-diff/promdiff.py` asks the
rest, on the one scenario that has a Prometheus: a fixed set about every metric it holds — its
samples as they are stored, what an instant picks, the functions an investigation reaches for, each
as a window too, and the queries that fail — and every `promq` command the recorded agents typed.
A metric's value is not something two askings can be held to, since no two share a now. An instant
is. So each query is put about the instant of the freeze, by name — `promq --at`, which is also how
a time read in a pod log is asked about, and which the prompt an agent is given does not mention
yet, the recorded rounds having run without it — to the Prometheus, to the frozen store, and to
the Prometheus again, and the three answers have to be the same text. The frozen store
is then asked once more as an agent asks it, naming no instant, and has to say what it said by name.
And because not every agent reads through `promq` — HolmesGPT has a Prometheus tool of its own —
the same requests are made of the HTTP API, with those a client finds its way about by, and the
answers compared as they were sent.

The first time, on 2026-10-08: **of 546 queries, 42 answered otherwise and 12 were refused in other
words, 4 of the 96 an agent had typed among them; and 55 the frozen store itself answered two ways.**
Four reasons, and each is repaired:

- *The order of series*: 39 of the 42. PromQL leaves the order of an instant vector open, and an
  engine goes by the order its store hands it the series in. A Prometheus hands them over as its
  head created them, which is the order an application first exposed them in; the frozen store had
  sorted them by label. So 38 of the 46 instant answers of more than one series came out in another
  order, an aggregation's groups with them (the engine sorts a window's series itself, and none of
  those 47 did), and — the thirty-ninth, which changes an answer and not only how it looks —
  `topk` over equal series kept others than the Prometheus kept. A case now keeps its series in the order the
  Prometheus listed them at the freeze (`case-format.md`), and `promq` prints an instant vector in
  the order of its labels unless the query orders it itself, so that what an agent reads through
  it does not depend on which store answered: 60 series are printed, and they should be the same 60.
- *The `@` modifier was refused*: the other 3. The engine was built without saying it is on, as it
  has been in a Prometheus since 2.33 — and a negative offset with it, which was not asked.
- *A refusal was in this tool's words*: the 12. `1:5: parse error: …` where a Prometheus says
  `invalid parameter "query": 1:5: parse error: …`, and so for a step and for too many points. An
  agent corrects its query by those words.
- *Now was a few milliseconds before the freeze*: the 55 — the frozen clock, above.

More queries found more. A subquery without a step, `max_over_time(x[5m:])`, asked the second time
round, closed the connection: the engine asks its caller for the Prometheus's evaluation interval and
had no one to ask. `freeze` now asks the Prometheus, `freeze.json` carries the answer, and a panic on
the way to an answer is an answer. And when the API was asked beside `promq`: a selector that does
not parse, no selector at all and a time that is none were refused in other words than a
Prometheus's.

Three more were repaired from Prometheus's source before the API was first compared, and the
comparison did not show them: a time written `…354.000` where Prometheus writes `…354` — which the
comparison then read as one number, and no longer does; a value never written with an exponent,
of which the scenario has none small enough to need one; and what the engine remarks on beside an
answer, `metric might not be a counter`, which the store had never sent.

**Four reviews, each by a reader given the code and the sweeps' data and no account of either,
found what asking about the freeze by name cannot** — and the second, the third and the fourth
found it in the repair before. The first: the window that overshoots the freeze, above; that `limit`, `timeout` and
`lookback_delta`, which a Prometheus reads from a request, were read by nothing, and that how far
the Prometheus itself looked back (`--query.lookback-delta`) was not frozen with it — it is now,
beside the evaluation interval; that `freeze` read the metrics at the very instant it named, before
a scrape under way at that instant had committed, where it now reads them last and up to that
instant; and six things wrong with the comparison itself. The second, of the repair of the window:
taken as written, it was evaluated past the freeze, and three windows that round 38's runs had
asked for would have been answered with a traffic falling to nothing in the minutes after their
case was frozen — which is why no step is evaluated there. It also found the comparison passing any
reordering of what the API sends; six of the twenty-five windows the other agent's tool had asked
for, and its five questions about what kind of metric something is, dropped without a word; and
`limit` read for a query and not for names, values or series. The third, of the repair of that: a
request had been told for one thing or the other by whether its end lay nearer the freeze or now,
and so by the age of the replay: "five minutes ago" was the freeze to a replay between five and
ten minutes old, which round 38's were, and five minutes before it to an older one, and the
incident's own hour was moved back by any replay younger than twice its overshoot. It also found a known difference excused whatever status it came with,
and a log with a line moved from its start to its end let by as several logs in another order
(`test/replay-diff/README.md`). The fourth read the repair of that and nothing else — the three
readings, the fourth rule above — and found it going by the age of the replay still, for a window
that begins after the freeze: the hour to a stale now, and the two seconds; `promq --at`, some
seconds past the freeze, answering an instant with nothing and the minute before it with two
points; one instant, written to a tenth of a millisecond, past the freeze as RFC 3339 and at it
as seconds; the remark not reaching an agent on HolmesGPT's tool; two sentences of this section
saying the rule did not change with the age of the replay, which it did; and fourteen changes
one could make to the rule that no test noticed.

**A fifth review read the fifth rule, and it was wrong as first written too.** It took a request
back by how far past the freeze it was, and that distance runs out at 292 years: a time in
milliseconds sent for one in seconds, or the last day of the year 9999, was answered with
nothing, and with a remark that the nothing was the state at the end of the case. It still asked
the server's clock one thing — whether to say anything, nothing being said of a request that
ended within two seconds of now — so that one request had two bodies, and a `promq` two seconds
off the server was told to name times it had named none of. Twelve sentences — of this section,
of the change log, and beside the code and its tests — said more than the code did: that nothing
is evaluated past the freeze,
which a query that reaches forward is; that "five minutes ago" is the freeze, which it is only
to a replay older than that; that a now ninety seconds stale lost the end of its incident under
the rules before, which under this one it still does on a replay younger than its staleness.
And of seventy-nine changes it made to the rule and what is said, twenty-five no test noticed.
So a request that is moved is now placed from the freeze, and not taken back by a distance; the
server has no clock, the store says so whenever it moves a request, and `promq` prints that for
a time it was given; the costs above are written as the reviewer found them, which is what they
are; and a time in seconds is read fraction first, as a Prometheus reads one — multiplied whole
by a thousand, a time written to nine decimals came out a millisecond late about one time in
eight thousand. Forty-six
changes to the rule, the reading of a time, what is said and what `promq` prints are each caught
by a test. What it found that is not repaired is what the rule costs, and is written above as
that: a time that means "some time ago", a clock behind the freeze, a window moved though none of
its steps was past the end, and a query that reaches forward itself. No reader but their writer
has read these last repairs.

**Then: 636 queries, 635 answered the same and one refused in other words; of the frozen store's
636 answers to an agent's own asking, 636 what it says by name; and 725 requests to the API, 722
the same, two refused otherwise and one answered with nothing where a Prometheus has something to
say — what kind of metric one is.** Of the 636, 130 are ones a
recorded agent asked — `promq` commands, and the queries HolmesGPT's own tool sent, each as a
window of the length it asked for — and of those 86 were answered with something, one was refused,
and 43 were answered with nothing: 39 name a metric this Prometheus does not have, two a label it
does not have, and two divide one vector by another that shares no label with it. All 130 the same.
What the comparison does not reach is in §8.

**Then it was pointed at a Prometheus that holds more** (`test/replay-diff/prom`; `ROADMAP.md` §7,
1f). The scenario's Prometheus is eight minutes old when it is frozen, with one job, fifteen series,
no histogram, and all of it in its head: the questions asked of it were the ones it could answer.
The fixture's has a past — three hours of its targets written into blocks before it starts, the
same series it then scrapes into its head, and a target that stopped before the head began — three
targets and one that does not answer, rules, a histogram and a summary, values that are not finite
or want an exponent, labels that hold what a label may, a series that comes and goes, and itself
for one more target: a thousand series and more, under 367 names. **The first time,
of 18,129 queries 397 were answered otherwise, and of 18,938 requests 942**
(`test/replay-diff/2026-10-08/prom/before.md`). Four things, and a fifth that was the fixture's
own:

- *The case had less than it was asked about.* 53 queries and 53 requests reached further back
  than the thirty minutes that were frozen — an hour's window, `offset 2h`, and the first point of
  the thirty minutes themselves, which looks five minutes back for its sample — and were answered
  from nothing, with nothing to say whether it was the metric that began there or the case. Two
  repairs. `freeze` reads as far again before the window as an instant looks back, so that an
  instant anywhere in the window, and a range no longer than that at its very beginning, find what
  the Prometheus found (`freeze.json`: `from_ms`). And a query that looks further back than that is
  told so beside its answer, among the warnings, where a Prometheus says that an answer may not be
  whole: `frozen case: it holds no samples before 2026-10-08T10:09:19.751Z, and this query looks
  25m0s further back than that: its points before 2026-10-08T10:39:19.751Z may be missing, or come
  of less than its Prometheus had`. An engine tells its store how far back each selector looks — an
  instant with what it looks back, a range, an offset, an `@`, a subquery — and the store goes by
  the earliest. `promq` prints the line whenever it is said: of a request moved back from past the
  end a caller may know that it meant now, and of this no caller knows anything.

  It goes by what the query looks at, not by what turned out to be missing, which a case cannot
  know: an instant a minute after the beginning looks four minutes before it, and is told so though
  the sample it found is the one the Prometheus found. Of a window it names the instant from which
  each step looks at nothing before the beginning, and of a query that holds a selector to an
  instant of its own, with `@`, only how far back it looks, since that is as far at every step. A
  query the Prometheus would have refused for what it found there — two series where one may be —
  is answered, with the nothing. And a case that does not say where it begins says nothing: its
  oldest sample is not where it begins, when its Prometheus was younger than the window.
- *The order of the series, again.* 218 requests. A Prometheus hands a query its series as its head
  made them, and by label where the query reaches a block, since what comes of two stores is
  merged. `freeze` read thirty minutes, which reached into the blocks, and so read the series by
  label; a query of the last minute or two, which the Prometheus answered from its head alone, was
  answered by the case in another order. A case now keeps its series in the head's order and
  knows where the blocks end (`head_from_ms`). A Prometheus that lists its blocks says that
  outright, and it is the very number it holds a query's earliest instant against; one that does
  not — the list is newer than 3.5 — says its head's least time, which is where its blocks ended
  when it started over them and, from the first time it cuts its own head, the oldest sample the
  head has left, some seconds later; and whether there is a block at all it is asked by whether
  it knows of a label from before then. The head's order `freeze` asks for apart: the series
  endpoint, for the one selector it was given, over the time since the blocks ended. That listing
  is believed only if every series read with a sample since then is in it; otherwise the case is
  left as it was read, and `freeze.json` does not say its order is the head's (`series_order`).
  Nor does it when `freeze` was given several selectors, whose series were read one selector
  after another, or when a list of blocks has one in it that does not say where it ends. A replay
  hands a query its series the one way or the other by how far back the whole query looks, which
  is how a Prometheus chooses its stores; and a listing of series by where the window it is asked
  for begins.
- *What kind of metric each is.* 266 requests, and the five an agent of round 38 had made. A case
  carries what `/api/v1/metadata` answered (`metrics-metadata.json`) and answers the same: every
  family or the one named, `limit`, `limit_per_metric`, and the two refusals in Prometheus's words.
- *The two engines*, and none of it the store's doing: the paragraph after next.
- *And two requests were the fixture's own.* A label's values are a head's, whenever its series
  were made: an alert that began to fire a moment after the freeze, and the Prometheus's count of
  its own refusals, which the comparison itself caused, each made a series the case could not
  have. The fixture's alert no longer comes and goes, and its Prometheus is refused once of each
  kind before it is frozen. That a Prometheus lists the labels of its whole head, and a case those
  of its window, is in §8.

The comparison had a fault of its own at this size, which is why its very first run is not the
one counted above. `promq` is a process a query and a connection each, fourteen thousand of them in
a minute, and the requests to the API were then made a connection each as well: the machine ran out
of ports to connect from, and of 15,260 requests 5,816 got no answer in the first asking of the
Prometheus and 5,164 in the second. Requests are now made over one connection a worker, kept, and
no answer is no longer something three askings can agree on.

**Two readers, given the change and the sweeps' answers and no account of either, then found more
in it than the sweep had.** One read the code. A Prometheus that adds external labels to what it
sends elsewhere — any that writes to another store — has them on every series a remote read
returns, and not on what it lists of its head or answers of its own: no series of the listing was
one that had been read, so every one fell after the head's by label, and the case said its order
was the head's. They are taken off again when a case is read, the fixture's Prometheus has two,
and `freeze.json` names them (`external_labels`). The head's beginning is not always where the
blocks end, which is why the blocks are now asked for; a listing that was no list was taken for
an empty head; the listing of series read its window for the order and not for which series to
list; the line about a case's beginning said neither
which points it concerned nor, plainly, that it was the case that began there, and was among the
infos, which is not where a Prometheus says that an answer may be the poorer; and twenty-four
changes to the code went unnoticed by its tests, among them every one to what `freeze.json`
carries into a replay. Writing those tests found that `serve` crashed where it should have
refused: any failure once its session was made closed that session through a name that no longer
held one — a metrics file that cannot be read, a snapshot that will not unpack.

The other read the comparison, and found that the store could excuse itself: an answer that said
it had looked back further than the case reaches was counted and held to nothing, by its own
words, so that every answer replaced with one wrong line and those words passed, twenty thousand
of twenty thousand. It is now this comparison that says which questions look back — three it asks
for that, of a selector and nothing worked out from one — and what their answers have to be: the
Prometheus's own with the points before the case's beginning taken off and nothing else, every
series in its order, none missing that the case can know, under the Prometheus's status and in the
words the case's own beginning gives; the same words under any other answer are a difference. No
answer at all — a request nothing came back for, a `promq` that timed out — had been an answer
three askings could agree on. And of ninety-two changes to the comparison its self-test had
noticed thirty-four. What that reader's own recount bore
out of the first reports is kept as they found it: of the 369 answers a case frozen with half an
hour had marked, every one did look back, 342 were answered as the Prometheus answered them all
the same — a metric with no sample before the head — and 27 were its answer with the leading
points gone.

**A third reader was given those repairs, and found them wrong in their turn.** In the code:

- *The external labels were taken off by guess.* A series that had such a label of its own, with
  the very value, lost it with the rest, and the repair had said that what is read cannot tell the
  two apart. The Prometheus can: `freeze` now asks it which of its series have each such label
  themselves, and a series that was read is the listed one it would be read as, if there is one
  (§8).
- *The listing of series for a window was made to go by samples, and a Prometheus goes by chunks.*
  The repair listed a series whose samples lie on both sides of the window: 234 of them for forty
  seconds in which the fixture's Prometheus has no sample and, by its source, lists none; and
  nothing at all for a request that names a start and no end, once the freeze is past. A real
  head left a series out of two of 299 windows between two of its samples, where it had cut a
  chunk, which a case cannot know. A case's listing is of the whole case again, and §8 says so:
  the window is read for the order alone.
- *`series_order` was said where the order was not known*: of a case frozen with several
  selectors, whose series are read one selector after another; and of a list of blocks in which a
  block did not say where it ends, taken for a Prometheus with no block.
- `serve`, refusing a case, named a log in a directory it had just removed; the distance in the
  line about a case's beginning overflowed for a query that looks back before any time there is;
  and a request by POST was answered on paths a Prometheus answers only to GET.

And in the comparison, which had been made to hold the answers that look back to what they have to
be, and did it by reading what `promq` prints as a person would:

- *It crashed* on an answer of more series than `promq` prints, at the line that says so, and
  with it the whole report.
- *It let wrong answers by*: a series with something more in it than its name and its points,
  instants a part of a thousandth off, a result that was no list, a time of day there is none of
  — and a point at the very instant the case begins, where the case has no sample. What the API
  sent is now held as it was sent, and what `promq` printed line by line (above, and
  `test/replay-diff/README.md`). And it failed right ones: a Prometheus that changed its own
  answer between its two askings, which of any other question is said to be that.
- *Its own measure of what it lets by did not reach the rule it had just been given.* Every
  spoiling of an answer that looks back spoiled what it says too, and failed for that; none left
  the words alone and changed the answer under them. Six do now, and the first of them, run on
  the same answers, let that point at the case's beginning by: the window was asked in steps of
  five minutes, one of which falls where a case holds what an instant looks back before its hour
  and has a point or not by where its samples lie. It is asked in steps of ten, none of which
  does.
- *The fixture's head was too young for what was asked of it.* A sweep asks of the freeze and of
  a minute and a half before it, and the fixture's blocks ended 340 seconds before the freeze: an
  instant ninety seconds earlier reached them, and came by label on both sides, so that the head's
  order was never met there. The fixture now waits until its head is seven minutes old.
- *On the scenario the three questions that look back hold next to nothing*: its Prometheus is
  younger than the hour a case holds, so there is nothing before the case for the case to have
  lost — of the two instants an answer of nothing is right of any store, and of the window the
  Prometheus has two points, both inside the case. It is on the fixture that they are held (§8).
- And of 160 changes to the comparison, its sweep and its fixture, 63 went unnoticed by
  `selftest.py`; of 88 to the code, 17 by its tests.

**A fourth reader was given those, and found no answer of a sweep wrong by them** — the three
questions that look back, asked again in steps of ten minutes of the answers a sweep had recorded,
held on both cases, and of tens of thousands of wrong answers made up for them one kind was let by.
What it did find:

- *Taking the external labels off was dear where it should be nothing.* The repair tried every
  way of taking some of them off, the fewest first, and where no series has one of its own —
  nearly every Prometheus — the right way was the last it tried: with sixteen such labels, six
  tenths of a second a series. A series that was read is now looked up among the listed ones by
  what it is without them.
- *Two series that differ by nothing but such a label, one having it of its own, are read alike*,
  and the second was dropped without a word, its samples perhaps under the other's name. What is
  read cannot say which is which: `freeze` refuses it, and says why.
- *A listing that could not be had took a label off a series another listing had named whole*;
  and all of them had one fifteen seconds between them, so that a slow first left the next
  unasked. Each has its own time, and a series is the listed one whichever listing named it.
- *`serve`, refusing a case, left its directory behind whenever its error named a file there* —
  which was the last repair's doing, for the sake of a log: an archive that will not unpack left
  what it had unpacked, each time it was tried. What the snapshot server wrote before it gave up
  is in the error itself now, and the directory goes with the session.
- A request that looks back less than a millisecond (`lookback_delta=0.0005`) was taken for one
  so far back that the numbers had run out, and told that it looks more than 292 years before
  the case. A POST to a path a case does not serve was refused for its method, where a GET is told
  the path is not served: `read`, `format_query`, anything.
- In the comparison: warnings that were no list, with the case's words for a key, passed for the
  words; a window of which the right answer is nothing failed as `promq` prints it and passed as
  the API sends it; an instant was held to the Prometheus's exit code and not to its having
  answered; and the spoiling that puts back a point took it from the Prometheus's first series,
  whichever that was. The fixture's wait took an asking that failed for a target that is gone.
- And of 108 changes, to the code and to the comparison, 55 went unnoticed by their tests.

Each of these is repaired and held by a test, and nobody has read those repairs but their
author: four readers running, each given the last one's repairs, found them wrong.

What the last two readers changed to see whether a test would notice was changed again, in the
code and the comparison as they stand, with as much again of the author's. Of 211 changes that
still apply to the comparison, its sweep and its fixture, `selftest.py` notices 199; the twelve
it does not are ones no answer a Prometheus sends can tell from what is there. Of 137 to the code,
its tests notice 129; the eight are six changes, two of them made by two hands: a branch that
cannot be reached, three that leave every answer as it was, one that this store's own server
never gives occasion for — and one that is not held, a method refused on a path under `/api/`
that is not `/api/v1/`, which no Prometheus has.

**The engine a case is replayed with, beside the one it was frozen from.** A case is evaluated by
the engine this tool links, v0.315 of the module, which is Prometheus 3.15's; the scenario's
Prometheus was 3.5.0. Put over one Prometheus's samples — the fixture's, run on 3.5.0 — the two part
on 345 of 18,131 queries as `promq` prints them, and on 385 of 19,261 requests: `first_over_time`,
which the older does not have, on each of 320 metrics; `histogram_fraction` over classic buckets,
`NaN` from the older and a number from this one, on 22; what a parser says it expected after `up
offset`; and the last digits of `stdvar_over_time`, `stddev_over_time`, `deriv` and
`predict_linear` on 42 of what the API sends. Which answers part in a last digit changes with the
samples — 36, 46 and 51 in other runs of the same — and mostly past the ten digits `promq`
prints, though not always: two of this run's differ in the tenth
(`test/replay-diff/2026-10-08/prom/engines-3.5.0.md`). On a Prometheus
3.15.0, the same engine on both sides, none of these differs. That is how the fixture runs, and
since 2026-10-08 the scenario too, so that what a sweep reports is the store's doing and not two
engines': `selftest.py` holds both to the version in `go.mod`, and `PROM_IMAGE` puts the fixture
on another. A case still answers as this tool's engine does, whatever its Prometheus ran (§8), and
`freeze.json` names the version.

**Now, on the fixture: 20,841 queries, 20,838 answered the same and three that look back further
than the case reaches, held to what they have to be; and of 22,121 requests 22,089 the same, 28
the same in an order that is no one's or in one of two answers the Prometheus itself gave — each
one call of `histogram_quantile`, `histogram_fraction` or `count_values` — the three that look
back, and one refused otherwise, which is known. On the scenario: 718 queries and 833 requests,
the same but for those three of each and that one refusal — the 130 queries and the 154 requests
of recorded agents among them, every one the same.** And of the frozen stores' answers to an
agent's own asking, with no instant named, every one is what it says by name. The fixture's
blocks ended 432 seconds before its freeze, so that an instant at the freeze and one a minute and
a half before it were answered from the head alone, in the head's order; and the 922 series its
Prometheus scrapes of itself were listed for the last minute in that order by both
(`test/replay-diff/2026-10-08/prom/`).

The sweep froze thirty minutes at first, as it had for the scenario when its Prometheus held
eight, and then an hour's window of every metric looked back further than the case reached, which
nine of the recorded agents' queries did too. It freezes an hour now, which is what `freeze` takes
unasked, so that an agent's last hour is whole and is judged.

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
from the guard's verbs, it refused every `kubectl -n <namespace> get` in round 39's first set, where an agent
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
- Of the Prometheus HTTP API: `query`, `query_range`, `labels`, `label/<name>/values`, `series` and
  `metadata` are served — `query`, `query_range`, `labels` and `series` to GET and POST, a
  label's values and `metadata` to GET, as a Prometheus has them; `rules`, `alerts`, `targets`
  and `query_exemplars` answer empty, whatever the Prometheus had of them; anything else is refused in the API's error shape — a path a Prometheus does not
  have either among them, where a Prometheus says `404 page not found` as any web server does.
  `stats` is not answered, and of a label's values under a `limit` a case sends the first by name,
  where a Prometheus sends whichever it met first; the same of the families of `metadata` under a
  `limit`, and of one family's entries under `limit_per_metric`.
- **A listing of series, and the names and values of labels, are of the whole case, whatever
  window is asked for.** `series`, `labels` and `label/<name>/values` are answered for all a case
  holds, whatever `start` and `end` say (they refuse what is not a time); of `series` the start
  decides one thing, the order (§3). A Prometheus lists for a window the series that have a chunk
  reaching into it — not the ones with a sample inside, which a case could tell, and for a while
  did: a chunk is some two hours of a block and whatever the head has not cut, and a case has
  samples and no chunks. And it answers labels for everything in the stores the window touches,
  its whole head among them — a series that ended before the window, and one made after the
  freeze. So a case lists more than its Prometheus would for a window shorter than itself, and
  the comparison asks for the whole of what is frozen, but for one listing of the last minute, of
  series that are all there all the while. Its fixture is kept from making a series after the
  freeze; a real Prometheus is not.
- **What kind of metric each is, is what the Prometheus said when it was frozen**, of the targets
  it was scraping then: a metric no target exposed any more is not described, though the case has
  its samples. A case frozen before `freeze` asked (it has since 2026-10-08), one made by `pack`,
  and one read from a store that does not have the endpoint carry none, and answer that they know
  of none.
- **A case holds a window, and says so of a query by what the query looks at.** A query that looks
  further back than the case's metrics reach is answered from nothing and told so among the
  warnings beside the answer (§3) — whether or not anything was in fact missing, which a case
  cannot know; and a client that does not show warnings does not show it. A case that does not say
  where its metrics begin, one frozen before `freeze` recorded it or made by `pack`, says nothing.
- **The engine is this tool's, not the Prometheus's.** A case is evaluated by the PromQL engine
  `lapilli-case` was built with, v0.315 of the module, which is Prometheus 3.15's, whatever the
  Prometheus it was frozen from ran; `freeze.json` names that Prometheus's version since
  2026-10-08. Where the two differ in the language, in an answer or in the words of a refusal, a
  case answers as this tool's. Measured against 3.5.0, the scenarios' version, over the fixture's
  samples (§3): `first_over_time` is a function here and not there; `histogram_fraction` over
  classic buckets is `NaN` there and a number here; what the parser says it expected after `up
  offset`; and in their last digits `deriv`, `predict_linear`, `stddev_over_time` and
  `stdvar_over_time` — mostly past the ten that `promq` prints, and now and then in the tenth.
  That is one pair of versions, and eighteen thousand queries of
  somebody's choosing about three hundred metrics: a reviewer who ran the two engines over other
  samples and some 3,500 other queries found sixteen refusals worded otherwise, of which this met
  one. What `--enable-feature` switched on in the Prometheus is not carried either.
- **The order of a case's series is its Prometheus's: the head's, and by label for a query that
  reaches a block** (§3). Which of several equal series `topk` keeps, and the order a reader of the
  API meets them in, follow it. It is not, where `freeze` could not learn where the blocks end or
  could not list the head — a store that is no Prometheus, a listing that does not account for
  what was read, a list of blocks with one that does not say where it ends — and `freeze.json`
  then does not say `series_order`, and the case is in the order it was read in, by label if the
  reading reached a block, whatever is asked. Nor is it when `freeze` was given more than one
  selector, whose series come one selector after another: `freeze.json` does not say
  `series_order` of such a case either. Nor of a Prometheus that takes samples out of order,
  whose head is two; and of a Prometheus older than the list of blocks, the boundary is its
  head's least time, which is where its blocks ended when it started and some seconds after its
  last block ends once it has cut one itself. `series_order` says the order was learned: of a
  Prometheus with no block at all it is said too, with no `head_from_ms`, since everything is
  then the head's. A case frozen before `freeze` recorded this (it has since
  2026-10-08) does not say where its blocks ended, and one frozen before that same day's earlier
  change has its series by label. A case made by `pack` from a file `export-metrics` wrote has the
  head's order and not where the blocks ended — nor where its metrics begin, nor what kind they
  are: the file holds samples (`ROADMAP.md` §7, 1g).
- **A Prometheus's external labels are taken off what is read of it**, since its own answers do
  not have them. A series that has a label of the same name and the same value of its own keeps
  it: what a remote read returns cannot tell the two apart, so `freeze` asks the Prometheus which
  of its series have each such label themselves, and a series that was read is the listed one it
  would be read as. Where the Prometheus does not answer that — a store that speaks remote read
  and not the series endpoint — the label is taken off every series that has it with that value.
  Two series that differ by nothing but such a label are read alike, and which samples are whose
  cannot be told: `freeze` refuses such a Prometheus's metrics, and says so. And a selector given
  to `freeze` is read as a remote read reads it: a matcher for an external label with its very
  value means, to a Prometheus, the series that have no such label of their own — every series as
  another store would see it — and not the ones its own queries find by it.
- A subquery without a step is evaluated at the interval `freeze.json` records, and an instant looks
  back as far as it records. A case frozen before 2026-10-08, or made by `pack`, has neither, and is
  replayed at Prometheus's defaults, a minute and five: the first is not what the scenarios'
  Prometheus was set to.
- **`promq` and the API were compared on two Prometheuses**, both of the engine's own version:
  the scenario's — one job, some fifteen series and no histogram among them, eight minutes old, all
  of it in the head — and the fixture's: a past in blocks it was given before it started, three
  targets and itself, a target that stopped before its head began, rules, external labels, a
  histogram and a summary, a thousand series and more (§3). Not compared: a Prometheus that has
  cut its own blocks from its head, with native histograms, with samples that came out of order,
  with another store behind it, or of a version other than those two; a case frozen with more than
  one selector; `rules`, `alerts` and `targets`, which a case answers empty and a Prometheus that
  has any does not, with times in the answer that change from one asking to the next; a request
  made by POST; a listing of series, or the names and values of labels, for a window other than
  the whole of what is frozen, which a case does not answer by (above) — but for the one listing
  of the last minute; what a case answers about a time before its beginning, of any query but the
  three the comparison asks for that, and of those three on the scenario's Prometheus, which is
  younger than the hour a case holds and has nothing there for a case to have lost; a request that
  reaches past the freeze, of which a Prometheus asked
  afterwards knows a later — so that how such a request is answered (§3) is held by unit tests and
  by four requests of round 38's records, and by no Prometheus; and one with no time named, sent
  to the API by a client other than `promq`, which a Prometheus answers about its own now. A time
  that is no instant is read here in one way on every machine — a number too large is the last
  instant there is, and `NaN` is refused as no time — where a Prometheus makes of it what its
  processor does.
- Native histograms are not carried; `freeze` refuses a series that has them. Start timestamps and
  exemplars are not carried.

**The recorded runs were made before most of this.** The 28 of 2026-10-06 before the front
existed: `kubectl describe pod` in their frozen condition averages 10.5 KB against 2.7 KB live, and
`--tail` was ignored. The 72 of round 38 with the selector and the tail repaired and the tables not:
a sorted event list came back empty, a pod listing asked for `-o wide` had no IP and no NODE,
ReplicaSets, Endpoints and a pod asked for by name listed as `NAME AGE`, and `kubectl get all`
printed bare names — at least 33 steps in 19 of that round's 36 frozen runs
([`design-review-round38.md`](design-review-round38.md), *Outside the rule*). The 39 of round 39
are the only ones made on the replay as it is: none shows any of those, and the 238 `kubectl`
reads they typed that can be asked as they stand were put to a cluster as well, none differing —
the first time through a reading of their command lines that was wrong in 22 places, and then
again
([`design-review-round39.md`](design-review-round39.md)). What they asked of `promq` was put to a
Prometheus only afterwards (§3): the 96 different queries of rounds 38 and 39 answer the same now.
As the replay was when the runs were made, a Prometheus answered 35 of them otherwise than the case
did: four in the order of their series or the words of a refusal, and 31 from the fifth digit of a
rate on, the case having evaluated a window a few milliseconds off.

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

What CI recomputes from those records, and so what cannot drift: every run's process grade, all 139.
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
