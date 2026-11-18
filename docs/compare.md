# Before a change is merged

`--compare` sets the rules proposed beside the rules in place: both are
replayed over the same period and judged against the same incidents, and
the report shows what the change does. Each rule that differs gets its
stones and severity before → after, firings are counted by severity, and
each incident caught differently is listed with both catches. Rules alike
in both are replayed once, for both; `--changed-only` doesn't replay them
at all, which is much faster, but then an incident shows only the changed
rules' catches.

```
$ lapilli verdict --rules proposed/ --compare alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)
Rules compared: 1 changed, 1 added, 0 taken away, 42 the same
Firings 148 → 42
By severity: critical 125 → 16, warning 23 → 26
Of 3 incidents: with no alert 1 → 0

RULE           SEVERITY     FIRED    ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx   critical     118 → 9  0 → 0     118 → 9    0% → 0%    41 → 2
DiskFillingUp  – → warning  – → 3    – → 1     – → 2      – → 33%    – → 0

INCIDENT            START
INC-43 Batch stuck  2026-10-31 02:40
  before  ✗ nothing fired
  after   DiskFillingUp +4m
```

Without `--incidents` or `--impact`, `--compare` still runs: the firings
are counted, not judged, and the report leaves out what only incidents
could say (caught, nothing, precision, the incidents caught differently),
keeping how often each rule fires and re-fires. Rules left alone are
replayed too, once, so an incident one of them catches is caught on both
sides: a longer `--since` judges against more incidents, and takes longer.
In CI, run it on each pull request that changes the rules
([Pull request backtests in CI](pr-backtests.md)).

## What a rule is expected to do

`--expect FILE` holds the rules that differ to what is expected of them,
written beside them ([docs/design/expectations.md](design/expectations.md)):
how many times each may fire in a week, how many of those while people
slept (`--sleep-hours`, 22:00–08:00 by default), and the incidents it must
keep catching, within how long. A rule is named, or a group's rules:

```yaml
expectations:
  - rule: CheckoutErrors
    budget:
      week: 10       # firings in each whole week of the period
      asleep: 2      # of them, while people slept
    catches:
      - incident: INC-43
        within: 5m
  - group: payments
    budget: {week: 20}
```

A budget is held to each whole week of the period, counted back from its
end, and the week that fired most is shown; a period shorter than a week
is scaled to one, and says so. A catch is judged as the verdict judges
incidents: an incident not in the period, or no incident given, is not
known, never met or broken; a rule the change takes away catches nothing.
The report gets a section, each expectation before → after: met, broken
by this change (met before, or a rule the change adds), broken before too
(which the change isn't held to), or not known.

```
Expectations: 1 broken by this change, 0 broken before too, 2 met
Budgets are held to each of the period's 2 whole weeks, counted back from its end: the most in one is shown.
RULE            HELD TO                                 BEFORE  AFTER  
CheckoutErrors  at most 10 a week                       3       41     ✗ broken by this change
CheckoutErrors  at most 2 a week while people slept     0       1      met
CheckoutErrors  catches INC-43, within 5m               +2m     +2m    met
```

Entries for the same rule or group are made one when read, and the same
budget or incident given twice is refused. A name several rules share is
told apart in a run (`CheckoutErrors [severity=page]`): `rule:
CheckoutErrors` holds each of them, and the name told apart one. A rule a
change moves out of a group that held it is said. `--expect-before FILE` gives the expectations as they were,
for the rules in place: a budget the change raises is held to its old value
before it, and the report says it was raised; one it adds is held on both
sides and said new; one it takes away, whole or a field of it, is still
shown, held to what it was, and fails nothing, as a budget raised doesn't:
loosening is for the reviewer to see. `--fail-expectations` exits 1 when the
change breaks one; `--expect-required` when it adds a rule no expectation
holds. Nothing here needs incidents: a budget holds a rule with none
written down. The file is read strictly, a key it doesn't know an error,
1 MiB, 2,000 expectations and 2,000 catches at most, and before anything is replayed. The backtest recipes read it from both
commits (`EXPECT`, [Pull request backtests in CI](pr-backtests.md)). The rules that differ are held to 100,000
expectations at most, each rule to each budget and catch that holds it (a
group's catch holds every rule of the group the change touches); past that,
the run fails and says so.
