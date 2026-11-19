# Judging refined: a lead per signal, episodes beside firings, a person's verdict on one firing, and rows by label

Status: accepted. Merged on 2026-11-19, before its week was out, by the
maintainer's decision, its open questions decided ("Open questions").
Under [GOVERNANCE.md](../../GOVERNANCE.md) this changes how a firing is
judged (a lead of a signal's or a rule entry's own, a person's word that
turns one white stone black, rules judged by nothing, and a signal taken
away judging no one) and adds new kinds of data to the ledger (episodes,
and people's verdicts on firings), so it was designed first, as a pull
request (#147), with its reviews' findings taken in. The direction of
items 1 to 3 was decided by the maintainer on 2026-11-19, and this note
works out how; item 5, whose record the windows of a signal taken away
keep, was added the same day; item 4, rows by label, is decided not to
be built now. Each item is built as its own pull request, in the order
"Steps" gives: item 1, item 5, then items 2 and 3.

It builds on [signals.md](signals.md) (named impact signals and rule
entries, built from 0.53.0 to 0.58.0), whose open question 4 item 1
closes; on [marked.md](marked.md) and [goals.md](goals.md), whose counts
and measures items 2 and 3 add to; and on deciding impact windows and
"This was real" ([serve.md](serve.md), "Incidents and settings").

## Why

A team deploying Lapilli measured it against its own alerts for a week,
and found five things. Their numbers are rounded and their names left out:

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
- **A signal taken away still judges everyone.** Its windows stay as
  incidents, so for their weeks and the two after they keep every rule's
  record, rules an entry ties only to a `record: false` signal among
  them, and serve judges what `lapilli verdict` leaves unjudged.

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
incidents are all written down gets nothing from this, as decided (open
question 1).

**Judging otherwise keeps its shape.** A firing catches an incident when it
started from its lead before the incident's start to its end, and agrees
on its labels. One on since before its lead catches nothing: an entry's 1h
makes white a firing that began up to an hour before a window, and leaves
black one that began two hours before and is still on.

**Everything that reaches from an incident back to the firings it can
catch reaches by the longest lead**, `Options.MaxLead()`, the longest of
`--lead`, each signal's and each entry's. Today each of these reads
`o.Lead`, and each takes `MaxLead()` instead:

- `window` and `incidentsMeeting` in `internal/ledger/judge.go:241` and
  `:254` (the incidents read for a span, `start_at <= hi + lead`), and
  `catches` there (`:555`);
- the span a run judges, `lo - lead` (`internal/ledger/record.go:384`);
- `signalRun.lead`, set from `o.Lead` in `record.go:550` and
  `internal/ledger/signalsui.go:492`, which pends from `start - lead` when
  a window found going on gains labels (`record.go:781-782`): with only
  `--lead`, the firings 15 to 60 minutes before a widened window that a
  1h entry catches would never be judged again;
- the span `internal/ledger/impact.go:493` pends, and firings copied in
  (`internal/ledger/recorded.go:363`);
- the near misses' replay, which reads from `min(o.Lead, 24h)` before each
  incident (`internal/source/nearmiss.go:420`): `MaxLead()`, still a day at
  most;
- the incident page's timeline (`internal/serve/timeline.go:85`), drawn
  from the longest lead any rule is judged by against that incident.

The near misses' judging (`internal/verdict/nearmiss.go:105`, `:154`,
`:263`) looks from each rule's own lead for the incident (`LeadFor`).

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
- **What is worked out once a span is whole waits for it.** Today the
  digest waits only for the cursor to pass the week's end
  (`cursor.Before(to)` in `digest`, `internal/serve/work.go:1472`), so a
  firing in the week's last minutes that a window after midnight makes
  white is told as black, with `--lead` alone as much as with longer
  ones. This makes the wait the reach: the digest, `goal.missed`, the
  goals' tasks and the leader's goals work wait, always, until the run
  cursor is at least the span's end plus the most, over every pair of a
  signal and a rule entry that judges against it, of the lead and the
  signal's `for` (with the signal with no name, `--lead` and
  `--impact-for`; with written incidents alone, `--lead`), and `--settle`
  after that: every window a firing in the span can catch has been
  found, and a run has read again the samples that came late, so no stone
  of the span turns after it was told. The same for a week and for an
  occurrence of `each: KIND` ([goals.md](goals.md)).
- **What moves with the defaults** (15m, 5m and 1h, an hour and 20
  minutes): not the digest, which goes out at `--digest`, Monday 09:00
  by default (`cmd/lapilli/serve.go:631`), well after the wait. What
  moves is what is worked out as soon as the week is whole: `goal.missed`,
  the goals' tasks and the goals' `/metrics`, from about 00:00 on Monday
  to about 01:20.
- **A ledger with no runs**, whose firings are copied in, waits on
  `copiedTo` in place of the run cursor
  (`internal/serve/work.go:1466-1472`). Nothing reads samples again there,
  so `--settle` buys nothing and isn't added: a source that copies up to
  the week's end in one batch would otherwise hold the digest until its
  next copy. Nor is there a signal's
  `for`: without runs no signal is read, so there are no windows, and
  copied firings are judged against written incidents by `--lead`. Such a
  ledger waits for the span's end plus `--lead`.

### Judged again when a lead changes

- **`--lead`**, on the command line or the Settings page: every firing
  again, a week at a time, as now (`JudgeAll`).
- **A signal's or an entry's lead**: nothing is read from Prometheus. The
  spans it changes are pended (`pend`), each from the longer of the old
  and the new lead before the incident's start to its end: of a signal,
  its windows and the incidents its confirmations wrote (which carry its
  name, `incidents.signal`); of an entry, the windows of each of its
  signals and the incidents their confirmations wrote. `JudgePending`
  gives the firings there their stones again, and works out again what
  caught those incidents. A signal's windows are 2,000 a week at most, so
  this is less than judging every firing; judging catches is all a lead
  changes, and catches are near incidents.
- **Not every firing, by mistake.** Today a change to the entries judges
  every firing again: `SyncSignals` compares the entries by `entryKey`,
  sets `out.Entries`, and then pends all (`pendAll`,
  `internal/ledger/signals.go:497-504`); a change to a signal's row by
  `sameDef` (`signals.go:117`). A lead in `entryKey` or `sameDef` would do
  the same for every lead changed. So both compare without it, and a
  difference in leads alone sets `out.Leads`, which pends the spans above
  and no more; a lead changed with anything else judges as that change
  does.
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
- **`--impact-file`.** `lead:` on a signal and on a rule entry, and
  `require:` on an entry, read strictly, as the rest of the file
  (`readImpactFile`).
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
lost it (item 3, "Today"). Four things keep it in view; the last
changes `Agree`, for the rules that ask for it alone:

- **An entry's `match`** names the labels its rules' alerts carry, so
  `match: [service]` keeps `checkout`'s memory alert from `catalog`'s
  windows. It does nothing for an alert with no `service` at all.
- **Catches that agreed on no label are counted**: a catch where none of
  the labels it is matched on is carried by both the firing and the
  window, so any window at that time would have been caught. The rule's
  page says "3 of its 9 catches agreed on no label: they would have caught
  any service's window"; the try counts them among the catches a lead
  gains; MCP's `rule` gives `catches_on_no_label`. A count, not a stone.
- **A person's verdict** (item 3): a catch that was a coincidence is marked
  "in the window but unrelated", and that firing alone turns black.
- **`require:` on an entry**, opt-in (decided, open question 2): labels a
  firing of its rules and a window of its signals must both carry.
  Against those windows, a firing that lacks one, or whose window lacks
  it, agrees with none, so `require: [service]` keeps a node's memory
  alert, which has no `service`, from every service's window; the rules
  of an entry without it agree as now. Given in `--impact-file` and the
  Settings page's entries, held as `match` is (16 labels, each a
  Prometheus label name), and in the report as
  `settings.impact_rules[].require`†, which numbers.md's "incidents
  caught: counted again" reads.

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
  against incidents written down: the match in effect (`--match`, or the
  Settings page's), whatever its rule entry says. An entry's `match`
  holds only against its signals' windows: `Options.Agrees` takes it only
  for an incident with a signal in effect
  (`internal/verdict/signals.go:560`), so a written incident is matched on
  the match in effect for every rule, and so is an episode. Of each label,
  the firing's value, or none for a label it doesn't carry. The labels
  that tell its alerts apart and aren't matched on (`pod`, `instance`)
  don't part them. With no match at all, every overlapping firing of a
  rule is one episode, as each of them could catch any incident.
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
nothing until some eight weeks of episodes have been read (decided, open
question 3). The rule's page says the two
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
  firing; and, so that which firings make one can be counted again from
  the report alone, `settings.zone`†, the zone days are counted in (an
  IANA name), and `firings[].day`†, the day its start falls on there
  (`2026-11-19`): jq can't turn a zone's name into its offsets, and the
  day is all an episode needs.
- **The CSV**: an `episode` column, at the end.
- **`/metrics`**: `lapilli_rule_episodes` and `lapilli_rule_episodes_caught`
  (`rule`, [`severity`], [`team`]), over the last seven days, as the rule
  metrics are; in `lapilli verdict --metrics` too.
- **MCP**: `rules` and `rule` give each rule's episodes and
  `noisy_episodes`; `summary` the row's `episodes`; `firings` each
  firing's episode.
- **[numbers.md](../numbers.md)**: a section, "Episodes", with blocks that
  count them again: their stones and counts from `firings[].episode`, and
  which firings make one, from each firing's rule, labels, start, end and
  `day`, with `settings.match` and `settings.refire_ns`.
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
- **Followed by every change.** A change to `--match` or `--refire` judges
  every firing again today; episodes follow it.
  An incident written, a window decided, a person's verdict: the span's
  days, as the tally counts them again.
- **Upgrading** adds the columns at 0. The worker then counts episodes a
  week a write, from the newest week back, so that the weeks people read
  first are counted first: a walk of its own, not `pending`, which
  `JudgePending` takes from the oldest. Until it has been through a week,
  that week's rows say episodes aren't counted yet (`episodes: null` in
  the summary, "not counted yet" on the pages), by how far back it got
  (meta `episodes_from`). No stone changes.
- **A change of zone.** Opened in another `--tz`, the ledger counts every
  day and week again in one write today (`zone`,
  `internal/ledger/ledger.go:293`). That write doesn't walk episodes: as
  their days changed, it sets every `episode` to 0 and `episodes_from`
  back to the newest week, and the same walk counts them again a week a
  write. One write over every firing kept would hold the ledger's one
  writer as long as all of them take.

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
  caught more is refused it. And the firing's own start and end then, so
  that it can be found again when a run or a replay moves it (below).
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
  firing is (`excused := !white && excuses(r.f)`,
  `internal/ledger/judge.go:498`). Excused, it leaves `firings`,
  `refired.firings`, `critical.firings` and every share's denominator, as
  every excused firing does; it is counted apart (`unrelated_excused`) so
  that the numbers as Lapilli judged them can count it again ("How
  reports count them").
- **Where.** On Alert history, beside each white firing, as "This was
  real" is beside each black one; on an incident's page, a link from each
  rule that caught it to its firings that did. Not on a black firing,
  which caught nothing to be unrelated to. A firing made black so says
  "Black: unrelated to INC-12, by NAME on DATE", with Undo.
- **Not "This was real" as well.** The two are exclusive. Today
  `Ledger.Real` checks only `f.White` (`internal/ledger/real.go:32`), so on
  a firing a person made black it would write an incident from the
  firing, which it catches, and turn it white again: the person's word
  undone by another door, and noted as something else. The page doesn't
  offer it there, and `Real` refuses a firing with "unrelated" on it
  (409, "a person said this firing was unrelated: undo that first").

### Did it need action?

Each firing has an answer: yes, no, or none (the default). It changes no
stone. Asked on Alert history (one at a time, and the firings picked, 100
at a time, as "This was real" takes them), on a rule's latest firings and
on an incident's page. Counted (below):

- **answered**, **yes**, **no**, and the share of yes among those
  answered, once enough are answered: what the deploying team tracks;
- **yes on a black stone**: someone had to act, and it caught nothing
  written down; the line offers "This was real";
- **no on a white stone**: it caught something, and nobody had to act, a
  coincidence or a thing that healed itself; the line offers "Unrelated".

### Who, when, and the trail

- **"Unrelated"** is for editors of every team and admins, whom `s.write`
  lets decide an impact window and press "This was real": it changes a
  stone, and the incidents it takes a catch from may be another team's
  (the coincidence above was another service's).
- **"Needed action"** is for a team's editors too, of their team's rules'
  firings, as their notes on rules are: it changes no stone.
- **A team's editor leaves "unrelated" as it is.** The forms and the API
  are under `s.teamWrite`, which lets a team's editor in and leaves the
  ledger to refuse only rows of another team (`inScope`;
  `internal/serve/roles.go:293-314`). Refusing a team's editor only the
  setting of "unrelated" isn't enough: a `PUT` replaces the row and a
  `DELETE` undoes it. So the verdict's write goes part by part: a team's
  editor's write leaves `unrelated` as it is stored, neither set, nor
  cleared, nor deleted with the row. A `PUT` of theirs that gives
  `unrelated` otherwise than stored is refused (403), and a `DELETE` of
  theirs clears their part, "needed action", alone.
- **Each part has its own who and when**: `unrelated_by` and
  `unrelated_at`, `action_by` and `action_at`, and a note of each part's
  own. So "unrelated, by NAME" names who said it, never whoever last
  answered "needed action" on the same firing.
- **Viewers** set neither; `--read-only` refuses both.
- **How far back.** An editor, and a team's editor, set, change or clear
  a verdict only on a firing that started in the last 14 days; an admin on
  any firing kept. Goals follow later edits: an incident written down
  later "turns last week's goal met or missed as it turns the week's
  stones" ([goals.md](goals.md), `docs/design/goals.md:182-185`). So does
  a verdict. Flipping stones twelve weeks back would turn those weeks'
  goals met, put "no longer missed" on their tasks (which a person then
  closes, reading it), and move the four weeks before that a
  `no_worse_than: before` occurrence is set beside. Fourteen days covers
  the week's review and the one before it. Past that, it is an admin's,
  as the judging settings are. Whatever its age, a goal's notice and its
  task's comment say when people's verdicts moved it: "no longer missed,
  with 3 people's verdicts since it was told".
- **A verdict moved on an old firing** (an admin's signal change reads
  four weeks again, and a window under it moves) is said again, or
  undone, by an admin alone, as any verdict on a firing that old is; the
  overview's "Look at these first" names those to an admin ("2 people's
  verdicts older than 14 days to look at again"), and to an editor only
  those they may say again.
- **"This was real" is held the same.** Today it has no limit of age: an
  editor can turn a black stone of any week white. It moves the same
  numbers as "unrelated", the other way, so from step 4 it is an
  editor's on the last 14 days' firings and an admin's beyond. Writing an
  incident down by hand stays an editor's at any age: an incident log is
  copied in late as a matter of course, and an incident written is said
  as such, `real` counting only those "This was real" wrote.
- **The trail.** Every set and every undo is noted in `changes` with its
  before and after (`verdict on RULE {labels} at START`), as an incident's
  decision is (`change` in `internal/ledger/incidents.go:501`), and the
  activity log shows it. Outcomes count them as use, as every edit
  through the UI and the API.
- **A bulk post is one change.** The activity log shows the latest 100
  changes (`Ledger.Changes`, `internal/ledger/misc.go:93`, read with 100
  on the Run history page, `internal/serve/views.go:2284`), so a post of
  100 firings noted a change each would push every other change off it.
  One row a post ("needed action: yes, on 100 firings of 3 rules", the
  firings in its after).
- **30 verdict writes an hour a person, of every kind.** A form's post, a
  bulk post and a call of `PUT /api/v1/verdicts`, `PUT
  /api/v1/verdicts/bulk` or `DELETE /api/v1/verdicts` each count one, by
  the name the write is noted under, across replicas (kept in the ledger,
  as the tries of the Settings page are counted). Past 30 in the hour,
  the write is refused with 429 and `Retry-After`, and nothing is
  written: no verdict, no change row, nothing judged again. Thirty is
  room for a person: the bulk form and the bulk call take 100 firings in
  one. It bounds
  what one name can put among the activity log's latest 100 to 30 rows an
  hour, and the log, besides, shows each person's verdict rows of an hour
  as one line ("NAME: 30 verdicts"), reading its latest 100 other changes
  beside them, so a script holding one token can't push the rest off it;
  `changes` keeps every row.

### Which firing a post is about

Firing ids are reused. `firings.id` is `INTEGER PRIMARY KEY` with no
`AUTOINCREMENT` (`internal/ledger/schema/001.sql:41`), and a run deletes
the firings of its period and inserts them again (`record.go:295`), so
SQLite gives the new rows ids the old ones had. A page read before a run
can post "unrelated" for an id that is another alert's firing by then.
So the forms carry the firing's alert (its series) and start beside its
id, and the write refuses a mismatch (409, "this firing changed since the
page was read: look again"); the API is keyed by rule, labels and start
already. "This was real" (`POST /firings/{id}/real`,
`internal/serve/web.go:1417`) and its bulk form have the same flaw today;
step 4 fixes them the same way.

### Through judging again

Judging reads a person's verdict, and never writes it but to say it
moved.

- **Kept by its alert and start**, not by the firing's id. A run replaces
  the firings of its period (`DELETE FROM firings WHERE start_at >= ?`,
  `internal/ledger/record.go:295`), and they get new ids; the same firing
  is found by its alert and its start, as a run finds one it had (`WHERE
  series_id = ? AND start_at = ?`). `stones` reads the verdicts of the
  firings it judges, joined on both, and gives the stones as above.
- **Moved**, the one rule by which a verdict finds its firing again, in
  the ledger and in every replay (below): the firing of the same rule and
  match values (its own alert's first) that is on at the verdict's start;
  else the first that starts within the span it was said of, its start to
  its end as kept. A verdict set while its firing was still on (no end
  kept) has a span of its start to its start plus its rule's `for` and
  `--refire`, so that it never reaches a firing hours later. A run whose
  replay starts the firing a step later (late samples) moves the verdict
  so in its write, and marks it moved ("its firing starts at 14:03 now").
  With none, it stays, on no firing, listed on the Cleanup page with what
  it said, and is pruned with `--keep`.
- **Two verdicts, one firing.** A replay can merge two firings into one,
  and the fallback can come to another pod's firing that has a verdict of
  its own. The verdict said of that very alert (its series) wins; between
  two of other alerts, the one set latest. In the ledger the other is
  marked moved ("another verdict holds this firing"), listed with the
  rest, and judges nothing until it is said again. A replay judged in
  memory writes nothing, so there the other is dropped, not marked. When
  both say "unrelated", the firing they share keeps the union of their
  incidents as unrelated, whichever wins the rest: otherwise it would be
  white by the incidents the other was said of, and a replay that merges
  two firings would bring back the very bias that carrying verdicts into
  replays takes away.
- **Where an incident's key moves, its verdicts' keys move, in the same
  write.** Three places move keys today, each with the tasks and comments
  on them:
  - `remapImpact` (`internal/ledger/record.go:1052`), when windows are read
    again: it skips a window no longer there that has no tasks or comments
    (`record.go:1066`, `if n == 0 { continue }`), so it counts the
    verdicts naming it too, and a window with only a verdict on it isn't
    skipped;
  - a signal renamed in place (`internal/ledger/signalsui.go:283`);
  - the last named signal taken away, the one with no name given its
    windows back (`signalsui.go:442`).

  Each marks the verdict moved where the incident's span changed.
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

**Replays carry verdicts, on both sides.** Today a replay judged in memory
judges both sides of a change with `verdict.Judge` and nothing of people
(`replayCompared`, `internal/serve/work.go:1448`). With verdicts kept by
alert and start alone, the side as it is would find a person's
"unrelated" and keep that firing black, and the side with a longer `for`
or another expression, whose firings start later, would find none and be
white again: every proposal's backtest, every `--mcp-compare` and every
longer `for` the digest suggests would look better, on exactly the firings
people called coincidences. So `verdict.Judge` takes the verdicts of the
period (`Options.Verdicts`), and gives each to a replayed firing by the
rule for a move above, on both sides alike: a proposal's backtest and
`backtest_again`, `compare`, a pull request's backtest through serve, the
Settings page's try, and the digest's suggestions and near misses
(`source.Suggest`, `source.NearMisses`). A verdict set in a proposal's
weeks since its backtest makes it stale, as an incident written there
does (`staleRecorded`): "a person said a firing of its rule was unrelated
in its period since it was backtested".

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
- **Entries apart from named signals.** Today an entry is looked at only
  with named signals: `Signals.Index` returns at once without them
  (`internal/verdict/signals.go:366`), `entryIndex` finds none (`:419`),
  and `Agrees` reads an entry only for a window of a named signal in
  effect (`:568`). So `judged: false` without named signals would do
  nothing. The entries are kept and indexed whenever there are any, named
  signals or none; `Catches`, `Judge` and the ledger's `stones` and
  `catches` read a rule's `judged` first; what is about signals (ties, an
  entry's `signals` and `match` against windows) still waits for named
  ones, as now.
- **Which entry is a rule's** is found as now: the first, in order, whose
  pattern matches its name, else whose labels it has.
- **Who.** An admin's, as every entry is (the Settings page, or the
  command line), noted with its before and after. It takes a whole rule's
  black stones out of precision, noisy, the digest's noisy rules and
  suggestions, and every goal that needs the record: so the Goals page
  says, beside every goal, how many firings of its week were not judged
  by an entry ("12 firings of 2 rules not judged"), as it says people's
  verdicts, and the rules list shows the rule as "not judged".
- So far an entry was about signals ([signals.md](signals.md)); this makes
  entries what a rule is judged by, signals one of the things they say.
  Judged by nothing is judging, not a quiet window: a quiet window
  excuses firings that caught nothing, and these catch nothing by
  definition, so it would excuse every one of them and count them in no
  number but `excused`.

### API and MCP

- **The UI.** `POST /firings/{id}/verdict` (`unrelated` set or not,
  `action` yes, no or none, a note for each, and the firing's series and
  start), as `POST /firings/{id}/real` is; `POST /firings/verdict`, the
  firings picked, each with its series and start, with one answer to
  "needed action", 100 at most, as `POST /firings/real`. Both under
  `s.teamWrite`, the write going part by part as above.
- **The API**, by the firing's alert and start, as a report gives them (a
  report's firings have no id):
  - `PUT /api/v1/verdicts`: `{"rule", "labels", "start", "unrelated",
    "unrelated_note", "action", "action_note"}`; a part left out is left
    as it is. 404 with no such firing; 409 for "unrelated" on a firing
    that caught nothing, or one "This was real" wrote an incident from;
    403 for a part the caller's role doesn't have, or a firing older than
    14 days for one who isn't an admin.
  - `PUT /api/v1/verdicts/bulk`: `{"firings": [{"rule", "labels",
    "start"}, ...], "action", "action_note"}`, up to 100 firings with one
    answer to "needed action", as the bulk form takes them. One call is
    one write against the cap and one row of the activity log, so a tool
    that copies a team's answers in reaches 3,000 firings an hour, enough
    for a busy team to reach the 80% bar. It is all or nothing: a firing
    not found, of another team for a team's editor, or older than 14 days
    for one who isn't an admin refuses the call with what was wrong, and
    nothing is written. "Unrelated" isn't taken in bulk, as on the form:
    a person looks at each.
  - `DELETE /api/v1/verdicts` with `rule`, `labels`, `start` and `part`
    (`unrelated`, `action`, or both when left out): undone, part by part,
    as the caller may.
  - `GET /api/v1/verdicts?from=&to=`: the verdicts of the firings that
    started in the period, 31 days at most, each with its firing, each
    part with its note, who said it and when, and whether it moved; a
    viewer's.
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
  would be an agent's word, which this field isn't: out of this design
  (decided, open question 6).

### How reports count them

The stones keep the people's verdicts in them, as they keep an incident a
person wrote down: `caught` and `precision` are what the stones say. Beside
them, each summary row gains `people`:

```json
"people": {"unrelated": 2, "unrelated_excused": 1, "real": 1, "real_white": 3,
           "caught_by_lapilli": 12, "precision_by_lapilli": 0.29,
           "action": {"firings": 120, "answered": 110, "yes": 30, "no": 80, "share": 0.27,
                      "yes_black": 1, "no_white": 4}}
```

- `unrelated`: firings of the row white by Lapilli and black by a person;
  `unrelated_excused`, those of them a quiet window then excused, which
  left `firings`, `refired.firings`, `critical.firings` and the
  denominators, as every excused firing does.
- `real`: incidents written from a firing of the row's rules ("This was
  real", `incidents.real_of`) that started in the week, the other way a
  person turns a stone; `real_white`, the row's firings white only by
  such incidents.
- `caught_by_lapilli`, `precision_by_lapilli`: as Lapilli's own judging
  would have them: every "unrelated" white, those excused for it counted
  again, and the incidents "This was real" wrote left out, their firings
  black, or not judged where only such an incident kept their week's
  record (the record read without them), or excused where they came in a
  quiet window that matches them, as a black firing there is. Bit 8 alone
  can't tell the last, so `stones`, which has the quiet windows in hand
  (`excuses`), sets bit 16 too, white only by such incidents and in a
  matching quiet window, tallied as `real_excused`, which leaves
  `firings`, `refired.firings`, `critical.firings` and the denominators
  of the numbers as Lapilli judged them. So a team's precision reads with
  people's word, and without it, both ways.
- `action`: the row's `firings`, those `answered`, `yes` and `no`, the
  two crossings above, and `share`, yes over those answered, given only
  once 80% of the row's firings are answered (null below it, and a goal on
  it missed: "Goals", below). Of the two ways to keep a self-reported
  share honest, this one: divided by every firing, an unanswered one
  would count as "no", and the share would follow how much was answered,
  falling in a busy week and rising as the team catches up, a fact about
  answering more than about the alerts. With a bar of answers, it stays a
  share of the alerts, and answering only the firings that needed action
  gives no share at all, not 100%. The bar is fixed, and shown beside
  ("110 of 120 answered").

**What people's verdicts move**, which the Goals page says beside each
goal whose measure they moved in a week, with how many verdicts, "This was
real" incidents and firings not judged by an entry went into it:

- **"Unrelated"**: `precision`; `critical.share_caught`;
  `incidents.missed`, `incidents.noisy_only` and
  `incidents.median_lead_seconds`, and the same of `impacts`;
  `during_deploys.precision_without`; inside a marked kind, its precision
  and incidents; `episodes.precision`; a rule's `noisy`, `first`, `alone`
  and `spare`. And through a quiet window excusing the firing it made
  black, the counts: `firings`, `refired.firings`, `refired.share`,
  `critical.firings`, `asleep.*` and `off_hours.*`, `episodes.total`.
- **"This was real"**: the same, the other way, as an incident written
  down does.
- **`judged: false`**: every measure that needs the record, as no record
  does, and the incidents its rules no longer catch.
- **"Needed action"**: `action.share` alone.

Elsewhere:

- **A rule's page**: beside its precision, the precision as Lapilli judged
  it when people's verdicts moved it ("31%; 35% as Lapilli judged it: 2
  firings were said to be unrelated"), and its answers to "needed action".
- **The report**: `rules[].unrelated`†, `rules[].action_yes`† and
  `rules[].action_no`†; `firings[].unrelated`†, the ids of the incidents it
  catches by Lapilli's judging that a person said it is unrelated to (so
  that white by Lapilli is `white`, or `unrelated` not empty), and
  `firings[].action`† (`yes`, `no`). An incident's `real`†, written from a
  firing. Not who: a report goes outside (`--redact`), and who is
  `/api/v1/verdicts`'.
- **The CSV**: `unrelated` and `action` columns, at the end.
- **Goals**: a measure, `action.share` (higher is better, needs no record,
  not with `each: KIND`). A whole week under 80% answered is **missed**,
  "under 80% answered", not unknown: a week not known is neither met nor
  missed ([goals.md](goals.md), "Checked each week"), so a team about to
  miss could stop answering at 79% and never be missed. A week with no
  firing is not known (`no_value`), as nothing was to answer; the week
  under way is `open`, as every goal's is.
- **A goal on Lapilli's stones alone** (decided, open question 5): a goal
  may say `stones: lapilli`, and is then checked against the numbers as
  Lapilli judged them, people's verdicts and the incidents "This was
  real" wrote both left out: `precision` reads `people.precision_by_lapilli`.
  A goal of a measure the summary doesn't give as Lapilli judged it is
  refused with its line, until the summary gives that one so.
- **`/metrics`**: `lapilli_rule_action` (`rule`, [`severity`], [`team`],
  `answer`: `yes`, `no`), over the last seven days.
- **The digest**: a line, "People said 2 firings were unrelated, and
  answered 110 of 120 firings: 30 needed action."
- **[numbers.md](../numbers.md)**: a section, "A person's verdict", with
  its blocks counted again, and the stones' section saying a firing a
  person said is unrelated is black.

### In the ledger

- **`verdicts`**, a row a firing a person said something of: `series_id`
  and `start_at` (the key), `end_at` (the firing's end when it was said,
  for the rule of a move); `unrelated` (a JSON list of the incidents, each
  `{key, start, end}`, `[]` for none), `unrelated_by`, `unrelated_at` and
  `unrelated_note`; `action` (`''`, `yes` or `no`), `action_by`,
  `action_at` and `action_note`; each note 500 characters at most, one
  line; and `moved` (`''`, or why).
- **`firings.person`**: bits `stones` writes from the verdicts and the
  incidents, for the tallies: 1, unrelated (white by Lapilli, black by a
  person); 2, needed action; 4, didn't; 8, white only by incidents "This
  was real" wrote.
- **`days` and `weeks`** gain `unrelated`, `unrelated_excused`,
  `real_white`, `action_yes` and `action_no`.
- **`incidents.real_of`**: the alert and start "This was real" wrote it
  from, `''` for any other; incidents written so before the upgrade are
  `''`, and counted as none.
- **`lapilli ledger copy`** carries `verdicts` with the rest; `--keep`
  prunes a verdict with its firing, and one on no firing once its start is
  older than `--keep`, as a firing's would be.

## 4. Rows by labels other than team: not now

Decided on 2026-11-19, by the maintainer: not now (open question 8). What
follows is kept as the place to start if it is built later: groups by the
alerts' labels, within one team, with goals taking `group:` in a later
step. The two other shapes weighed are below it.

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

## 5. A signal taken away: its windows keep no rule's record

Decided on 2026-11-19, by the maintainer: part of this design rather than
a fix of its own, and its shape (open question 9). A judging fix, built
right after the leads, as step 2.

### Today

A signal taken away, on the Settings page or by starting serve without it,
keeps its windows, with its name, "as incidents" (signals.md, "Changing
signals"). Both paths end in `syncSignals`
(`internal/ledger/signals.go:352`, reached from `SyncSignalSet` and from
`ChangeSignals` in `internal/ledger/signalsui.go:59`), which deletes the
signal's row, ties every alert again from the signals left and pends
every firing (`signals.go:497-504`). From then on its windows are judged
as if a person had written them down:

- **They keep everyone's record.** `verdict.RecordOf` sets a window aside
  by its signal only while that signal is in effect: `i < 0`, "a window of
  a signal no longer read, which stays as an incident"
  (`internal/verdict/record.go:75-77`), goes with the incidents written
  down, which keep every rule's record for their week and the two after
  (`Record.Judges`). With no named signal in effect at all, every incident
  is taken as written (`recordOf(incs, o, o.ImpactFrom)`,
  `record.go:65-66`).
- **Any rule catches them.** `Options.Agrees` reads a rule's entry only
  for a window of a signal in effect (`internal/verdict/signals.go:568`),
  so a rule an entry ties to other signals alone catches a taken-away
  signal's window on its match labels, as anyone's.
  `TestSignalRemovedWindowsCaughtByAnyRule`
  (`internal/serve/signals_test.go:890`) holds both.

So in those weeks a rule an entry ties only to a `record: false` signal,
which its operator means to leave unjudged, is judged, and its firings
are black. `lapilli verdict` with the same rules and file doesn't read the
signal taken away, finds none of its windows, and leaves them unjudged:
the two disagree. #151 meets the same case from the other side: when
serve is started with named signals in place of `--impact PROMQL`, it
ties the windows of the signal with no name to `impact`; with no
`impact` in the new file, they are a taken-away signal's windows, and the
deploying team's file has none. The docs' way round it today is to give
the file the old expression as `impact`.

### What changes

A window of a signal not in effect is one of a signal taken away on the
Settings page, dropped by a start without it, or set aside at a start
past eight (signals.md, "Step 3, as built": "a start with more signals
than before leaves the UI's newest aside"). Such a window:

- **Keeps no rule's record.** `RecordOf` leaves it out of the incidents
  that keep the record, with named signals in effect or none. A rule's
  record in those weeks is what the signals in effect and the incidents
  written down keep, as `lapilli verdict`, which never read that signal,
  judges it. No rows are kept for it, and nothing is taken at the
  taking away.
- **Is caught by the rules with no entry, alone**, on the match labels it
  was found with (`incidents.match_labels`), and never by a rule
  `judged: false` (item 3). `Agrees` reads a rule's entry for such a
  window too: a rule with an entry catches the windows of its entry's
  signals in effect and no others, as it does while they are read.
  `TestSignalRemovedWindowsCaughtByAnyRule` is turned round: the window
  stays caught by a rule with no entry, and the rule with an entry
  doesn't catch it. The entries are read for it even with no named
  signal in effect: the part of item 3's "entries apart from named
  signals" this needs comes with step 2.
- **Is written down when it is confirmed.** Whoever may decide impact
  windows (editors and admins, `s.write`) can confirm those that were
  real outages; the incident the confirmation writes keeps everyone's
  record and is caught as its window was, as every confirmation is
  (signals.md, "Windows: each signal's own"). That is how to keep the
  record of those weeks.

**Rules whose entry names no signal in effect.** The Settings page
refuses to take away a signal an entry names, and `--impact-file`
refuses an entry naming a signal it doesn't declare; but entries kept in
the UI (`impact-rules`) can name a signal a start dropped, or one set
aside past eight. `NewSignals` leaves a name not in effect out of the
entry (`e.idx`), so such an entry names no signal in effect. Its rules
are judged as signals.md says of "an entry whose signals are none of
those in effect": tied to no signal, so no record by reading; their
record kept by incidents written down alone; and they catch incidents
written down alone, not the windows of the signal their entry names,
which isn't in effect. Given back, the signal's windows are its again,
and its entries' rules catch them and are judged by them.

**Serve and a fresh `lapilli verdict`** agree on every rule's record:
which firings are judged, and so whether a rule is noisy, has a
precision or meets a goal that needs the record. Their stones differ
only where serve holds a window `lapilli verdict` can't read: a firing of
a rule with no entry that catches one is white in serve.

**#151's case is fixed with it.** With no `impact` in the new file, the
windows #151 ties to `impact` are a signal not in effect's: they keep no
rule's record, and the deploying team's rules, tied by entries only to a
`record: false` signal, are left unjudged, as `lapilli verdict` leaves
them. Step 2 makes the docs' way round it unneeded: its pull request
takes "give the file the old expression as `impact`" out of the docs.
It is built on #151, so that a window of the signal with no name, no
longer read, carries the name `impact`.

### Judged again

- **The record** is worked out when it is read, as every record is
  ([serve.md](serve.md), "The impact criterion"): taking a signal away
  needs no stone given again for it.
- **The catches** change with the taking away and with the entries: both
  already pend every firing (`pendAll`); nothing more is pended.
- **The upgrade** pends the spans of the windows of signals not in effect
  that the ledger keeps, from `MaxLead` before each one's start to its
  end, so that the rules with an entry stop catching them: 2,000 windows
  a week a signal at most.

### With the other items

- **Leads (item 1).** A window of a signal not in effect is judged by
  `--lead` (`LeadFor`'s last case): its signal's lead, and its entries',
  aren't in effect either.
- **`judged: false` and people's verdicts (item 3).** A rule judged by
  nothing never catches these windows. A verdict naming one keeps its
  key, which the window keeps; a firing whose rule has an entry, and so
  no longer catches it, says "it no longer catches INC-12", as for an
  incident gone.
- **Episodes (item 2)** follow the stones, and change no more than they
  do.

### The report, and where it shows

- **The report** needs no field: a window of a signal not in effect is an
  incident with `candidate` and a `signal` none of
  `settings.impact_signals` names. numbers.md's "No record" says it keeps
  no rule's record, and its blocks leave it out of the incidents that
  keep it. Its "incidents caught: counted again" already reads a rule
  with an entry as catching only its entry's signals' windows
  (`$in.signal == null or $e == null or any($e.signals[]; . ==
  $in.signal)`): it counts the code as this makes it, and needs no change.
- **A window's page**: "probe, not read since 2026-11-10: keeps no rule's
  record, and is caught by the rules with no entry; confirm it to keep
  the record of its weeks".
- **A rule's page**, of a rule whose entry names no signal in effect:
  "its entry names probe, which isn't read: judged by incidents written
  down alone".
- **The Incident sources panel**: a row for the windows of signals not in
  effect, how many, and of which signals.
- **MCP**: `incidents` says, of a window, whether its signal is in effect.

### Cost

Nothing is kept: no table, and no read of `series` at the taking away.
`RecordOf` sets such a window aside by a lookup of its signal's name, as
it does now; `Agrees` reads a rule's entry from the map `Signals.Index`
makes once a change (`TestScale` holds it by the counter of entries
looked up, `Signals.Looked`). The upgrade's spans are its windows', 2,000
a week a signal at most.

## What stays the same, and what changes

[numbers.md](../numbers.md) lists what keeps its name and its meaning, and
the pull request that builds each step changes it with the code, as it
says:

- **Step 1** changes "A white or a black stone": a firing catches an
  incident from the lead it is judged by (its entry's, its signal's, or
  `lead_ns`), and its recount block reads `lead_ns` from
  `settings.impact_signals` and `settings.impact_rules`. It adds those two
  fields to "The report's fields". Every number of a ledger with no lead
  of a signal's or an entry's stays as it is. The goals' wait
  ("`--settle`, a signal's `for`, a rule's `for`") is the one thing that
  changes with the defaults: `goal.missed`, the goals' tasks and their
  `/metrics` move from about 00:00 on Monday to about 01:20; the digest,
  at Monday 09:00, doesn't move.
- **Step 2** changes the record of every rule in the weeks a window of a
  signal not in effect kept it (item 5), which it keeps no more, and the
  catches of the rules with an entry, which no longer catch such a
  window. numbers.md's "No record" says so, and its blocks leave such a
  window out of the incidents that keep the record; the report gains no
  field. A ledger that never took a signal away judges as before.
- **Step 3** changes no number. It adds "Episodes", `rules[].episodes`†,
  `firings[].episode`†, `firings[].day`† and `settings.zone`† to the
  report's fields
  (`TestReportFieldsStable`), the CSV's `episode` (`TestCSVColumns`), and
  `lapilli_rule_episodes` and `lapilli_rule_episodes_caught`
  (`TestMetricsStable`).
- **Step 4** changes the stones of the firings a person says are
  unrelated, and of the rules an entry says are judged by nothing, and
  nothing else. It adds "A person's verdict", the report's
  `rules[].unrelated`†, `action_yes`†, `action_no`†, `not_judged`†,
  `firings[].unrelated`†, `action`†, `not_judged`† and `incidents[].real`†,
  the CSV's `unrelated` and `action`, and `lapilli_rule_action`. "No
  record"'s recount of `unjudged` counts `not_judged` too.
- **The summary** keeps `schema` 1 throughout: `episodes`, `people` and
  `not_judged` are fields added, and no field changes its meaning.
- **`verdict.Semantics`** isn't bumped by any step: a ledger with no lead,
  no verdict, no entry judged by nothing and no signal taken away judges
  as before, and episodes judge nothing. A backtest judges against the
  ledger's incidents, a taken-away signal's windows among them, by the
  same `RecordOf` and `Agrees`, so it follows item 5 with the ledger.

## In the ledger: schema numbers

Steps 1, 3 and 4 each add a schema, in a pair (`schema/NNN.sql` and
`schema/postgres/NNN.sql`), each with its row in
[upgrading.md](../upgrading.md) (`TestSchemasDocumented`):

- step 1: `impact_signals.lead_ns` and `impact_rules.lead_ns` (NULL to
  follow), so that the ledger holds the leads its options are judged by,
  beside the rest of each signal (`SignalDef`) and entry, compared apart
  from them (`sameDef`, `entryKey`: "Judged again when a lead changes");
- step 2: none (item 5 keeps nothing);
- step 3: `firings.episode`, `firings.episode_white`, and `episodes` and
  `episodes_white` on `days`, `weeks`, `marked_days` and `marked_weeks`;
- step 4: `verdicts`, `firings.person`, `unrelated`, `unrelated_excused`,
  `real_white`, `action_yes` and `action_no` on `days` and `weeks`,
  `incidents.real_of`, and `impact_rules.judged`.

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
  two. The upgrade's walk and a change of zone's take a week a write, from
  the newest back. `TestScale` holds it by the rows it reads and writes.
  Pages read the tallies' two columns more, never the firings:
  `TestPagesScale` holds the rows each page reads and its heap peak, the
  least of three runs, as now. `lapilli verdict` groups the period's
  firings once more, by rule and match values.
- **Verdicts.** One a firing at most, 16 incidents each, two notes of
  500 characters. A verdict's write judges again its firing's span and the
  days it falls in: less than an incident's write, which judges from its
  lead before to its end. A bulk post is 100 firings and one change row;
  30 verdict writes of any kind an hour a person. `stones` reads the
  verdicts of the firings it
  judges in one query by alert and start, along an index on
  `(series_id, start_at)`; a replay judged in memory reads those of its
  period once, and looks each replayed firing's up by rule and match
  values.
- **Rules judged by nothing.** One more field of an entry, looked up as
  the entry is.
- **A signal taken away** (item 5): nothing kept; a window set aside by
  its signal's name, as now, and a rule's entry looked up for it from
  `Signals.Index`'s map; on the upgrade, its windows' spans pended.
- **Groups** (if built): 32, a row per group per week; `series.grp` worked
  out per alert as ties are.

## Security: as an attacker reviews it

**What can someone send before they prove who they are?** Nothing new.
Every new write is on the UI's port, behind its sign-in and roles: `POST
/firings/{id}/verdict`, `POST /firings/verdict`, `PUT` and `DELETE
/api/v1/verdicts` and `PUT /api/v1/verdicts/bulk` (100 firings), under
`s.teamWrite`, which lets a team's editor in for their team's rules
alone; the verdict's write goes part by part, so a
team's editor can neither set, clear nor delete "unrelated"; each body cut
at 1 MiB, as `teamWrite` cuts it (`http.MaxBytesReader`). Leads and rule
entries, `judged: false` among them, are set on the command line, in
`--impact-file`, or by an admin on the Settings page.

**A post about another firing than the one shown.** Firing ids are reused
(`internal/ledger/schema/001.sql:41`, `record.go:295`), so a post by id
alone, made from a page read before a run, could write about another
alert's firing; every form carries the firing's series and start, and the
write refuses a mismatch. "This was real" has the flaw today
(`internal/serve/web.go:1417`), and step 4 fixes it with the rest.

**Cross-site requests.** Each form posts to the mux behind
`http.NewCrossOriginProtection` (`internal/serve/web.go`), as every form
does: a page elsewhere can't post a verdict with a signed-in person's
cookie. `PUT` and `DELETE` are under it too; a caller with a bearer token
isn't a browser, and writes as the name the token gives.

**How much can a holder make Lapilli keep or judge again?**

- A verdict: one a firing, its two notes 500 characters each on one
  line, its incidents 16; written again, it is the same row. Its write
  judges again one firing's span and its days, less than an editor's
  incident write already does.
- Writes: 30 verdict writes an hour a person, a form's post, a bulk post
  or a bulk call (100 firings, one change row) and any other API call
  alike; past that, 429 and nothing written. The activity log shows a
  person's verdict rows of an hour as one line, so neither one post nor a
  script holding a token can push its other changes off its latest
  100.
- A lead: an admin's, 24h at most, pending its signal's windows, among
  the twelve saves an hour.

**Where do the names they choose end up?** A note goes to the firing's
line and the incident's page, escaped and set apart in `<bdi>` as rule
names are; to `/api/v1/verdicts`; and to MCP's `firings` as data, which
the server's instructions already tell an agent to treat as data, not
instructions, as incident titles and notes. Never to the digest, Slack, a
notice, a report or `/metrics`. `unrelated_by` and `action_by` are shown
as every `updated_by` is, a name the sign-in gave (`signedName`).

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
  (`people.unrelated`, `unrelated_excused`, `precision_by_lapilli`), and
  said beside a goal whose measure it moved; and a goal may be held to
  Lapilli's stones alone (`stones: lapilli`, open question 5).
- **Back in time.** A verdict on an old firing turns old weeks' goals, says
  so on their tasks, and moves the weeks a `no_worse_than: before`
  occurrence is set beside. Editors set verdicts on the last 14 days'
  firings alone; older ones are an admin's; and a goal's notice and task
  say when verdicts moved it.
- **Needed action** is the team's own word, and changes no stone. A share
  is given only once 80% of the firings are answered, so answering the
  firings that needed action alone gives none, not 100%; and a goal on it
  is missed under the bar, so answering less can't move a missed week to
  not known.
- **Judged by nothing.** `judged: false` takes a whole rule's black stones
  out of precision, noisy, the digest's noisy rules and suggestions, and
  every goal that needs the record: a rule that does nothing but page
  stops counting against anyone. It is an admin's, as every entry is,
  noted with its before and after; the rule's page and the rules list say
  "not judged", the summary gives `not_judged` beside `unjudged`, and the
  Goals page says beside every goal how many firings of its week an entry
  kept from being judged.
- **A lead** makes rules look better: more white. It is an admin's,
  noted with its before and after, given in the summary's and the
  report's settings beside every number it judged, and the try says how
  many of the catches it gains agreed on no label.

**Taking a signal away.** It is an admin's, on the Settings page, or
whoever starts serve, and it takes back the record its windows kept:
weeks whose firings were judged by those windows alone are then not
judged, and a goal on a measure that needs the record is not known there
rather than met or missed. So it is noted as every change to the signals
is, and the window's page says that confirming it keeps the record of
its weeks. It hides no incident: its windows stay, caught or missed, in
every count of incidents.

**Agents.** No MCP tool writes a verdict, a lead or an entry, and the
replays agents ask for (`compare`, `propose`'s backtest, `backtest_again`)
carry people's verdicts on both sides, so a change can't look better on
the firings people called coincidences. An agent holding a person's
bearer token writes through the API as that person, as it could write an
incident; it is noted under that person's name.

The paths for the review: the two form handlers and `/api/v1/verdicts`,
with their check of series and start and of each part's role; the
ledger's write of a verdict (one write: the firing read again in it, as
`Ledger.Real` reads it), and `Real` refusing a firing with "unrelated";
`stones` reading verdicts; `remapImpact`, the two renames in
`signalsui.go` and `Decide` moving their keys; `verdict.Judge` giving
verdicts to replayed firings; and `readImpactFile` and `readEntries`
taking `lead` and `judged`.

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
  - *Firings made again with `AUTOINCREMENT`*, as schema 016 made tasks,
    so that an id is never another firing's. SQLite adds it only to a
    table it makes, so every firing kept would be copied; and a run still
    replaces a firing with a new row and a new id, so a post by id alone
    would still name a row that is gone. A firing is its alert and start.
  - *`action.share` over every firing*, unanswered ones as "no": the share
    would follow how much was answered (above, "How reports count them").
- **Rules judged by nothing.**
  - *Use what exists: a signal that never returns*, as the team did
    (above).
  - *A note's status* (`keep`, `tune`, `retire`). An editor's, about what
    is to become of a rule, not how it is judged, which is an admin's.
- **A signal taken away** (item 5).
  - *Use what exists: give the file the old expression as `impact`*, as
    #151's workaround does: the windows are a signal in effect's again,
    and it is read every run, one of the eight, to judge weeks it no
    longer measures.
  - *The record of the rules tied to it while it was read*, kept by rule
    as it is taken away (open question 9). It keeps the record those
    windows kept for the rules they were about, at the cost of rows kept
    at every taking away, and of serve keeping records a fresh `lapilli
    verdict` can't; decided against.
  - *Ties by alert, kept from what the signal had seen.* Its `seen` and
    `match`, kept at the taking away, would tie each alert again as they
    did, at the cost of reading the alerts' labels wherever the record is
    read, which the tallies, kept by rule, don't hold.
- **Groups**: above, "Two other shapes".

## Not in this design

- An entry's lead against written incidents: written incidents keep
  `--lead` (open question 1).
- Deploys by episode: `during_deploys` stays by firing.
- People's verdicts on firings inside a marked window, counted apart:
  `marked` counts what the stones say.
- Verdicts set by an agent, or copied in by a job from a chat's reactions
  (open question 6).
- An episode's own thresholds of noisy: the same as firings' (open
  question 4).
- Noisy by episodes deciding anything, before some eight weeks of
  episodes have been read (open question 3).
- Rows by label: not now (item 4, open question 8).

## Three cases

1. **A cause rule.** Team `shop`'s `MemoryPressure` fires on `checkout`'s
   pods some 40 minutes before its 5xx window, week after week, and is
   black: 30 firings, 1 white. An admin adds an entry, `^MemoryPressure$`,
   signals `[errors]`, match `[service]`, lead `1h`. The try says it gains
   6 catches, one of which agreed on no label: a node's alert with no
   `service`. Saved, the windows' spans of four weeks are pended and
   judged again; its precision reads 23%, and the incident page of each
   window shows the memory alert first, 38 minutes before. The one that
   agreed on no label an on-call engineer marks unrelated. With
   `require: [service]` on the entry (step 1) it wouldn't have caught the
   window at all.
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
   engineer says it again. That week the team answered 110 of its 120
   firings, 30 of them needing action, 27%: its goal of `action.share` at
   least 25% is met, "110 of 120 answered". The week before it answered
   60 of 115, and the goal was missed, "under 80% answered", whatever
   share those 60 gave.

## Open questions

Decided on 2026-11-19, by the maintainer, before the design was written,
and worked out above:

- **A lead per signal and per rule entry**, judging otherwise keeping its
  shape.
- **Episodes beside firings**, changing no number of today's.
- **A person's verdict on one firing**: "in the window but unrelated", for
  that firing alone, and "did it need action?".
- **The order** of the steps: leads, episodes, people's verdicts, then
  rows by label, each its own pull request.
- **A signal taken away** (item 5) is this design's, not a fix of its
  own, placed right after the leads: a judging fix, which changes
  `Agrees` as the leads do, and needs nothing of episodes or verdicts.

The questions this design left open, each decided on 2026-11-19, by the
maintainer, as its review recommended:

1. **An entry's lead against written incidents.** Decided on 2026-11-19,
   by the maintainer: no. Written incidents keep `--lead`, as they keep
   the match in effect.
2. **Labels a firing must carry.** Decided on 2026-11-19, by the
   maintainer: `require:` on an entry, opt-in, built in step 1
   ("Coincidences"), since the coincidence of the first of the three
   cases needs it.
3. **Noisy by episodes deciding.** Decided on 2026-11-19, by the
   maintainer: it decides nothing (not `noisy_only`, the suggestions or
   the noisy badge) until some eight weeks of episodes have been read;
   then a design of its own.
4. **Thresholds of their own for episodes.** Decided on 2026-11-19, by
   the maintainer: the same as firings' (`noisy_at`), no
   `--noisy-episodes`.
5. **A goal held to Lapilli's stones alone.** Decided on 2026-11-19, by
   the maintainer: yes, `stones: lapilli` on a goal, leaving out people's
   verdicts and the incidents "This was real" wrote too, as
   `precision_by_lapilli` does (item 3, "How reports count them").
6. **Verdicts from elsewhere** (a job copying "needed action" from a
   chat's reactions, an agent drafting answers for a person). Decided on
   2026-11-19, by the maintainer: out of this design. If built later, it
   writes under a name of its own, as `ledger.FiringsCopier` names what
   the firings port copies in, and what it writes never counts toward
   `action.share`.
7. **Midnight, or not.** Decided on 2026-11-19, by the maintainer: an
   episode parts at midnight in `--tz`.
8. **Rows by label.** Decided on 2026-11-19, by the maintainer: not now.
   If built later: groups by the alerts' labels, within one team, with
   goals taking `group:` in a later step (item 4).
9. **Whose record a taken-away signal's windows keep.** Decided on
   2026-11-19, by the maintainer: no rule's. Only the rules with no entry
   may catch such a window, on the match labels it was found with; no
   rows are kept for it; on the record, serve agrees with a fresh
   `lapilli verdict` everywhere; and whoever wants those weeks kept
   confirms the windows that were real outages, as a confirmed window is
   an incident written down (item 5).

## Steps

1. **Leads.** `verdict.Options.LeadFor` and `MaxLead`, and every place
   that reached back by `o.Lead`; `--impact-lead NAME=DUR`; `lead:` in
   `--impact-file`'s signals and entries; the Settings page's field and
   the entries' YAML, the try with its catches that agreed on no label,
   and `/settings.yaml`; the next schema (`lead_ns`); the spans pended on a
   change, leads compared apart from the rest of a signal or an entry; the
   digest's and goals' wait, for weeks and occurrences; the report's and
   the summary's settings; numbers.md's stone section; signals.md's open
   question 4 closed there; and `require:` on an entry, opt-in.
2. **A signal taken away** (item 5), on #151, with no schema. `RecordOf`
   leaving a window of a signal not in effect out of the record, and
   `Agrees` reading entries for it, with named signals in effect or none;
   the upgrade's spans pended; a window's page, a rule's page, the
   Incident sources panel and MCP; numbers.md's "No record";
   `TestSignalRemovedWindowsCaughtByAnyRule` turned round; #151's case
   with no `impact` in the file judged as `lapilli verdict` judges it,
   and the docs' way round it ("give the file the old expression as
   `impact`") taken out.
3. **Episodes.** `verdict.Judge`'s episodes; the next schema; the tally's
   walk, and the walk from the newest week back on an upgrade or a change
   of zone; the summary's `episodes`, a rule's page, the rules list's
   column and facet, the digest's noisy line, MCP, the report (with its
   zone and each firing's day), the CSV and `/metrics`; the marked counts
   and the two goal measures; numbers.md's "Episodes".
4. **People's verdicts.** The next schema; the ledger's write, part by
   part, and `stones`; verdicts in every replay, on both sides; the keys
   moved with incidents' keys; the forms (with series and start, "This
   was real"'s among them), `/api/v1/verdicts`, roles, the 14 days and the
   trail, a bulk post one change, 30 verdict writes an hour a person, the
   same 14 days for "This was real"; moved verdicts on the Cleanup page and
   the overview; entries apart from named signals, and `judged: false`;
   the summary's `people` and `not_judged`, the report, the CSV,
   `action.share`, `stones: lapilli` on a goal and `lapilli_rule_action`;
   what the Goals page says beside each goal; numbers.md's "A person's
   verdict". It may be two pull
   requests, as signals' step 2 was: the ledger, judging, the API and the
   numbers first, the pages after.
5. **Rows by label**: not now, as decided (open question 8).
