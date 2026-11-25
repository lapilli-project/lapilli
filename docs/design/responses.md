# Responses: what people did when a rule fired

Status: built (schema 034); "As built" at the end says where it departs
from what is written above. Under
[GOVERNANCE.md](../../GOVERNANCE.md) this changes how a rule is found
noisy and adds kinds of data to the ledger, so it is a design. With one
maintainer it is merged with the code that builds it, sooner than a week,
and this note says so. "Questions for review" at the end are open.

## Why

A firing is judged against incidents: white when it caught one, black when
it caught nothing written down, and not judged at all when nothing was
being written down ([How it judges](../how-it-judges.md)). Two kinds of
firing come out wrong:

- **An alert that worked.** A disk at 85%, a certificate a week from
  expiring, a queue backing up: someone acts, nothing becomes an incident,
  and the firing is black. A rule that does its job is as black as one
  nobody needed. With ten firings or more and under 20% caught it is noisy,
  and Lapilli suggests a longer `for` for it.
- **A page nobody answered.** In a week with no incident written down and
  no `--impact`, nothing is judged. Yet the paging tool knows: the page was
  never acknowledged, and Alertmanager resolved it four minutes later. That
  is the page an on-call engineer calls noise, and no incident is needed to
  say so.

Alerting practice holds a page to whether it is actionable, not to whether
an outage followed ([Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/),
"every page should be actionable"). A person's "needed action"
([judging.md](judging.md), "Did it need action?") says so by hand, a firing
at a time, in serve alone, and moves no rule. Most of it is already written
down somewhere else:

- the paging tool keeps whether a person acknowledged a page, and whether
  a person or the alert manager resolved it;
- kube-state-metrics, in the history Lapilli already reads, keeps when a
  workload was rolled out, rolled back or scaled;
- the deploys file serve already takes ([noc.md](noc.md), §2) keeps
  deploys.

This note reads them, gives each firing a **response**, and lets a rule be
found noisy by what was done about its firings, beside what they caught.
Stones don't change: precision is what it was.

## What is built

### What comes in

**Responses**, what the paging tool says of each page. `--responses FILE`
in both commands (serve reads it again each run, as `--incidents`), and
in serve `PUT /api/v1/responses/SOURCE?from=FROM&to=TO` on the incidents
port with `--responses-token-file`, which replaces what that source said
of pages sent in that span, as `PUT /api/v1/firings/SOURCE` does
([recorded.md](recorded.md)):

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

- `id`, `alerts` (one at least) and `sent` are needed; `resolved_by` is
  needed with `resolved`. Any other key is refused, as an incident's
  alerts refuse one, and so is a value that isn't text.
- An alert is written and held to the same bounds as an incident's linked
  alerts ([links.md](links.md), "Cost and bounds").
- No person is named. Who acknowledged or resolved a page is not read
  into Lapilli: only that a person did, as mutes keep no silence's
  author ([mutes.md](mutes.md)).

**Changes on the alert's target**, two ways:

- `--rollouts` (both commands, off by default; the chart's `rollouts`):
  read from the history, from kube-state-metrics, for the namespaces the
  period's firings carry:
  - a Deployment's **rollout**: a ReplicaSet created in the period
    (`kube_replicaset_created`, its value the time), owned by the
    Deployment (`kube_replicaset_owner{owner_kind="Deployment"}`), at the
    second it was created;
  - a Deployment's **rollback**: a ReplicaSet created before the period
    whose `kube_replicaset_spec_replicas` goes from 0 to more, at the first
    step it does;
  - a StatefulSet's rollout: `kube_statefulset_status_update_revision`'s
    `revision` changing; a DaemonSet's: `kube_daemonset_metadata_generation`
    going up; at the first step it shows;
  - a **scale** of a Deployment or StatefulSet that no HorizontalPodAutoscaler
    targets: `kube_deployment_spec_replicas` or `kube_statefulset_replicas`
    changing, the autoscaler's targets read from
    `kube_horizontalpodautoscaler_info`'s `scaletargetref_kind` and
    `scaletargetref_name`. kube-state-metrics marks that series
    experimental; where the history has none, every scale is counted, and
    the report says so.
  - A workload's changes are known from where its series begin
    (`kube_deployment_spec_replicas`, `kube_statefulset_replicas`,
    `kube_daemonset_metadata_generation`): before that, and outside the
    namespaces read, no change is known, not "no change".
- `--deploys FILE`, serve's, now in `lapilli verdict` too, in the same
  format. A deploy is a change at its start. A target the file lists has
  its changes known from its earliest deploy listed on.

**A person's word**: in serve, "needed action" yes or no, as now.

### A firing's response

Each firing has one, the first of these that holds:

1. **changed**: a change on its target started after the firing started and
   no later than its end (now, for one still on). Its target is the
   change's:
   - a rollout, rollback or scale: the firing carries the change's
     `namespace`, and names its workload: a `deployment`, `statefulset` or
     `daemonset` label of the workload's kind and name; or a `pod` the
     workload owns (`kube_pod_owner`, then `kube_replicaset_owner` for a
     Deployment's), read for the pods the period's firings name; or a label
     `--rollout-match LABEL` names (repeatable; the chart's `rolloutMatch`)
     whose value is the workload's name, for teams whose `service` is
     their Deployment's name;
   - a deploy: tied to the firing's labels as serve ties a deploy to a
     firing today (`ledger.Deploy.During`'s labels).
   A change is **routine** when its target changed so often in the period
   that one falling in a span as long as the firing's is likely anyway: with
   n changes of the target over the period T (rollouts, rollbacks and
   scales of a workload together; a deploy's target by its labels) and the
   firing lasting s, 1 − e^(−(n−1)·s/T) at or above `--routine` (0.5; both
   commands, the chart's `routine`). A target a pipeline deploys every hour
   changes in any two-hour firing. A routine change is chance, not
   evidence: it doesn't hold here, and the firing goes on to its page. It
   is still kept with the response and shown. When the firing has
   non-routine changes, the earliest is the one it rests on.
2. **acknowledged**: its page was acknowledged by a person, or resolved by
   one.
3. **unanswered**: its page was not acknowledged, and its alert manager
   resolved it.
4. **routine**: no page, and only routine changes.
5. **none**: nothing above is known (a page still open and unacknowledged
   is none until it resolves).

**Its page** is a response whose alerts list the firing (its rule and
labels agreeing, as an incident's linked alerts are held to a firing,
`incident.Alert.Links`), sent between the firing's start and its end plus
two of its rule's evaluations (the window a notification is tied to a
firing by, `Within`); the one sent nearest its start, when several are. One page sent
for a group of alerts answers each firing it lists.

A firing with a change and a page is `changed`; the page is still in the
report.

### Useful firings

A person's answer comes first: "needed action" yes makes a firing
**useful**, no makes it **not useful**, whatever else holds (a white
firing a person says needed nothing is the coincidence "Unrelated" is
offered for). With no answer, a firing is **useful** when:

- it caught an incident (white), or
- it is `changed` (by a change that isn't routine).

It is **not useful** when:

- it is `unanswered`; or
- it is `acknowledged` and its target's changes are known at its time:
  someone looked, and nothing changed; or
- it is black and judged (its record kept), and neither of the above
  makes it useful: an acknowledgement doesn't.

Otherwise it is **unknown**: not judged by stones and nothing else known,
`acknowledged` with its target's changes unknown, or a routine change with
no stone to judge it.

A rule's **actionable** share is its useful firings over those useful or
not, beside its precision.

### Which rules are noisy

`--noisy-by` (both commands; the chart's `noisyBy`; the Settings page,
tried on the last week first as the noisy thresholds are):

- `caught`: by stones, as before: `--noisy-firings` or more judged, under
  `--noisy-precision` white. The default with neither `--responses` nor
  `--rollouts` given, `--deploys` alone included.
- `useful`: `--noisy-firings` or more firings useful or not, under
  `--noisy-precision` of them useful. The default once `--responses` or
  `--rollouts` is given: they are given to be used. `--deploys` alone leaves
  `caught`: serve took it before responses, and an upgrade must not change
  which rules are noisy for a deployment that gives only that. Serve with
  people's answers and none of them stays on `caught` until someone chooses
  `useful`. `--noisy-by` given always wins, in both commands.

With `useful`, the `for` suggested for a noisy rule
([How it judges](../how-it-judges.md), "What to change") is the shortest
that keeps every useful firing and fires at most half as often: a firing
of the rule replayed with the longer `for` overlapping it and starting
before its change, or catching the incidents it caught, as now. Firings
that a page nobody answered came of, being short, fall away under a longer
`for`; that is the point.

**With no record.** A rule none of whose firings an incident record judges
can still have an actionable share and be noisy by `useful`, from its
responses alone. The report's no-record line says how many firings were
judged by responses alone. Stones still need a record.

### In the report

When `--responses`, `--rollouts` or `--deploys` is given, `lapilli
verdict`'s text has a RESPONSES table after the rules: each rule's FIRED,
ACTIONABLE, CHANGED (routine ones in brackets), ACKNOWLEDGED, UNANSWERED
and NONE; then how many pages matched no firing (a renamed rule, labels the
tool cut), and, with `--rollouts`, whether autoscalers' targets were found.

- `--json`: each firing's `response`
  `{"kind", "useful", "change": {"kind", "at", "labels", "routine"}, "page": {"id", "sent", "acknowledged", "resolved", "resolved_by"}}`.
  `kind` is `changed`, `routine`, `acknowledged`, `unanswered` or `none`;
  `useful` is true, false or null. `change` (its `kind` is `rollout`,
  `rollback`, `scale` or `deploy`; `labels` left out when it has none) is
  there for a firing with a change (a routine one is kept too), and `page`
  for one with a page;
  `acknowledged`, `resolved` and `resolved_by` are left out when the page
  has none. Each rule's `responses`
  `{"changed", "routine", "acknowledged", "unanswered", "none", "useful", "not_useful", "actionable"}`
  (the five kinds add up to its firings; `actionable` is null when no firing
  is known useful or not); the report's `responses`
  `{"pages", "pages_unmatched", "changes", "autoscalers_known"}`;
  `settings.noisy_by` (`caught` or `useful`) and `settings.routine`.
- CSV: the rule's counts and actionable as columns after the others.
- `--metrics`: `lapilli_rule_responses{rule, response}` and
  `lapilli_rule_actionable{rule}`.
- `--compare`: ACTIONABLE beside precision for each side, when given.
- `--redact`: page ids aliased, labels as labels are.
- docs/numbers.md says how each is counted, as `TestReportFieldsStable`
  and `TestCSVColumns` hold them.

### In the ledger: schema 034

- `responses`: `source`, `id`, `sent_at`, `acknowledged_at`, `resolved_at`,
  `resolved_by`, `alerts` (JSON, sorted, as `incidents.alerts`); unique on
  source and id. The file's source is `file`.
- `rollouts`: `kind`, `at`, `namespace`, `workload_kind`, `workload`,
  `revision`; and `rollout_spans`, the spans each workload's changes are
  known over. Each run reads its hours' and keeps them, so judging the
  ledger again asks Prometheus nothing.
- `firings.response` (0 none, 1 changed, 2 routine, 3 acknowledged,
  4 unanswered) and `firings.routine` (1 when a routine change is kept
  with an acknowledged or unanswered response) and `firings.useful` (−1 unknown, 0, 1): worked out as
  stones are, and again whenever stones are.
- Day and week counts `resp_changed`, `resp_routine`,
  `resp_acknowledged`, `resp_unanswered`, `useful`, `not_useful`.
- `judgedBy` goes to `j3`, so an upgrade judges every firing once again,
  as schema 032 did.

### In lapilli serve

All of it shows once responses do: the ledger keeps pages or changes read
from the history, or rules are found noisy by what was done about their
firings (`noisy_by` `useful`, which is where a person's "needed action" is
counted). Before that no page changes: neither a column, a number nor a
filter. The deploys file alone, which serve took before this, shows none
while `noisy_by` is `caught`.

- **Alert history**: a Response column: "Rolled out 12:04", "Rolled back
  12:04", "Scaled 12:04" or "Deployed 12:04" with the change's labels under
  it, "Acknowledged", "Nobody acknowledged; resolved by its alert manager",
  "Routine change" (said to be chance, not counted), the page's id when there
  is one, and nothing for none. `?response=changed|acknowledged|unanswered|
  routine|none` picks by it, the form has the choice, and a chip takes it away;
  without a day it is the last day with one.
- **A rule's page**: Actionable beside Precision (the same percentage, `–`
  when no firing is known useful or not), the counts, and the latest firings
  picked by `?tab=firings&response=`, each kind a link with its count.
- **Rules list**: Actionable as a column and in the total, and an order,
  "actionable, the least first", which puts a rule whose share isn't known last.
- **Overview**: "Pages nobody answered", last week beside the week before,
  fewer being better, linked to the alert history picked by `unanswered`.
- **Summary, goals, metrics, digest, MCP**: `responses` and
  `actionable.share` on each summary row; goals take `actionable.share`;
  `/metrics` `lapilli_rule_responses` and `lapilli_rule_actionable`; the
  digest a line ("12 of 40 pages last week nobody answered"); MCP's rule
  and firing tools the same fields.
- **Settings**: `noisy_by`, a choice of `caught` or `useful`, tried on the
  last week first as the noisy thresholds are: the rules whose state would
  change are listed with their actionable share and whether each is noisy
  before and after.
- English, Korean, Japanese and Spanish, as every page is.

### The recipes

`recipes/responses/` writes `responses:` for a period, one script a tool,
as `recipes/incidents/` does: the token from a file, https, 16 MiB an
answer, `MAX_PAGES`; `put.sh` sends it to serve. Each reads only what its
tool documents, and is tested against that, with a server standing in:

- **PagerDuty**: the period's incidents (an Alertmanager page each), their
  alerts as the incidents recipe reads them, and their log entries for
  who acknowledged and what resolved them, by the types PagerDuty's
  published OpenAPI document gives (`PagerDuty/api-schema`): a resolve by
  a service or an integration is `alert`, a user's is `person`.
- **Jira Service Management** (Opsgenie's alerts): an alert's
  acknowledgement and, from its log, who closed it.
- **Compass**: an alert's log, where "Alert closed via API by
  Alertmanager", by "System", is `alert`, and a person's close is `person`.
- **Grafana IRM**: an OnCall alert group's `acknowledged_at`. What
  `resolved_by` holds when the alert source resolved a group isn't in the
  API's reference; it is read from OnCall's own serializer, and left out
  if that doesn't settle it, in which case no group is `unanswered`
  (questions, below).

## Cost and bounds

- A file or a write holds 100,000 pages at most; each is matched against
  the firings of its alerts' rules, by rule.
- `--rollouts` reads, each run (or once for `lapilli verdict`), a handful
  of queries for the namespaces the firings carry, at most 200 to a
  query's regular expression, each name quoted: an instant query of the
  ReplicaSets created in the period and their owners, and range queries at
  `--step` of the revision, generation and replica series, within
  `--max-samples` and `--max-rate` as every query is. Pods' owners are read
  for the pods the firings name, 200 to a query.
- A target's changes over the period are counted once, for the routine
  test.

## Attacker's look

- **Who writes responses.** The file is whoever runs Lapilli; the PUT
  takes the incidents port's token for responses, which writes only the
  pages of its source. A response can only make a firing `acknowledged` or
  `unanswered`, not useful: forged pages can make a rule look noisy, never
  look better. A noisy rule gets a suggestion, and a change to it is made
  only once a person approves it ([changes.md](changes.md)). No stone
  changes.
- **What can make a firing useful.** A change on its target: from the
  history (a forged series is beyond what Lapilli guards; it reads what
  Prometheus has) or the deploys file (whoever runs Lapilli).
- **Names.** No person's name is read or kept. The recipes decide
  "a person" from what the tool says and drop the rest; what they say on
  stderr is counts.
- **Queries.** Namespace and pod names in a regular expression are quoted
  (`regexp.QuoteMeta`), and come from the firings Lapilli already holds.
- **Ids.** A page's id is 256 bytes at most, of letters, digits and
  `-_.:`; the recipes put an id into a URL only once it is checked so.

## Questions for review

1. `--routine` at 0.5: is a change more likely than not by chance the line?
2. `acknowledged` with the target's changes unknown is unknown, not "not
   useful": someone may have fixed it in a way no change shows.
3. A page a person resolved without acknowledging is `acknowledged`.
4. Grafana IRM: what `resolved_by` holds when a source resolves a group.
5. incident.io's alerts say whether they resolved, not who acknowledged:
   no recipe until its API says.
6. `--noisy-by useful` as the default once `--responses` or `--rollouts` is
   given (not `--deploys`, which serve took before this: decided in
   review).

## Sources

- Monitoring Distributed Systems, in Site Reliability Engineering:
  https://sre.google/sre-book/monitoring-distributed-systems/
- kube-state-metrics' workload metrics (`replicaset-metrics.md`,
  `deployment-metrics.md`, `statefulset-metrics.md`,
  `daemonset-metrics.md`, `pod-metrics.md`,
  `horizontalpodautoscaler-metrics.md`):
  https://github.com/kubernetes/kube-state-metrics/tree/main/docs/metrics/workload
- PagerDuty's log entries, and its OpenAPI document:
  https://docs.pagerduty.com/developer/api/reference/rest/log-entries,
  https://github.com/PagerDuty/api-schema
- Grafana OnCall's alert groups:
  https://grafana.com/docs/grafana-cloud/alerting-and-irm/irm/oncall/oncall-api-reference/alertgroups/
- Jira Service Management's alerts:
  https://developer.atlassian.com/cloud/jira/service-desk-ops/rest/v1/api-group-alerts/

## As built

As above, except:

- **Serve counts a change as routine over the firing's own week.** `lapilli
  verdict` counts the changes of a target over the report's period, and the
  ledger, which judges a firing when it is kept and again whenever stones
  are, over the ledger week the firing started in (Monday to Monday, to the
  latest end of a firing of that week): its n and T are the week's. A firing
  is therefore routine or not by the same count wherever it is looked at
  from, whatever period a page shows, and a report of a week and a ledger
  agree; a report of another period may not.
- **One more table, `rollout_owners`.** The pod each firing names, and the
  workload it belongs to (namespace, pod, workload kind and name, when it was
  seen), are kept with the changes, so that judging the ledger again, which
  asks the history nothing, can still tie a firing that names a pod alone to
  its workload. Schema 034 has it beside `rollouts` and `rollout_spans`.
- **A page ties to a firing that started up to 14 days before it was sent**
  (`ledger.ResponseReach`): a page is tied to a firing that was on when it was
  sent, and one lasts days at most before its page. Writing a page, or
  changing one, judges again the firings that started that long before it.
- **`pages_unmatched` is worked out by `lapilli verdict` alone.** It needs
  every page read against the firings of the period, which a ledger keeping
  pages by source and judging a firing at a time doesn't do: in serve's
  report it is 0.
- **Firings recorded through `PUT /api/v1/firings` get no rollouts read.**
  The changes are read by a run, from the history, for the namespaces its
  firings carry; the firings a source writes weren't found by a run, so no
  change is known for them from the history, and one is not called changed,
  nor not useful for want of one. Their deploys, and pages, do count.
- **The deploys file alone isn't a response input for the default.**
  `--noisy-by` is `useful` by default only with `--responses` or
  `--rollouts`: serve took `--deploys` before this, and an upgrade must not
  change which rules are noisy for a deployment that gives only that. It is
  said in both commands, and `--noisy-by` given always wins. In serve,
  responses show when the ledger keeps pages or changes read from the history,
  or `noisy_by` is `useful`.
- **Serve's pages, as written above, with these departures.** The rules
  list's Actionable column and order are there once responses show, in
  place of "once any response comes in". The rule page's panel has no figure
  of how Actionable changed from the span before. The overview's tile
  links to the alert history picked by `unanswered`, which shows the last day
  with one and goes back day by day: the history is a day at a time, not a
  week. A firing's page and its change are read as the page is worked out,
  from the ledger, while the filter and the counts read what the ledger
  worked out of each firing when it judged it: a page written brings the firings
  it answers up to date at once.
