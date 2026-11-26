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
# Then Loki's ruler is changed through its configuration API, as
# test/e2e/changes.sh changes Mimir's (docs/design/rulers.md, "Loki's
# ruler"): a second Loki, with tenants and its rules on the filesystem
# through its object store client, which takes writes as its local store
# doesn't; lapilli serve reading it with --loki-loaded-rules and its
# tenant, --changes-in loki:shop. A longer for is applied (Loki's ruler
# loads it) and undone byte for byte; a rule changed in Loki since it was
# proposed is refused as stale; a namespace --changes-in doesn't name is
# left for a person; a new rule is added to a group and undone.
#
# It says how many evaluations Loki and Grafana missed in the period (a busy
# host makes a server run one late, which no replay does), and a failed
# comparison says so when some were missed (test/e2e/missed.sh).
#
# Needs Docker, Go, curl and jq. Takes 17 to 24 minutes (MINUTES=6 for
# fewer). Leaves everything up when KEEP is set.
# PROMETHEUS_IMAGE, GRAFANA_IMAGE and LOKI_IMAGE run other images than the
# pinned ones.
# shellcheck disable=SC2016,SC2329 # LogQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/e2e/missed.sh
. test/e2e/missed.sh

prom=${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996}
grafana=${GRAFANA_IMAGE:-grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf}
loki=${LOKI_IMAGE:-grafana/loki:3.7.9@sha256:0db216b630ae1f348a1584c67f3cead8b4c1fc839169a39501db664744158e28}
# Which images this run was given (an override is pulled, and its digest said).
for o in PROMETHEUS_IMAGE GRAFANA_IMAGE LOKI_IMAGE; do
  [ -n "${!o:-}" ] || continue
  docker pull -q "${!o}" >/dev/null 2>&1 || true
  echo "$o=${!o} ($(docker image inspect --format '{{index .RepoDigests 0}}' "${!o}" 2>/dev/null || true))"
done
p=${PREFIX:-lapilli-e2e-loki} # containers and network: another PREFIX runs beside
minutes=${MINUTES:-10}
port=${PORT:-28190} # serve takes port

work=$(mktemp -d)
pusher=
served=
cleanup() {
  [ -n "$pusher" ] && kill "$pusher" 2>/dev/null || true
  [ -n "$served" ] && kill "$served" 2>/dev/null || true
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  docker rm -fv "$p-loki" "$p-lokiw" "$p-prom" "$p-grafana" >/dev/null 2>&1 || true
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
# reduces Grafana works out, no data states, a classic condition, which
# the replay reduces itself, and a MySQL query, which isn't replayed.
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
promq() { # refId: vector(1) on Prometheus, instant
  jq -nc --arg r "$1" '{refId: $r, queryType: "instant", relativeTimeRange: {from: 600, to: 0}, datasourceUid: "prom",
    model: {expr: "vector(1)", instant: true, queryType: "instant", refId: $r, intervalMs: 1000, maxDataPoints: 43200}}'
}
cond() { # evaluator, params, operator, query, reducer: one classic condition
  jq -nc --arg e "$1" --argjson p "$2" --arg o "$3" --arg q "$4" --arg r "$5" \
    '{evaluator: {type: $e, params: $p}, operator: {type: $o}, query: {params: [$q]}, reducer: {type: $r}}'
}
classic() { # refId, conditions as JSON objects joined by commas
  step "$1" "{\"type\":\"classic_conditions\",\"conditions\":[$2]}"
}
errors='sum(count_over_time({app="shop", level="error"}[30s]))'
cart='sum(count_over_time({app="cart", level="error"}[30s]))'
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
  # Each classic reducer over Loki, a range, no_value, conditions joined by
  # or and logic-or, one that is no data (the replay reduces them itself),
  # and a condition on Prometheus with a Loki query beside it.
  for r in "max gt 20" "min lt 10" "sum gt 400" "count gt 55" "last gt 30" "median gt 15" "diff gt 10" "diff_abs gt 25" \
    "percent_diff gt 50" "percent_diff_abs gt 80" "count_non_null lt 30"; do
    set -- $r
    # Titled in camel case: percent_diff_abs is LkClassicPercentDiffAbs.
    title=LkClassic$(awk -F_ '{for (i = 1; i <= NF; i++) printf "%s%s", toupper(substr($i, 1, 1)), substr($i, 2)}' <<<"$1")
    rule "$title" B OK 0s "$(query A "$errors" range 60)" "$(classic B "$(cond "$2" "[$3]" and A "$1")")"
  done
  rule LkClassicRange B OK 0s "$(query A "$errors" range 60)" "$(classic B "$(cond within_range "[5,30]" and A avg)")"
  # The shop's info lines are every second: 0/0 is NaN, which reduces to none.
  rule LkClassicNoValue B OK 0s "$(query A 'sum(count_over_time({app="shop", level="info"}[30s]) * 0) / sum(count_over_time({app="shop", level="info"}[30s]) * 0)' range 60)" \
    "$(classic B "$(cond no_value "[]" and A last)")"
  rule LkClassicOr C OK 0s "$(query A "$errors" range 60)" "$(query B "$cart" range 60)" \
    "$(classic C "$(cond gt "[30]" and A max),$(cond gt "[10]" or B max)")"
  rule LkClassicLogicOr C NoData 0s "$(query A "$errors" range 60)" "$(query B "$cart" range 60)" \
    "$(classic C "$(cond gt "[30]" and A max),$(cond gt "[10]" logic-or B max)")"
  rule LkClassicNoData B NoData 0s "$(query A 'sum by (app) (count_over_time({level="error"}[1m]))' range 120)" \
    "$(classic B "$(cond gt "[10]" and A avg)")"
  rule LkBeside B NoData 0s "$(promq A)" "$(threshold B A 0)" "$(query L "$errors" instant 600)"
  rule LkBesideRange B NoData 0s "$(promq A)" "$(threshold B A 0)" "$(query L "$errors" range 120)"
  rule LkBesideOK B OK 0s "$(promq A)" "$(threshold B A 0)" "$(query L "$errors" instant 600)"
  # A threshold on a range query: Grafana's evaluation errors ("only reduced
  # data can be alerted on"), which isn't replayed: listed.
  rule LkSeries B OK 0s "$(query A "$errors" range 60)" "$(threshold B A 10)"
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
# missed: what Loki's ruler and Grafana have missed since they started.
missed() { printf '%s %s' "$(missed_url "$lu" loki_prometheus_rule_group_iterations_missed_total)" \
  "$(missed_url "$gf" grafana_alerting_schedule_rule_evaluations_missed_total)"; }

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
# minutes after the ruler's groups have all run, so that the evaluation
# before the period, which the replay takes to have run (a placed group is
# replayed from it), was run by a ruler that had loaded them: one started
# within an interval of the period's start hasn't (measured: a restarted
# Loki ran its 1m group first two and a half minutes on). On any minute: a
# firing on when the period starts that ends before its group's first
# evaluation in it is replayed from the evaluation before, and must be
# alike (docs/design/states.md, section 5).
until_true 300 sh -c "curl -sf '$lu/prometheus/api/v1/rules' | jq -e '[.data.groups[].lastEvaluation] | length > 0 and all(. > \"2000\")'"
from=$((($(date +%s) + 120) / 60 * 60 + 60))
until [ "$(date +%s)" -ge "$from" ]; do sleep 5; done
missed_from=$(missed)
period_from=$(date +%s)
until [ "$(date +%s)" -ge $((from + minutes * 60 + 10)) ]; do sleep 5; done
missed_in=$(missed_delta "$missed_from" "$(missed)")
period_until=$(date +%s)
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
for want in "LkSQL: its condition reads a mysql data source" "LkSeries: its condition B comes to query A's time series"; do
  if grep -qF "$want" <<<"$listed"; then echo "ok: listed: $want"; else echo "FAILED: not listed: $want"; fail=1; fi
done
# Grafana's own state for the rule the replay lists: an error, at each
# evaluation, which --check-replay says it left out.
if [ "$(jq '.errors_left_out.LkSeries // 0' <<<"$out")" -gt 0 ]; then echo "ok: LkSeries is in Grafana's error state"; else echo "FAILED: LkSeries isn't in Grafana's error state"; fail=1; fi
# The classic conditions over Loki and the rules with a Loki query beside
# their condition are replayed, none listed as not.
if grep -E "^ *(LkClassic|LkBeside)[A-Za-z]*:" <<<"$listed"; then echo "FAILED: listed, not replayed"; fail=1; else echo "ok: classic and beside rules replayed"; fi
for name in LkClassicMax LkClassicMin LkClassicNoValue LkClassicPercentDiff LkClassicNoData LkClassicLogicOr LkBeside LkBesideRange LkBesideOK; do
  n=$(jq --arg n "$name" '[.rules[] | select(.rule == $n)] | length' <<<"$out")
  if [ "$n" = 1 ]; then echo "ok: $name checked"; else echo "FAILED: $name not in the check"; fail=1; fi
done

missed_say "Loki Grafana" "$missed_in"
if [ "$fail" != 0 ]; then
  missed_explain "Loki Grafana" "$missed_in"
  if missed_some "$missed_in"; then missed_ticks "$p-grafana" "$period_from" "$period_until"; fi
fi

# Loki's ruler, changed through its configuration API. Its local store,
# above, is read-only to the API; this Loki keeps its rules on the
# filesystem through its object store client, which takes writes, and has
# tenants: every request names acme, or Loki refuses it.
cat >"$work/lokiw.yaml" <<'YAML'
auth_enabled: true
server: {http_listen_port: 3100, log_level: warn}
common:
  path_prefix: /loki
  replication_factor: 1
  ring: {kvstore: {store: inmemory}}
storage_config:
  use_thanos_objstore: true
  object_store: {filesystem: {dir: /loki/objects}}
schema_config:
  configs:
    - {from: 2020-10-24, store: tsdb, object_store: filesystem, schema: v13, index: {prefix: index_, period: 24h}}
ruler_storage: {backend: filesystem, filesystem: {dir: /loki/rules}}
ruler:
  rule_path: /loki/rules-tmp
  enable_api: true
  poll_interval: 10s
  wal: {dir: /loki/ruler-wal}
limits_config: {reject_old_samples: false}
analytics: {reporting_enabled: false}
YAML
docker run -d --name "$p-lokiw" --network "$p" -p 127.0.0.1::3100 -v "$work/lokiw.yaml:/etc/loki/loki.yaml:ro" \
  "$loki" -config.file=/etc/loki/loki.yaml >/dev/null
lw=http://$(docker port "$p-lokiw" 3100/tcp | head -1)
until_true 180 sh -c "curl -sf '$lw/ready' | grep -q ready"
lrules=$lw/loki/api/v1/rules
tenant=(-H 'X-Scope-OrgID: acme')
lpost() { curl -sf "${tenant[@]}" -X POST -H 'Content-Type: application/yaml' --data-binary @- "$lrules/$1" >/dev/null; }
lput() { printf '%s' "$2" | lpost "$1"; } # namespace, group: retried as one
lns() { curl -s "${tenant[@]}" "$lrules/$1"; }
lloaded() { # namespace, alert: the rule as Loki's ruler has loaded it
  curl -s "${tenant[@]}" "$lw/prometheus/api/v1/rules" | jq -c --arg f "$1" --arg n "$2" '.data.groups[] | select(.file == $f) | .rules[] | select(.name == $n)'
}
shop_api='name: api
interval: 1m
rules:
  - alert: LogErrors
    expr: sum(count_over_time({app="shop", level="error"}[1m])) > 20
    for: 1m
    labels:
      severity: page
  - alert: LogSlow
    expr: max(max_over_time({app="shop"} | logfmt | unwrap latency_ms [1m])) > 500
    for: 2m
'
# The ruler takes writes once it is running.
until_true 120 lput shop "$shop_api"
lpost other <<'YAML'
name: other
rules:
  - alert: OtherErrors
    expr: sum(count_over_time({app="other", level="error"}[1m])) > 5
YAML
check_line() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
check_line "Loki refuses a request without its tenant" "$(curl -s -o /dev/null -w '%{http_code}' "$lrules")" '^401$'

head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/loki-write-token"
"$work/lapilli" serve --user-header X-Forwarded-Email --editors '*' --admins ana@example.com --approvers 'ana@example.com,cy@example.com' \
  --backfill 2h --settle 1m --watch 10s --prometheus "$pu" --loki "$lw" --loki-tenant acme --loki-loaded-rules \
  --ledger "$work/ledger" --listen "127.0.0.1:$port" \
  --changes --changes-in loki:shop --changes-loki-token-file "$work/loki-write-token" >"$work/serve.log" 2>&1 &
served=$!
sl=http://127.0.0.1:$port
until_true 60 curl -sf -H 'X-Forwarded-Email: bo@example.com' "$sl/api/v1/proposals"
until_true 60 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sl/api/v1/rules' | grep -q '\"LogErrors\"'"

# A person's browser, behind a proxy that names them, as in changes.sh.
propose() { # who, form fields...
  local who=$1 args=()
  shift
  for f in "$@"; do args+=(--data-urlencode "$f"); done
  curl -s -o /dev/null -w '%{redirect_url}' -H "X-Forwarded-Email: $who" -H 'Sec-Fetch-Site: same-origin' "${args[@]}" "$sl/proposals" |
    sed -nE 's|.*/proposals/([0-9]+)$|\1|p'
}
act() { # who, id, approve or undo: the form on the proposal's page
  local key
  key=$(curl -s -H "X-Forwarded-Email: $1" "$sl/proposals/$2" |
    sed -nE "s|.*action=\"/proposals/$2/$3\"><input type=\"hidden\" name=\"key\" value=\"([0-9a-f]+)\".*|\1|p" | head -1)
  [ -n "$key" ] || return 1
  curl -s -o /dev/null -H "X-Forwarded-Email: $1" -H 'Sec-Fetch-Site: same-origin' --data-urlencode "key=$key" --data-urlencode "read=1" "$sl/proposals/$2/$3"
}
proposal() { curl -s -H 'X-Forwarded-Email: ana@example.com' "$sl/api/v1/proposals/$1"; }
state() { proposal "$1" | jq -r '(.proposal // .) | .state'; }
reason() { proposal "$1" | jq -r '(.proposal // .) | .reason // ""'; }
is() { [ "$(state "$1")" = "$2" ]; }
newest() { curl -s -H 'X-Forwarded-Email: ana@example.com' "$sl/api/v1/proposals" | jq '[.proposals[].id] | max'; }

shop_before=$(lns shop)
id=$(propose bo@example.com rule=LogErrors kind=for for=5m)
check_line "a Loki ruler's proposal, on its own path" "$(proposal "$id" | jq -r '.path')" '^loki$'
act ana@example.com "$id" approve
until_true 90 is "$id" applied || true
check_line "a longer for, applied in Loki" "$(state "$id")" '^applied$'
check_line "the namespace differs by that one line" "$(diff <(echo "$shop_before") <(lns shop) | grep -E '^[<>]' | tr '\n' ' ')" '^< +for: 1m > +for: 5m $'
until_true 60 sh -c "curl -s -H 'X-Scope-OrgID: acme' '$lw/prometheus/api/v1/rules' | jq -e '.data.groups[] | select(.file == \"shop\") | .rules[] | select(.name == \"LogErrors\" and .duration == 300)'" || true
check_line "Loki's ruler loaded it" "$(lloaded shop LogErrors | jq .duration)" '^300$'
act ana@example.com "$id" undo
undo=$(newest)
until_true 90 is "$undo" applied || true
check_line "undone in Loki" "$(state "$id")" '^undone$'
check_line "the namespace as it was, byte for byte" "$(diff <(echo "$shop_before") <(lns shop) | wc -l | tr -d ' ')" '^0$'

# The rule itself changed in Loki between proposing and approving: refused
# as stale, nothing written.
until_true 30 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sl/api/v1/proposals?state=applying,approved' | jq -e '.proposals | length == 0'" || true
id=$(propose bo@example.com rule=LogErrors kind=for for=7m)
lns shop/api | sed 's/> 20/> 30/' | lpost shop
act ana@example.com "$id" approve
until_true 90 is "$id" refused || true
check_line "LogErrors changed since: refused as stale" "$(state "$id"): $(reason "$id")" "^refused: the rule changed in Loki's ruler since it was proposed"
check_line "LogErrors's for as Loki has it" "$(lns shop/api | grep -cE '^ +for: 7m')" '^0$'

id=$(propose bo@example.com rule=OtherErrors kind=for for=5m)
check_line "a namespace --changes-in doesn't name is for a person, with why" \
  "$(proposal "$id" | jq -r '.path + ": " + (.evidence.no_path // "")')" \
  "^none: its namespace in Loki's ruler isn't one --changes-in names, as loki:NAMESPACE$"

# A new rule, in LogQL, held to its expectations (a budget, which a rule
# that fires nowhere in the twelve weeks meets), written into Loki's group
# through its API, loaded by Loki's ruler, and undone by deleting it.
until_true 120 sh -c "curl -s -H 'X-Forwarded-Email: bo@example.com' '$sl/proposals/new' | grep -q ': shop, api</option>'"
place=$(curl -s -H 'X-Forwarded-Email: bo@example.com' "$sl/proposals/new" | sed -nE 's|.*<option value="([0-9a-f]+)">[^<]*: shop, api</option>.*|\1|p' | head -1)
added=$'alert: LogErrorsAdded\nexpr: sum(count_over_time({app="shop", level="error"}[1m])) > 5\nfor: 1m\n'
held=$'expectations:\n  - rule: LogErrorsAdded\n    budget:\n      week: 100000\n'
id=$(propose bo@example.com kind=add "place=$place" "rule=$added" "expectations=$held")
check_line "a rule added to Loki's ruler, on its own path" "$(proposal "$id" | jq -r '.path')" '^loki$'
act ana@example.com "$id" approve
until_true 90 is "$id" applied || true
check_line "a rule added, applied in Loki" "$(state "$id")" '^applied$'
check_line "the group holds it beside the others" "$(lns shop/api | grep -cE 'alert: (LogErrorsAdded|LogErrors|LogSlow)$')" '^3$'
until_true 60 sh -c "curl -s -H 'X-Scope-OrgID: acme' '$lw/prometheus/api/v1/rules' | jq -e '.data.groups[] | select(.file == \"shop\") | .rules[] | select(.name == \"LogErrorsAdded\")'" || true
check_line "Loki's ruler loaded it" "$(lloaded shop LogErrorsAdded | jq -r .name)" '^LogErrorsAdded$'
act ana@example.com "$id" undo
undo=$(newest)
until_true 90 is "$undo" applied || true
check_line "a rule added, undone in Loki" "$(state "$id")" '^undone$'
check_line "the group without it" "$(lns shop/api | grep -c LogErrorsAdded)" '^0$'

if [ "$fail" != 0 ]; then
  echo "--- serve (Loki's ruler)"; tail -30 "$work/serve.log"
fi
exit $fail
