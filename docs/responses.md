# What people did when a rule fired

A rule that fires and is right is not always a rule that caught an incident.
A disk at 85%, a certificate a week from expiring: someone acts, nothing
becomes an incident, and the firing is a black stone. And in a week with no
incident written down, no firing is judged at all, though the paging tool
knows a page was never acknowledged and resolved itself four minutes later.

Lapilli can read what was done. Each firing then gets a **response**, and is
**useful** or **not useful**, and a rule can be found noisy by that, beside
what its firings caught. Stones don't change: precision is what it was.

It reads three things, any of them, with `lapilli verdict`:

| Flag | What it reads |
|---|---|
| `--responses FILE` | the pages a paging tool sent, and what was done with each |
| `--rollouts` | rollouts, rollbacks and scales of the alert's workload, from kube-state-metrics in the history |
| `--deploys FILE` | deploys, as `lapilli serve` reads them |

With none of them, nothing here applies, and every number and every field is
what it was.

## The responses file

A YAML file of the pages the paging tool sent:

```yaml
responses:
  - id: PD-Q1W2E3                       # the tool's id of the page
    alerts:                             # the alerts it was sent for, as an incident's are written
      - {rule: KubePodCrashLooping, labels: {namespace: shop, pod: cart-7f9c}}
    sent: 2026-11-20T03:12:00Z
    acknowledged: 2026-11-20T03:15:00Z  # by a person; left out when no one did
    resolved: 2026-11-20T03:20:00Z      # left out while open
    resolved_by: alert                  # alert: its alert manager resolved it; person: someone did
```

- `id`, `alerts` (one at least) and `sent` are needed. `resolved_by` is
  needed with `resolved`, and is `alert` or `person`.
- Any other key is refused, and so is a value that isn't text.
- An alert is written as an incident's linked alerts are, and held to the
  same bounds ([Incidents kept elsewhere](incidents.md)).
- No person is named. Lapilli reads only that a person acknowledged or
  resolved a page, not who.
- A file holds 100,000 pages at most.

A script for each paging tool, which writes this file, is in
[recipes/responses](../recipes/responses).

A page belongs to a firing when it lists the firing's rule, its labels
agreeing with the alert's on those both carry, and it was sent between the
firing's start and its end plus two evaluations of its rule. When several
were, the one sent nearest the start. One page sent for a group of alerts
answers each firing it lists. A page that belongs to no firing, a rule
renamed or labels the tool cut, is counted and said in the report.

## Rollouts

`--rollouts` reads from the history, from kube-state-metrics, what was
changed on the workload an alert is about. It needs `--prometheus` (or
`--gcp-project`), with `--auth`, tenant and limits as every read of the
history has. It asks for the namespaces the period's firings carry and the
pods they name, a handful of questions in all.

It finds:

- a Deployment's **rollout**: a ReplicaSet created in the period, at the
  second it was created;
- a Deployment's **rollback**: a ReplicaSet whose replicas go from 0 to
  more, at the step they do (one created in the period only after it was
  above 0 and then 0: its first scale up is its rollout's), unless the
  Deployment's own replicas go from 0 to more then, a step either side (a
  scale from zero). Only the ReplicaSets that were both 0 and above it in
  the period are read step by step, as many names to a query as the sample
  cap holds at the period's points (200 at most);
- a StatefulSet's rollout, when its update revision changes, and a
  DaemonSet's, when its generation goes up;
- a **scale** of a Deployment or StatefulSet that no HorizontalPodAutoscaler
  targets, when its replicas change. kube-state-metrics marks the autoscaler
  series experimental. Where the history has none, every scale is counted,
  an autoscaler's too, and the report says so.

A firing is about a workload when it carries its `namespace` and names it by
a `deployment`, `statefulset` or `daemonset` label, or by a `pod` the
workload owns. Teams whose `service` label is their Deployment's name add
`--rollout-match service`, as often as they need (the Helm chart's
`rolloutMatch`).

A workload's changes are known from where its series begin. Before that, and
in a namespace not read, no change is known, which is not the same as no
change.

## Deploys

`--deploys FILE` takes the file `lapilli serve` takes ([Keep judging](serve.md)):
each deploy is the labels that say what it is of, and when. A deploy is a
change at its start, tied to a firing that carries every label the deploy
lists (a deploy that lists none is no firing's: it names no target, and
covers none). That is stricter than
the deploy stone's tie in `lapilli serve` (`--deploy-before`, `--deploy-after`),
which is left as it was (there, a deploy that lists no label is still
everything's). What the file lists has its changes known from its
earliest deploy on.

## How a response is decided

Each firing has one response, the first of these that holds:

1. **changed**: a change on its target started after the firing started and
   no later than its end (the period's end, for one still on).
2. **acknowledged**: its page was acknowledged by a person, or resolved by
   one.
3. **unanswered**: its page was not acknowledged, and its alert manager
   resolved it.
4. **routine**: no page, and only routine changes (below).
5. **none**: nothing above is known. A page still open and unacknowledged is
   none until it resolves.

A firing that has a change and a page is `changed`; its page is still in the
report.

A change is **routine** when its target changed so often in the period that
one falling in a span as long as the firing is likely anyway. With n changes
of the target over the period of length T, and a firing lasting s, the chance
is 1 − e^(−(n−1)·s/T). At or above `--routine` (0.5), the change is routine.
A target a pipeline deploys every hour changes in any two-hour firing. A
routine change is chance, not evidence: it doesn't make a firing `changed`,
and the firing goes on to its page. It is still kept and shown with the
response.

## Useful and not useful

A firing is **useful** when it caught an incident (white), or when it is
`changed`.

It is **not useful** when it is `unanswered`; or `acknowledged` and its
target's changes are known at its time (someone looked, and nothing changed);
or black and judged, and not useful by the lines above.

Otherwise it is **unknown**: not judged by stones and nothing else known,
`acknowledged` with its target's changes not known, or a routine change with
no stone to judge it.

`lapilli verdict` has no person's answer. In `lapilli serve`, a person's
"needed action" comes before all of this.

A rule's **actionable** share is its useful firings over those useful or not,
beside its precision. Unknown firings are in neither.

## Noisy by useful

`--noisy-by` says what a rule is found noisy by:

- `caught`: by stones, as before. At least `--noisy-firings` firings judged,
  and under `--noisy-precision` of them white.
- `useful`: at least `--noisy-firings` firings useful or not, and under
  `--noisy-precision` of them useful.

It is `useful` once `--responses` or `--rollouts` is given (in `lapilli
serve`, `--responses-token-file` too), and `caught` otherwise. `--deploys` alone doesn't change it: `lapilli serve` took
`--deploys` before it took responses, and an upgrade must not change which
rules are noisy for a deployment that gives only that. Give `--noisy-by
useful` to find rules noisy by what was done about them with the deploys
alone. `--noisy-by` given always wins: `--noisy-by caught` keeps the old way
while the responses are still shown.

With `useful`, the longer `for` suggested for a noisy rule is the shortest
(5m, 10m, 15m, 30m, 1h, 2h) that fires at most half as often and keeps every
useful firing. A firing is kept when a firing of the rule replayed with the
longer `for` overlaps it and starts before its change, or catches the
incidents it caught. Short firings that pages nobody answered came of fall
away under a longer `for`. That is the point.

A rule none of whose firings an incident record judges can have an actionable
share, and be noisy by `useful`, from its responses alone. The report's
no-record line says how many firings were judged that way. Stones still need
a record.

## In the report

The text has a RESPONSES table after the rules:

```
RULE        FIRED  ACTIONABLE  CHANGED  ACKNOWLEDGED  UNANSWERED  NONE
ApiErrors   2      50%         1        0             1           0
DiskFull    1      0%          0        1             0           0
```

CHANGED counts firings a change on the target followed, and in brackets
those whose change was routine. Under it: how many pages matched no firing,
and with `--rollouts`, whether autoscalers were found.

`--json` has each firing's `response`, each rule's `responses`, the report's
`responses`, and `settings.noisy_by` and `settings.routine`. `--csv` has
columns after the others, `--metrics` has `lapilli_rule_responses` and
`lapilli_rule_actionable`, and `--compare` puts ACTIONABLE beside precision
for each side. `--redact` gives page ids an alias, and the labels of a change
as it gives labels. Each is defined in [The numbers](numbers.md#what-people-did).

## What it does not do

- It reads no person's name, and keeps none.
- A page can only make a firing `acknowledged` or `unanswered`, and neither is
  useful, so forged pages make a rule look noisier. They can, though, make it
  look less so: an acknowledged page turns an unanswered firing, which is not
  useful, into one of unknown use when the changes of its target aren't
  known, and unknown firings are in neither count of the actionable share. In
  `lapilli serve` that is bounded by the page's source: its token writes its
  own source's pages and no other's, and the `--responses` file's page for a
  firing outranks any other source's.
- A change from the history or the deploys file is believed as it is read.
- `lapilli serve` reads the same inputs, with `--responses-token-file
  SOURCE=FILE` and `PUT /api/v1/responses/SOURCE` for the pages (JSON, 16 MiB
  at most, a token for each source) ([the API](api.md#the-incidents-port),
  [the design](design/responses.md)); it works each firing's response out as
  it judges the firing, from what its ledger keeps, and counts a rule's
  changes over the firing's week, not the report's period.

## In lapilli serve

`lapilli serve` reads the same inputs, and keeps them in its ledger. Once it
keeps pages or changes read from the history, or rules are found noisy by
`useful`, its pages show them:

- Alert history has a Response column ("Rolled out 12:04", "Acknowledged",
  "Nobody acknowledged; resolved by its alert manager", "Routine change") and
  `?response=changed|acknowledged|unanswered|routine|none` picks by it.
- A rule's page has Actionable beside Precision, the counts, and its latest
  firings picked by response; the rules list has an Actionable column and an
  order of it; the overview has "Pages nobody answered", last week beside the
  week before.
- The Settings page has `noisy_by`, `caught` or `useful`, tried on the last
  week first as the noisy thresholds are, and saved the same way. The
  default is `useful` with `--responses`, `--responses-token-file` or
  `--rollouts`, not with `--deploys` alone.

Firings written through `PUT /api/v1/firings/SOURCE` get no changes read from
the history. [Keep judging](serve.md#what-was-done-about-a-firing) says
where each shows.

See [How it judges](how-it-judges.md#what-people-did) for where this sits
beside stones, and the [design](design/responses.md) for why.
