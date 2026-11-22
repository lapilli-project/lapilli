# `lapilli demo`: a first report, with nothing to connect

Status: proposed, built in the same pull request. Under
[GOVERNANCE.md](../../GOVERNANCE.md) a new command is designed first, as a
pull request open for a week; this one carries the code too, so the
design can be read beside what it does. It adds nothing to the ledger,
and changes how no firing is judged.

## Why

Before anything shows, a newcomer needs a Prometheus with a week of
history, the rule files it was evaluated with, and incidents written
down. Each takes a while to find, and getting one wrong shows up only
after a run. Seeing first what a report says, on rules anyone can read,
says whether the rest is worth it.

## What it runs

`lapilli demo` is `lapilli verdict`, its own code from the flags on: the
rules read from a file, replayed, judged, the suggestions and near misses
worked out, the report written as `lapilli verdict` writes it. Only the
history is another: the sample's, answered in the process.

- **The sample**, embedded, a few KB: `cmd/lapilli/demo/rules.yaml`, seven
  rules of a made-up shop as a Prometheus rule file has them, and
  `demo/sample.yaml`, the incidents of a week and, for each expression a
  rule compares with a number, its series: a value all week and the
  spans it is another. A time there is a weekday and a time of day.
- **The history** (`demoHistory`) answers an expression the sample has a
  series of, alone or compared with a number at its end, as Prometheus
  would: the steps where it holds. Anything else has no series. So a
  rule replayed with a longer `for`, or a nearer number, is answered as
  the rule is.
- **The period** is a fixed week, Monday 9 November 2026 on, laid in the
  zone shown (`--tz`, or the machine's): a firing at 01:10 there came
  while people slept wherever it is run. The report is the same every
  run, and in every zone but for the zone it names.
- **The files**: the rules and the incidents (the week's and the week
  before's, which keep the record of its first days) are written to a
  directory of `os.MkdirTemp`'s and read from there as anyone's are, and
  the directory removed after.

It takes `--json` and `--tz`, as verdict does, and nothing else. After the
report it says how to run the same on one's own rules, and `lapilli
impact suggest` for those with no incidents written down: on stdout, or
on stderr with `--json`, so that stdout is the JSON alone.

The seam is `verdictOn`: `lapilli verdict` with a history given in place
of `--prometheus`'s, which is then never asked. `newHistory`, the only
way to one, isn't called; a test fails if it is.

`TestDemo` holds the report's lines that say what Lapilli is for: the
noisy rule and its longer `for`, the catches, the incident missed and the
rule that nearly caught it, the firings that woke people. A change to how
firings are judged that changes them fails it, and the sample is changed
with it, on purpose.

## What it never does

- Read a Prometheus, or anything over the network: there is no client in
  its path.
- Send anything: no flag of verdict's that sends (`--push`, `--slack-webhook-file`)
  is taken.
- Write anywhere but its temporary directory, which it removes.
- Read a file of the user's: its flags name none.

## Later: `--serve`

`lapilli demo --serve` would start `lapilli serve` on a temporary ledger
of the sample, the weeks of the sample laid on the four weeks before now,
on loopback alone, and remove the ledger on exit. It is a step of its own,
with its own look from an attacker's point of view: a port, however
local.
