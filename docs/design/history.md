# Rule versions from before the ledger began

Status: proposed, for v0.21.0. It changes which rules a first start's
backfill replays, so it is written down first, under
[GOVERNANCE.md](../../GOVERNANCE.md). The companion to [serve.md](serve.md),
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
