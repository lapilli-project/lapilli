#!/usr/bin/env bash
# Pull request backtests against real servers rather than stand-ins
# (recipes/backtest): a real Gitea keeps the rules and the pull requests, a
# real Prometheus the history. recipes/backtest/gitea.sh, run in a checkout
# of a pull request as a CI job runs it, with a bot's token, comments the
# report on a pull request that slows a rule; run again after another push,
# it edits that comment; it posts nothing on a pull request that changes no
# rule. A rule a pull request names to mention, reference and break things
# is rendered by Gitea itself: no mention, no link, no HTML, the table
# whole. A symbolic link whose path mentions a user and references an issue
# is refused, its path only in the job's log: the user is notified of
# nothing, and the issue isn't referenced.
#
# Needs Docker, Go, git, curl and jq. Leaves everything up when KEEP is set.
set -euo pipefail
cd "$(dirname "$0")/../.."
repo=$PWD

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
gitea=gitea/gitea:28.3.0-rootless@sha256:40c5a68a0463c0940e1ae32626c8300dd0572f3558e390073ad8379ca67c0e1d
p=lapilli-e2e-bt

work=$(mktemp -d)
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  docker rm -fv "$p-gitea" "$p-prom" >/dev/null 2>&1 || true
  docker network rm "$p" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

fail=0
check() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
until_true() { # seconds, command...: run it until it succeeds
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 2
  done
}

go build -o "$work/lapilli" ./cmd/lapilli
mkdir -p "$work/secrets" "$work/prom"
export GIT_CONFIG_GLOBAL=$work/gitconfig GIT_CONFIG_NOSYSTEM=1
printf '[user]\n\tname = Reviewer\n\temail = rev@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n' >"$GIT_CONFIG_GLOBAL"

docker network create "$p" >/dev/null
docker run -d --name "$p-gitea" --network "$p" -p 127.0.0.1::3000 \
  -e GITEA__security__INSTALL_LOCK=true -e GITEA__database__DB_TYPE=sqlite3 \
  -e GITEA__service__DISABLE_REGISTRATION=true "$gitea" >/dev/null
# Prometheus scrapes itself every 2s: a minute of history is enough.
printf 'global: {scrape_interval: 2s}\nscrape_configs:\n  - job_name: prometheus\n    static_configs: [{targets: ["localhost:9090"]}]\n' >"$work/prom/prometheus.yml"
docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 \
  -v "$work/prom/prometheus.yml:/etc/prometheus/prometheus.yml:ro" "$prom" --config.file=/etc/prometheus/prometheus.yml >/dev/null
gt=http://$(docker port "$p-gitea" 3000/tcp | head -1)
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
until_true 120 curl -sf "$gt/api/v1/version"
until_true 60 curl -sf "$pu/-/ready"
started=$(date +%s)

# The repository is a reviewer's; a bot comments on its pull requests, and
# its token (write:issue, read:user) is the recipe's.
for u in reviewer lapilli-bot victim; do
  docker exec "$p-gitea" gitea admin user create --username "$u" --password "e2e-$u-pass-123" \
    --email "$u@example.com" --must-change-password=false >/dev/null
done
docker exec "$p-gitea" gitea admin user generate-access-token --username reviewer --token-name r \
  --scopes write:repository,write:issue,write:user --raw | tail -1 >"$work/secrets/rev"
docker exec "$p-gitea" gitea admin user generate-access-token --username lapilli-bot --token-name b \
  --scopes write:issue,read:repository,read:user --raw | tail -1 >"$work/secrets/bot"
docker exec "$p-gitea" gitea admin user generate-access-token --username victim --token-name v \
  --scopes read:notification,read:user --raw | tail -1 >"$work/secrets/victim"
rev=$(cat "$work/secrets/rev")
gapi() { curl -sf -H "Authorization: token $rev" -H 'Content-Type: application/json' "$@"; }
gapi -X POST "$gt/api/v1/user/repos" -d '{"name":"monitoring","default_branch":"main","private":true}' >/dev/null
gapi -X PUT "$gt/api/v1/repos/reviewer/monitoring/collaborators/lapilli-bot" -d '{"permission":"read"}' >/dev/null
gapi -X PUT "$gt/api/v1/repos/reviewer/monitoring/collaborators/victim" -d '{"permission":"read"}' >/dev/null
gapi -X POST "$gt/api/v1/repos/reviewer/monitoring/issues" -d '{"title":"An issue a path could reference"}' >/dev/null

rules() { # for, and more rules
  cat <<EOF
groups:
  - name: shop
    rules:
      # Fires all the time here: a scrape always takes some time.
      - alert: ScrapeSlow
        expr: scrape_duration_seconds{job="prometheus"} > 0.000001
        for: $1
        labels:
          severity: page
      - alert: TargetDown
        expr: up == 0
        for: 5m
        labels:
          severity: page
$2
EOF
}
mkdir -p "$work/seed/alerts"
rules 20s "" >"$work/seed/alerts/shop.yaml"
printf 'rules\n' >"$work/seed/README.md"
git -C "$work/seed" init -q
git -C "$work/seed" add -A
git -C "$work/seed" commit -qm rules
authed=${gt/http:\/\//http://reviewer:$rev@}/reviewer/monitoring.git
git -C "$work/seed" push -q "$authed" HEAD:main

# pr BRANCH TITLE: pushes the seed's HEAD as BRANCH and opens a pull request
# from it into main; writes its number.
pr() {
  git -C "$work/seed" push -q -f "$authed" "HEAD:refs/heads/$1"
  gapi -X POST "$gt/api/v1/repos/reviewer/monitoring/pulls" -d "{\"head\":\"$1\",\"base\":\"main\",\"title\":\"$2\"}" | jq -r .number
}
# backtest N: checks pull request N out as a CI job does, its own commit,
# and runs the recipe there; the history is the last few minutes.
backtest() {
  rm -rf "$work/ci"
  # Each line the job's log has of RENDER and lapilli is after a "| ".
  git clone -q "$authed" "$work/ci"
  git -C "$work/ci" fetch -q origin "refs/pull/$1/head"
  git -C "$work/ci" checkout -q --detach FETCH_HEAD
  (cd "$work/ci" && PR=$1 GITEA_API=$gt/api/v1 GITEA_REPO=reviewer/monitoring GITEA_TOKEN_FILE=$work/secrets/bot \
    LAPILLI=$work/lapilli PATHS=alerts TARGET_BRANCH=main \
    sh "$repo/recipes/backtest/gitea.sh" --prometheus "$pu" --since "$(($(date +%s) - started + 5))s" --step 5s --tz UTC)
}
comments() { gapi "$gt/api/v1/repos/reviewer/monitoring/issues/$1/comments"; }

# A minute of history, for ScrapeSlow's 20s to fire in.
sleep 60

# A pull request slowing ScrapeSlow past what the history holds.
git -C "$work/seed" checkout -q -b slower
rules 1h "" >"$work/seed/alerts/shop.yaml"
git -C "$work/seed" commit -qam "ScrapeSlow waits an hour"
n=$(pr slower "Slow ScrapeSlow down")
backtest "$n" 2>"$work/run1.log" || { cat "$work/run1.log"; exit 1; }
c=$(comments "$n")
check "one comment, the bot's" "$(jq -r '[.[] | .user.login] | join(",")' <<<"$c")" '^lapilli-bot$'
body=$(jq -r '.[0].body' <<<"$c")
id=$(jq -r '.[0].id' <<<"$c")
check "its first line, the marker" "$(head -1 <<<"$body")" '^\[//\]: # \(lapilli-backtest default\)$'
check "the summary line" "$(sed -n 2p <<<"$body")" '"nothing_changed":false,"changed":1,'
check "the slowed rule's row" "$body" '^\| `ScrapeSlow` \| `page` \| 1 → 0 \| 0 → 0 \|$'
check "the period, in UTC" "$body" '^### Lapilli backtest, .* UTC$'

# Another push to it: the same comment, edited.
rules 25s "" >"$work/seed/alerts/shop.yaml"
git -C "$work/seed" commit -qam "Not that slow"
git -C "$work/seed" push -q -f "$authed" HEAD:refs/heads/slower
backtest "$n" 2>"$work/run2.log" || { cat "$work/run2.log"; exit 1; }
c=$(comments "$n")
check "still one comment" "$(jq -r 'length' <<<"$c")" '^1$'
check "the same one" "$(jq -r '.[0].id' <<<"$c")" "^$id$"
check "edited: the rule fires as it did" "$(jq -r '.[0].body' <<<"$c")" '1 changed rules fire as they did'
check "said it edited it" "$(cat "$work/run2.log")" "updated comment $id on pull request $n"

# A pull request that changes no rule: no comment.
git -C "$work/seed" checkout -q main
git -C "$work/seed" checkout -q -b docs
printf 'rules, and how to read them\n' >"$work/seed/README.md"
git -C "$work/seed" commit -qam "Docs"
n=$(pr docs "Docs only")
backtest "$n" 2>"$work/run3.log" || { cat "$work/run3.log"; exit 1; }
check "no comment when nothing changed" "$(comments "$n" | jq -r length)" '^0$'
check "said why" "$(cat "$work/run3.log")" 'nothing that decides when a rule fires changed, and there is no comment to update: none posted'

# A rule named to mention, reference, link, break the table and hide what
# follows, as Gitea renders the comment.
git -C "$work/seed" checkout -q main
git -C "$work/seed" checkout -q -b hostile
hostile='@reviewer #1 reviewer/monitoring#1 <b>bold</b> [link](https://evil.example) | cell `tick` <!-- hide'
rules 20s "      - alert: \"${hostile//\"/\\\"}\"
        expr: scrape_duration_seconds{job=\"prometheus\"} > 0.000001
        for: 20s
        labels:
          severity: \"@reviewer | <i>x</i>\"" >"$work/seed/alerts/shop.yaml"
git -C "$work/seed" commit -qam "A rule with a name"
n=$(pr hostile "Hostile")
backtest "$n" 2>"$work/run4.log" || { cat "$work/run4.log"; exit 1; }
body=$(comments "$n" | jq -r '.[0].body')
html=$(jq -n --arg t "$body" '{Text: $t, Mode: "comment", Context: "reviewer/monitoring"}' | gapi -X POST "$gt/api/v1/markdown" -d @-)
check "the hostile rule is in the comment" "$body" 'reviewer/monitoring#1'
check "no mention, no reference, no link" "$(grep -oE '<a [^>]*>' <<<"$html" | tr '\n' ' ' || true)" '^$'
check "no HTML of the rule's" "$(grep -oE '<(b|i)>' <<<"$html" | tr '\n' ' ' || true)" '^$'
check "nothing hidden" "$(grep -c '<!--' <<<"$html" || true)" '^0$'
check "the rule's name, whole, in code" "$html" '<code>@reviewer #1 reviewer/monitoring#1 &lt;b&gt;bold&lt;/b&gt; \[link\]\(https://evil.example\) \| cell `tick` &lt;!-- hide</code>'
cells=$(tr -d '\n' <<<"$html" | sed 's/<tr>/\n<tr>/g' | grep '^<tr>' | while read -r row; do grep -o '<t[dh][ >]' <<<"$row" | wc -l | tr -d ' '; done | sort -u | tr '\n' ' ')
check "every row of each table whole" "$cells" '^3 4 $'

# A symbolic link whose path mentions and references, refused.
git -C "$work/seed" checkout -q main
git -C "$work/seed" checkout -q -b link
d="$work/seed/alerts/x @victim #1 reviewer/monitoring#1/www.evil.example <b>bold</b>"
mkdir -p "$d"
ln -s /etc/hosts "$d/**ALL CLEAR - merge it**"
rules 30s "" >"$work/seed/alerts/shop.yaml"
git -C "$work/seed" add -A
git -C "$work/seed" commit -qm "A link"
n=$(pr link "A link")
backtest "$n" 2>"$work/run5.log" || { cat "$work/run5.log"; exit 1; }
body=$(comments "$n" | jq -r '.[0].body')
html=$(jq -n --arg t "$body" '{Text: $t, Mode: "comment", Context: "reviewer/monitoring"}' | gapi -X POST "$gt/api/v1/markdown" -d @-)
check "the link refused" "$body" 'holds a symbolic link, which could lead to any file the job can read: not read'
check "its path only in the job's log" "$(grep -c 'victim\|ALL CLEAR\|www.evil' <<<"$body" || true)" '^0$'
check "where, in the job's log" "$(cat "$work/run5.log")" 'a symbolic link in the pull request.s commit: alerts/x @victim'
check "no mention, no reference, no link" "$(grep -oE '<a [^>]*>' <<<"$html" | tr '\n' ' ' || true)" '^$'
sleep 3
notified() { curl -sf -H "Authorization: token $(cat "$work/secrets/victim")" "$gt/api/v1/notifications?all=true" | jq -r length; }
referenced() { gapi "$gt/api/v1/repos/reviewer/monitoring/issues/1/timeline" | jq -r '[(. // [])[] | select(.ref_issue != null or .ref_comment != null)] | length'; }
check "the user notified of nothing" "$(notified)" '^0$'
check "the issue referenced by nothing" "$(referenced)" '^0$'
# The same words written by a person do both: the checks above can fail.
gapi -X POST "$gt/api/v1/repos/reviewer/monitoring/issues/$n/comments" -d '{"body":"@victim look at #1"}' >/dev/null
sleep 3
check "a person's mention notifies" "$(notified)" '^[1-9]'
check "a person's reference references" "$(referenced)" '^[1-9]'

if [ "$fail" != 0 ]; then
  echo "--- the last comment, rendered"
  echo "$html"
  exit 1
fi
echo "all passed"
