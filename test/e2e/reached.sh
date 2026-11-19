#!/usr/bin/env bash
# Which of CI's end-to-end jobs a pull request's changes reach
# (.github/workflows/ci.yml, changes): the changed paths on stdin, a line
# each; on stdout, a line for each job, its id=true or id=false, as
# $GITHUB_OUTPUT takes them. On a pull request, a job runs only when
# reached; on main, on the schedule and when asked, every job runs,
# whatever this says.
#
# A job is reached by what its script builds, runs, sources or mounts:
# lapilli itself, which each one builds (go.mod, go.sum, cmd/ and
# internal/, but their tests and testdata), the script, and what it reads
# of the repository. A path that reaches none is named so (Markdown, docs/,
# site/, assets/, Go's tests, other workflows); a path named nowhere here
# reaches every job.
# shellcheck disable=SC2086 # a list of jobs, split into its ids
set -euo pipefail

jobs="e2e e2e-replicas e2e-serve-lb e2e-service e2e-postgres e2e-signin e2e-changes e2e-backtest e2e-loki e2e-rendered e2e-dashboard e2e-ratelimit"
chart="e2e e2e-replicas e2e-serve-lb e2e-service e2e-rendered e2e-dashboard"
image="e2e e2e-replicas e2e-serve-lb e2e-service"

reached=" "
to() {
  for j in "$@"; do reached="$reached$j "; done
}

while IFS= read -r f || [ -n "$f" ]; do
  case $f in
    '') ;;
    # lapilli, which every job builds, and what runs the jobs
    go.mod | go.sum | .github/workflows/ci.yml | test/e2e/reached.sh) to $jobs ;;
    *.md | docs/* | site/* | assets/* | .github/* | LICENSE | .gitignore) ;;
    *_test.go | */testdata/* | test/screenshots/*) ;;
    cmd/* | internal/*) to $jobs ;;
    charts/*) to $chart ;;
    Dockerfile | .dockerignore) to $image ;;
    test/e2e/stack.yaml) to e2e e2e-replicas e2e-serve-lb ;;
    test/e2e/signin-lib.sh) to e2e-signin e2e-serve-lb e2e-service ;;
    test/e2e/run.sh) to e2e ;;
    test/e2e/replicas.sh) to e2e-replicas ;;
    test/e2e/serve-lb.sh) to e2e-serve-lb ;;
    # the browser's pages: browse.mjs, with test/a11y's packages
    test/e2e/service.sh | test/e2e/service.yaml | test/e2e/browse.mjs | test/a11y/*) to e2e-service ;;
    test/e2e/postgres.sh) to e2e-postgres ;;
    test/e2e/signin.sh) to e2e-signin ;;
    test/e2e/changes.sh | recipes/changes/*) to e2e-changes ;;
    test/e2e/backtest.sh | recipes/backtest/*) to e2e-backtest ;;
    test/e2e/loki.sh) to e2e-loki ;;
    # the chart's git.sh, checked to be recipes/versions/git.sh
    test/e2e/rendered.sh | recipes/versions/*) to e2e-rendered ;;
    test/e2e/dashboard.sh) to e2e-dashboard ;;
    test/e2e/ratelimit.sh) to e2e-ratelimit ;;
    # the other recipes are run by Go's tests, against stand-ins
    recipes/firings/* | recipes/incidents/* | recipes/trackers/*) ;;
    *) to $jobs ;;
  esac
done

for j in $jobs; do
  case $reached in
    *" $j "*) echo "$j=true" ;;
    *) echo "$j=false" ;;
  esac
done
