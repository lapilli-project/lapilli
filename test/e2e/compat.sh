#!/usr/bin/env bash
# The Prometheus-compatible APIs `--prometheus` takes besides Prometheus,
# real ones rather than stand-ins: a Thanos reading a Prometheus through a
# sidecar, and a VictoriaMetrics that Prometheus writes to, beside a
# vmalert evaluating the same rules there and writing what they fired back
# (docs/verdict.md, docs/check-replay.md). Prometheus's recording rules make
# the series from the time alone; three alert rules fire and resolve on
# them every four minutes: one a series each, one summed, one with a for.
# After some minutes, over the same period:
#   - `lapilli verdict --json` against Thanos and against VictoriaMetrics
#     gives the same firings, each its rule, labels, start and end, as
#     against Prometheus;
#   - `--check-replay` finds every firing alike against each of the three:
#     ALERTS is Prometheus's (through the sidecar, Thanos's too) and, in
#     VictoriaMetrics, vmalert's;
#   - `--loaded-rules` reads vmalert's rules API, through VictoriaMetrics'
#     proxy to it, and checks them the same way.
#
# Prometheus 3.5.0; Thanos 0.42.5, a sidecar and `thanos query`;
# VictoriaMetrics 1.156.0, single-node; vmalert 1.156.0.
#
# Needs Docker, Go, curl and jq. Takes 13 to 19 minutes (MINUTES=4 for
# fewer: a multiple of four). Leaves everything up when KEEP is set.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup and has run through trap and until_true
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
thanos=quay.io/thanos/thanos:v0.42.5@sha256:0a546b98a8dd47735456ab389983675ee363d68127e334937a9fceeea10da5e7
vm=victoriametrics/victoria-metrics:v1.156.0@sha256:da8a90ac0912f16589be2b96a6441a2fca4bca415c83783d972d92e3b5b66970
vmalert=victoriametrics/vmalert:v1.156.0@sha256:4fea7d1a888d4db4ff18804434ef0f3ca8eb6482e289c50d32001c2db218287c
p=lapilli-e2e-compat-$$
minutes=${MINUTES:-8}
if [ $((minutes % 4)) != 0 ] || [ "$minutes" -lt 4 ]; then
  echo "MINUTES=$minutes: want a multiple of four, the rules' cycle" >&2
  exit 2
fi

work=$(mktemp -d)
containers=(vm vmalert prom sidecar query)
logs() {
  docker ps -a --filter "name=$p-" --format '{{.Names}}: {{.Status}}' 2>&1 || true
  for c in "${containers[@]}"; do
    echo "--- $p-$c, its last lines"
    docker logs --tail 40 "$p-$c" 2>&1 || true
  done
}
cleanup() {
  local code=$?
  [ "$code" = 0 ] || logs
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  for c in "${containers[@]}"; do docker rm -fv "$p-$c" >/dev/null 2>&1 || true; done
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
mkdir -p "$work/rules" "$work/prom"

# The series, from the time alone, as Prometheus's recording rules write
# them every 10s. In each four minutes (t % 240): the shop's errors are high
# (5, else 1) in its first minute, the cart's from 2:00 to 2:30, and the
# cart's latency (0.9s, else 0.1) from 1:00 to 2:10. From 2:30 to 4:00
# nothing is: the period checked starts and ends at 3:00, every firing
# over. Steps from 1 to 5 and back, never absent, so that no store's
# lookback or staleness can tell them apart.
cat >"$work/rules/series.yaml" <<'YAML'
groups:
  - name: series
    interval: 10s
    rules:
      - record: e2e_errors
        expr: >-
          label_replace(1 + 4 * (vector(time()) % 240 < bool 60), "service", "shop", "", "")
          or label_replace(1 + 4 * (vector(time()) % 240 >= bool 120) * (vector(time()) % 240 < bool 150), "service", "cart", "", "")
      - record: e2e_latency_seconds
        expr: label_replace(0.1 + 0.8 * (vector(time()) % 240 >= bool 60) * (vector(time()) % 240 < bool 130), "service", "cart", "", "")
YAML
# The alert rules Prometheus and vmalert both evaluate, and lapilli
# replays. Each four minutes: ErrorsHigh fires twice (shop, cart),
# ErrorsAny twice, LatencySlow once, from 1:30 to 2:10.
cat >"$work/rules/alerts.yaml" <<'YAML'
groups:
  - name: shop
    interval: 10s
    rules:
      - alert: ErrorsHigh
        expr: e2e_errors > 3
        labels:
          severity: page
        annotations:
          summary: "Errors high on {{ $labels.service }}"
      - alert: ErrorsAny
        expr: sum(e2e_errors) > 5
        labels:
          severity: ticket
      - alert: LatencySlow
        expr: e2e_latency_seconds > 0.5
        for: 30s
        labels:
          severity: page
YAML
want=$((minutes * 5 / 4)) # five a cycle

# Prometheus writes what it records to VictoriaMetrics, but not ALERTS:
# there, what fired is vmalert's alone. Thanos's sidecar refuses a
# Prometheus without external labels that tell it apart
# (https://thanos.io/tip/thanos/storage.md#external-labels); thanos query
# takes this one as its replica label and leaves it out of its answers
# (https://thanos.io/tip/components/query.md, --query.replica-label and
# dedup, on unless asked otherwise). Remote write adds it to what it
# sends before write_relabel_configs, which drops it.
cat >"$work/prom/prometheus.yml" <<YAML
global:
  evaluation_interval: 10s
  external_labels: {replica: a}
rule_files: [/etc/prometheus/rules/*.yaml]
remote_write:
  - url: http://$p-vm:8428/api/v1/write
    write_relabel_configs:
      - {source_labels: [__name__], regex: "ALERTS|ALERTS_FOR_STATE", action: drop}
      - {regex: replica, action: labeldrop}
YAML

docker network create "$p" >/dev/null
# VictoriaMetrics answers /api/v1/rules from vmalert, as Grafana reads them
# through it, with -vmalert.proxyURL: lapilli asks --prometheus for both
# the rules (--loaded-rules) and the history. vmalert asks it for what the
# rules say (-datasource.url) and writes ALERTS to it (-remoteWrite.url,
# to which it adds /api/v1/write); with no Alertmanager, its notifications
# go nowhere (-notifier.blackhole). It evaluates 30s behind
# (-rule.evalDelay, as VictoriaMetrics shows only what is 30s old to a query
# of now, -search.latencyOffset), each time on a multiple of the group's
# interval (eval_alignment), and stamps ALERTS with that time
# (https://docs.victoriametrics.com/victoriametrics/vmalert/; the stamp in
# app/vmalert/rule/group.go and alerting.go, v1.156.0). Both are left as
# they come.
docker run -d --name "$p-vm" --network "$p" -p 127.0.0.1::8428 "$vm" \
  -retentionPeriod=1d -vmalert.proxyURL="http://$p-vmalert:8880" >/dev/null
docker run -d --name "$p-vmalert" --network "$p" -p 127.0.0.1::8880 \
  -v "$work/rules/alerts.yaml:/etc/vmalert/alerts.yaml:ro" "$vmalert" \
  -rule=/etc/vmalert/alerts.yaml -datasource.url="http://$p-vm:8428" \
  -remoteWrite.url="http://$p-vm:8428" -notifier.blackhole >/dev/null
vu=http://$(docker port "$p-vm" 8428/tcp | head -1)
au=http://$(docker port "$p-vmalert" 8880/tcp | head -1)
until_true 120 curl -sf "$vu/health"
until_true 120 curl -sf "$au/health"

docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 \
  -v "$work/prom/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -v "$work/rules:/etc/prometheus/rules:ro" \
  "$prom" --config.file=/etc/prometheus/prometheus.yml >/dev/null
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
until_true 120 curl -sf "$pu/-/ready"

# Thanos in its smallest real setup: a sidecar serving the Prometheus's
# TSDB as a Store API (read through Prometheus's remote read; with no
# bucket it uploads nothing), and a query reading that.
docker run -d --name "$p-sidecar" --network "$p" "$thanos" sidecar \
  --prometheus.url="http://$p-prom:9090" --grpc-address=0.0.0.0:10901 --http-address=0.0.0.0:10902 >/dev/null
docker run -d --name "$p-query" --network "$p" -p 127.0.0.1::10902 "$thanos" query \
  --grpc-address=0.0.0.0:10901 --http-address=0.0.0.0:10902 \
  --endpoint.sd-config="endpoints: [{address: '$p-sidecar:10901'}]" --query.replica-label=replica >/dev/null
tu=http://$(docker port "$p-query" 10902/tcp | head -1)
until_true 120 curl -sf "$tu/-/ready"

# Each has both series: Prometheus by its rules, Thanos through the
# sidecar, VictoriaMetrics by remote write, 30s on; and vmalert has
# loaded the rules.
has() { curl -sf "$1/api/v1/query" --data-urlencode 'query=count(e2e_errors)' | jq -e '.data.result[0].value[1] == "2"'; }
until_true 120 has "$pu"
until_true 120 has "$tu"
until_true 180 has "$vu"
until_true 60 sh -c "curl -sf '$au/api/v1/rules' | jq -e '[.data.groups[].rules[] | select(.type == \"alerting\")] | length == 3'"

# The check begins two minutes on, once every group has run, at 3:00 of a
# four minutes, and ends MINUTES later, at 3:00 again: no firing on at
# either end. It runs once vmalert, 30s behind, has evaluated the end, and
# VictoriaMetrics shows it.
from_s=$((($(date +%s) + 120) / 60 * 60 + 60))
while [ $((from_s % 240)) -ne 180 ]; do from_s=$((from_s + 60)); done
to_s=$((from_s + minutes * 60))
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
from=$(iso "$from_s")
to=$(iso "$to_s")
echo "checking $from to $to: $want firings each, once it is $(iso $((to_s + 90)))"
until [ "$(date +%s)" -ge $((to_s + 90)) ]; do sleep 5; done
printf 'incidents:\n  - id: INC-1\n    title: Shop errors\n    start: %s\n    end: %s\n' \
  "$(iso $((from_s + 60)))" "$(iso $((from_s + 120)))" >"$work/incidents.yaml"

fail=0
ok() { echo "ok: $1"; }
failed() { echo "FAILED: $1"; fail=1; }

# lapilli verdict --json against each: its firings, each its rule, labels,
# start and end, in one order, to firings-NAME.json.
verdict() { # name, URL
  local code=0
  "$work/lapilli" verdict --json --rules "$work/rules/alerts.yaml" --prometheus "$2" --incidents "$work/incidents.yaml" \
    --from "$from" --to "$to" --step 10s >"$work/verdict-$1.json" 2>"$work/verdict-$1.err" || code=$?
  if [ "$code" != 0 ]; then
    failed "verdict against $1 exited $code"
    tail -20 "$work/verdict-$1.err"
    echo '[]' >"$work/firings-$1.json"
    return
  fi
  if jq -e '(.unreplayed // []) | length > 0' "$work/verdict-$1.json" >/dev/null; then
    failed "verdict against $1: rules not replayed"
    jq -c '.unreplayed[]' "$work/verdict-$1.json"
  fi
  jq -S '[.firings // [] | .[] | {rule, labels, start, end}] | sort_by(.rule, .start, (.labels | tostring))' \
    "$work/verdict-$1.json" >"$work/firings-$1.json"
}
verdict prometheus "$pu"
verdict thanos "$tu"
verdict victoriametrics "$vu"
n=$(jq length "$work/firings-prometheus.json")
if [ "$n" = "$want" ]; then
  ok "verdict against Prometheus: $n firings"
else
  failed "verdict against Prometheus: $n firings, want $want"
  jq -c '.[]' "$work/firings-prometheus.json"
fi
for s in thanos victoriametrics; do
  if diff -u "$work/firings-prometheus.json" "$work/firings-$s.json" >"$work/diff-$s"; then
    ok "verdict against $s: the same $n firings as against Prometheus"
  else
    failed "verdict against $s: firings differ from Prometheus's (- Prometheus, + $s)"
    cat "$work/diff-$s"
  fi
done

# --check-replay against each: every firing alike, as loki.sh asks.
alike() { # name, what, flags...
  local name=$1 what=$2 code=0 replayed real same
  shift 2
  "$work/lapilli" verdict --check-replay --json --from "$from" --to "$to" --step 10s "$@" \
    >"$work/check-$name.json" 2>"$work/check-$name.err" || code=$?
  replayed=$(jq -r '.replayed // 0' "$work/check-$name.json" 2>/dev/null || echo 0)
  real=$(jq -r '.real // 0' "$work/check-$name.json" 2>/dev/null || echo 0)
  same=$(jq -r '.alike // 0' "$work/check-$name.json" 2>/dev/null || echo 0)
  if [ "$code" = 0 ] && [ "$replayed" -gt 0 ] && [ "$replayed" = "$real" ] && [ "$same" = "$real" ]; then
    ok "$what: $same of $real firings alike"
  else
    failed "$what: exit $code, $replayed replayed, $real real, $same alike"
    jq -c '.rules[]? | select(.alike != .replayed or .alike != .real)' "$work/check-$name.json" 2>/dev/null || true
    jq -c '{series, silent, elsewhere, unfinished}' "$work/check-$name.json" 2>/dev/null || true
    tail -20 "$work/check-$name.err"
  fi
}
alike prometheus "--check-replay against Prometheus" --rules "$work/rules/alerts.yaml" --prometheus "$pu"
alike thanos "--check-replay against Thanos, Prometheus's ALERTS through the sidecar" --rules "$work/rules/alerts.yaml" --prometheus "$tu"
alike victoriametrics "--check-replay against VictoriaMetrics, vmalert's ALERTS" --rules "$work/rules/alerts.yaml" --prometheus "$vu"
alike loaded "--check-replay of vmalert's rules, --loaded-rules through VictoriaMetrics" --loaded-rules --prometheus "$vu"
got=$(jq -r '[.rules[]?.rule] | sort | join(",")' "$work/check-loaded.json" 2>/dev/null || true)
if [ "$got" = "ErrorsAny,ErrorsHigh,LatencySlow" ]; then
  ok "--loaded-rules read vmalert's three rules"
else
  failed "--loaded-rules read $got, want ErrorsAny,ErrorsHigh,LatencySlow"
fi

# vmalert's own rules API, and VictoriaMetrics' proxy to it, which
# --loaded-rules read: the three rules, LatencySlow's for in seconds.
rulesOf() { curl -sf "$1/api/v1/rules" | jq -c '[.data.groups[].rules[] | select(.type == "alerting") | {name, duration}] | sort_by(.name)'; }
want_rules='[{"name":"ErrorsAny","duration":0},{"name":"ErrorsHigh","duration":0},{"name":"LatencySlow","duration":30}]'
for u in "$au" "$vu"; do
  got=$(rulesOf "$u" || true)
  if [ "$got" = "$want_rules" ]; then ok "$u/api/v1/rules: $got"; else failed "$u/api/v1/rules: $got, want $want_rules"; fi
done

if [ "$fail" != 0 ]; then
  echo "--- what each holds over the period: samples of e2e_errors, and of ALERTS firing"
  names=(prometheus thanos victoriametrics)
  urls=("$pu" "$tu" "$vu")
  for i in 0 1 2; do
    for q in "count_over_time(e2e_errors[${minutes}m])" "count_over_time(ALERTS{alertstate=\"firing\"}[${minutes}m])"; do
      echo "${names[i]}: $q: $(curl -s "${urls[i]}/api/v1/query" --data-urlencode "query=$q" --data-urlencode "time=$to" |
        jq -c '[.data.result[]? | {m: .metric, v: .value[1]}]' 2>/dev/null || true)"
    done
  done
fi
exit "$fail"
