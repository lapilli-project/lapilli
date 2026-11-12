#!/usr/bin/env bash
# The chart's Grafana dashboard (charts/lapilli/dashboards/lapilli.json)
# against a real Grafana and Prometheus, the Prometheus scraping a real
# lapilli serve (docs/design/noc.md). lapilli serve replays three rules
# written on time alone (no metrics needed) over seventeen days, so a whole
# week is among them: one flapping every twenty minutes, one firing five
# minutes before users are hurt each day, one firing every two hours for
# nothing; --impact finds a window of
# user impact each day; two incidents are written down, one caught, one
# missed; a deploy each day of what the flapping rule is about; a team owns
# two of the rules; firings are copied in from a source, and a notification
# taken from an alert manager. Once Prometheus has scraped /metrics a while,
# the dashboard is loaded into Grafana, which must take it, and every
# panel's every query is asked of Grafana (/api/ds/query) with each
# variable at All, and again for the team: each must return data.
#
# Needs Docker, Go, curl, jq and python3. Takes about two minutes. Leaves
# everything up when KEEP is set.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
grafana=grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf
p=lapilli-e2e-dashboard

work=$(mktemp -d)
serve=
cleanup() {
  [ -n "$serve" ] && kill "$serve" 2>/dev/null || true
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  docker rm -fv "$p-prom" "$p-grafana" >/dev/null 2>&1 || true
  docker network rm "$p" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

until_true() { # seconds, command...: run it until it succeeds
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 2
  done
}
port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
day() { date -u -d "$1 days ago" +%F 2>/dev/null || date -u -v-"$1"d +%F; }

go build -o "$work/lapilli" ./cmd/lapilli
mkdir -p "$work/deploys" "$work/prov/datasources"

# Rules on time alone, so a Prometheus with no history replays them.
cat >"$work/rules.yaml" <<'YAML'
groups:
  - name: shop
    rules:
      - alert: SearchFlapping
        expr: label_replace(vector(1), "service", "search", "", "") and on () (minute(vector(time())) % 20 < 3)
        labels: {severity: critical}
        annotations: {summary: Search is flapping, runbook_url: "https://runbooks.example/search"}
      - alert: SearchDown
        expr: label_replace(vector(1), "service", "search", "", "") and on () (hour(vector(time())) == 9) and on () (minute(vector(time())) >= 55)
        labels: {severity: critical}
        annotations: {summary: Search is down, playbook: "docs/runbooks/search.md#down"}
      - alert: CartLatency
        expr: label_replace(vector(1), "service", "cart", "", "") and on () (hour(vector(time())) % 2 == 0) and on () (minute(vector(time())) == 30)
        labels: {severity: warning}
YAML
impact='label_replace(vector(1), "service", "search", "", "") and on () (hour(vector(time())) == 10) and on () (minute(vector(time())) < 15)'
d1=$(day 1)
d2=$(day 2)
cat >"$work/incidents.yaml" <<YAML
incidents:
  - {id: INC-1, title: Search errors, start: "${d2}T09:58:00Z", end: "${d2}T10:20:00Z", labels: {service: search}}
  - {id: INC-2, title: Checkout down, start: "${d1}T03:00:00Z", end: "${d1}T03:30:00Z", labels: {service: checkout}}
YAML
# As recipes/versions/argocd.sh writes an Application's history.
for i in 3 2 1; do printf 'a1b2c3d%s %sT10:15:00Z\n' "$i" "$(day "$i")"; done >"$work/deploys/search.txt"
printf 'deploys:\n  - labels: {service: search}\n    history: search.txt\n' >"$work/deploys/deploys.yaml"
printf 'e2e-firings-token-0123456789\n' >"$work/firings-token"
printf 'e2e-notify-token-0123456789\n' >"$work/notify-token"

docker network create "$p" >/dev/null
up=$(port)
mp=$(port)
ip=$(port)
np=$(port)
# Prometheus reaches lapilli's metrics port on this machine: Docker Desktop
# carries host.docker.internal to its loopback; Linux to the host's
# addresses, so there it listens on them.
mhost=127.0.0.1
[ "$(uname)" = Linux ] && mhost=0.0.0.0
cat >"$work/prometheus.yml" <<YAML
global: {scrape_interval: 5s, evaluation_interval: 5s}
scrape_configs:
  - job_name: lapilli
    static_configs: [{targets: ["host.docker.internal:$mp"]}]
YAML
docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 --add-host host.docker.internal:host-gateway \
  -v "$work/prometheus.yml:/etc/prometheus/prometheus.yml:ro" "$prom" >/dev/null
cat >"$work/prov/datasources/prom.yaml" <<YAML
apiVersion: 1
datasources:
  - {name: Prometheus, uid: prom, type: prometheus, access: proxy, url: "http://$p-prom:9090", isDefault: true}
YAML
docker run -d --name "$p-grafana" --network "$p" -p 127.0.0.1::3000 -e GF_SECURITY_ADMIN_PASSWORD=e2e-admin-pass \
  -e GF_ANALYTICS_REPORTING_ENABLED=false -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
  -v "$work/prov/datasources:/etc/grafana/provisioning/datasources:ro" "$grafana" >/dev/null
pu="http://$(docker port "$p-prom" 9090/tcp | head -1)"
gu="http://$(docker port "$p-grafana" 3000/tcp | head -1)"
until_true 60 curl -fsS "$pu/-/ready"

"$work/lapilli" serve --prometheus "$pu" --rules "$work/rules.yaml" --impact "$impact" --impact-for 5m --match service \
  --incidents "$work/incidents.yaml" --deploys "$work/deploys/deploys.yaml" --ledger "$work/ledger" --tz UTC --backfill 17d \
  --listen "127.0.0.1:$up" --metrics-listen "$mhost:$mp" --allowed-hosts host.docker.internal \
  --incidents-listen "127.0.0.1:$ip" --firings-token-file "$work/firings-token" \
  --notify-listen "127.0.0.1:$np" --notify-token-file "$work/notify-token" >"$work/serve.log" 2>&1 &
serve=$!
until_true 120 curl -fsS "http://127.0.0.1:$up/readyz" || { cat "$work/serve.log"; exit 1; }

# A team owning the search rules; firings copied in from a source; a
# notification an alert manager sent.
curl -fsS -X POST -H 'Content-Type: application/json' -d '{"name": "search", "rules": "^Search"}' "http://127.0.0.1:$up/api/v1/teams" >/dev/null
printf '{"rule": "DiskFull", "labels": {"service": "cart", "severity": "warning"}, "start": "%sT04:00:00Z", "end": "%sT04:20:00Z"}\n' "$d1" "$d1" |
  curl -fsS -X PUT -H "Authorization: Bearer $(cat "$work/firings-token")" -H 'Content-Type: application/x-ndjson' --data-binary @- \
    "http://127.0.0.1:$ip/api/v1/firings/cloud-e2e?from=${d1}T00:00:00Z&to=${d1}T23:59:59Z" >/dev/null
now=$(date -u +%FT%TZ)
curl -fsS -X POST -H "Authorization: Bearer $(cat "$work/notify-token")" -H 'Content-Type: application/json' \
  -d '{"version": "4", "groupKey": "{}:{alertname=\"SearchFlapping\"}", "status": "firing", "receiver": "oncall",
       "groupLabels": {"alertname": "SearchFlapping"}, "commonLabels": {"alertname": "SearchFlapping", "service": "search"},
       "commonAnnotations": {}, "externalURL": "http://alertmanager.example",
       "alerts": [{"status": "firing", "labels": {"alertname": "SearchFlapping", "service": "search", "severity": "critical"},
                   "annotations": {}, "startsAt": "'"$now"'", "endsAt": "0001-01-01T00:00:00Z", "fingerprint": "e2e1"}]}' \
  "http://127.0.0.1:$np/v1/alertmanager" >/dev/null

# What a dashboard and other tools read: the summary keeps its schema.
curl -fsS "http://127.0.0.1:$up/api/v1/summary?team=*" >"$work/summary.json"
jq -e '.schema == 1 and (.rows | length) >= 2 and ([.rows[].refired.firings] | add) > 0 and ([.rows[] | select(.team == "") | .impacts.caught] | add) > 0' \
  "$work/summary.json" >/dev/null || { echo "the summary:"; cat "$work/summary.json"; exit 1; }

# Scraped since the team and the rest were written, long enough for a rate
# over the scrapes.
has() { # a query Prometheus answers with something
  [ "$(curl -fsS --get "$pu/api/v1/query" --data-urlencode "query=$1" | jq -r '.data.result | length')" -gt 0 ]
}
until_true 120 has 'lapilli_refired_weeks{team="search"} > 0'
until_true 120 has 'lapilli_serve_notifications_last_timestamp_seconds'
until_true 120 has 'count_over_time(lapilli_rule_firings[1m]) > 5'

# The dashboard as the chart ships it, which Grafana must take.
until_true 120 curl -fsS "$gu/api/health"
g=(-fsS -u admin:e2e-admin-pass -H 'Content-Type: application/json')
jq '{dashboard: (. + {id: null}), overwrite: true}' charts/lapilli/dashboards/lapilli.json |
  curl "${g[@]}" -X POST --data-binary @- "$gu/api/dashboards/db" >"$work/saved.json"
jq -e '.status == "success"' "$work/saved.json" >/dev/null || { cat "$work/saved.json"; exit 1; }
curl "${g[@]}" "$gu/api/dashboards/uid/lapilli" >"$work/loaded.json"
jq -e --slurpfile want charts/lapilli/dashboards/lapilli.json '(.dashboard.panels | length) == ($want[0].panels | length)' "$work/loaded.json" >/dev/null

# Each panel's each query, asked of Grafana with the variables filled in.
failed=0
asked=0
for team in '.*' 'search'; do
  while IFS=$'\t' read -r title ref instant expr; do
    expr=${expr//'$instance'/.*}
    expr=${expr//'$team'/$team}
    expr=${expr//'$severity'/.*}
    expr=${expr//'$critical'/critical}
    body=$(jq -n --arg expr "$expr" --arg ref "$ref" --argjson instant "$instant" \
      '{from: "now-15m", to: "now", queries: [{refId: $ref, datasource: {type: "prometheus", uid: "prom"}, expr: $expr,
        instant: $instant, range: ($instant | not), intervalMs: 15000, maxDataPoints: 200}]}')
    out=$(curl "${g[@]}" -X POST -d "$body" "$gu/api/ds/query") || out='{}'
    n=$(jq --arg ref "$ref" '[.results[$ref].frames[]?.data.values[1]? // [] | length] | add // 0' <<<"$out")
    asked=$((asked + 1))
    if [ "$n" -gt 0 ]; then
      printf 'ok   %-6s %-45s %s %d\n' "${team/.\*/all}" "$title" "$ref" "$n"
    else
      printf 'NONE %-6s %-45s %s %s\n' "${team/.\*/all}" "$title" "$ref" "$(jq -c --arg ref "$ref" '.results[$ref].error // .message // ""' <<<"$out")"
      failed=$((failed + 1))
    fi
  done < <(jq -r '.panels[] | select(.type != "row") | .title as $t | .targets[] | [$t, .refId, (.instant | tostring), .expr] | @tsv' \
    charts/lapilli/dashboards/lapilli.json)
done
echo "$asked queries asked, $failed returned nothing"
[ "$failed" -eq 0 ]
