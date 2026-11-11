# Versions of the rules from where they are kept

Rules change: a `for` is raised, a rule is added after an incident. A replay
is right only with the rules as they stood at each time. `--rules-history DIR`
reads a directory of earlier versions, one subdirectory each, named by when
it took effect and holding the rule files as they stood then:

```
history/
  20261001T100000Z/alerts/...
  20261020T083000Z/alerts/...
  20261025T120000Z/alerts/...
```

A name is an RFC 3339 time, `2026-10-20T08:30:00Z`, or its basic form,
`20261020T083000Z`. Names starting with a dot are passed over; any other name
that isn't a time is refused.

- **`lapilli verdict`** reads it as each version given as `--rules DIR@TIME`.
- **`lapilli serve`** reads it beside `--rules` on a start whose ledger has
  no version yet, so its first backfill replays each day with that day's
  rules. A ledger with versions of its own keeps them and passes the
  directory over, so the flag can stay. A version that can't be read stops
  the start, with the ledger as it was; one from after now is passed over,
  and an empty directory starts with today's rules.

## From git

`git.sh` writes the directory from a clone with its history:

```sh
REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 OUT=history ./git.sh
```

| Variable | |
|---|---|
| `REPO` | the clone (default: the directory it runs in) |
| `PATHS` | the rule files or directories in it, space-separated, as `--rules` reads them |
| `SINCE` | from when, a date (the start of that day, in UTC) or an RFC 3339 time |
| `OUT` | the directory to write: new, or empty |
| `RENDER` | optional: a command run in each commit's files that writes the rules on its stdout |
| `MAX` | the most versions written (default 500): commits, or with `DEPLOYS` the deploys that changed the rules |

- **Which versions.** The rules as they stood at `SINCE`, from the last
  commit that changed `PATHS` before it, then each commit on the branch that
  changed them. Only the branch's own commits count (`git log
  --first-parent`): a merge is a version at the merge's time, not at the
  times of the commits merged.
- **When.** A version is named by its commit's time, in UTC, and at least a
  second after the version before it: a commit's time is what its
  committer's clock said, and one dated back mustn't reorder them. A commit
  isn't when a change was in effect: if it reached Prometheus or Grafana
  later, rename the directory to that time.
- **Rules rendered from a chart.** `RENDER` runs in a copy of each commit's
  files, and what it writes becomes the version's `rules.yaml`:
  `RENDER='helm template monitoring charts/monitoring'`, or a `kustomize
  build`. It runs without `TOKEN_FILE` and with a `HOME` of its own.
- **Links.** A version is files: a commit with a symbolic link among
  `PATHS`, or in the files `RENDER` reads, is refused.
- **A shallow clone** has no history; it says so and writes only the rules as
  they are. Clone the branch whole, or with `--shallow-since` before `SINCE`.
- It needs `sh` and `git`, reads the clone and changes nothing in it, and
  writes `OUT` only once every version is written: what fails leaves it as it
  was. More than `MAX` versions is refused: give a later `SINCE`.

## When a version was deployed

A commit isn't when a change was in effect: Argo CD or Flux deploys it,
minutes or days later. With `DEPLOYS=FILE`, the versions are the rules each
deploy put in place, by when it did. The file is `REVISION TIME` a line,
the earliest first, as these write it, reading only:

| Script | Reads | Needs |
|---|---|---|
| `argocd.sh` | an Argo CD Application's `status.history`: each sync's `revision` (`revisions[ARGOCD_SOURCE]` for several sources) and `deployedAt` | `ARGOCD_URL`, `ARGOCD_TOKEN_FILE` (a token that may `get` applications), `ARGOCD_APP`; or the Application on stdin, with `-` |
| `flux.sh` | a Flux Kustomization's `status.history` (Flux 2.7 on), failed reconciles left out, or before 2.7, its `lastAppliedRevision` at its last reconcile's time (not the revision's first: run it often); a HelmRelease's releases that were deployed (`deployed` or `superseded`, not `failed`, pending, uninstalled or unknown) whose chart version ends `+SHA` (or `+SHA.N`, a chart from values files). A chart from an OCIRepository ends with its digest, which names no commit: `git.sh` passes it over as a revision the clone doesn't have | the resource on stdin, as `kubectl get -o json` writes it |

```sh
kubectl get application alerts -n argocd -o json | DEPLOYS_KEEP=deploys.txt ./argocd.sh - > deploys.new && mv deploys.new deploys.txt
REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 OUT=history DEPLOYS=deploys.txt ./git.sh
```

- Argo CD keeps ten syncs, Flux five snapshots: run them each time,
  `DEPLOYS_KEEP` keeping what earlier runs saw, each deploy (a revision
  and a time) once; a revision deployed again, a rollback, is a deploy
  again.
- The deploys are walked in time from the last at or before `SINCE`, the
  rules in effect then (with none, the rules as committed at `SINCE`
  stand for them, and it says so). Each deploy that changed the rules from
  the one before is a version, a rollback too; commits deployed together
  are one, and a deploy that changed nothing (Flux before 2.7 writes one
  each reconcile) isn't one, nor counted against `MAX`. A commit no deploy
  holds yet isn't in effect.
- A deploy is never dated before its revision's commit: Flux dates a
  snapshot a revert brought back by when it was first applied.
- A revision reaches git only as 7 to 64 hex digits, taken only as the
  commit whose full name starts with it, found among the clone's objects
  (a tag or a branch named like it is neither taken nor hides it; two
  commits that start alike are passed over, and said), and a time only as `YYYY-MM-DDTHH:MM:SSZ`, not after now:
  anything else in the file is refused, and nothing is written.

In Kubernetes, the chart's `serve.rulesHistory` runs `git.sh` in an init
container on each start, without `DEPLOYS`.
