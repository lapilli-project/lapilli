# Round 39 — three runs with the adapter as it was merged, and what was looked at again

Nothing here is under a rule of [`docs/design-review-round39.md`](../../../../docs/design-review-round39.md).

**Three runs**, judged by no one: one run a case, made on 2026-10-07 at 23:12 UTC with
`lapilli-case` built in a tree at `2628f48` — the commit of the adapter and the guard — whose
documents and one test's counts were not yet committed, so the binary records its tree as modified.
They are to see that an agent given `kubectl` whole — with the guard deciding what is a read and
what the run withholds — investigates as before. Twenty-five steps, every one of which ran; nothing
refused by Claude Code or by the guard; none of `signs.py`'s five signs.

None of the three happens to write a namespace before the verb, which is the form the change was
made for. That form is held by tests — of the guard, of the adapter, and of the whole chain an
agent's `kubectl` goes through — and by putting command lines to Claude Code with a stand-in for
`kubectl`; not by these.

**Two instruments, run again** after a reader found what the first running of each could not see
(Part 4, *After the fact*):

| path | what |
|---|---|
| `signs-again.md` | `test/round39/posthoc.py` — R1's five signs with flags allowed between `kubectl` and its verb — on round 38's records, on both sets and on the three runs here |
| `sweep-again/<case>.md` | the sweep's report for each scenario with its reading of a command line repaired, and the commands of all thirty-nine runs of the round as the ones an agent typed: 233 of them, none differing |

`internal/grade` recomputes the process checks of the three runs.
