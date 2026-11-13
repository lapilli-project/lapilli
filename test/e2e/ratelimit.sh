#!/usr/bin/env bash
# A history API with a request quota, as a managed Prometheus has one: a
# real Prometheus behind nginx's limit_req, which answers HTTP 429 to what
# goes past RATE requests a second (BURST more at once). Several `lapilli
# verdict --compare` runs at once, as CI runs them on several pull
# requests, must each replay every rule: asked again after a wait, fewer at
# once. Then the same where each 429 says Retry-After. Then with --max-rate,
# the runs' rates adding up to under RATE: nginx turns nothing away.
#
# OLD=path/to/lapilli runs an earlier lapilli the first way too, and says
# how many rules it couldn't replay.
#
# Needs Docker and Go. Takes 4 to 8 minutes. Leaves everything up when KEEP
# is set.
# shellcheck disable=SC2016,SC2329 # nginx's variables, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
nginx=nginx:1.28.0-alpine@sha256:30f1c0d78e0ad60901648be663a710bdadf19e4c10ac6782c235200619158284
p=lapilli-e2e-ratelimit
rate=${RATE:-2}
burst=${BURST:-4}
runs=${RUNS:-3}
rules=${RULES:-12}

work=$(mktemp -d)
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  docker rm -fv "$p-nginx" "$p-prom" >/dev/null 2>&1 || true
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
    sleep 1
  done
}

go build -o "$work/lapilli" ./cmd/lapilli

# Each rule asks a query of its own, so that no two share a request; the
# rules proposed change each threshold.
rulesOf() { # threshold
  printf 'groups:\n  - name: load\n    rules:\n'
  for i in $(seq 1 "$rules"); do
    printf '      - alert: Load%02d\n        expr: prometheus_tsdb_head_series > %d\n        for: 2m\n' "$i" "$(($1 + i))"
  done
}
rulesOf 100000 >"$work/now.yaml"
rulesOf 200000 >"$work/proposed.yaml"
printf 'incidents:\n  - {id: INC-1, title: load, start: "%s"}\n' "$(date -u +%Y-%m-%dT%H:00:00Z)" >"$work/incidents.yaml"

# Four doors to one Prometheus, each a quota of its own and a log of what
# it answered: 429 alone; 429 with Retry-After; one for --max-rate; one for
# OLD.
server() { # port, zone, log, more
  cat <<NGINX
  server {
    listen $1;
    access_log /var/log/nginx/$3.log brief;
    location / {
      limit_req zone=$2 burst=$burst nodelay;
      proxy_pass http://$p-prom:9090;
    }
    $4
  }
NGINX
}
{
  printf 'events {}\nhttp {\n  log_format brief "$msec $status";\n  limit_req_status 429;\n'
  for z in plain told rated old; do
    printf '  limit_req_zone $server_port zone=%s:1m rate=%sr/s;\n' "$z" "$rate"
  done
  server 9090 plain plain ''
  server 9091 told told 'error_page 429 = @told;
    location @told { add_header Retry-After 3 always; return 429 "too many requests\n"; }'
  server 9092 rated rated ''
  server 9093 old old ''
  printf '}\n'
} >"$work/nginx.conf"

docker network create "$p" >/dev/null
docker run -d --name "$p-prom" --network "$p" "$prom" --config.file=/etc/prometheus/prometheus.yml >/dev/null
docker run -d --name "$p-nginx" --network "$p" -p 127.0.0.1::9090 -p 127.0.0.1::9091 -p 127.0.0.1::9092 -p 127.0.0.1::9093 \
  -v "$work/nginx.conf:/etc/nginx/nginx.conf:ro" "$nginx" >/dev/null
port() { docker port "$p-nginx" "$1/tcp" | head -1; }
# Asked from inside, so that no quota is spent on it.
until_true 60 docker exec "$p-nginx" wget -qO- "http://$p-prom:9090/-/ready"

fail=0
check() { # what, ok
  if [ "$2" = 1 ]; then echo "ok: $1"; else echo "FAILED: $1"; fail=1; fi
}
answered() { # log, status: how many requests that door answered so
  docker exec "$p-nginx" sh -c "cat /var/log/nginx/$1.log" | awk -v s="$2" '$2 == s' | wc -l | tr -d ' '
}

# runs N lapilli verdict --compare at once through a door, each to its own
# out.N, err.N and code.N.
together() { # binary, door, name, more flags...
  local bin=$1 door=$2 name=$3
  shift 3
  local pids=()
  for n in $(seq 1 "$runs"); do
    "$bin" verdict --rules "$work/proposed.yaml" --compare "$work/now.yaml" --prometheus "http://$(port "$door")" \
      --incidents "$work/incidents.yaml" --since 2d --timeout 15m "$@" \
      >"$work/$name.out.$n" 2>"$work/$name.err.$n" &
    pids+=($!)
  done
  for n in $(seq 1 "$runs"); do
    local code=0
    wait "${pids[$((n - 1))]}" || code=$?
    echo "$code" >"$work/$name.code.$n"
  done
}
dropped() { # name: the rules, before or after, the runs couldn't replay
  if [ "${2:-}" = json ]; then
    cat "$work/$1".out.* | grep -c '"failed": true' || true
  else
    cat "$work/$1".out.* | grep -c '^  Load[0-9]* (\(before\|after\)): \(query failed\|not finished\)' || true
  fi
}
replayedAll() { # name: every run exited 0 and replayed every rule
  for n in $(seq 1 "$runs"); do
    [ "$(cat "$work/$1.code.$n")" = 0 ] || return 1
    ! grep -q "couldn't be replayed\|Could not replay" "$work/$1.out.$n" || return 1
    grep -q "Rules compared: $rules changed" "$work/$1.out.$n" || return 1
  done
}

if [ -n "${OLD:-}" ]; then
  start=$(date +%s)
  together "$OLD" 9093 old
  echo "OLD: $runs runs at once in $(($(date +%s) - start))s, $(answered old 200) answers of 200, $(answered old 429) of 429: $(dropped old) rules couldn't be replayed"
  grep -h -m1 '^  Load[0-9]* (\(before\|after\)): query failed' "$work"/old.out.* | head -1 || true
fi

start=$(date +%s)
together "$work/lapilli" 9090 plain
echo "429 alone: $runs runs at once in $(($(date +%s) - start))s, $(answered plain 200) answers of 200, $(answered plain 429) of 429: $(dropped plain) rules couldn't be replayed"
check "429 alone: the quota was hit" "$([ "$(answered plain 429)" -gt 0 ] && echo 1)"
check "429 alone: every run replayed every rule" "$(replayedAll plain && echo 1)"
check "429 alone: the 429s were said on stderr" "$(grep -q 'with HTTP 429, too many requests' "$work/plain.err.1" && echo 1)"
check "429 alone: progress on stderr, not stdout" "$(grep -q 'replays of the rules before and after done' "$work/plain.err.1" && ! grep -q 'lapilli verdict' "$work/plain.out.1" && echo 1)"

start=$(date +%s)
together "$work/lapilli" 9091 told
echo "Retry-After: $runs runs at once in $(($(date +%s) - start))s, $(answered told 200) answers of 200, $(answered told 429) of 429: $(dropped told) rules couldn't be replayed"
check "Retry-After: the quota was hit" "$([ "$(answered told 429)" -gt 0 ] && echo 1)"
check "Retry-After: every run replayed every rule" "$(replayedAll told && echo 1)"

# Each run at a third of the rate less a tenth: together, under it.
each=$(awk -v r="$rate" -v n="$runs" 'BEGIN { printf "%.3f", r / n * 0.9 }')
start=$(date +%s)
together "$work/lapilli" 9092 rated --max-rate "$each" --json
echo "--max-rate $each each: $runs runs at once in $(($(date +%s) - start))s, $(answered rated 200) answers of 200, $(answered rated 429) of 429: $(dropped rated json) rules couldn't be replayed"
check "--max-rate: nothing turned away" "$([ "$(answered rated 429)" = 0 ] && echo 1)"
check "--max-rate: every run replayed every rule" "$(for n in $(seq 1 "$runs"); do
  [ "$(cat "$work/rated.code.$n")" = 0 ] && grep -q '"rules": \[' "$work/rated.out.$n" && ! grep -q '"unreplayed"' "$work/rated.out.$n" || exit 1
done && echo 1)"
# The busiest second nginx saw, of all the runs together.
busiest=$(docker exec "$p-nginx" sh -c 'cat /var/log/nginx/rated.log' | awk '{ n[int($1)]++ } END { m = 0; for (s in n) if (n[s] > m) m = n[s]; print m }')
echo "--max-rate: at most $busiest requests in one second, of $rate a second and $burst more at once"

if [ "$fail" != 0 ]; then
  for f in "$work"/*.code.*; do
    n=${f%.code.*}.out.${f##*.}
    echo "== $(basename "$n"): exit $(cat "$f")"
    grep -h "query failed\|not finished" "$n" | sort -u | head -5 || true
    tail -3 "${f%.code.*}.err.${f##*.}"
  done
fi
exit "$fail"
