#!/usr/bin/env bash
# Changes to rules, carried out against real servers rather than stand-ins
# (docs/design/changes.md, docs/design/rulers.md): a real Prometheus loading
# the rules from a checkout of a real Gitea repository, a real Grafana, and
# a real Mimir, which that Prometheus writes to. A person's browser is
# played with curl behind a proxy's header, as the unit tests do. On the
# git path, a longer for, a fix to what a rule says and a deletion go from
# proposal to pull request (opened by recipes/changes/gitea.sh with a bot's
# token), are merged, and are read back as applied; the deletion is undone.
# On the Grafana path, a longer for is applied and undone byte for byte, a
# rule is turned off, and a rule edited in Grafana since, or kept by another
# tool, is refused. On the ruler path, through Mimir's configuration API, a
# longer for is applied (one line of its namespace, loaded by Mimir's ruler)
# and undone byte for byte; a runbook link is applied; a change made afresh
# when another rule in its group changed, and refused when the rule itself
# did; a group's last rule deleted (the group goes) and undone; a
# namespace's only rule deleted; and a namespace --changes-in doesn't name
# left for a person; a group Mimir writes back in a form YAML can't read is
# passed over, the rest read, a namespace of more than 128 bytes among them. Amazon's and Azure's rulers have stand-ins
# only.
#
# Needs Docker, Go, git, curl and jq. Leaves everything up when KEEP is set.
#
# GitLab is too heavy to start each run (ten minutes, 4 GB). Checked by
# hand against GitLab CE 19.5.4 on 2026-11-11: recipes/changes/gitlab.sh
# opened, and saw merged, a merge request read back as applied; and
# recipes/trackers/gitlab.sh linked a task to the issue it made (GitLab
# links issues as work items, -/work_items/N), with the quick actions in
# the task's text left as text, and moved the task to done when the issue
# was closed.
# shellcheck disable=SC2016,SC2329 # templates are Prometheus's; functions run through until_true
set -euo pipefail
cd "$(dirname "$0")/../.."
repo=$PWD

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
grafana=grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf
gitea=gitea/gitea:28.3.0-rootless@sha256:40c5a68a0463c0940e1ae32626c8300dd0572f3558e390073ad8379ca67c0e1d
mimir=grafana/mimir:3.2.4@sha256:e4ff85d922c7c0a5131576d2659a1b98b3abfd73c92448dccf8ad3a6049a7ed8
p=lapilli-e2e-chg
port=${PORT:-28180} # serve takes port to port+5

work=$(mktemp -d)
pids=()
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  docker rm -fv "$p-gitea" "$p-grafana" "$p-prom" "$p-mimir" >/dev/null 2>&1 || true
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
mkdir -p "$work/secrets" "$work/prom" "$work/mimir"
export GIT_CONFIG_GLOBAL=$work/gitconfig GIT_CONFIG_NOSYSTEM=1
printf '[user]\n\tname = Reviewer\n\temail = rev@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n' >"$GIT_CONFIG_GLOBAL"

docker network create "$p" >/dev/null
docker run -d --name "$p-gitea" --network "$p" -p 127.0.0.1::3000 \
  -e GITEA__security__INSTALL_LOCK=true -e GITEA__database__DB_TYPE=sqlite3 \
  -e GITEA__service__DISABLE_REGISTRATION=true "$gitea" >/dev/null
docker run -d --name "$p-grafana" --network "$p" -p 127.0.0.1::3000 \
  -e GF_SECURITY_ADMIN_PASSWORD=e2e-admin-pass "$grafana" >/dev/null
# Mimir, in one process with storage on its filesystem: its ruler polls
# what is stored every 10s.
cat >"$work/mimir/mimir.yaml" <<'YAML'
multitenancy_enabled: false
target: all
server: {http_listen_port: 9009, log_level: warn}
common:
  storage: {backend: filesystem, filesystem: {dir: /data/common}}
blocks_storage: {storage_prefix: blocks, tsdb: {dir: /data/tsdb}}
ruler_storage: {storage_prefix: ruler}
ruler: {rule_path: /data/ruler-tmp, poll_interval: 10s}
ingester: {ring: {replication_factor: 1}}
store_gateway: {sharding_ring: {replication_factor: 1}}
YAML
docker run -d --name "$p-mimir" --network "$p" -p 127.0.0.1::9009 \
  -v "$work/mimir/mimir.yaml:/etc/mimir.yaml:ro" "$mimir" -config.file=/etc/mimir.yaml >/dev/null
gt=http://$(docker port "$p-gitea" 3000/tcp | head -1)
gf=http://$(docker port "$p-grafana" 3000/tcp | head -1)
mu=http://$(docker port "$p-mimir" 9009/tcp | head -1)
until_true 120 curl -sf "$gt/api/v1/version"
until_true 120 curl -sf "$gf/api/health"
until_true 120 sh -c "curl -sf '$mu/ready' | grep -q ready"

# Gitea: the repository belongs to a reviewer; a bot may write to it, and
# its token is the recipe's.
for u in reviewer lapilli-bot; do
  docker exec "$p-gitea" gitea admin user create --username "$u" --password "e2e-$u-pass-123" \
    --email "$u@example.com" --must-change-password=false >/dev/null
done
docker exec "$p-gitea" gitea admin user generate-access-token --username reviewer --token-name r \
  --scopes write:repository,write:user --raw | tail -1 >"$work/secrets/rev"
docker exec "$p-gitea" gitea admin user generate-access-token --username lapilli-bot --token-name b \
  --scopes write:repository,read:user --raw | tail -1 >"$work/secrets/bot"
rev=$(cat "$work/secrets/rev")
gapi() { curl -sf -H "Authorization: token $rev" -H 'Content-Type: application/json' "$@"; }
gapi -X POST "$gt/api/v1/user/repos" -d '{"name":"monitoring","default_branch":"main","private":true}' >/dev/null
gapi -X PUT "$gt/api/v1/repos/reviewer/monitoring/collaborators/lapilli-bot" -d '{"permission":"write"}' >/dev/null

mkdir -p "$work/seed/alerts" "$work/seed/.gitea/workflows"
cat >"$work/seed/alerts/shop.yaml" <<'EOF'
groups:
  - name: shop
    rules:
      # Fires all the time here: a scrape always takes some time.
      - alert: ScrapeSlow
        expr: scrape_duration_seconds{job="prometheus"} > 0.000001
        for: 1m
        labels:
          severity: page
        annotations:
          summary: "Scrape slow on {{ $labels.instance }}"
      - alert: TargetDown
        expr: up == 0
        for: 5m
        labels:
          severity: page
        annotations:
          summary: "{{ $labels.job }} is down"
          runbook_url: https://wiki.example.com/runbooks/target-down
EOF
printf 'on: [push]\njobs:\n  check:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo check\n' >"$work/seed/.gitea/workflows/ci.yaml"
git -C "$work/seed" init -q
git -C "$work/seed" add -A
git -C "$work/seed" commit -qm rules
authed=${gt/http:\/\//http://reviewer:$rev@}/reviewer/monitoring.git
git -C "$work/seed" push -q "$authed" HEAD:main
git clone -q "$authed" "$work/checkout"
original=$(git -C "$work/checkout" rev-parse HEAD)
sync() { git -C "$work/checkout" pull -q "$authed" main; curl -sf -X POST "$pu/-/reload"; }

# Prometheus loads the rules from the checkout, as from a git-sync, and
# writes what it scrapes to Mimir.
printf 'global: {scrape_interval: 5s, evaluation_interval: 5s}\nrule_files: [/etc/prometheus/rules/*.yaml]\nscrape_configs:\n  - job_name: prometheus\n    static_configs: [{targets: ["localhost:9090"]}]\nremote_write:\n  - url: http://%s-mimir:9009/api/v1/push\n' "$p" >"$work/prom/prometheus.yml"
docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 \
  -v "$work/prom/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -v "$work/checkout/alerts:/etc/prometheus/rules:ro" \
  "$prom" --config.file=/etc/prometheus/prometheus.yml --web.enable-lifecycle >/dev/null
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
until_true 60 curl -sf "$pu/-/ready"

# Grafana: Prometheus as a data source; a Viewer's token to read the rules,
# an Admin's to change them (an Editor's pause is kept: see changes.md).
ga=(-u admin:e2e-admin-pass -H 'Content-Type: application/json')
curl -sf "${ga[@]}" -X POST "$gf/api/datasources" \
  -d "{\"name\":\"prom\",\"uid\":\"prom\",\"type\":\"prometheus\",\"url\":\"http://$p-prom:9090\",\"access\":\"proxy\"}" >/dev/null
curl -sf "${ga[@]}" -X POST "$gf/api/folders" -d '{"uid":"shop","title":"shop"}' >/dev/null
for role in Viewer Admin; do
  id=$(curl -sf "${ga[@]}" -X POST "$gf/api/serviceaccounts" -d "{\"name\":\"lapilli-$role\",\"role\":\"$role\"}" | jq -r .id)
  curl -sf "${ga[@]}" -X POST "$gf/api/serviceaccounts/$id/tokens" -d "{\"name\":\"$role\"}" | jq -j .key >"$work/secrets/grafana-$role"
done
grule() { # title, expression, comparison, threshold, provenance header
  jq -n --arg t "$1" --arg e "$2" --arg c "$3" --argjson v "$4" '{folderUID: "shop", ruleGroup: "shop", title: $t, condition: "C",
    data: [{refId: "A", relativeTimeRange: {from: 600, to: 0}, datasourceUid: "prom", model: {expr: $e, refId: "A", instant: true}},
      {refId: "C", datasourceUid: "__expr__", model: {type: "threshold", expression: "A", refId: "C",
        conditions: [{evaluator: {type: $c, params: [$v]}}]}}],
    noDataState: "OK", execErrState: "Error", for: "1m", labels: {severity: "page"},
    annotations: {summary: "Grafana sees {{ $labels.instance }}"}}' |
    curl -sf "${ga[@]}" ${5:+-H "$5"} -X POST "$gf/api/v1/provisioning/alert-rules" -d @- | jq -r .uid
}
slow=$(grule GrafanaScrapeSlow 'scrape_duration_seconds{job="prometheus"}' gt 0.000001 'X-Disable-Provenance: true')
low=$(grule GrafanaUpLow up lt 0.5 'X-Disable-Provenance: true')
grule KeptByAnotherTool up lt 0.5 >/dev/null # provenance api

# Mimir's rules, written as Grafana's UI or a person would write them: the
# shop's namespace (a group of two alert rules and a recording rule, and a
# group of one), a namespace Alloy's mimir.rules.kubernetes would name with
# a ScrapeSlow of its own, and a namespace of one rule.
mrules=$mu/prometheus/config/v1/rules
mpost() { curl -sf -X POST -H 'Content-Type: application/yaml' --data-binary @- "$mrules/$1" >/dev/null; }
shop_group='name: shop
interval: 30s
rules:
  - alert: ScrapeSlow
    expr: scrape_duration_seconds{job="prometheus"}>0.000001
    for: 1m
    labels:
      severity: page
    annotations:
      summary: "Scrape slow on {{ $labels.instance }}"
  - record: job:up:sum
    expr: sum by (job) (up)
  - alert: TargetDown
    expr: up == 0
    for: 5m
    labels:
      severity: page
'
mpost shop <<<"$shop_group"
mpost shop <<'YAML'
name: edge
rules:
  - alert: EdgeDown
    expr: up{job="edge"} == 0
    for: 2m
YAML
mpost alloy%2Fpayments%2Frules%2F0b1c <<'YAML'
name: shop
rules:
  - alert: ScrapeSlow
    expr: scrape_duration_seconds{job="prometheus"} > 1
    labels:
      severity: page
YAML
mpost solo <<'YAML'
name: solo
rules:
  - alert: SoloDown
    expr: up{job="solo"} == 0
YAML
# An expression whose first line starts with spaces: Mimir writes it back
# as a block YAML can't read (|4, its lines two short).
mpost lead <<'YAML'
name: lead
rules:
  - alert: Lead
    expr: "  up\n== 0\n"
YAML
# A namespace of more than 128 bytes, as Alloy names them: Mimir lists it
# as a long key, "? NAME" and then ":" and its groups.
long=alloy/kube-prometheus-stack/kube-prometheus-stack-kubernetes-system-controller-manager/3f2c5a1e-7d4b-4c1e-9a0f-2b6e8d1c4a7f-extra
mpost "${long//\//%2F}" <<'YAML'
name: long
rules:
  - alert: LongNamespace
    expr: up{job="long"} == 0
YAML

# lapilli serve, three times: the rules from the checkout (the git path),
# Grafana's (its API), and Mimir's (its configuration API).
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/secrets/changes-git"
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/secrets/changes-grafana"
roles=(--user-header X-Forwarded-Email --editors '*' --admins ana@example.com --approvers 'ana@example.com,cy@example.com'
  --backfill 2h --settle 1m --watch 10s --prometheus "$pu")
"$work/lapilli" serve "${roles[@]}" --rules "$work/checkout/alerts" --ledger "$work/ledger-git" \
  --listen "127.0.0.1:$port" --incidents-listen "127.0.0.1:$((port + 1))" \
  --changes --changes-token-file "$work/secrets/changes-git" >"$work/serve-git.log" 2>&1 &
pids+=($!)
"$work/lapilli" serve "${roles[@]}" --grafana "$gf" --grafana-token-file "$work/secrets/grafana-Viewer" --ledger "$work/ledger-grafana" \
  --listen "127.0.0.1:$((port + 2))" --incidents-listen "127.0.0.1:$((port + 3))" \
  --changes --changes-token-file "$work/secrets/changes-grafana" --changes-grafana-token-file "$work/secrets/grafana-Admin" \
  >"$work/serve-grafana.log" 2>&1 &
pids+=($!)
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/secrets/changes-mimir"
mroles=("${roles[@]}")
mroles[${#mroles[@]} - 1]=$mu/prometheus
"$work/lapilli" serve "${mroles[@]}" --ruler "$mrules" --ledger "$work/ledger-mimir" \
  --listen "127.0.0.1:$((port + 4))" --incidents-listen "127.0.0.1:$((port + 5))" \
  --changes --changes-token-file "$work/secrets/changes-mimir" --changes-in shop,solo >"$work/serve-mimir.log" 2>&1 &
pids+=($!)
sg=http://127.0.0.1:$port
sf=http://127.0.0.1:$((port + 2))
sm=http://127.0.0.1:$((port + 4))
until_true 60 curl -sf -H 'X-Forwarded-Email: bo@example.com' "$sg/api/v1/proposals"
until_true 60 curl -sf -H 'X-Forwarded-Email: bo@example.com' "$sf/api/v1/proposals"
until_true 60 curl -sf -H 'X-Forwarded-Email: bo@example.com' "$sm/api/v1/proposals"

# A person's browser, behind a proxy that names them.
propose() { # serve, who, form fields...
  local base=$1 who=$2 args=()
  shift 2
  for f in "$@"; do args+=(--data-urlencode "$f"); done
  curl -s -o /dev/null -w '%{redirect_url}' -H "X-Forwarded-Email: $who" -H 'Sec-Fetch-Site: same-origin' "${args[@]}" "$base/proposals" |
    sed -nE 's|.*/proposals/([0-9]+)$|\1|p'
}
form() { # serve, who, id, approve or undo: the form's key on the proposal's page
  curl -s -H "X-Forwarded-Email: $2" "$1/proposals/$3" |
    sed -nE "s|.*action=\"/proposals/$3/$4\"><input type=\"hidden\" name=\"key\" value=\"([0-9a-f]+)\".*|\1|p" | head -1
}
act() { # serve, who, id, approve or undo
  local key
  key=$(form "$@")
  [ -n "$key" ] || return 1
  curl -s -o /dev/null -H "X-Forwarded-Email: $2" -H 'Sec-Fetch-Site: same-origin' --data-urlencode "key=$key" "$1/proposals/$3/$4"
}
state() { curl -s -H 'X-Forwarded-Email: ana@example.com' "$1/api/v1/proposals/$2" | jq -r '(.proposal // .) | .state'; }
reason() { curl -s -H 'X-Forwarded-Email: ana@example.com' "$1/api/v1/proposals/$2" | jq -r '(.proposal // .) | .reason // ""'; }
is() { [ "$(state "$1" "$2")" = "$3" ]; }
newest() { curl -s -H 'X-Forwarded-Email: ana@example.com' "$1/api/v1/proposals" | jq '[.proposals[].id] | max'; }

mkdir -p "$work/bot"
printf '%s\n' "${gt/http:\/\//http://lapilli-bot:$(cat "$work/secrets/bot")@}" >"$work/bot/credentials"
printf '[credential]\n\thelper = store --file=%s\n[commit]\n\tgpgsign = false\n' "$work/bot/credentials" >"$work/bot/gitconfig"
recipe() { # as a CI job holding the bot's credentials runs it
  env -i PATH="$PATH" HOME="$work/bot" GIT_CONFIG_GLOBAL="$work/bot/gitconfig" GIT_CONFIG_NOSYSTEM=1 \
    LAPILLI_URL="http://127.0.0.1:$((port + 1))" LAPILLI_TOKEN_FILE="$work/secrets/changes-git" \
    CLONE="$gt/reviewer/monitoring.git" RULES_PATHS=alerts \
    GITEA_API="$gt/api/v1" GITEA_REPO=reviewer/monitoring GITEA_TOKEN_FILE="$work/secrets/bot" \
    sh "$repo/recipes/changes/gitea.sh" >>"$work/recipe.log" 2>&1
}
pr() { gapi "$gt/api/v1/repos/reviewer/monitoring/pulls/$1${2:-}"; }
merge() { gapi -X POST "$gt/api/v1/repos/reviewer/monitoring/pulls/$1/merge" -d '{"Do":"squash","delete_branch_after_merge":true}' >/dev/null; }
through() { # id: the pull request opened, merged, synced and read back
  recipe
  check "#$1 opened as a pull request" "$(state "$sg" "$1")" '^opened$'
  local n
  n=$(curl -s -H 'X-Forwarded-Email: ana@example.com' "$sg/api/v1/proposals/$1" | jq -r '(.proposal // .) | .link' | sed 's|.*/||')
  check "#$1's pull request by the bot, into main" "$(pr "$n" | jq -r '.user.login + " " + .base.ref')" '^lapilli-bot main$'
  merge "$n"
  sync
  recipe
  until_true 60 is "$sg" "$1" applied || true
  check "#$1 merged and read back" "$(state "$sg" "$1")" '^applied$'
}

# The git path.
id=$(propose "$sg" bo@example.com rule=ScrapeSlow kind=for for=2m)
check "a person proposes" "$id" '^[0-9]+$'
check "no approving one's own" "$(form "$sg" bo@example.com "$id" approve)" '^$'
act "$sg" ana@example.com "$id" approve
until_true 30 is "$sg" "$id" ready
recipe
n=$(curl -s -H 'X-Forwarded-Email: ana@example.com' "$sg/api/v1/proposals/$id" | jq -r '(.proposal // .) | .link' | sed 's|.*/||')
check "the pull request changes one line" "$(pr "$n" .diff | grep -cE '^[-+] ')" '^2$'
merge "$n"
sync
recipe
until_true 60 is "$sg" "$id" applied || true
check "a longer for, applied" "$(state "$sg" "$id")" '^applied$'
until_true 30 sh -c "curl -s '$pu/api/v1/rules' | jq -e '.data.groups[].rules[] | select(.name == \"ScrapeSlow\" and .duration == 120)'" || true
check "Prometheus holds it" "$(curl -s "$pu/api/v1/rules" | jq '.data.groups[].rules[] | select(.name == "ScrapeSlow") | .duration')" '^120$'

said='Scrape of {{ $labels.instance | reReplaceAll ":.*" "" | toUpper }} took {{ printf "%.3f" $value }}s{{ if gt $value 1.0 }}, over a second{{ end }}. "Quoted" and \back\slash.'
id=$(propose "$sg" bo@example.com rule=ScrapeSlow kind=says annotation=description "text=$said")
act "$sg" cy@example.com "$id" approve
until_true 30 is "$sg" "$id" ready
through "$id"
until_true 60 sh -c "curl -s '$pu/api/v1/alerts' | jq -e '.data.alerts[] | select(.annotations.description // \"\" | startswith(\"Scrape of LOCALHOST\"))'" || true
check "Prometheus says it" "$(curl -s "$pu/api/v1/alerts" | jq -r '.data.alerts[] | select(.labels.alertname == "ScrapeSlow") | .annotations.description')" \
  '^Scrape of LOCALHOST took [0-9.]+s\. "Quoted" and \\back\\slash\.$'
check "a template that grows, refused" "$(propose "$sg" bo@example.com rule=TargetDown kind=says annotation=description \
  'text={{ range $labels }}{{ range $labels }}x{{ end }}{{ end }}')" '^$'

check "a rule file has no off: retiring is its deletion" "$(propose "$sg" bo@example.com rule=TargetDown kind=disable)" '^$'
id=$(propose "$sg" bo@example.com rule=TargetDown kind=delete)
act "$sg" ana@example.com "$id" approve
until_true 30 is "$sg" "$id" ready
through "$id"
check "Prometheus no longer has it" "$(curl -s "$pu/api/v1/rules" | jq -c '[.data.groups[].rules[].name]')" '^\["ScrapeSlow"\]$'
act "$sg" ana@example.com "$id" undo
undo=$(newest "$sg")
until_true 30 is "$sg" "$undo" ready
through "$undo"
check "the deletion, undone" "$(state "$sg" "$id")" '^undone$'
check "the file as it was, but for the two changes" "$(git -C "$work/checkout" diff "$original" HEAD | grep -cE '^[-+] ')" '^3$'
check "the recipe wrote only under RULES_PATHS" "$(git -C "$work/checkout" diff --name-only "$original" HEAD)" '^alerts/shop.yaml$'

# The Grafana path.
export_rules() { curl -s -H "Authorization: Bearer $(cat "$work/secrets/grafana-Viewer")" "$gf/api/v1/provisioning/alert-rules/export?format=yaml"; }
graf() { curl -s "${ga[@]}" "$gf/api/v1/provisioning/alert-rules/$1"; }
before=$(export_rules)
id=$(propose "$sf" bo@example.com rule=GrafanaScrapeSlow kind=for for=3m)
act "$sf" ana@example.com "$id" approve
until_true 60 is "$sf" "$id" applied || true
check "a longer for, applied in Grafana" "$(state "$sf" "$id") $(graf "$slow" | jq -r '.for + " " + (.provenance // "")')" '^applied 3m $'
check "nothing else changed" "$(diff <(echo "$before") <(export_rules) | grep -cE '^[<>]')" '^2$'
act "$sf" ana@example.com "$id" undo
undo=$(newest "$sf")
until_true 60 is "$sf" "$undo" applied || true
check "undone, byte for byte" "$(diff <(echo "$before") <(export_rules) | wc -l | tr -d ' ')" '^0$'

id=$(propose "$sf" bo@example.com rule=GrafanaUpLow kind=disable)
act "$sf" ana@example.com "$id" approve
until_true 60 is "$sf" "$id" applied || true
check "turned off in Grafana" "$(state "$sf" "$id") $(graf "$low" | jq .isPaused)" '^applied true$'
check "deleted only four weeks after" "$(propose "$sf" bo@example.com rule=GrafanaUpLow kind=delete)" '^$'

id=$(propose "$sf" bo@example.com rule=GrafanaScrapeSlow kind=says annotation=runbook_url text=https://wiki.example.com/runbooks/scrape)
graf "$slow" | jq '.labels.team = "shop" | del(.id, .updated, .provenance)' |
  curl -sf "${ga[@]}" -H 'X-Disable-Provenance: true' -X PUT "$gf/api/v1/provisioning/alert-rules/$slow" -d @- >/dev/null
act "$sf" ana@example.com "$id" approve
until_true 60 is "$sf" "$id" refused || true
check "edited in Grafana since: refused" "$(state "$sf" "$id"): $(reason "$sf" "$id")" '^refused: the rule changed in Grafana'
until_true 30 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sf/api/v1/rules' | grep -q KeptByAnotherTool"
id=$(propose "$sf" bo@example.com rule=KeptByAnotherTool kind=for for=5m)
act "$sf" ana@example.com "$id" approve
until_true 60 is "$sf" "$id" refused || true
check "kept by another tool: refused" "$(reason "$sf" "$id")" 'another tool keeps this rule'

# The ruler path, through Mimir's configuration API.
mns() { curl -s "$mrules/$1"; }
mloaded() { # namespace, alert: the rule as Mimir's ruler has loaded it
  curl -s "$mu/prometheus/api/v1/rules" | jq -c --arg f "$1" --arg n "$2" '.data.groups[] | select(.file == $f) | .rules[] | select(.name == $n)'
}
until_true 60 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sm/api/v1/rules' | grep -q 'ScrapeSlow \\[alloy%2F'"
names=$(curl -s -H 'X-Forwarded-Email: bo@example.com' "$sm/api/v1/rules" | grep -oE 'ScrapeSlow \[[^]"]*\]' | sort -u | tr '\n' ',')
check "the two ScrapeSlows told apart, by namespace" "$names" '^ScrapeSlow \[alloy%2Fpayments%2Frules%2F0b1c/shop\],ScrapeSlow \[shop/shop\],$'
check "Mimir writes the lead group as YAML can't read back" "$(mns lead | grep -c '|4')" '^1$'
check "a namespace of more than 128 bytes, listed as a long key, is read" \
  "$(( ${#long} > 128 )) $(curl -s "$mrules" | grep -c '^? alloy/kube') $(curl -s -H 'X-Forwarded-Email: bo@example.com' "$sm/api/v1/rules" | grep -q '"LongNamespace"' && echo read)" '^1 1 read$'
check "it is passed over, and logged once; the rest are read" \
  "$(grep -c 'namespace \\"lead\\", group \\"lead\\"' "$work/serve-mimir.log") $(curl -s -H 'X-Forwarded-Email: bo@example.com' "$sm/api/v1/rules" | grep -c '"Lead"')" '^1 0$'
shop_before=$(mns shop)
id=$(propose "$sm" bo@example.com 'rule=ScrapeSlow [shop/shop]' kind=for for=2m)
check "a ruler's proposal" "$(curl -s -H 'X-Forwarded-Email: ana@example.com' "$sm/api/v1/proposals/$id" | jq -r '.path')" '^ruler$'
act "$sm" ana@example.com "$id" approve
until_true 60 is "$sm" "$id" applied || true
check "a longer for, applied in Mimir" "$(state "$sm" "$id")" '^applied$'
check "the namespace differs by that one line" "$(diff <(echo "$shop_before") <(mns shop) | grep -E '^[<>]' | tr '\n' ' ')" '^< +for: 1m > +for: 2m $'
until_true 60 sh -c "curl -s '$mu/prometheus/api/v1/rules' | jq -e '.data.groups[] | select(.file == \"shop\") | .rules[] | select(.name == \"ScrapeSlow\" and .duration == 120)'" || true
check "Mimir's ruler loaded it" "$(mloaded shop ScrapeSlow | jq .duration)" '^120$'
act "$sm" ana@example.com "$id" undo
undo=$(newest "$sm")
until_true 60 is "$sm" "$undo" applied || true
check "undone in Mimir" "$(state "$sm" "$id")" '^undone$'
check "the namespace as it was, byte for byte" "$(diff <(echo "$shop_before") <(mns shop) | wc -l | tr -d ' ')" '^0$'

id=$(propose "$sm" bo@example.com 'rule=ScrapeSlow [shop/shop]' kind=says annotation=runbook_url text=https://wiki.example.com/runbooks/scrape-slow)
act "$sm" cy@example.com "$id" approve
until_true 60 is "$sm" "$id" applied || true
until_true 60 sh -c "curl -s '$mu/prometheus/api/v1/rules' | jq -e '.data.groups[] | select(.file == \"shop\") | .rules[] | select(.annotations.runbook_url != null)'" || true
check "a runbook link, in Mimir's rules API" "$(mloaded shop ScrapeSlow | jq -r .annotations.runbook_url)" '^https://wiki.example.com/runbooks/scrape-slow$'

# Another rule in the group changed between proposing and approving: made
# afresh. The rule itself changed: refused as stale.
id=$(propose "$sm" bo@example.com 'rule=ScrapeSlow [shop/shop]' kind=for for=3m)
curl -s "$mrules/shop/shop" | sed 's/for: 5m/for: 10m/' | mpost shop
act "$sm" ana@example.com "$id" approve
until_true 60 is "$sm" "$id" applied || true
check "another rule changed since: applied" "$(state "$sm" "$id")" '^applied$'
check "TargetDown as changed, ScrapeSlow as proposed" "$(curl -s "$mrules/shop/shop" | grep -E '^ +for:' | tr -d ' ' | tr '\n' ',')" '^for:3m,for:10m,$'
until_true 30 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sm/api/v1/proposals?state=applying,approved' | jq -e '.proposals | length == 0'" || true
id=$(propose "$sm" bo@example.com 'rule=ScrapeSlow [shop/shop]' kind=for for=4m)
curl -s "$mrules/shop/shop" | sed 's/>0.000001/>0.000002/' | mpost shop
act "$sm" ana@example.com "$id" approve
until_true 60 is "$sm" "$id" refused || true
check "ScrapeSlow itself changed since: refused as stale" "$(state "$sm" "$id"): $(reason "$sm" "$id")" '^refused: the rule changed in the ruler since it was proposed'

id=$(propose "$sm" bo@example.com rule=EdgeDown kind=delete)
act "$sm" ana@example.com "$id" approve
until_true 60 is "$sm" "$id" applied || true
check "EdgeDown deleted: its group answers 404" "$(state "$sm" "$id") $(curl -s -o /dev/null -w '%{http_code}' "$mrules/shop/edge")" '^applied 404$'
act "$sm" ana@example.com "$id" undo
undo=$(newest "$sm")
until_true 60 is "$sm" "$undo" applied || true
check "undone: the group is back" "$(state "$sm" "$undo") $(curl -s "$mrules/shop/edge" | grep -E 'alert:|for:' | tr -d ' -' | tr '\n' ',')" '^applied alert:EdgeDown,for:2m,$'

id=$(propose "$sm" bo@example.com rule=SoloDown kind=delete)
act "$sm" ana@example.com "$id" approve
until_true 60 is "$sm" "$id" applied || true
check "solo's only rule deleted: the group goes" "$(state "$sm" "$id") $(mns solo)" '^applied \{\}$'
id=$(propose "$sm" bo@example.com 'rule=ScrapeSlow [alloy%2Fpayments%2Frules%2F0b1c/shop]' kind=for for=5m)
check "the alloy namespace's ScrapeSlow is for a person, with why" \
  "$(curl -s -H 'X-Forwarded-Email: ana@example.com' "$sm/api/v1/proposals/$id" | jq -r '.path + ": " + (.evidence.no_path // "")')" \
  "^none: its namespace isn't one --changes-in names$"

if [ "$fail" != 0 ]; then
  echo "--- serve (git)"; tail -20 "$work/serve-git.log"
  echo "--- serve (Grafana)"; tail -20 "$work/serve-grafana.log"
  echo "--- serve (Mimir)"; tail -20 "$work/serve-mimir.log"
  echo "--- recipe"; tail -20 "$work/recipe.log"
fi
exit "$fail"
