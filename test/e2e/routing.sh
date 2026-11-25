#!/usr/bin/env bash
# Is the model of who a firing reached right? The notifications lapilli works
# out from an Alertmanager's configuration (docs/notifications-worked-out.md,
# docs/design/routing.md), held to the ones real Alertmanagers sent: a real
# Prometheus evaluating rules that fire and resolve at known times, sending
# to two real Alertmanagers at once, the newest release and the oldest one
# held to (the docs name no oldest for the model; 0.27.0, before which time
# intervals and `matchers` weren't all there is to read, is taken), each with
# short timings and the same configuration, and every receiver a webhook of
# one keeper (test/e2e/routing/keeper) that keeps each notification and the
# time it got it. Nothing outside Docker is reached.
#
# The configuration, whose timings are group_wait 5s, group_interval 15s,
# repeat_interval 1m:
#   - nested routes with `continue`: a db route that goes on to a later one,
#     and a queue route whose child pages and goes on to another child;
#   - group_by: the two db rules grouped into one notification by team, the
#     two disk rules by svc, the rest by alertname;
#   - an inhibition rule: a critical alert mutes a warning one that agrees
#     with it on `equal` svc and team, absent agreeing with absent (the
#     DiskCritical, and MiscCrit, which have no svc);
#   - a mute time interval on the batch route, a window of two minutes on the
#     minute, in the run's own time;
#   - a silence made through the API for one alert, for part of the run,
#     which starts in the future;
#   - a receiver of two webhooks (pager), and one that doesn't send resolves.
#
# The rules' times are given from T0 (the Unix time the run starts at),
# each one an alert over an expression of time() alone, so that its firing
# and resolving are known, and lapilli replays the same rules over
# Prometheus's history. Then
#   lapilli verdict --rules ... --prometheus ... --alertmanager-config ...
#     --silences (GET /api/v2/silences) --paging-receivers ... --json
# gives the report, and test/e2e/routing/compare holds its notifications to
# what each Alertmanager's webhooks got, integration by integration: the same
# number, each within one group_interval (15s) of its time, with the same
# status (firing, resolved) and the same alerts. The report's own numbers
# (notifications, pages, and the kinds of mute found) are held to them too.
# Prometheus and lapilli evaluate every 5s, each on its own clock, so a
# notification may be off by that and by the delivery; the tolerance is the
# rest.
#
# Needs Docker, Go, curl and jq. Takes about nine minutes. Leaves everything
# up when KEEP is set. PROMETHEUS_IMAGE, ALERTMANAGER_IMAGE (the newest)
# and ALERTMANAGER_OLDEST_IMAGE run other images than the pinned ones.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup and until_true run through trap
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996}
am_new=${ALERTMANAGER_IMAGE:-quay.io/prometheus/alertmanager:v0.35.1@sha256:03d4e362b9948eee910802ddac55f9175b320862a2de1fdef5206c3213875a09}
am_old=${ALERTMANAGER_OLDEST_IMAGE:-quay.io/prometheus/alertmanager:v0.27.0@sha256:e13b6ed5cb929eeaee733479dce55e10eb3bc2e9c4586c705a4e8da41e5eacf5}
busybox=busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
# Which images this run was given (an override is pulled, and its digest said).
for o in PROMETHEUS_IMAGE ALERTMANAGER_IMAGE ALERTMANAGER_OLDEST_IMAGE; do
  [ -n "${!o:-}" ] || continue
  docker pull -q "${!o}" >/dev/null 2>&1 || true
  echo "$o=${!o} ($(docker image inspect --format '{{index .RepoDigests 0}}' "${!o}" 2>/dev/null || true))"
done
# Pulled before the run's time is chosen, which a slow pull would pass.
for i in "$prom" "$am_new" "$am_old" "$busybox"; do docker pull -q "$i" >/dev/null; done
p=lapilli-e2e-routing-$$
tolerance=15s # one group_interval

work=$(mktemp -d)
containers=(keeper am-new am-old prom)
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
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
hhmm() { date -u -d "@$1" +%H:%M 2>/dev/null || date -u -r "$1" +%H:%M; }

go build -o "$work/lapilli" ./cmd/lapilli
go build -o "$work/compare" ./test/e2e/routing/compare
case $(docker info --format '{{.Architecture}}') in
  aarch64 | arm64) arch=arm64 ;;
  *) arch=amd64 ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$work/keeper" ./test/e2e/routing/keeper
mkdir -p "$work/prom" "$work/am-new" "$work/am-old"

# The run's time. T0 is on 5s, two minutes from now, with room for the
# containers to start and the first evaluations; the mute window starts on
# the first minute 90s after it and lasts two, and is moved on if it would
# cross midnight (UTC, which time intervals are in unless told another).
t0=$((($(date +%s) + 120) / 5 * 5))
while :; do
  mute_from=$((($t0 + 90 + 59) / 60 * 60))
  [ $(((mute_from % 86400) + 120)) -le 86400 ] && break
  t0=$((t0 + 60))
done
mute_to=$((mute_from + 120))
sil_from=$((t0 + 90))
sil_to=$((t0 + 170))
end=$((t0 + 420))
echo "T0 $(iso "$t0"): the mute window $(hhmm "$mute_from")-$(hhmm "$mute_to"), the silence from $(iso "$sil_from") to $(iso "$sil_to"), the period to $(iso "$end")"

# The rules: NAME, LABELS, from and to in seconds after T0, and a for.
# What each does, with the routes below (a time is T+seconds):
#   DiskCritical 20-200, DiskWarning 10-230   infra, grouped by svc; the
#       warning is inhibited while the critical fires
#   DbReplicaLag 30-150, DbConnections 70-130 db, grouped by team into one
#       notification, and on to the warnings route (continue)
#   QueueBacklog 40-330                       queue, then pager (two
#       webhooks) and on to gold, which sends no resolves; repeats each minute
#   QueueStale 100-190                        queue, gold only
#   NightJob 50-300                           batch, muted in the window
#   Silenced 30-260                           sil, silenced 90-170
#   MiscCrit 20-120, Misc 60-180              the root's receiver, and the
#       warnings route, where Misc is inhibited while MiscCrit fires (neither
#       has a svc: absent agrees with absent)
rule() { # name, labels, from, to, [for]
  printf '      - alert: %s\n        expr: vector(1) and on() (vector(time()) >= %d) and on() (vector(time()) < %d)\n' "$1" $((t0 + $3)) $((t0 + $4))
  [ -z "${5:-}" ] || printf '        for: %s\n' "$5"
  printf '        labels: {%s}\n' "$2"
}
{
  echo 'groups:'
  echo '  - name: routing'
  echo '    interval: 5s'
  echo '    rules:'
  rule DiskCritical 'severity: critical, team: infra, svc: disk' 20 200
  rule DiskWarning 'severity: warning, team: infra, svc: disk' 10 230
  rule DbReplicaLag 'severity: warning, team: db' 30 150
  rule DbConnections 'severity: warning, team: db' 70 130 15s
  rule QueueBacklog 'severity: page, team: queue, tier: gold' 40 330
  rule QueueStale 'severity: ticket, team: queue, tier: gold' 100 190
  rule NightJob 'severity: ticket, team: batch' 50 300
  rule Silenced 'severity: ticket, team: sil' 30 260
  rule MiscCrit 'severity: critical, team: other' 20 120
  rule Misc 'severity: warning, team: other' 60 180
} >"$work/rules.yaml"

# The configuration, once for each Alertmanager: they differ only in the
# webhooks' addresses, which say which one sent a notification.
config() { # instance
  local k=http://$p-keeper:8080/$1
  cat <<YAML
global:
  resolve_timeout: 5m
route:
  receiver: default
  group_by: [alertname]
  group_wait: 5s
  group_interval: 15s
  repeat_interval: 1m
  routes:
    - matchers: ['team="db"']
      receiver: db-team
      group_by: [team]
      continue: true
    - matchers: ['team="queue"']
      receiver: queue-team
      routes:
        - matchers: ['severity="page"']
          receiver: pager
          continue: true
        - matchers: ['tier="gold"']
          receiver: gold
    - matchers: ['team="batch"']
      receiver: batch
      mute_time_intervals: [window]
    - matchers: ['team="sil"']
      receiver: sil
    - matchers: ['team="infra"']
      receiver: infra
      group_by: [svc]
    - matchers: ['severity="warning"']
      receiver: warnings
inhibit_rules:
  - source_matchers: ['severity="critical"']
    target_matchers: ['severity="warning"']
    equal: [svc, team]
time_intervals:
  - name: window
    time_intervals:
      - times: [{start_time: "$(hhmm "$mute_from")", end_time: "$(hhmm "$mute_to")"}]
receivers:
  - name: default
    webhook_configs: [{url: "$k/default/0"}]
  - name: db-team
    webhook_configs: [{url: "$k/db-team/0"}]
  - name: warnings
    webhook_configs: [{url: "$k/warnings/0"}]
  - name: queue-team
    webhook_configs: [{url: "$k/queue-team/0"}]
  - name: pager
    webhook_configs: [{url: "$k/pager/0"}, {url: "$k/pager/1"}]
  - name: gold
    webhook_configs: [{url: "$k/gold/0", send_resolved: false}]
  - name: batch
    webhook_configs: [{url: "$k/batch/0"}]
  - name: sil
    webhook_configs: [{url: "$k/sil/0"}]
  - name: infra
    webhook_configs: [{url: "$k/infra/0"}]
YAML
}
config new >"$work/am-new/alertmanager.yml"
config old >"$work/am-old/alertmanager.yml"
paging='^(pager|infra)$'

cat >"$work/prom/prometheus.yml" <<YAML
global:
  evaluation_interval: 5s
rule_files: [/etc/prometheus/rules.yaml]
alerting:
  alertmanagers:
    - static_configs:
        - targets: ['$p-am-new:9093', '$p-am-old:9093']
YAML

docker network create "$p" >/dev/null
docker run -d --name "$p-keeper" --network "$p" -p 127.0.0.1::8080 \
  -v "$work/keeper:/keeper:ro" "$busybox" /keeper >/dev/null
# No cluster: a lone Alertmanager waits for no peer before its first flush.
am() { # name, image
  docker run -d --name "$p-$1" --network "$p" -p 127.0.0.1::9093 \
    -v "$work/$1/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro" "$2" \
    --config.file=/etc/alertmanager/alertmanager.yml --cluster.listen-address= >/dev/null
}
am am-new "$am_new"
am am-old "$am_old"
ku=http://$(docker port "$p-keeper" 8080/tcp | head -1)
nu=http://$(docker port "$p-am-new" 9093/tcp | head -1)
ou=http://$(docker port "$p-am-old" 9093/tcp | head -1)
until_true 60 curl -sf "$ku/ready"
until_true 120 curl -sf "$nu/-/ready"
until_true 120 curl -sf "$ou/-/ready"
versions=$(for u in "$nu" "$ou"; do curl -sf "$u/api/v2/status" | jq -r '.versionInfo.version'; done | paste -sd' ' -)
echo "Alertmanager $versions"

# A silence for the Silenced rule, through the API, in the future.
for u in "$nu" "$ou"; do
  jq -n --arg from "$(iso "$sil_from")" --arg to "$(iso "$sil_to")" \
    '{matchers: [{name: "alertname", value: "Silenced", isRegex: false, isEqual: true}], startsAt: $from, endsAt: $to, createdBy: "e2e", comment: "routing.sh"}' |
    curl -sf -XPOST "$u/api/v2/silences" -H 'Content-Type: application/json' -d @- >/dev/null
done

docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 \
  -v "$work/prom/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -v "$work/rules.yaml:/etc/prometheus/rules.yaml:ro" \
  "$prom" --config.file=/etc/prometheus/prometheus.yml >/dev/null
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
until_true 120 curl -sf "$pu/-/ready"
until_true 60 sh -c "curl -sf '$pu/api/v1/rules' | jq -e '[.data.groups[].rules[]] | length == 10'"
if [ "$(date +%s)" -ge $((t0 + 5)) ]; then
  echo "the servers took until $(iso "$(date +%s)") to start, past T0 $(iso "$t0"): run it again" >&2
  exit 1
fi

# Until every rule has fired and resolved, and the last flush has told of it
# (the period ends at T0+420s, 90s after the last alert resolves).
echo "waiting for $(iso $((end + 10)))"
until [ "$(date +%s)" -ge $((end + 10)) ]; do sleep 5; done

curl -sf "$ku/notifications" >"$work/keeper.json"
fail=0
ok() { echo "ok: $1"; }
failed() { echo "FAILED: $1"; fail=1; }
from=$(iso $((t0 - 30)))
to=$(iso "$end")
check() { # instance, name, URL
  curl -sf "$3/api/v2/silences" >"$work/silences-$1.json"
  local code=0
  "$work/lapilli" verdict --json --rules "$work/rules.yaml" --prometheus "$pu" \
    --alertmanager-config "$work/am-$1/alertmanager.yml" --silences "$work/silences-$1.json" \
    --paging-receivers "$paging" --from "$from" --to "$to" --step 5s \
    >"$work/verdict-$1.json" 2>"$work/verdict-$1.err" || code=$?
  if [ "$code" != 0 ]; then
    failed "verdict (the configuration of $2) exited $code"
    tail -20 "$work/verdict-$1.err"
    return
  fi
  echo
  echo "=== the notifications worked out from $2's configuration, and those it sent (times from T0, $(iso "$t0"))"
  if "$work/compare" -verdict "$work/verdict-$1.json" -config "$work/am-$1/alertmanager.yml" \
    -silences "$work/silences-$1.json" -keeper "$work/keeper.json" -instance "$1" -paging "$paging" \
    -tolerance "$tolerance" -origin "$t0" -to "$to"; then
    ok "$2: the worked-out notifications are those sent"
  else
    failed "$2: the worked-out notifications differ from those sent"
  fi
}
check new "Alertmanager $(sed -n 1p <<<"${versions// /$'\n'}")" "$nu"
check old "Alertmanager $(sed -n 2p <<<"${versions// /$'\n'}")" "$ou"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Notifications worked out and sent"
    echo
    echo "Alertmanager $versions, Prometheus $(curl -sf "$pu/api/v1/status/buildinfo" | jq -r .data.version)"
    for i in new old; do
      echo "- $i: $(jq -r '.notifications | "\(.notified) notifications worked out, \(.paged) pages"' "$work/verdict-$i.json" 2>/dev/null || echo none), $(jq "[.[] | select(.instance == \"$i\")] | length" "$work/keeper.json") sent"
    done
  } >>"$GITHUB_STEP_SUMMARY"
fi
exit "$fail"
