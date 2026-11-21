# `lapilli impact suggest`: which `--impact` the history can read

Status: proposed, built in the same pull request. Under
[GOVERNANCE.md](../../GOVERNANCE.md) a new command is designed first, as a
pull request open for a week; this one carries the code too, so the
design can be read beside what it does. It adds nothing to the ledger,
changes no stone, and writes nothing anywhere. The companion to
[signals.md](signals.md), whose windows it counts.

## Why

A firing is judged only against a record. A team with no incident written
down has `--impact` ([Incidents nobody wrote down](../impact.md)), whose
table gives a query for six stacks in front of services. Picking one
means knowing which stack is there and whether its metrics are scraped
under the names the query reads. A query whose selectors have no data
keeps no record ("none for a selector with no data"), so a wrong guess
judges nothing, and says so only after a whole run. The history can say
which stack is there in a few requests.

## What it is

`lapilli impact suggest`, under a new command `lapilli impact` so that
more about impact signals can go beside it later. One list in code,
`impactStacks` in `cmd/lapilli/suggest.go`: each stack's name, the metrics
its `--impact` reads, and the `--impact`. `TestImpactStacksDocumented`
fails when the list and the table in `docs/impact.md` differ, in order,
names or queries, or when a stack's metrics aren't those its query reads.

For each stack, in the list's order:

1. **Its series.** One request a metric, `count(SEL)` over the period at
   an hour's step, or coarser past 41 days (`source.SeriesCount`, the step
   `source.FirstSample` already asks where an impact signal's data begins
   at): the most series at once. SEL is the selector of the metric that
   reads every request, `istio_requests_total{reporter="destination"}`,
   not the failures': a healthy service has no 5xx series, and the bare
   metric, scraped without a label the query filters on (`reporter`),
   would count series the query never reads. The test holds each such
   selector to be the one the others narrow.
2. **Its windows**, for a stack found: its `--impact` read over the period
   at `--step`, and its windows found by `incident.FromImpact` with
   `--impact-for` 5m, as `lapilli verdict --impact` finds those in the
   period.

It prints the stacks found with their series and windows, the stacks not
found on one line, those not known (a request failed) with why, and each
found stack's `--impact '...'`, quoted for a shell. `--json` gives the
same, with an id for each stack that stays.

## It chooses none

The stacks are listed in the list's order, whatever they found: no score,
no "best". The threshold is the one in the table, and a team's own
should be what its users notice; label names depend on how each stack is
scraped. The output says both. A person reads it and gives `--impact` to
`lapilli verdict`, or writes their own.

## Flags

Those of `lapilli verdict` that reach a Prometheus and say the period,
read by verdict's own code (`readFlags`, split from `parseFlags` for it)
and checked as verdict checks them (`validHistory`): `--prometheus`,
`--gcp-project`, `--auth`, `--aws-region`, `--bearer-token-file`,
`--max-rate`, `--max-samples`, `--since`, `--from`, `--to`, `--step`,
`--tz`, `--timeout`, and `--json`. Any other flag is refused, so that
`--rules` or `--impact-for` given by habit doesn't pass unread.

## Cost and bounds

Asked one request at a time, through the client verdict asks with: its
`--max-rate`, `--max-samples`, `--timeout` (10m), the five minutes a
request and its retries.

- Six counts, each one series of a thousand points at most.
- For each stack found, its `--impact` over the period, a day a request
  at a minute's step: seven for the default week. Each query is summed by
  a label, so its answer is a series a service, held to `--max-samples`.

A query the history refuses as written (`prom.Refused`: HTTP 400 or 422,
an answer too large) leaves that stack not known, and the others are
asked. Any other failure is the history's (not reached, not let in,
turned away to the end, the timeout): the stacks from it on are not
known, with the same why, and not asked, so an unreachable history costs
one request's retries, not six. It exits 0 when every stack was asked, 1
when one wasn't, and 2 when the flags are wrong.

## What it never does

It writes no file, sends nothing but its reads to the history, keeps no
ledger, and asks no model. What it prints never holds the token; an error
the history's client gives names its URL, as verdict's do.

## Alternatives

- **The metadata APIs** (`/api/v1/label/__name__/values`,
  `/api/v1/series`): not every Prometheus-compatible API serves them
  alike, or over a range. A range query is what every history Lapilli
  reads answers already.
- **Choosing the stack with the most windows**: more windows can be a
  threshold too low as well as more impact; the person who knows the
  services chooses.

## Not in this design

- Suggestions on `lapilli serve`'s Settings page, beside the signals.
- Stacks beyond the table: a row in `docs/impact.md` and an entry in the
  list, which the test keeps together.
- Checking the labels a query groups by (`destination_service_name`).

## Open questions

1. A Mimir or a Cortex with tenants and no gateway: verdict sends
   `--ruler-tenant` to `--prometheus` only with `--ruler`, so suggest
   doesn't take it. A tenant flag of the history's own would serve both.
