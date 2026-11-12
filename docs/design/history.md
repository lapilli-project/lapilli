# Rule versions from before the ledger began

Status: built, for v0.21.0. It changes which rules a first start's
backfill replays, so it was written down first, under
[GOVERNANCE.md](../../GOVERNANCE.md), as a pull request (#38); it was merged
on 2026-11-10, before its week was out, by the maintainer's decision, with
no comments. The companion to [serve.md](serve.md),
whose "Rule versions" it extends.

## Why

`lapilli serve` keeps a version of the rules each time they change, and
replays each hour with the version in effect then. Before its first start
it knows only the rules as they are: its first version is "the rules as they
stood before the ledger began". So a first start's backfill, four weeks by
default, replays all four weeks with today's rules.

That is wrong exactly where it matters. A rule tuned ten days ago is
replayed with its new `for` over the eighteen days before the change, and
judged on firings it never had; a rule added after an incident is replayed
over that incident, and is credited with catching it. The first check
against a production Grafana found such a case: an incident the replay of
that day's rules caught within a minute of the real alert, and the replay
of today's rules missed. `lapilli
verdict` takes `--rules FILE@TIME` for this. `lapilli serve` refuses it,
since it records each version itself, from its start on.

Most teams keep their rules in git. The versions are there.

## What it proposes

`--rules-history DIR`, for `serve` and `verdict`: a directory of earlier
versions of the rules, one subdirectory each, named by the time it took
effect, holding the rule files as they stood then.

```
rules-history/
  2026-09-28T10:12:00Z/alerts/...
  2026-10-14T16:40:31Z/alerts/...
  2026-10-31T09:05:12Z/alerts/...
```

A name is an RFC 3339 time in UTC; the basic form, `20261014T164031Z`, is
taken as well, for tools that don't like colons in names.

- **`serve`** adds them as versions on a start whose ledger has no version
  before them, in time order, before its first run: the one in effect at the
  backfill's start is the latest at or before it, and each later one takes
  effect at its time. Rules keep their names from one version to the next
  as they do now. It never changes a version it recorded itself, so the
  flag can stay in a deployment: on later starts, versions older than the
  ledger's own are already there or are passed over, and it logs which.
  The rules `--rules` names are still the current ones, and a version from
  the directory that is the same as them adds nothing.
- **`verdict`** reads it as it reads several `--rules DIR@TIME`, and the
  same checks hold: the first must be from before the period starts.

Only rules read from files have a history this way; those read from an API
(`--loaded-rules`, `--grafana`) don't.

## Where the directory comes from

`recipes/versions/git.sh`, beside the recipes for incidents and firings: in
a clone with the history, for `PATHS` (files or directories) and `SINCE`, it
writes the rules as each commit that changed them left them, and as they
stood at `SINCE`, into `OUT/TIME/`. The time is the commit's.

- **Rules rendered from a chart,** as many Prometheus rules are, need the
  chart rendered at each commit: `RENDER`, a command run in a checkout of
  each commit, writes them instead (`helm template ... > rules.yaml`, or a
  `kustomize build`).
- **Bounded:** 500 commits at most, read only; it changes nothing in the
  clone.
- **In Kubernetes,** an init container with git runs it into a volume
  `serve` reads. The chart's `serve.rulesHistory` adds one, with the
  repository, the paths and a Secret for the credentials, from an image
  pinned by digest. A shallow clone, as git-sync makes by default, has no
  history: the recipe says so and writes only today's.

Lapilli itself doesn't read git. Its image has no git, a Go library for it
would add a large dependency for one start, and rendering a chart at each
commit is the user's toolchain's to do. A directory of versions is something
any tool can write, git or not, and a person can write by hand.

## When a version took effect

A commit is when the change was written, not when it was in effect. The
lag is minutes for a pipeline that deploys on merge, days for one that
deploys on a schedule. The recipe takes the commit's time; where the deploy
time is known (from a deployment tool's history), the directory names can
say it instead, and the README says so. A version a few minutes early
replays those minutes with the new rules: a far smaller error than four
weeks of today's.

## Alternatives

- **Reading git in lapilli,** with a Go library. Covered above: a large
  dependency, no rendering, and shallow clones in clusters.
- **`--rules FILE@TIME` for `serve`,** repeated. Fine for two versions, and
  the directory is the same thing for twenty, in one flag that can stay in
  a deployment.
- **Replaying only from the first start,** with no backfill. The backfill is
  what gives a new ledger something to show on its first day.

## Questions for review

- The commit's time, or the merge's? For a merge commit they differ; the
  recipe takes the time of the commit that is on the branch, which for a
  merge is the merge.
- Should `serve` refuse a history whose newest version differs from the
  rules `--rules` reads now, as a sign the directory is stale, or only log
  it?

## As built

Built for v0.21.0 as above, the questions for review settled:

- **The merge's time.** The recipe takes the branch's own commits
  (`git log --first-parent`): a merge is a version at the merge's time.
- **A stale directory** is logged, not refused: when the rules `--rules`
  reads differ from the newest version, they are taken as changed at the
  start, as they would be without the directory.

Where the building differs, or says more:

- `serve` adds the directory's versions only to a ledger with no version
  at all, not to one with versions from after them: a ledger that has
  begun keeps its own, whole.
- Every version is read before any is kept. One that can't be read was
  first meant to stop the start with the ledger as it was; since 0.31.0 it
  is passed over, saying so, and the one before it stays in effect (see
  below). A version from after now, a commit dated ahead of the clock, is
  passed over with a warning; an empty directory, as when git couldn't be
  reached, starts with today's rules. A ledger with versions of its own
  doesn't read the directory at all, so it can't hold a restart up.
- A version, or a file in one, that is a link to outside it is refused: the
  files come from a repository, which could hold a link to any file
  `serve` can read; a version that is itself a link is checked where it
  leads. The recipe refuses a commit with a link among `PATHS`; elsewhere
  in the repository, a link is the repository's own business.
- The recipe names each version at least a second after the one before
  it, in the branch's order: a commit's time is its committer's clock's. A
  commit dated after now stops it, as it would name every later version
  after it too, and `serve` passes versions from the future over.
- In the chart, one init container clones with the token, and another,
  without it mounted, runs the recipe and `RENDER`, so a chart's commands
  never reach it; the clone takes every object, so nothing is fetched
  later. The credential helper gives the token to the repository's host
  alone.
- With `verdict`, `--rules` beside `--rules-history` are the rules as they
  are now: the newest version, from a second after the directory's newest.
- The version in effect when the backfill starts is kept as the rules from
  before the ledger began; those before it aren't kept. When none is from
  before the backfill, the earliest is taken as those, and the log says so.
- `verdict` keeps, of the versions from before its period, only the latest.
- The chart's `serve.rulesHistory` clones the branch without checking it
  out (`--no-checkout`), and `since` is by default the
  backfill and a day more before the start. The image is `alpine/git`
  v2.52.0, pinned by digest; the recipe is in the chart as
  `files/versions-git.sh`, and a test keeps it the same as
  `recipes/versions/git.sh`.

## Rules a repository renders, followed (0.31.0)

Measured on a real setup: the rules lived in a Helm chart, as Grafana's
provisioning files in several ConfigMaps labelled for its sidecar, beside
the chart's Deployments and Services, changed by many pull requests a week.
Replaying each day with that day's rules meant rendering each commit with
`helm template`, filtering the output down to the rule ConfigMaps with
`yq`, and, at older commits that didn't vendor the subchart's archive, a
`helm dependency build` or a copied archive first. And the history was
read once, at the first start: the commits after it were seen only as the
deployed ConfigMaps changed, if lapilli could mount them at all.

- **Rendered manifests are read for their rules.** A file may be a stream
  of documents of any kinds: those that hold rules are kept, the others
  passed over and counted, and said. `--rules-select`, a Kubernetes label
  selector, keeps only the objects it matches; a rule file that isn't an
  object has no labels, and is kept. Each document kept is kept as it was
  written; serve keeps of a version in `--rules-history` only its
  documents of rules (no Secret a chart rendered reaches the ledger), and
  of a `--rules` file what the selector left out isn't kept either, so
  reading the ledger's documents again leaves it out too. Untrimmed, a
  `--rules` file is kept whole, as the change recipes need it.
- **Rendering each commit, reliably.** `RENDER_PREPARE` runs before
  `RENDER` in each commit's files, as `RENDER` runs (no token, a `HOME` of
  its own kept for the run), given `REPO`, the clone. `UNRENDERED=skip`
  passes over a commit that can't be rendered, saying which and why; by
  default it stops the recipe, as before.
- **Following.** Each version holds `.commit`; `git.sh AFTER=COMMIT` writes
  the commits after it, named after the newest version, and `follow.sh`
  adds them to the directory, renamed in from a hidden directory. It
  fetches nothing: the credentials stay with whatever fetches. `serve`
  given `--rules-history` without `--rules` follows the directory: its
  newest version from before now is the rules in effect, read every
  `--watch`, and each version added later is recorded from its own time.
  The hours since then were replayed with the version before: the ledger
  notes where to replay from again (no further back than `--backfill`),
  the next run replays from there, recording firings over those hours as
  any run does, and the check sets those days beside what really fired
  again.
- **In the chart,** with no `rules.configMaps`, the repository is the
  rules: one container fetches the branch with the token, another, without
  it, runs `follow.sh`. The clone is in a volume of its own that only the
  fetching containers write; the ones running `render` mount it read-only,
  so nothing they run can change the configuration or hooks the next fetch
  runs with. `render` runs in `renderImage`, which must be pinned by
  digest: Argo CD's, which has git, helm and kustomize, so a chart renders
  as Argo CD renders it.

Not done: a webhook from the git host, for a commit seen at once rather
than within a fetch's interval (`every`, 300 s by default). When the
extras beside a followed history (alarms, Loki) change, the version
recorded for them from now holds the history's rules as they were; a
commit dated before it, fetched after, is in effect only until then, and
the next look records it again from now.

### Reviewed before release

Two reviews of the above, one as an attacker and one for correctness,
found these, all fixed before release:

- **Following stopped for good on one commit.** A version serve couldn't
  read (a rendering with no rules, ConfigMaps that lost the selected
  label, a file that doesn't parse) failed every look after it, and
  follow.sh stopped on a commit that couldn't be rendered, one dated
  after now, or more than `MAX` new commits. Now serve passes such a
  version over, once, saying so, the one before it staying in effect, at
  a first start as when following; the recipes pass a commit over, name
  one from the future now, and write the newest `MAX`. The chart's
  `unrendered` is `skip`.
- **A commit dated back rewrote the ledger.** A commit's date is what its
  committer's clock said, or when it was written in a branch that waited
  days to be merged. Named by it, a version took effect from days back,
  and serve replayed every hour since, up to `--backfill`, judging those
  hours with rules that weren't in effect then, for each such commit. A
  version's time is now bounded below by when the clone's branch first
  held its commit, by the branch's reflog, less the time between fetches
  and a minute. The bound holds against the repository's content: the
  reflog is written by the fetch, in a volume the side that renders reads
  only. It doesn't hold against the git host (one serving a commit late
  delays it, as any fetch would), and a commit already in the clone when
  it was made (the history written at the start) is named by its date.
  An honest commit, pushed and fetched within the bound, keeps its time.
- **A version recorded during a run was lost.** The watcher records a
  version while the worker's run, begun before it, replays its hours with
  the rules before. Each version now notes its time to replay again
  whatever the cursor, and each note bumps a sequence: a run forgets the
  note only if nothing was noted since it began, and a check keeps what it
  found only then. A good version followed by a bad one in one look is
  noted too, before it is kept.
- **Checks kept stale rows.** Sending the check back now takes away what
  it kept from that day on, in the same write.
- **Following beside alarms or Loki** read each version without them, so
  the next look recorded the same rules with them from now, and every
  later commit took effect when seen. Versions are now read with them,
  and following goes on from the last version followed.
- **What a commit holds, and what reading it costs.** git.sh read a
  commit through the render image's git configuration (Git LFS, required,
  its server the commit's choice) and the commit's own `.gitattributes`
  (`export-ignore`, `export-subst`); with `RENDER`, a link outside `PATHS`
  carried what it led to into a rendered rule; one blob named many times
  in a tree was gigabytes written out, and nothing bounded the volumes,
  the versions kept or a file of rules read. Now git runs with the
  clone's configuration alone, from an empty work tree and index; a link
  anywhere in a rendered commit, or a tree over `MAX_TREE_FILES` or
  `MAX_TREE_BYTES` by `git ls-tree`, is passed over; the chart's volumes
  have size limits and its containers ephemeral-storage limits; follow.sh
  takes away versions older than the backfill and a day; and a file of
  rules is read 16 MiB at most.
- **The picker** read a Secret with a top-level `groups` as rules (and
  kept it whole under a selector), didn't read a `ConfigMapList`, and
  counted a ConfigMap whose rule file doesn't parse as holding no rules.
