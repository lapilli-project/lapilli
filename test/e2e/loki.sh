#!/usr/bin/env bash
# Rules written in LogQL, replayed against a real Loki and set beside what
# really fired (docs/design/loki.md): a real Loki's ruler, writing ALERTS to
# a real Prometheus as its remote_write; and a real Grafana with rules on
# that Loki, writing its state history to the same Prometheus. Log lines go
# in every second, each stamped at .95 of the second after the one it is
# sent in, so none is late and none falls in the fraction of a second a
# Loki group runs past its second. After some minutes, `lapilli verdict
# --check-replay` reads Loki's rules from its API and Grafana's from its
# own, asks Loki directly and through Grafana's data source proxy, and
# every firing must be alike. A classic condition over Loki and a
# condition reading a MySQL data source are listed, not replayed.
#
# Needs Docker, Go, curl and jq. Takes 15 to 21 minutes (MINUTES=6 for
# fewer). Leaves everything up when KEEP is set.
# shellcheck disable=SC2016,SC2329 # LogQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
grafana=grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf
loki=grafana/loki:3.7.9@sha256:0db216b630ae1f348a1584c67f3cead8b4c1fc839169a39501db664744158e28
p=lapilli-e2e-loki
minutes=${MINUTES:-10}

work=$(mktemp -d)
pusher=
cleanup() {
  [ -n "$pusher" ] && kill "$pusher" 2>/dev/null || true
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  docker rm -fv "$p-loki" "$p-prom" "$p-grafana" >/dev/null 2>&1 || true
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

go build -o "$work/lapilli" ./cmd/lapilli
mkdir -p "$work/rules/fake" "$work/prov/datasources" "$work/prov/alerting"

cat >"$work/loki.yaml" <<YAML
auth_enabled: false
server: {http_listen_port: 3100, log_level: warn}
common:
  path_prefix: /loki
  storage: {filesystem: {chunks_directory: /loki/chunks, rules_directory: /loki/rules-store}}
  replication_factor: 1
  ring: {kvstore: {store: inmemory}}
schema_config:
  configs:
    - {from: 2020-10-24, store: tsdb, object_store: filesystem, schema: v13, index: {prefix: index_, period: 24h}}
ruler:
  storage: {type: local, local: {directory: /etc/loki/rules}}
  rule_path: /loki/rules-tmp
  enable_api: true
  remote_write: {enabled: true, clients: {prom: {url: "http://$p-prom:9090/api/v1/write"}}}
  wal: {dir: /loki/ruler-wal}
limits_config: {reject_old_samples: false}
analytics: {reporting_enabled: false}
YAML
cat >"$work/rules/fake/shop.yaml" <<'YAML'
groups:
  - name: shop
    interval: 10s
    rules:
      - alert: ShopErrors
        expr: sum(count_over_time({app="shop", level="error"}[30s])) > 20
      - alert: ShopErrorRate
        expr: sum by (app) (rate({app="shop"} |= "error" [1m])) > 0.5
        for: 30s
  - name: api
    interval: 1m
    rules:
      - alert: ApiSlow
        expr: max by (app) (max_over_time({app="api"} | logfmt | unwrap latency_ms [1m])) > 500
      - alert: CartErrors
        expr: sum by (app) (count_over_time({app="cart"} |= "error" [1m])) > 30
YAML
cat >"$work/prov/datasources/ds.yaml" <<YAML
apiVersion: 1
datasources:
  - {name: Prometheus, type: prometheus, uid: prom, url: "http://$p-prom:9090", access: proxy, jsonData: {prometheusType: Prometheus}}
  - {name: Loki, type: loki, uid: loki, url: "http://$p-loki:3100", access: proxy}
  - {name: Orders, type: mysql, uid: orders, url: "mysql:3306", access: proxy}
YAML
# Grafana's rules, as its editor writes them: instant and range queries,
# reduces Grafana works out, no data states, a classic condition and a
# MySQL query, which aren't replayed.
query() { # refId, LogQL, queryType, window in seconds
  jq -nc --arg r "$1" --arg e "$2" --arg q "$3" --argjson f "$4" '{refId: $r, queryType: $q, relativeTimeRange: {from: $f, to: 0},
    datasourceUid: "loki", model: {expr: $e, queryType: $q, refId: $r, intervalMs: 1000, maxDataPoints: 43200}}'
}
step() { jq -nc --arg r "$1" --argjson m "$2" '{refId: $r, datasourceUid: "__expr__", model: ($m + {refId: $r})}'; }
threshold() { step "$1" "{\"type\":\"threshold\",\"expression\":\"$2\",\"conditions\":[{\"evaluator\":{\"type\":\"gt\",\"params\":[$3]}}]}"; }
reduce() { step "$1" "{\"type\":\"reduce\",\"expression\":\"$2\",\"reducer\":\"$3\"}"; }
rule() { # uid, condition, noDataState, for, data...
  local uid=$1 cond=$2 nodata=$3 for=$4
  shift 4
  jq -nc --arg u "$uid" --arg c "$cond" --arg n "$nodata" --arg f "$for" --slurpfile d <(printf '%s\n' "$@") \
    '{uid: $u, title: $u, condition: $c, noDataState: $n, execErrState: "Error", for: $f, labels: {team: "logs"}, data: $d}'
}
errors='sum(count_over_time({app="shop", level="error"}[30s]))'
{
  rule LkInstant B OK 0s "$(query A "$errors" instant 600)" "$(threshold B A 20)"
  rule LkNoData B NoData 0s "$(query A 'sum by (app) (count_over_time({level="error"}[1m]))' instant 600)" "$(threshold B A 30)"
  rule LkFor B OK 20s "$(query A "$errors" instant 600)" "$(threshold B A 40)"
  rule LkRangeMax C OK 0s "$(query A "$errors" range 120)" "$(reduce B A max)" "$(threshold C B 50)"
  rule LkRangeMean C OK 0s "$(query A 'max by (app) (max_over_time({app="api"} | logfmt | unwrap latency_ms [10s]))' range 300)" \
    "$(reduce B A mean)" "$(threshold C B 250)"
  rule LkMath C OK 0s "$(query A 'sum(rate({app="shop"} |= "error" [1m]))' instant 600)" "$(query B 'sum(rate({app="shop"}[1m]))' instant 600)" \
    "$(step C '{"type":"math","expression":"$A / $B > 0.3"}')"
  rule LkClassic B OK 0s "$(query A "$errors" range 60)" \
    "$(step B '{"type":"classic_conditions","conditions":[{"evaluator":{"type":"gt","params":[10]},"operator":{"type":"and"},"query":{"params":["A"]},"reducer":{"type":"avg"}}]}')"
  rule LkSQL B OK 0s '{"refId":"A","relativeTimeRange":{"from":600,"to":0},"datasourceUid":"orders","model":{"refId":"A","rawSql":"SELECT 1"}}' \
    "$(threshold B A 5)"
} | jq -s '{apiVersion: 1, groups: [{orgId: 1, name: "logs", folder: "logs", interval: "10s", rules: .}]}' >"$work/prov/alerting/rules.json"

docker network create "$p" >/dev/null
printf 'global: {scrape_interval: 15s}\n' >"$work/prometheus.yml"
docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 -v "$work/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
  "$prom" --config.file=/etc/prometheus/prometheus.yml --web.enable-remote-write-receiver >/dev/null
docker run -d --name "$p-loki" --network "$p" -p 127.0.0.1::3100 -v "$work/loki.yaml:/etc/loki/loki.yaml:ro" \
  -v "$work/rules:/etc/loki/rules:ro" "$loki" -config.file=/etc/loki/loki.yaml >/dev/null
docker run -d --name "$p-grafana" --network "$p" -p 127.0.0.1::3000 -e GF_SECURITY_ADMIN_PASSWORD=e2e-admin-pass \
  -e GF_UNIFIED_ALERTING_STATE_HISTORY_ENABLED=true -e GF_UNIFIED_ALERTING_STATE_HISTORY_BACKEND=prometheus \
  -e GF_UNIFIED_ALERTING_STATE_HISTORY_PROMETHEUS_TARGET_DATASOURCE_UID=prom \
  -v "$work/prov:/etc/grafana/provisioning:ro" "$grafana" >/dev/null
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
lu=http://$(docker port "$p-loki" 3100/tcp | head -1)
gf=http://$(docker port "$p-grafana" 3000/tcp | head -1)
until_true 120 curl -sf "$pu/-/ready"
until_true 180 sh -c "curl -sf '$lu/ready' | grep -q ready"
until_true 120 curl -sf "$gf/api/health"

# A Viewer's token reads Grafana's rules and asks Loki through its proxy.
ga=(-u admin:e2e-admin-pass -H 'Content-Type: application/json')
id=$(curl -sf "${ga[@]}" -X POST "$gf/api/serviceaccounts" -d '{"name":"lapilli","role":"Viewer"}' | jq -r .id)
curl -sf "${ga[@]}" -X POST "$gf/api/serviceaccounts/$id/tokens" -d '{"name":"viewer"}' | jq -j .key >"$work/viewer"

# The logs: the shop's errors, two a second for 40s of every three minutes;
# the cart's, one a second for the first minute of every five; the api's
# latency, 900 for 20s of every two minutes.
push() {
  while :; do
    local s
    s=$(($(date +%s) + 1))
    local ns="${s}950000000"
    local streams
    streams=$(jq -nc --arg ns "$ns" --argjson s "$s" '[
      {stream: {app: "shop", level: "info"}, values: [[$ns, "msg=ok"]]},
      {stream: {app: "api", level: "info"}, values: [[$ns, "msg=done latency_ms=\(if $s % 120 < 20 then 900 else 100 + $s % 7 end)"]]}]
      + (if $s % 180 < 40 then [{stream: {app: "shop", level: "error"}, values: [[$ns, "msg=error n=1"], [$ns, "msg=error n=2"]]}] else [] end)
      + (if $s % 300 < 60 then [{stream: {app: "cart", level: "error"}, values: [[$ns, "msg=error"]]}] else [] end)')
    curl -s -o /dev/null -H 'Content-Type: application/json' -X POST "$lu/loki/api/v1/push" -d "{\"streams\":$streams}" || true
    sleep 1
  done
}
push &
pusher=$!

# Loki's ruler and Grafana start on their own; the check begins two
# minutes in, once both have run every group, on a minute when no Loki
# rule's firing is about to end: one on when a period starts, that ends
# before its group's first evaluation in it, was decided before the period,
# and the replay doesn't see it (docs/design/loki.md). The shop's errors
# are quiet two minutes into their three, and the cart's firing ends a
# minute and a half into its five; the check ends on a whole minute.
from=$((($(date +%s) + 120) / 60 * 60 + 60))
while [ $((from % 180)) -ne 120 ] || [ $((from % 300)) -eq 120 ]; do from=$((from + 60)); done
until [ "$(date +%s)" -ge $((from + minutes * 60 + 10)) ]; do sleep 5; done
to=$(($(date +%s) / 60 * 60 - 60))
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
from=$(iso "$from")
to=$(iso "$to")
echo "checking $from to $to"

fail=0
check() { # what, JSON of --check-replay --json
  local replayed real alike
  replayed=$(jq .replayed <<<"$2")
  real=$(jq .real <<<"$2")
  alike=$(jq .alike <<<"$2")
  if [ "$replayed" -gt 0 ] && [ "$replayed" = "$real" ] && [ "$alike" = "$real" ]; then
    echo "ok: $1: $alike of $real firings alike"
  else
    echo "FAILED: $1: $replayed replayed, $real real, $alike alike"
    jq -c '.rules[] | select(.alike != .replayed or .alike != .real)' <<<"$2"
    fail=1
  fi
}
out=$("$work/lapilli" verdict --check-replay --json --loki-loaded-rules --loki "$lu" --prometheus "$pu" --from "$from" --to "$to" --step 10s)
check "Loki's ruler" "$out"
out=$("$work/lapilli" verdict --check-replay --json --grafana "$gf" --grafana-token-file "$work/viewer" --prometheus "$pu" \
  --from "$from" --to "$to" --step 10s 2>/dev/null)
check "Grafana on Loki" "$out"
listed=$("$work/lapilli" verdict --check-replay --grafana "$gf" --grafana-token-file "$work/viewer" --prometheus "$pu" \
  --from "$from" --to "$to" --step 10s 2>/dev/null)
for want in "LkClassic: classic conditions over a Loki query" "LkSQL: its condition reads a mysql data source"; do
  if grep -qF "$want" <<<"$listed"; then echo "ok: listed: $want"; else echo "FAILED: not listed: $want"; fail=1; fi
done
exit $fail
