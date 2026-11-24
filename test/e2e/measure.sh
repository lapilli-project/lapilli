#!/usr/bin/env bash
# The replay's measurements, taken again (docs/design/states.md, "Running the
# measurement again"): what a real Grafana, Prometheus and Loki fired, kept
# as test data in internal/replay/testdata and replayed by TestRealGrafana,
# TestRealPrometheus and TestRealLoki on every change. Run at any version of
# the three, it writes a new recording beside the old ones, in directories
# named for the version it finds running (grafana-13.4.0, prometheus-3.6.0,
# loki-3.8.0, and grafana-13.4.0-export, -scraped, -change, -loki), each with
# its period in period.json, and the tests replay it with nothing else
# changed.
#
#   test/e2e/measure.sh up       start the servers, provision the rules, feed them;
#                                the change of rules made through Grafana's API is
#                                part of it, so it returns some ten minutes in
#   test/e2e/measure.sh record   wait for the period to end, then record each
#                                recording with LAPILLI_RECORD and the tests
#   test/e2e/measure.sh down     remove the servers and what they kept
#   test/e2e/measure.sh status   say where they are and when each period is
#   test/e2e/measure.sh config   write the servers' configuration and rules
#                                under $STATE, and no more (for servers you run
#                                yourself: EXTERNAL=1, below)
#   test/e2e/measure.sh          up, record, down (KEEP=1: without down)
#
# The servers keep what they recorded until `down`: after a change to what
# the replay asks of them, `record` asks again and takes minutes, not
# another hour. Their rules are the ones in the newest recording of each kind
# already in testdata, so a recording of a new version has the rules of the
# last, and a rule added to a recording's directory is provisioned too.
#
# One Prometheus, Grafana and Loki run every recording's rules at once, fed
# by test/e2e/measure-feed (numbers that are functions of the clock, and
# log lines each second); each test takes what really fired for its own
# rules. Needs Docker, Go, curl, jq and yq (mikefarah's v4).
#
# PROMETHEUS_IMAGE, GRAFANA_IMAGE and LOKI_IMAGE run other images than the
# pinned ones. MINUTES (default 60, at least 20) is how long they run: a
# shorter run is a dry run, whose recordings may not hold enough firings for
# the tests. PREFIX (lapilli-r73) names the containers and network;
# PROM_PORT, GRAFANA_PORT and LOKI_PORT (28391 to 28393, on 127.0.0.1) say
# where they listen; STATE is where it keeps its files; MIN_FREE_GB (30) is
# the free disk below which it stops.
#
# EXTERNAL=1 starts no container: the servers are yours, started with what
# `config` wrote under $STATE (Prometheus's prometheus.yml and rules.yml,
# Loki's loki.yaml and loki-rules/, Grafana's prov/ and the settings
# docs/design/states.md lists), and the feed (test/e2e/measure-feed, with
# -loki) running; PROMETHEUS_URL, GRAFANA_URL and LOKI_URL say where they
# are; GRAFANA_PASSWORD is the admin's.
# shellcheck disable=SC2016,SC2154 # jq and yq programs, not the shell's; the variables of $state/env
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996}
grafana=${GRAFANA_IMAGE:-grafana/grafana:13.3.1@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf}
loki=${LOKI_IMAGE:-grafana/loki:3.7.9@sha256:0db216b630ae1f348a1584c67f3cead8b4c1fc839169a39501db664744158e28}
p=${PREFIX:-lapilli-r73}
minutes=${MINUTES:-60}
pport=${PROM_PORT:-28391}
gport=${GRAFANA_PORT:-28392}
lport=${LOKI_PORT:-28393}
state=${STATE:-${TMPDIR:-/tmp}/$p-state}
state=${state%/}
out=internal/replay/testdata
pass=${GRAFANA_PASSWORD:-measure-admin-pass}
external=${EXTERNAL:-}
pu=${PROMETHEUS_URL:-http://127.0.0.1:$pport}
gu=${GRAFANA_URL:-http://127.0.0.1:$gport}
lu=${LOKI_URL:-http://127.0.0.1:$lport}

die() {
  echo "measure: $*" >&2
  exit 1
}
until_true() { # seconds, command...: run it until it succeeds
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 2
  done
}
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
sleep_until() { # epoch seconds
  while [ "$(date +%s)" -lt "$1" ]; do sleep 1; done
}
ga=(-u "admin:$pass" -H 'Content-Type: application/json')
gcurl() { curl -sf "${ga[@]}" "$@"; }
# Version of a server as a directory name says it: 13.3.1, not 13.3.1-security-01.
version() { local v=${1#v}; echo "${v%%[!0-9.]*}"; }
# The newest recording of a server, by version, with its suffix (or none).
newest() { # server, suffix
  local re="^$1-[0-9][0-9.]*${2:+-$2}\$" d
  for d in "$out"/"$1"-*; do [[ ${d##*/} =~ $re ]] && echo "${d##*/}"; done | sort -V | tail -1
}
source_of() { # server, suffix: its newest recording, which must be there
  local d
  d=$(newest "$1" "$2")
  [ -n "$d" ] || die "no recording of $1${2:+-$2} in $out to take the rules from"
  echo "$out/$d"
}

# --- what the servers are given -------------------------------------------

config() {
  local gm pm gl gx gs lm ns f
  gm=$(source_of grafana "")
  pm=$(source_of prometheus "")
  gl=$(source_of grafana loki)
  gx=$(source_of grafana export)
  gs=$(source_of grafana scraped)
  lm=$(source_of loki "")
  mkdir -p "$state/loki-rules/fake" "$state/prov/datasources" "$state/prov/alerting"
  chmod -R u+rwX,go+rX "$state"

  # Prometheus: the groups placed by its own scheduler, a scrape of the feed
  # every 15s, and Loki's ruler and Grafana writing what they fired to it.
  cp "$pm/rules.yml" "$state/rules.yml"
  cat >"$state/prometheus.yml" <<YAML
global: {scrape_interval: 15s}
rule_files: [/etc/prometheus/rules.yml]
scrape_configs:
  - job_name: fid
    static_configs: [{targets: ["$p-feed:9100"]}]
YAML

  # Loki, its ruler on, its rules from files, writing ALERTS to Prometheus.
  # Its rules are the configuration API's answer kept in the recording: a
  # namespace, a file, to its groups.
  cat >"$state/loki.yaml" <<YAML
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
  rm -f "$state"/loki-rules/fake/*
  while read -r ns; do
    NS=$ns yq '{"groups": .[strenv(NS)]}' "$lm/rules.yaml" >"$state/loki-rules/fake/$ns"
  done < <(yq -r 'keys | .[]' "$lm/rules.yaml")

  # Grafana: its data sources, and the rules of each recording, provisioned
  # from the files kept with it as they are.
  cat >"$state/prov/datasources/ds.yaml" <<YAML
apiVersion: 1
datasources:
  - {name: Prometheus, type: prometheus, uid: prom, url: "http://$p-prom:9090", access: proxy, jsonData: {prometheusType: Prometheus}}
  - {name: Prometheus 30s, type: prometheus, uid: prom30, url: "http://$p-prom:9090", access: proxy, jsonData: {prometheusType: Prometheus, timeInterval: 30s}}
  - {name: Loki, type: loki, uid: loki, url: "http://$p-loki:3100", access: proxy}
  - {name: Orders, type: mysql, uid: orders, url: "mysql:3306", access: proxy}
YAML
  rm -f "$state"/prov/alerting/*
  cp "$gm/rules.json" "$state/prov/alerting/fidelity.json"
  cp "$gl/rules.yaml" "$state/prov/alerting/logs.yaml"
  yq -I4 '.groups |= map(select(.name == "dollars"))' "$gx/rules.yaml" >"$state/prov/alerting/dollars.yaml"
  # The group the change is made to is made through the API, below.
  yq -I4 '.groups |= map(select(.name == "fid2"))' "$gs/rules.yaml" >"$state/prov/alerting/fid2.yaml"
  chmod -R u+rwX,go+rX "$state"
  # A title or a uid twice would be one rule lost to the other.
  f=$(rule_ids | sort | uniq -d)
  [ -z "$f" ] || die "rules provisioned twice: $f"
}

# uid and title of every rule provisioned from a file.
rule_ids() {
  local f
  for f in "$state"/prov/alerting/*.json; do jq -r '.groups[].rules[] | .uid + " " + .title' "$f"; done
  for f in "$state"/prov/alerting/*.yaml; do yq -r '.groups[].rules[] | .uid + " " + .title' "$f"; done
}

# --- the servers -----------------------------------------------------------

start_containers() {
  local arch
  case $(docker info --format '{{.Architecture}}') in
    aarch64 | arm64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) die "no Go architecture for $(docker info --format '{{.Architecture}}')" ;;
  esac
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o "$state/feed" ./test/e2e/measure-feed
  chmod 755 "$state/feed"
  for o in PROMETHEUS_IMAGE GRAFANA_IMAGE LOKI_IMAGE; do
    [ -n "${!o:-}" ] || continue
    docker pull -q "${!o}" >/dev/null 2>&1 || true
    echo "$o=${!o} ($(docker image inspect --format '{{index .RepoDigests 0}}' "${!o}" 2>/dev/null || true))"
  done
  docker network create "$p" >/dev/null
  docker run -d --name "$p-prom" --network "$p" -p "127.0.0.1:$pport:9090" \
    -v "$state/prometheus.yml:/etc/prometheus/prometheus.yml:ro" -v "$state/rules.yml:/etc/prometheus/rules.yml:ro" \
    "$prom" --config.file=/etc/prometheus/prometheus.yml --web.enable-remote-write-receiver >/dev/null
  docker run -d --name "$p-loki" --network "$p" -p "127.0.0.1:$lport:3100" \
    -v "$state/loki.yaml:/etc/loki/loki.yaml:ro" -v "$state/loki-rules:/etc/loki/rules:ro" \
    "$loki" -config.file=/etc/loki/loki.yaml >/dev/null
  docker run -d --name "$p-grafana" --network "$p" -p "127.0.0.1:$gport:3000" -e "GF_SECURITY_ADMIN_PASSWORD=$pass" \
    -e GF_ANALYTICS_REPORTING_ENABLED=false -e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
    -e GF_UNIFIED_ALERTING_STATE_HISTORY_ENABLED=true -e GF_UNIFIED_ALERTING_STATE_HISTORY_BACKEND=prometheus \
    -e GF_UNIFIED_ALERTING_STATE_HISTORY_PROMETHEUS_TARGET_DATASOURCE_UID=prom \
    -v "$state/prov:/etc/grafana/provisioning:ro" "$grafana" >/dev/null
  until_true 180 sh -c "curl -sf '$lu/ready' | grep -q ready" || die "Loki isn't ready"
  # The feed runs in the Prometheus image, which has a shell and nothing the
  # static binary needs.
  docker run -d --name "$p-feed" --network "$p" --entrypoint /feed -v "$state/feed:/feed:ro" \
    "$prom" -loki "http://$p-loki:3100" >/dev/null
}

stop_containers() {
  docker rm -fv "$p-feed" "$p-grafana" "$p-loki" "$p-prom" >/dev/null 2>&1 || true
  docker network rm "$p" >/dev/null 2>&1 || true
}

# --- up --------------------------------------------------------------------

# window LEN: seconds a recording of LEN minutes covers: its measured length,
# or what a shorter run leaves after six minutes of warming up.
window() { echo $((($1 < minutes - 6 ? $1 : minutes - 6) * 60)); }

up() {
  local gc folder want r e from cfrom
  [ ! -e "$state/env" ] || die "already up (state in $state): down first"
  [ "$minutes" -ge 20 ] || die "MINUTES is $minutes: at least 20"
  if [ -z "$external" ]; then
    command -v docker >/dev/null || die "needs Docker"
    docker info >/dev/null 2>&1 || die "Docker isn't running"
    free=$(df -Pk "${TMPDIR:-/tmp}" | awk 'NR == 2 {print int($4 / 1048576)}')
    [ "$free" -ge "${MIN_FREE_GB:-30}" ] || die "only $free GiB free; MIN_FREE_GB is ${MIN_FREE_GB:-30}"
  fi
  for c in curl jq yq go; do command -v "$c" >/dev/null || die "needs $c"; done
  mkdir -p "$state"
  trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR
  if [ -z "$external" ]; then
    config
    stop_containers
    start_containers
  else
    [ -d "$state/prov" ] || die "EXTERNAL: run config first, and start the servers with what it wrote"
  fi
  until_true 120 curl -sf "$pu/-/ready" || die "Prometheus isn't ready"
  until_true 120 sh -c "curl -sf '$lu/ready' | grep -q ready" || die "Loki isn't ready"
  until_true 180 curl -sf "$gu/api/health" || die "Grafana isn't ready"

  # Grafana has loaded every rule it was given.
  want=$(rule_ids | wc -l | tr -d ' ')
  until_true 180 sh -c "[ \"\$(curl -sf -u 'admin:$pass' '$gu/api/v1/provisioning/alert-rules' | jq length)\" = $want ]" ||
    die "Grafana has not loaded the $want rules it was given"
  until_true 120 sh -c "curl -sf '$pu/api/v1/targets' | jq -e '[.data.activeTargets[] | select(.health == \"up\")] | length == 1'" ||
    die "Prometheus doesn't scrape the feed"

  # A Viewer's token, for Loki through Grafana's data source proxy and for
  # Grafana's rules.
  id=$(gcurl -X POST "$gu/api/serviceaccounts" -d '{"name":"lapilli","role":"Viewer"}' | jq -r .id)
  gcurl -X POST "$gu/api/serviceaccounts/$id/tokens" -d '{"name":"viewer"}' | jq -j .key >"$state/viewer"

  # The group the change is made to, through the API.
  gc=$(source_of grafana change)
  folder=$(gcurl "$gu/api/folders?query=fid2" | jq -r '.[] | select(.title == "fid2") | .uid')
  [ -n "$folder" ] || die "Grafana has no folder fid2"
  r=$(date +%s)
  put_group "$folder" "$gc/rules-before.yaml"

  # The periods, all ending at e, a whole 10s after the run's minutes, but
  # for the change's, which starts a minute and a half in: a Loki ruler's
  # check starts on a minute when no firing is about to end (docs/design/loki.md);
  # and the change is made seven minutes and five seconds in.
  e=$(((r + minutes * 60) / 10 * 10))
  from=$(((e - $(window 40)) / 60 * 60))
  while [ $((from % 180)) -ne 120 ] || [ $((from % 300)) -eq 120 ]; do from=$((from - 60)); done
  [ "$from" -ge $((r + 120)) ] || die "no start for Loki's recording in MINUTES=$minutes: give it more"
  cfrom=$(((r + 90) / 10 * 10 + 10))
  {
    printf 'r=%q\ne=%q\n' "$r" "$e"
    printf 'main_from=%q\nexport_from=%q\nscraped_from=%q\n' "$((e - $(window 46)))" "$((e - $(window 6)))" "$((e - $(window 40)))"
    printf 'loki_from=%q\nloki_to=%q\n' "$from" "$((from + $(window 40)))"
    printf 'change_from=%q\nchange_to=%q\n' "$cfrom" "$((cfrom + 17 * 60))"
  } >"$state/env"

  # The change, at a second a little past a rule evaluation's, between two.
  sleep_until $((cfrom + 7 * 60 + 5))
  gcurl "$gu/api/v1/provisioning/alert-rules/export?format=yaml" |
    yq -I4 '.groups |= map(select(.folder == "fid2"))' >"$state/rules-before.yaml"
  put_group "$folder" "$gc/rules-after.yaml"
  changed=$(date +%s)
  gcurl "$gu/api/v1/provisioning/alert-rules/export?format=yaml" |
    yq -I4 '.groups |= map(select(.folder == "fid2"))' >"$state/rules-after.yaml"
  printf 'changed=%q\n' "$changed" >>"$state/env"
  echo "up: $(iso "$r"); the periods end $(iso "$e"); Prometheus $pu, Grafana $gu, Loki $lu"
}

# put_group FOLDER FILE: the group fid2api of an export, made or replaced
# through Grafana's provisioning API.
put_group() {
  yq -o=json '.groups[] | select(.name == "fid2api")' "$2" |
    jq -c --arg f "$1" '
      def secs: if endswith("s") then rtrimstr("s") | tonumber elif endswith("m") then rtrimstr("m") | tonumber * 60 else rtrimstr("h") | tonumber * 3600 end;
      {title: .name, interval: (.interval | secs),
       rules: [.rules[] | . + {folderUID: $f, ruleGroup: "fid2api", orgID: 1}
               | if has("keepFiringFor") then . + {keep_firing_for: .keepFiringFor} | del(.keepFiringFor) else . end]}' |
    curl -sf "${ga[@]}" -X PUT "$gu/api/v1/provisioning/folder/$1/rule-groups/fid2api" -d @- >/dev/null
}

# --- record ----------------------------------------------------------------

period() { # directory, from, to[, changed]
  jq -n --indent 1 --arg f "$(iso "$2")" --arg t "$(iso "$3")" --arg c "${4:+$(iso "$4")}" \
    '{from: $f, to: $t, step: "10s"} + (if $c == "" then {} else {changed: $c} end)' >"$out/$1/period.json"
}

record() {
  [ -e "$state/env" ] || die "not up: up first"
  # shellcheck disable=SC1091
  . "$state/env"
  [ -n "${changed:-}" ] || die "up has not finished: run it again after it has"
  echo "record: waiting for $(iso "$e") and a little more"
  sleep_until $((e + 30))

  local gv pv lv gm pm gx gs gc gl lm sm sp fail=0
  gv=$(version "$(curl -sf "$gu/api/health" | jq -r .version)")
  pv=$(version "$(curl -sf "$pu/api/v1/status/buildinfo" | jq -r .data.version)")
  lv=$(version "$(curl -sf "$lu/loki/api/v1/status/buildinfo" | jq -r .version)")
  [ -n "$gv" ] && [ -n "$pv" ] && [ -n "$lv" ] || die "no versions: Grafana '$gv', Prometheus '$pv', Loki '$lv'"
  gm=grafana-$gv
  gx=$gm-export
  gs=$gm-scraped
  gc=$gm-change
  gl=$gm-loki
  pm=prometheus-$pv
  lm=loki-$lv
  echo "record: Grafana $gv, Prometheus $pv, Loki $lv"
  sm=$(source_of grafana "")
  sp=$(source_of prometheus "")
  mkdir -p "$out/$gm" "$out/$gx" "$out/$gs" "$out/$gc" "$out/$gl" "$out/$pm" "$out/$lm"

  # What was provisioned, as the recording keeps it. Grafana's rules as it
  # exports them are what --grafana reads, and are read again from it.
  [ "$sm" = "$out/$gm" ] || cp "$sm/rules.json" "$out/$gm/rules.json"
  [ "$sp" = "$out/$pm" ] || cp "$sp/rules.yml" "$out/$pm/rules.yml"
  gcurl "$gu/api/v1/provisioning/alert-rules/export?format=yaml" |
    yq -I4 '.groups |= map(select(.name == "dollars"))' >"$out/$gx/rules.yaml"
  gcurl "$gu/api/v1/provisioning/alert-rules/export?format=yaml" |
    yq -I4 '.groups |= map(select(.folder == "fid2"))' >"$out/$gs/rules.yaml"
  cp "$state/rules-before.yaml" "$out/$gc/rules-before.yaml"
  cp "$state/rules-after.yaml" "$out/$gc/rules-after.yaml"
  # What Grafana said of its data sources (the scrape interval of each),
  # Prometheus's configuration and targets.
  for uid in prom prom30; do
    gcurl "$gu/api/datasources/uid/$uid" | jq --arg u "$uid" '{($u): (.jsonData.timeInterval // "")}'
  done | jq -s --indent 1 --arg v "$gv" '{"_": ("jsonData.timeInterval of each data source, as Grafana " + $v + "\u0027s /api/datasources/uid/{uid} answered"), dataSources: add}' >"$out/$gs/datasources.json"
  curl -sf "$pu/api/v1/status/config" | jq -r .data.yaml >"$out/$gs/prometheus.yml"
  curl -sf "$pu/api/v1/targets" >"$out/$gs/targets.json"

  period "$gm" "$main_from" "$e"
  period "$pm" "$main_from" "$e"
  period "$gx" "$export_from" "$e"
  period "$gs" "$scraped_from" "$e"
  period "$gc" "$change_from" "$change_to" "$changed"
  period "$gl" "$loki_from" "$loki_to"
  period "$lm" "$loki_from" "$loki_to"

  # Each recording is its own test, against the servers, which hold what
  # every recording's rules did. A test that finds a difference still writes
  # the recording, and is said here.
  go test -c -o "$state/replay.test" ./internal/replay
  rec() { # directory, test, what
    echo "record: $3: $1"
    if ! (cd internal/replay && LAPILLI_RECORD="$pu" LAPILLI_RECORD_DIR="$1" LAPILLI_RECORD_LOKI="$lu" \
      LAPILLI_RECORD_GRAFANA="$gu" LAPILLI_RECORD_GRAFANA_TOKEN="$state/viewer" \
      "$state/replay.test" -test.run "^$2\$" -test.count=1 -test.v 2>&1 | tail -40); then
      fail=1
    fi
  }
  rec "$gm" TestRealGrafana "Grafana's rules"
  rec "$pm" TestRealPrometheus "Prometheus's groups"
  rec "$gx" TestRealGrafanaExport "Grafana's export"
  rec "$gs" TestRealGrafanaScraped "Grafana on scraped metrics"
  rec "$gc" TestRealGrafanaChange "Grafana's change"
  rec "$lm" TestRealLoki "Loki's ruler"
  rec "$gl" TestRealLokiGrafana "Grafana on Loki"
  echo "record: written under $out: $gm $pm $gx $gs $gc $lm $gl"
  [ "$fail" = 0 ] || die "a recording's test failed: the recording is written, and the replay differs from what fired"
}

down() {
  [ -n "$external" ] || stop_containers
  rm -rf "$state"
}

status() {
  [ -e "$state/env" ] || die "not up"
  # shellcheck disable=SC1091
  . "$state/env"
  echo "Prometheus $pu, Grafana $gu (admin:$pass), Loki $lu; state in $state"
  echo "started $(iso "$r"), periods end $(iso "$e") (main from $(iso "$main_from"), Loki from $(iso "$loki_from"), change $(iso "$change_from") to $(iso "$change_to"))"
  echo "record again: $0 record"
}

case ${1:-all} in
  up) up ;;
  record) record ;;
  down) down ;;
  status) status ;;
  config) mkdir -p "$state" && config ;;
  all)
    [ ! -e "$state/env" ] || die "already up (state in $state): down first"
    trap '[ -n "${KEEP:-}" ] || down' EXIT
    up
    record
    ;;
  *) die "up, record, down, status, config, or nothing for up, record and down" ;;
esac
