# Judging refined: a lead per signal, episodes beside firings, a person's verdict on one firing, and rows by label

Status: proposed. Under [GOVERNANCE.md](../../GOVERNANCE.md) this changes
how a firing is judged (a lead of a signal's or a rule entry's own, a
person's word that turns one white stone black, and rules judged by
nothing) and adds new kinds of data to the ledger (episodes, and people's
verdicts on firings), so it is designed first, as a pull request open for
at least a week. The direction of items 1 to 3 was decided by the
maintainer on 2026-11-19; this note works out how. Item 4 is a proposal:
whether to build it, and its shape, are open ("Open questions"). Each item
is built as its own pull request, in the order of the items ("Steps").

It builds on [signals.md](signals.md) (named impact signals and rule
entries, built from 0.53.0 to 0.58.0), whose open question 4 item 1
closes; on [marked.md](marked.md) and [goals.md](goals.md), whose counts
and measures items 2 and 3 add to; and on deciding impact windows and
"This was real" ([serve.md](serve.md), "Incidents and settings").

## Why

A team deploying Lapilli measured it against its own alerts for a week,
and found four things. Their numbers are rounded and their names left out:

- **A cause comes before its incident by more than `--lead`.** A memory
  rule fired 41 times in seven days. Two of its firings were white, 22
  were re-fires, and one of the two white ones was a coincidence: another
  service's incident, 13 minutes later. A rule that fires well before the
  incident it causes is black, and one that fires long is never white.
- **A firing is one alert, so one problem is many stones.** The same
  memory rule's 41 firings, 22 of them re-fires, were 17 posts in the
  team's alert channel. Precision and noisy are counted by firing, so they
  swing with how many pods an alert names and how often it flaps.
- **A person can't say one white stone is wrong.** Dismissing an impact
  window turns every firing it caught black. The team counted by hand,
  from the channel, which alerts needed someone to act.
- **Rows are by team.** One team label holds two business domains, and the
  summary and goals can't tell them apart.

## 1. A lead per signal and per rule entry

### Today

`--lead` (15m by default, the Settings page's `lead` over it) is how long
before an incident's start a firing still catches it, one for every rule
and every incident: `verdict.Catches` takes `in.Start.Add(-o.Lead)`. A
firing that started earlier caught nothing ("it was on anyway",
[numbers.md](../numbers.md#a-white-or-a-black-stone)), however long it
lasts into the incident.

That fits a symptom: a 5xx rate rises with the incident. It doesn't fit a
cause. Memory pressure, restarts, a disk filling and a queue backing up
come before users are hurt, often by more than 15 minutes, and are black
for it. Raising `--lead` for everyone makes every rule catch more by
chance: each firing of every rule catches every window it agrees with in
the lead after it.

### What it is

A named signal, and a rule entry, may each give a `lead`:

```yaml
impact:
  signals:
    - {name: errors, expr: "...", for: 5m, match: [service, team]}
    - {name: probe, expr: "probe_success == 0", for: 2m, match: [service], lead: 5m}
  rules:
    - rules: ^(MemoryPressure|PodRestarting)$   # causes, before the errors
      signals: [errors]
      match: [service]
      lead: 1h
```

- **A signal's `lead`**: how long before one of its windows' start a firing
  still catches it, for every rule judged against it. A probe fails as
  users are hurt, so an alert has little reason to come much before it:
  5m.
- **An entry's `lead`**: for its rules, against its signals' windows, in
  place of each signal's. A cause rule's, as above.
- **Not given**, each follows the one above it, as `match` does: an
  entry's follows its signal's, a signal's follows `--lead`. Given, it is
  its own: a duration from 0 (from the window's start on) to 24h.

This closes [signals.md](signals.md)'s open question 4 ("`--lead` by
signal"); the pull request that builds step 1 says so there, and takes
"A `--lead` for each signal" out of its "Not in this design".

### Which lead a firing is judged by

`verdict.Options.LeadFor(rule, in)`, beside `Options.Agrees`, which
`Catches` takes in place of `o.Lead`, in order:

1. `in` is a window of a signal in effect, or the incident a confirmation
   of one wrote (which carries its window's signal, signals.md, "Windows:
   each signal's own"), and the rule's entry names that signal and gives a
   lead: the entry's.
2. `in` is such a window, and its signal gives a lead: the signal's.
3. Else `o.Lead`: an incident written down, copied in or read from the
   incidents file, a window of the signal with no name, a window of a
   signal no longer in effect (which `Agrees` takes as anyone's too).

**Written incidents keep `--lead`**, for every rule, as they keep the match
in effect: "an entry is about signals, which measure one kind of harm
each" ([signals.md](signals.md#which-rules-a-signal-judges)). A team whose
incidents are all written down gets nothing from this; open question 1.

**Judging otherwise keeps its shape.** A firing catches an incident when it
started from its lead before the incident's start to its end, and agrees
on its labels. One on since before its lead catches nothing: an entry's 1h
makes white a firing that began up to an hour before a window, and leaves
black one that began two hours before and is still on.

**Everything that reaches from an incident back to the firings it can
catch reaches by the longest lead**, `Options.MaxLead()`, the longest of
`--lead`, each signal's and each entry's: `window` and `incidentsMeeting`
in `internal/ledger/judge.go` (the incidents read for a span, `start_at <=
hi + lead`), `catches` there, the span a run judges (`lo - lead` in
`record.go`), the span `impact.go` pends, firings copied in
(`recorded.go`), and the incident page's timeline (`timeline.go`), which
draws from the longest lead any rule is judged by against that incident.
The near misses (`internal/verdict/nearmiss.go`) look from each rule's own
lead for the incident.

### `--settle`, a signal's `for`, a rule's `for`

- **A rule's `for`.** A lead is from the firing's start, which is after its
  condition held for its `for`. A rule with `for: 30m` whose condition
  begins 50 minutes before a window fires 20 minutes before it, and needs
  a lead of 20m or more. When its condition began (`held_from`) isn't
  judged, as today.
- **A signal's `for`.** A lead is to the window's start, which is where the
  signal first returned something, not its `for` later (signals.md:
  "`--impact-for` keeps a window's start where the signal first said users
  were hurt"). So a lead means the same whatever the signal's `for`.
- **`--settle`, and when a stone turns white.** A window is found by the
  run that reads it on for its signal's `for`, which comes `--settle` and
  up to `--every` after that. A firing that leads the window by an hour is
  black until then, and white once the window's span is judged, from
  `MaxLead` before its start. Today the same happens within 15 minutes.
- **What is worked out once a week is whole waits for it.** The digest,
  `goal.missed` and the leader's goals work wait until the cursor is past
  the week's end by the longest lead of a signal or an entry, and that
  signal's `for`. Today the digest waits only for the cursor to pass the
  week's end (`cursor.Before(to)` in `digest`, `internal/serve/work.go`),
  and a firing at the week's last minute that a window after midnight
  makes white is told as black; with no lead longer than `--lead`, it
  waits as today.

### Judged again when a lead changes

- **`--lead`**, on the command line or the Settings page: every firing
  again, a week at a time, as now (`JudgeAll`).
- **A signal's or an entry's lead**: nothing is read from Prometheus. The
  spans of the windows it changes are pended (`pend`), each from the
  longer of the old and the new lead before the window's start to its end:
  of a signal, its windows and the incidents its confirmations wrote; of
  an entry, the windows of its signals. `JudgePending` gives the firings
  there their stones again, and works out again what caught those
  windows. Its windows are 2,000 a week at most, so this is less than
  judging every firing, which a change to a signal's match or to the
  entries does today (signals.md, "Step 2, as built"); judging catches is
  all a lead changes, and catches are near windows.
- **In the same write** as the change (`SyncSignals` for the command
  line's, `ChangeSignals` for the Settings page's), counting a generation
  of the signals (`signals_gen`): a span pended is judged only by options
  read after it, as every change to the signals is.
- **Proposals.** A lead joins the digest of every signal's criterion, the
  entries and the match in effect that a proposal's backtest keeps
  (`staleImpact`): one waiting is held until it is backtested again with
  the lead.

### Where it is set

- **Flags.** `--impact-lead NAME=DUR`, in `lapilli verdict` and `serve`,
  beside `--impact-for`, `--impact-match` and `--impact-record`, refused for
  a name no `--impact` gives, as they are. The signal with no name
  (`--impact PROMQL`) is judged by `--lead`, as every incident is: there is
  no `--impact-lead DUR`. `lapilli verdict --impact` judges by the leads as
  serve does, and its text report says each signal's lead beside its
  `for`.
- **`--impact-file`.** `lead:` on a signal and on a rule entry, read
  strictly, as the rest of the file (`readImpactFile`).
- **The Settings page** (Incident criteria, as #116 built it): a signal's
  form gains Lead, empty to follow the page's own Lead above it ("15m, the
  lead above"); the rule entries, written as YAML, take `lead:`. Kept in
  `settings` as now: `impact-signal/NAME`'s JSON gains `lead` ("" to
  follow), and each entry of `impact-rules` its `lead`. An admin's, noted
  in the activity log with its before and after (`setting impact signal
  NAME`, `setting impact rule entries`).
- **The try.** A lead changed reads nothing from Prometheus: judged in
  memory over the ledger's firings and windows of the last four weeks, as
  a match changed is, it gives, rule by rule, the catches gained and lost,
  precision with the change and without, and, of the catches gained, those
  that agreed on no label (below). An admin's, one of the twelve tries an
  hour.
- **`/settings.yaml`** gives each `lead` in the chart's `impact` mapping.
- **`--config`**: nothing. A lead is a judging setting, the command line's
  and the Settings page's, as signals and entries are (signals.md:
  "`--impact-file` is the command line, not `--config`").

### In the numbers

- **The report** (`lapilli verdict --json`, `/api/v1/report`):
  `settings.impact_signals[].lead_ns` and `settings.impact_rules[].lead_ns`,
  each left out when it follows (`settings.lead_ns` then; a lead of 0 is
  given as 0), so that [numbers.md](../numbers.md)'s "incidents caught:
  counted again" counts with them.
- **`/api/v1/summary`**: `settings.impact_signals[].lead` and
  `settings.impact_rules[].lead`, left out when they follow;
  `settings.lead` stays `--lead`.
- **A median lead** ([numbers.md](../numbers.md#lead)) is how long before
  the incident's start its first catch came, so the windows of a signal
  caught from an hour before can have median leads up to an hour where
  they had 15 minutes. An incident's page says the lead its catches were
  judged by.
- **Outcomes.** A reviewed change is measured by its firings a week
  ([serve.md](serve.md#outcomes)), which a lead doesn't change; the
  incidents it caught before and after are both judged by the leads in
  effect, as every stone is.

### Coincidences

A longer lead catches more by chance. Each firing of a rule an entry gives
1h catches every window of its signals that it agrees with and that starts
in the hour after it: a rule that fires 41 times a week stands before 41
hours of possible windows a week, not ten. The measured coincidence came
within 15 minutes, from another service's window. A firing agrees with a
window on every match label one of them lacks (`Labels.Agree`): a node's
memory alert carries no `service`, and a window whose series met may have
lost it (item 3, "Today"). Three things keep it in view; none of them
changes `Agree`:

- **An entry's `match`** names the labels its rules' alerts carry, so
  `match: [service]` keeps `checkout`'s memory alert from `catalog`'s
  windows. It does nothing for an alert with no `service` at all.
- **Catches that agreed on no label are counted**: a catch where none of
  the labels it is matched on is carried by both the firing and the
  window, so any window at that time would have been caught. The rule's
  page says "3 of its 9 catches agreed on no label: they would have caught
  any service's window"; the try counts them among the catches a lead
  gains; MCP's `rule` gives `catches_on_no_label`. A count, not a stone.
  Open question 2 asks whether an entry may require a label.
- **A person's verdict** (item 3): a catch that was a coincidence is marked
  "in the window but unrelated", and that firing alone turns black.

## 2. Episodes beside firings

### Today

A firing is one alert, a rule and a label set, from when it fired to when
it resolved ([numbers.md](../numbers.md#which-firings-and-incidents-a-report-holds)).
An alert that names its pod is a firing per pod; one that resolves and
fires again within `--refire` (1h) is a second firing, marked `refired`
and counted as one more ([numbers.md](../numbers.md#re-fired)). So the
memory rule's 41 firings, 22 of them re-fires, were 41 stones where the
channel showed 17 posts, and a rule is noisy (10 judged or more, under 20%
caught) as much by how many targets fire at once, and how often they flap,
as by how often anyone was told.

Every number keeps counting firings. Episodes are counted beside them,
and change none of them.

### What an episode is

Firings of one rule with the same values of its match labels, each
starting while another of them is on, or within `--refire` of the latest
end among them so far. The first such firing of a day, in `--tz`, starts
a new one.

- **Its match labels** are the labels its rule's firings are matched on
  against incidents written down: its rule entry's `match` when it gives
  one, else the match in effect (`--match`, or the Settings page's). Of
  each, the firing's value, or none for a label it doesn't carry. The
  labels that tell its alerts apart and aren't matched on (`pod`,
  `instance`) don't part them. With no match at all, every overlapping
  firing of a rule is one episode, as each of them could catch any
  incident.
- **Every re-fire is in its alert's episode**, as its alert has the same
  rule and labels, unless midnight came between. So the memory rule's 41
  firings with 22 re-fires are 19 episodes at most (one more for each
  re-fire after midnight), and fewer where alerts of several pods
  overlapped: near the channel's 17 posts.
- **Excused firings take no part**: a quiet window's firing neither starts
  nor continues one, as it is in no number.
- **Midnight parts them** so that an episode is counted in one day's tally
  and worked out in one day's write ("In the ledger", below); an alert
  that flaps all week is seven episodes, not one.

`lapilli verdict` works them out in `verdict.Judge`, from the firings of
the period, by rule, match values and start, its days in `--tz` (the
machine's zone when it isn't given, as its hours are already counted in
it); `lapilli serve` keeps them in the ledger.

### How an episode is judged

- **White** when any of its firings is white.
- **Black, judged**, when none is white and its first firing was judged.
- **Not judged** when none is white and its first firing wasn't: no
  record at its time, or its rule judged by nothing (item 3). A firing
  that caught something was always judged, so this is the only way an
  episode isn't.
- **Its day, its week, its marks and the signals it waits for** are its
  first firing's, so that it is counted in one row of the tallies
  and judged by that row's record, as each firing is by its own.
- **People's verdicts** (item 3) count as the stones they leave: an
  episode whose one white firing a person said was unrelated is black.

**Precision by episodes** is its white episodes over those judged.
**Noisy by episodes** is noisy by the same thresholds (`noisy_at`) over
episodes: 10 judged or more, under 20% white.

**Noisy keeps its meaning.** `noisy` is by firings, and goes on deciding
what it decides today: which catches are a noisy rule's (`noisy_only`, a
median lead's first catch), the suggestions of a longer `for`, what a rule
adds to the others. `noisy_episodes` is beside it, shown, and decides
nothing in this design (open question 3). The rule's page says the two
together when they differ: "Noisy by firings, not by episodes: 41 firings
in 17 episodes."

### With marked windows, goals and the record

- **Marked windows.** An episode is during a kind when its first firing is
  marked with it ([marked.md](marked.md)); `marked_days` and
  `marked_weeks` count episodes and those caught too, and the summary's
  `marked.KIND` gains `episodes`, `episodes_caught` and
  `episodes_precision`.
- **Goals** ([goals.md](goals.md)). Two measures, from the summary's new
  field: `episodes.total` (lower is better, needs no record, with `each:
  KIND`) and `episodes.precision` (higher, needs the record, with `each:
  KIND` and `no_worse_than: before`). The goals' table in goals.md gains
  their rows in the step's pull request.
- **No record.** An episode not judged is in `episodes.unjudged`, in no
  precision and in no noisy, as a firing not judged is.

### Where it shows

- **A rule's page**: in its numbers, episodes beside firings, precision by
  each, noisy by each; on its latest firings, which episode each is of,
  and how many firings it held.
- **The rules list**: an Episodes column beside Firings, and
  `noisy:episodes` in the search box, a facet beside `noisy`.
- **The overview**: the noisiest rules' panel keeps counting black stones;
  its table gains each rule's episodes.
- **The digest**: its line of each noisy rule says its episodes ("41
  firings in 17 episodes, 2 caught"), and a rule noisy by firings and not
  by episodes says so.
- **`/api/v1/summary`**: each row gains `episodes`:

  ```json
  "episodes": {"total": 17, "caught": 2, "unjudged": 0, "precision": 0.12, "noisy_rules": 3}
  ```

  `noisy_rules` is how many of the row's rules are noisy by episodes in the
  week. `precision` is null as the row's is. `schema` stays 1: a field
  added.
- **`/api/v1/report`** and `lapilli verdict --json`: `rules[].episodes`†
  (`total`, `white`, `unjudged`†, `noisy`) and `firings[].episode`†, the
  episode it is of, numbered from 1 in the report in order of their first
  firing.
- **The CSV**: an `episode` column, at the end.
- **`/metrics`**: `lapilli_rule_episodes` and `lapilli_rule_episodes_caught`
  (`rule`, [`severity`], [`team`]), over the last seven days, as the rule
  metrics are; in `lapilli verdict --metrics` too.
- **MCP**: `rules` and `rule` give each rule's episodes and
  `noisy_episodes`; `summary` the row's `episodes`; `firings` each
  firing's episode.
- **[numbers.md](../numbers.md)**: a section, "Episodes", with blocks that
  count them again: their stones and counts from `firings[].episode`, and
  which firings make one, of a report in UTC, where days are UTC's.
- **i18n**: every new text in the four languages.

### In the ledger

- **`firings.episode`**: the id of the first firing of its episode, its
  own for a first, 0 for one a quiet window excused; and
  **`firings.episode_white`**, on a first firing, whether any firing of
  its episode is white.
- **`days`, `weeks`, `marked_days` and `marked_weeks`** gain `episodes`
  and `episodes_white`, counted from the first firings, by the key they
  have (day or week, rule, signals, and a kind for the marked ones).
- **Worked out in the tally.** `tally` (`internal/ledger/judge.go`)
  counts whole days, the days its span meets. Before it counts, it walks
  the firings of those days once, by rule, match values and start, their
  labels as `stones` reads them, and writes `episode` and `episode_white`
  where they changed. As an episode never crosses midnight, nothing
  outside those days can change: a write reaches no further than the
  tally already does. A firing's stones are given before (`stones`), so
  `episode_white` follows them.
- **Followed by every change.** A change to `--match`, `--refire` or the
  entries' `match` judges every firing again today; episodes follow it.
  An incident written, a window decided, a person's verdict: the span's
  days, as the tally counts them again.
- **Upgrading** adds the columns at 0 and pends every firing kept (meta
  `pending`, from the oldest), so the worker counts episodes a week at a
  time, as `JudgePending` judges; until it has been through a week, its
  rows say episodes aren't counted yet (`episodes: null` in the summary,
  "not counted yet" on the pages), by how far it got (meta
  `episodes_to`). No stone changes.

## 3. A person's verdict on one firing

### Today

- **An impact window** is confirmed, which writes an incident (an `L-` id)
  carrying its signal and match, or dismissed, which stops it counting
  (`Ledger.Decide`): every firing that caught only it turns black.
- **A black stone** has "This was real" (`Ledger.Real`), which writes an
  incident from the firing's start, end and match labels: it turns white,
  and every other firing that agrees with it.
- **Nothing between.** A cause rule that fired during a window by chance,
  or caught a window whose labels were thinned where its series met, is
  white, and the only way to make it black is to dismiss the window, for
  every firing. `incident.FromImpact` keeps, of windows that overlap, only
  the labels all of them have:

  ```go
  delete(in.Labels, k) // not all have it: no constraint
  ```

- **Whether a firing needed someone to act** is nowhere in the ledger. The
  deploying team counted it by hand, from the alert channel.

### In the window but unrelated

On a white firing, a person says it is unrelated to what it caught. That
firing alone turns black:

- **What is kept**: the incidents it caught when it was said, by key, each
  with its start and end as they were then; 16 at most, and a firing that
  caught more is refused it.
- **Its stone.** Black when every incident it catches is among them. If it
  catches another (an incident written down after, a window found under
  another key), it is white by that one, and the verdict says the window
  moved under it (below).
- **The incidents.** That firing doesn't catch them. What caught each is
  worked out again from the other firings (`catches`): a rule none of
  whose other firings caught it hasn't caught it, and an incident nothing
  else caught is missed. That is what the person said: the memory alert
  didn't catch the other service's incident.
- **The rest of its stones.** Re-fired is as it was. Turned black, it is
  excused when it came in a quiet window that matches it, as any black
  firing is (`excused := !white && excuses(r.f)` in `stones`).
- **Where.** On Alert history, beside each white firing, as "This was
  real" is beside each black one; on an incident's page, a link from each
  rule that caught it to its firings that did. Not on a black firing,
  which caught nothing to be unrelated to. A firing made black so says
  "Black: unrelated to INC-12, by NAME on DATE", with Undo, and not "This
  was real".

### Did it need action?

Each firing has an answer: yes, no, or none (the default). It changes no
stone. Asked on Alert history (one at a time, and the firings picked, 100
at a time, as "This was real" takes them), on a rule's latest firings and
on an incident's page. Counted (below):

- **answered**, **yes**, **no**, and the share of yes among those
  answered: what the deploying team tracks;
- **yes on a black stone**: someone had to act, and it caught nothing
  written down; the line offers "This was real";
- **no on a white stone**: it caught something, and nobody had to act, a
  coincidence or a thing that healed itself; the line offers "Unrelated".

### Who, and the trail

- **"Unrelated"** is for editors of every team and admins, whom `s.write`
  lets decide an impact window and press "This was real": it changes a
  stone, and the incidents it takes a catch from may be another team's
  (the coincidence above was another service's).
- **"Needed action"** is for a team's editors too, of their team's rules'
  firings (`s.teamWrite`, the ledger refusing another team's in the write,
  `inScope`), as their notes on rules are: it changes no stone.
- **Viewers** set neither; `--read-only` refuses both.
- **The trail.** The verdict keeps who set it and when (`set_by`,
  `set_at`). Every set and every undo is noted in `changes` with its
  before and after (`verdict on RULE {labels} at START`), as an incident's
  decision is (`change` in `internal/ledger/incidents.go`), and the
  activity log shows it. Outcomes count them as use, as every edit through
  the UI and the API.

### Through judging again

Judging reads a person's verdict, and never writes it but to say it
moved.

- **Kept by its alert and start**, not by the firing's id. A run replaces
  the firings of its period (`DELETE FROM firings WHERE start_at >= ?` in
  `record.go`), and they get new ids; the same firing is found by its
  alert and its start, as a run finds one it had (`WHERE series_id = ? AND
  start_at = ?`). `stones` reads the verdicts of the firings it judges,
  joined on both, and gives the stones as above.
- **Its firing found at another start** (late samples moved it a step):
  the run's write moves the verdict to the firing of its alert that is on
  at its start, or starts within two `--step`s of it, and marks it moved
  ("its firing starts at 14:03 now"). With none, it stays, on no firing,
  listed on the Cleanup page with what it said, and is pruned with
  `--keep`.
- **A window found again under another key**: `remapImpact`
  (`internal/ledger/record.go`) moves the tasks and comments of a window
  no longer there to the new one that covers most of it; it moves the
  verdicts' keys with them, in the same write, and marks them moved.
- **An incident whose span changed** (a window read again, a written
  incident's times edited): the verdict holds, since the firing still
  isn't that incident's, and is marked moved ("INC-12 was 14:00 to 14:40
  when this was said; it is 13:20 to 14:40 now").
- **An incident it now catches that it wasn't said unrelated to**: white
  by it, and marked ("it catches INC-15 now, which it wasn't said to be
  unrelated to").
- **A window confirmed**: the incident the confirmation writes takes the
  window's place in the verdicts naming it, in the same write (`Decide`),
  so confirming changes no person's verdict, as it changes no rule's
  catch.
- **A window dismissed, an incident deleted**: nothing is caught; the
  verdict stays, saying the incident is gone, harmless.
- **A verdict marked moved** says so on its firing's line; the overview's
  "Look at these first" counts them ("3 people's verdicts to look at
  again"), and the Cleanup page lists them. Said again, or undone, it is
  not marked any more.
- **Every re-judging alike**: `JudgeAll`, `JudgePending`, a lead changed,
  an incident written. A person's word stands over every change of
  settings.
- **Replays judged in memory** (a proposal's backtest, the try, the
  digest's near misses) give a verdict to a firing replayed with its alert
  and start; a changed rule's firing at another start has none. A verdict
  set in a proposal's weeks since its backtest makes it stale, as an
  incident written there does (`staleRecorded`): "a person said a firing
  of its rule was unrelated in its period since it was backtested".

### Rules judged by nothing

Some rules aren't about harm to anyone: a watchdog that always fires, a
capacity forecast, a reminder. Today a rule entry must name signals
(`readImpactFile` in `cmd/lapilli/signals.go` and `readEntries` in
`internal/serve/signalsweb.go` refuse `signals: []` with "no signals: name
the signals its rules are judged against"), so the deploying team named a
signal `none` that never returns (`vector(0) > 1`, `record: false`) and
pointed entries at it. That works in part: those rules catch no window and
have no record by reading, so they are counted, not judged; but a week an
incident was written down in keeps their record, and they are black that
week, against incidents they were never about. And the signal is read
every run, one of the eight, and shown on the Settings page as one.

An entry may say instead that its rules are judged by nothing:

```yaml
rules:
  - rules: ^(Watchdog|CapacityForecast.*)$
    judged: false
```

- **Counted, not judged.** Their firings are counted: firings, re-fires,
  episodes, hours, deploys and marks. They are judged by nothing: never
  white, never black, in no precision, never noisy. They catch nothing,
  written incidents included: an incident only they would have caught is
  missed, as it was for them.
- **In the numbers** they are `unjudged`, as firings with no record are,
  and said apart: the report's `rules[].not_judged`† and
  `firings[].not_judged`†, the summary's `not_judged` beside `unjudged`.
  The rule's page says "Not judged: rule entry 3", not "No record".
- **Without signals.** An entry gives `judged: false` or `signals`, not
  both. A file whose `rules:` are all `judged: false` needs no `signals:`,
  and goes with `--impact PROMQL` (the signal with no name) or with no
  impact signal at all; so do the Settings page's entries. `signals: []`
  stays refused, its message naming `judged: false`.
- **Which entry is a rule's** is found as now: the first, in order, whose
  pattern matches its name, else whose labels it has.
- So far an entry was about signals ([signals.md](signals.md)); this makes
  entries what a rule is judged by, signals one of the things they say.
  Judged by nothing is judging, not a quiet window: a quiet window
  excuses firings that caught nothing, and these catch nothing by
  definition, so it would excuse every one of them and count them in no
  number but `excused`.

### API and MCP

- **The UI.** `POST /firings/{id}/verdict` (`unrelated` set or not,
  `action` yes, no or none, `note`), as `POST /firings/{id}/real` is; `POST
  /firings/verdict`, the firings picked with one answer to "needed
  action", 100 at most, as `POST /firings/real`. Both under `s.teamWrite`,
  the handler refusing "unrelated" to a team's editor (`scoped`), as the
  roles above say.
- **The API**, by the firing's alert and start, as a report gives them (a
  report's firings have no id):
  - `PUT /api/v1/verdicts`: `{"rule", "labels", "start", "unrelated",
    "action", "note"}`; 404 with no such firing; 409 for "unrelated" on a
    firing that caught nothing.
  - `DELETE /api/v1/verdicts` with `rule`, `labels`, `start`: undone.
  - `GET /api/v1/verdicts?from=&to=`: the verdicts of the firings that
    started in the period, 31 days at most, each with its firing, what it
    says, its note, who set it and when, and whether it moved; a viewer's.
  - Each in [api.md](../api.md) with the role it needs, as
    `TestAPIDocumented` holds; `lapilli_rule_action` in
    [metrics.md](../metrics.md).
- **MCP reads them, and sets none.** `firings` gives each firing's verdict
  (unrelated to what, needed action, moved); `rule` its counts; `summary`
  the row's `people`. MCP reads, and with `--mcp-compare` replays, and with
  `--mcp-propose` proposes a change to a rule (`propose`, `backtest_again`)
  that waits for a person's approval ([serve.md](serve.md#mcp),
  [changes.md](changes.md)). A verdict has no approval: set, it is the
  stone. An agent that read a chat and said which alerts needed action
  would be an agent's word, which this field isn't (open question 6).

### How reports count them

The stones keep the people's verdicts in them, as they keep an incident a
person wrote down: `caught` and `precision` are what the stones say. Beside
them, each summary row gains `people`:

```json
"people": {"unrelated": 2, "caught_by_lapilli": 14, "precision_by_lapilli": 0.35, "real": 1,
           "action": {"answered": 30, "yes": 9, "no": 21, "share": 0.3, "yes_black": 1, "no_white": 4}}
```

- `unrelated`: firings of the row white by Lapilli and black by a person.
- `caught_by_lapilli`, `precision_by_lapilli`: as Lapilli judged them,
  every "unrelated" white.
- `real`: incidents written from a firing of the row's rules ("This was
  real") that started in the week: the other way a person turns a stone,
  counted apart for the first time (below, `incidents.real_of`).
- `action`: those answered, yes and no, the share of yes (null with none
  answered), and the two crossings above.

Elsewhere:

- **A rule's page**: beside its precision, the precision as Lapilli judged
  it when people's verdicts moved it ("31%; 35% as Lapilli judged it: 2
  firings were said to be unrelated"), and its answers to "needed action".
- **The report**: `rules[].unrelated`†, `rules[].action_yes`† and
  `rules[].action_no`†; `firings[].unrelated`†, the ids of the incidents it
  catches by Lapilli's judging that a person said it is unrelated to (so
  that white by Lapilli is `white`, or `unrelated` not empty), and
  `firings[].action`† (`yes`, `no`). Not who: a report goes outside
  (`--redact`), and who is `/api/v1/verdicts`'.
- **The CSV**: `unrelated` and `action` columns, at the end.
- **Goals**: a measure, `action.share` (higher is better, needs no record,
  not with `each: KIND`), not known with none answered (`no_value`). The
  Goals page says, beside a goal whose measure people's verdicts move
  (precision, `critical.share_caught`, the incidents and their lead,
  `action.share`), how many verdicts of the week went into it.
- **`/metrics`**: `lapilli_rule_action` (`rule`, [`severity`], [`team`],
  `answer`: `yes`, `no`), over the last seven days.
- **The digest**: a line, "People said 2 firings were unrelated, and
  answered 30 of 120 firings: 9 needed action."
- **[numbers.md](../numbers.md)**: a section, "A person's verdict", with
  its blocks counted again, and the stones' section saying a firing a
  person said is unrelated is black.

### In the ledger

- **`verdicts`**, a row a firing a person said something of: `series_id`
  and `start_at` (the key), `unrelated` (a JSON list of the incidents, each
  `{key, start, end}`, `[]` for none), `action` (`''`, `yes` or `no`),
  `note` (500 characters at most, one line), `set_by`, `set_at`, and
  `moved` (`''`, or why).
- **`firings.person`**: bits `stones` writes from the verdict, for the
  tallies: 1, unrelated (white by Lapilli, black by a person); 2, needed
  action; 4, didn't.
- **`days` and `weeks`** gain `unrelated`, `action_yes` and `action_no`.
- **`incidents.real_of`**: the alert and start "This was real" wrote it
  from, `''` for any other; incidents written so before the upgrade are
  `''`, and counted as none.
- **`lapilli ledger copy`** carries `verdicts` with the rest; `--keep`
  prunes a verdict with its firing, and one on no firing once its start is
  older than `--keep`, as a firing's would be.

## 4. Rows by labels other than team: a proposal

Whether to build this, and its shape, are the maintainer's to decide (open
question 8). What follows is one shape, worked out far enough to be
judged, and two others.

### Today

Every row of `/api/v1/summary`, the Goals page and the overview is for
everyone or a team (`summaryOf(ctx, w1, w2, who []*ledger.Team, ...)`): a
team's row counts its rules' firings (`own.rule[rule] == team.ID`) and the
incidents it owns, by a value of its labels or by catching them. A team
owns a rule by the rule's note, name or labels (`owners` in
`internal/serve/owners.go`), from the rule's labels, not its alerts'.

So when one team, `shop`, holds two domains, `checkout` and `catalog`, and
a rule over every service (`HighErrorRate`) is `shop`'s, with alerts that
carry `domain`, neither row says how each domain's alerts did, and no goal
can be one domain's.

**Use what exists**: two teams, `shop-checkout` and `shop-catalog`, by
rule patterns or rule labels. That works where each rule is one domain's,
and not for a rule whose alerts are of both, since a rule is owned whole.

### Groups, by the alerts' labels

`groups:` in `--config`, its seventh key after `teams`, `windows`,
`reports`, `grants`, `goals` and `marked` (`configKeys` in
`internal/serve/declared.go`):

```yaml
groups:
  - name: checkout
    team: shop                    # whose rows it splits; none for everyone's
    labels: {domain: checkout}
  - name: catalog
    team: shop
    labels: {domain: [catalog, search]}
```

- **What a group holds**: the firings of its team's rules (every rule's,
  with no team) whose alerts' labels have, for each label it names, one of
  its values; and the incidents with those values, as a team owns
  incidents by its labels. The first group, in order, whose labels an
  alert has is its group. A firing of no group is in its team's row and
  no group's.
- **Rows.** `/api/v1/summary?team=shop&group=*` gives `shop`'s row and a
  row for each of its groups, each with `group`; `group=checkout` one
  group's. Goals take `group:` beside `team:`. The page context's team
  picker gains a team's groups, so the overview and the Goals page read
  them as a team's.
- **In the ledger**: `groups`, as `teams` is (declared, kept to the file by
  `ledger.Reconcile`); `series.grp`, each alert's group, worked out from
  its labels when it is first kept, and again for every alert when the
  groups change, as signal ties are (`series.signals`); `group_days` and
  `group_weeks`, by day (or week), group, rule and signals, counting what
  `days` counts, episodes and people's verdicts included, in the same
  tally, and written only while groups are declared.
- **Bounds**: 32 groups, 16 labels each, 64 values a label; a row per
  group per week, read from its own tallies.

### Two other shapes

- **`split:` on a team.** `split: domain` gives a row for each value its
  rules' alerts carry, 20 at most, the rest as one row, "other". Nothing to
  name, but rows come and go as values do, and a goal can't name one
  ahead.
- **Groups by the rules' labels**, owned as teams own rules: no new tally,
  since `days` is already by rule, and a group is a second ownership. A
  rule over both domains is one group's, which is the case that started
  this.

## What stays the same, and what changes

[numbers.md](../numbers.md) lists what keeps its name and its meaning, and
the pull request that builds each step changes it with the code, as it
says:

- **Step 1** changes "A white or a black stone": a firing catches an
  incident from the lead it is judged by (its entry's, its signal's, or
  `lead_ns`), and its recount block reads `lead_ns` from
  `settings.impact_signals` and `settings.impact_rules`. It adds those two
  fields to "The report's fields". Every number of a ledger with no lead
  of a signal's or an entry's stays as it is.
- **Step 2** changes no number. It adds "Episodes", `rules[].episodes`†
  and `firings[].episode`† to the report's fields
  (`TestReportFieldsStable`), the CSV's `episode` (`TestCSVColumns`), and
  `lapilli_rule_episodes` and `lapilli_rule_episodes_caught`
  (`TestMetricsStable`).
- **Step 3** changes the stones of the firings a person says are
  unrelated, and of the rules an entry says are judged by nothing, and
  nothing else. It adds "A person's verdict", the report's
  `rules[].unrelated`†, `action_yes`†, `action_no`†, `not_judged`† and
  `firings[].unrelated`†, `action`†, `not_judged`†, the CSV's `unrelated`
  and `action`, and `lapilli_rule_action`. "No record"'s recount of
  `unjudged` counts `not_judged` too.
- **The summary** keeps `schema` 1 throughout: `episodes`, `people` and
  `not_judged` are fields added, and no field changes its meaning.
- **`verdict.Semantics`** isn't bumped by any step: a ledger with no lead,
  no verdict and no entry judged by nothing judges as before, and episodes
  judge nothing.

## In the ledger: schema numbers

Steps 1, 2 and 3 each add a schema, in a pair (`schema/NNN.sql` and
`schema/postgres/NNN.sql`), each with its row in
[upgrading.md](../upgrading.md) (`TestSchemasDocumented`):

- step 1: `impact_signals.lead_ns` and `impact_rules.lead_ns` (NULL to
  follow), so that the ledger holds the leads its options are judged by,
  as it holds the rest of each signal (`SignalDef`, `sameDef`) and entry
  (`entryKey`);
- step 2: `firings.episode`, `firings.episode_white`, and `episodes` and
  `episodes_white` on `days`, `weeks`, `marked_days` and `marked_weeks`;
- step 3: `verdicts`, `firings.person`, `unrelated`, `action_yes` and
  `action_no` on `days` and `weeks`, `incidents.real_of`, and
  `impact_rules.judged`.

Each takes the next free number when its pull request opens: `migrate`
numbers the ledger's schema by how many files there are
(`internal/ledger/ledger.go`), and Postgres refuses a ledger whose last
file's number isn't SQLite's (`migratePostgres`), so no number is held for
a design. Going back takes the ledger as it was, so a deployment keeps a
copy of it (a snapshot of its disk, or of its database) before it
upgrades, as with every schema change.

## Cost and bounds

As counts and bytes, which the tests hold, not times:

- **Leads.** 0 to 24h each. `MaxLead` widens the incidents a span reads
  (`incidentsMeeting`) and the spans pended by as much as the longest lead
  is over `--lead`. A change pends a signal's windows' spans, 2,000 a week
  at most, and counts among the admin's twelve saves an hour, as the
  entries' saves do. Judging looks a firing's lead up per incident, from
  the map of entries `Signals.Index` makes once a change, as `Agrees`
  does: `TestScale` holds it by the counter of entries looked up
  (`Signals.Looked`).
- **Episodes.** A write's tally walks the firings of the days it meets,
  once, as the tally already counts them in SQL: a run's write, a day or
  two. `TestScale` holds it by the rows it reads and writes. Pages read
  the tallies' two columns more, never the firings: `TestPagesScale`
  holds the rows each page reads and its heap peak, the least of three
  runs, as now. `lapilli verdict` groups the period's firings once more,
  by rule and match values.
- **Verdicts.** One a firing at most, 16 incidents each, a note of 500
  characters. A verdict's write judges again its firing's span and the
  days it falls in: less than an incident's write, which judges from its
  lead before to its end. `stones` reads the verdicts of the firings it
  judges in one query by alert and start, along an index on
  `(series_id, start_at)`.
- **Rules judged by nothing.** One more field of an entry, looked up as
  the entry is.
- **Groups** (if built): 32, a row per group per week; `series.grp` worked
  out per alert as ties are.

## Security: as an attacker reviews it

**What can someone send before they prove who they are?** Nothing new.
Every new write is on the UI's port, behind its sign-in and roles: `POST
/firings/{id}/verdict`, `POST /firings/verdict`, and `PUT` and `DELETE
/api/v1/verdicts`, under `s.teamWrite`, which lets a team's editor in for
their team's rules alone, and the handler refuses "unrelated" to anyone
below an editor of every team; each body cut at 1 MiB, as `teamWrite`
cuts it (`http.MaxBytesReader`). Leads are set on the command line, in
`--impact-file`, or by an admin on the Settings page.

**Cross-site requests.** Each form posts to the mux behind
`http.NewCrossOriginProtection` (`internal/serve/web.go`), as every form
does: a page elsewhere can't post a verdict with a signed-in person's
cookie. `PUT` and `DELETE` are under it too; a caller with a bearer token
isn't a browser, and writes as the name the token gives.

**How much can a holder make Lapilli keep or judge again?**

- A verdict: one a firing, its note 500 characters on one line, its
  incidents 16; written again, it is the same row. Its write judges again
  one firing's span and its days, less than an editor's incident write
  already does; a bulk write is 100 firings, as the other bulk forms.
- A lead: an admin's, 24h at most, pending its signal's windows, among
  the twelve saves an hour.

**Where do the names they choose end up?** A note goes to the firing's
line and the incident's page, escaped and set apart in `<bdi>` as rule
names are; to `/api/v1/verdicts`; and to MCP's `firings` as data, which
the server's instructions already tell an agent to treat as data, not
instructions, as incident titles and notes. Never to the digest, Slack, a
notice, a report or `/metrics`. `set_by` is shown as every `updated_by`
is, a name the sign-in gave (`signedName`).

**Moving the numbers.** A person's verdict decides a stone, so it can be
used to make the numbers say something:

- **Up.** "This was real" already turns a black stone white, as writing
  an incident does; now the summary counts it apart (`people.real`).
  "Unrelated" can't make a stone white.
- **Down, and sideways.** "Unrelated" lowers precision, but it takes a
  catch away: it can make an incident missed, take a rule's credit
  (`first`, `alone`, `spare`), and raise a median lead, by taking away the
  catches that came after their incidents' start, so that those incidents
  are missed and left out of the median. A goal on the median lead alone
  can be met so. That is why it is an editor's of every team and an
  admin's, not a team's editors': the incident it takes a catch from may
  be another team's. Every one is noted with who and when, counted apart
  (`people.unrelated`, `precision_by_lapilli`), and said beside a goal
  whose measure it moved; open question 5 asks whether a goal may be held
  to Lapilli's stones alone.
- **Needed action** is the team's own word. A goal on `action.share` is
  shown with how many firings were answered, and changes no stone.
- **A lead** makes rules look better: more white. It is an admin's,
  noted with its before and after, given in the summary's and the
  report's settings beside every number it judged, and the try says how
  many of the catches it gains agreed on no label.

**Agents.** No MCP tool writes a verdict, a lead or an entry. An agent
holding a person's bearer token writes through the API as that person, as
it could write an incident; it is noted under that person's name.

The paths for the review: the two form handlers and `/api/v1/verdicts`,
the ledger's write of a verdict (one write: the firing read again in it,
as `Ledger.Real` reads it), `stones` reading verdicts, `remapImpact` and
`Decide` moving their keys, and `readImpactFile` and `readEntries` taking
`lead` and `judged`.

## Alternatives

- **Leads.**
  - *Use what exists: a longer `--lead`.* Every rule catches by chance
    more, symptoms with causes.
  - *Use what exists: a longer `for` on the cause rule.* It fires later,
    nearer the incident, so within `--lead`: later than the warning it was
    written to give.
  - *A firing on at an incident's start catches it.* A long-firing alert
    would be white; one on for days would catch everything in them. Left
    out, as decided: judging keeps its shape, and a cause is given a lead.
  - *A lead by team, or by severity.* A cause is a kind of rule, not a
    team; an entry names the rules.
- **Episodes.**
  - *Count episodes in place of firings.* Every number would change, and
    `refired` with them; decided: beside.
  - *Episodes by Alertmanager's groups*, from the notifications kept
    ([notifications.md](notifications.md)). Only where the notifications
    port is set up, and a group key is a receiver's routing, not the
    alert's.
  - *A cap of a day from an episode's first firing* in place of midnight.
    Where an episode ends would hang on where the one before began, so a
    change at one firing could move every episode after it, as far as the
    alert kept flapping; the tally's write would reach as far.
  - *Midnight in UTC.* Recounted easily from a report, and an episode at
    nine in the morning in Seoul would part for nothing.
- **A person's verdict.**
  - *Use what exists: dismiss the window.* Every firing it caught turns
    black, the ones that were the alert as well.
  - *Use what exists: a quiet window over the firing.* It excuses only
    what caught nothing; this caught something.
  - *A comment on the incident.* Counted nowhere.
  - *"Unrelated" for a whole rule against one incident.* Coarser than the
    firing, and the same form gets there one firing at a time.
- **Rules judged by nothing.**
  - *Use what exists: a signal that never returns*, as the team did
    (above).
  - *A note's status* (`keep`, `tune`, `retire`). An editor's, about what
    is to become of a rule, not how it is judged, which is an admin's.
- **Groups**: above, "Two other shapes".

## Not in this design

- An entry's lead against written incidents (open question 1).
- A label a firing must carry to agree (open question 2).
- Deploys by episode: `during_deploys` stays by firing.
- People's verdicts on firings inside a marked window, counted apart:
  `marked` counts what the stones say.
- Verdicts set by an agent, or copied in by a job from a chat's reactions
  (open question 6).
- An episode's own thresholds of noisy (open question 4).
- Groups that cross teams, until open question 8 is decided.

## Three cases

1. **A cause rule.** Team `shop`'s `MemoryPressure` fires on `checkout`'s
   pods some 40 minutes before its 5xx window, week after week, and is
   black: 30 firings, 1 white. An admin adds an entry, `^MemoryPressure$`,
   signals `[errors]`, match `[service]`, lead `1h`. The try says it gains
   6 catches, one of which agreed on no label: a node's alert with no
   `service`. Saved, the windows' spans of four weeks are pended and
   judged again; its precision reads 23%, and the incident page of each
   window shows the memory alert first, 38 minutes before. The one that
   agreed on no label an on-call engineer marks unrelated.
2. **Pods restarting together.** `PodRestarting` on `search`'s twelve pods
   fires for each pod every night, when a reindex restarts them all: 60
   firings in five nights, five episodes, all black. In the same week a
   real incident restarts one pod twice, caught: 62 firings, 2 white, 3%,
   noisy; 7 episodes, 2 white, 29%, not noisy by episodes. The rule's page
   says "Noisy by firings, not by episodes: 62 firings in 7 episodes." The
   team writes a quiet window for the reindex, which excuses those 60
   firings, and keeps the rule.
3. **A disk during a window.** `DiskFillingUp` on `checkout`'s database
   fires at 14:05, during `errors`' window for `checkout`, 14:10 to 14:40,
   and agrees on `service`: white. The on-call engineer knows it was a log
   rotation. On Alert history they mark it unrelated, and that it needed
   no action; the window is still caught by `HighErrorRate`. Two days
   later an admin changes the `errors` expression; read again, the window
   is 14:08 to 14:45 under another key. The verdict follows it, marked
   moved; the overview says one verdict to look at again, and the
   engineer says it again. That week the team answered 30 of its 120
   firings, 9 needing action: its goal of `action.share` at least 25% is
   met, "30 of 120 answered".

## Open questions

Decided on 2026-11-19, by the maintainer, and worked out above:

- **A lead per signal and per rule entry**, judging otherwise keeping its
  shape.
- **Episodes beside firings**, changing no number of today's.
- **A person's verdict on one firing**: "in the window but unrelated", for
  that firing alone, and "did it need action?".
- **The order** of the steps: leads, episodes, people's verdicts, then
  rows by label, each its own pull request.

Open:

1. **An entry's lead against written incidents.** A team whose incidents
   are written down, not found by a signal, gets no lead for its cause
   rules. Should an entry's `lead` hold against every incident for its
   rules, as its `match` doesn't?
2. **Labels a firing must carry.** `require: [service]` on an entry: a
   firing without the label agrees with none of its signals' windows. It
   would end the coincidences of alerts that carry no `service`, and
   changes `Agree` for those rules.
3. **Noisy by episodes deciding.** Whether `noisy_only`, the suggestions
   and the noisy badge should go by episodes, in a later step, once
   episodes have been read for a while.
4. **Thresholds of their own for episodes** (`--noisy-episodes`), or the
   same as firings', as proposed.
5. **A goal held to Lapilli's stones alone** (`stones: lapilli`), which
   people's verdicts can't move.
6. **Verdicts from elsewhere.** A job copying "needed action" from a
   chat's reactions, or an agent drafting answers for a person to confirm:
   under whose name, and through which port.
7. **Midnight, or not.** Whether an episode should part at midnight in
   `--tz`, as proposed, or at the week's start, which reaches back up to a
   week.
8. **Rows by label.** Whether to build it at all; and if so, groups by the
   alerts' labels (proposed), `split:` on a team, or groups by the rules'
   labels; within a team or across teams; and whether goals take a
   group.

## Steps

1. **Leads.** `verdict.Options.LeadFor` and `MaxLead`, and every place
   that reached back by `o.Lead`; `--impact-lead NAME=DUR`; `lead:` in
   `--impact-file`'s signals and entries; the Settings page's field and
   the entries' YAML, the try with its catches that agreed on no label,
   and `/settings.yaml`; the next schema (`lead_ns`); the spans pended on a
   change; the digest's and goals' wait; the report's and the summary's
   settings; numbers.md's stone section; signals.md's open question 4
   closed there.
2. **Episodes.** `verdict.Judge`'s episodes; the next schema; the tally's
   walk; the summary's `episodes`, a rule's page, the rules list's column
   and facet, the digest's noisy line, MCP, the report, the CSV and
   `/metrics`; the marked counts and the two goal measures; numbers.md's
   "Episodes".
3. **People's verdicts.** The next schema; the ledger's write and
   `stones`; the forms, `/api/v1/verdicts`, roles and the trail; moved
   verdicts on the Cleanup page and the overview; entries `judged: false`;
   the summary's `people` and `not_judged`, the report, the CSV, `action.share`
   and `lapilli_rule_action`; numbers.md's "A person's verdict". It may be
   two pull requests, as signals' step 2 was: the ledger, judging, the API
   and the numbers first, the pages after.
4. **Rows by label**, only once open question 8 is decided, in the shape
   decided.
