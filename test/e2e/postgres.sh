#!/usr/bin/env bash
# The ledger in a real Postgres, in Docker (docs/design/postgres.md):
# lapilli serve started on it, firings copied in and judged, kept across a
# restart and a dropped connection; a second serve on the same ledger
# refused; the connection string on no command line and in no log; a
# SQLite ledger copied in with lapilli ledger copy and read back as SQLite
# reads it, and a second copy into it refused. No Prometheus: serve runs
# on firings copied in, so it starts at once.
#
# Needs Docker, Go, curl and jq. POSTGRES_IMAGE picks another Postgres.
# Leaves everything up when KEEP is set.
# shellcheck disable=SC2329 # cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

image=${POSTGRES_IMAGE:-postgres:18.6@sha256:e3d24f6906d0760bf4da0dae872415391ededefad23fd471c47dc53b0ddf3c42}
p=lapilli-e2e-postgres
pgport=${PG_PORT:-28632}
port=${PORT:-28640} # serve takes port to port+5
password=e2e-Ledger-pw-$RANDOM$RANDOM

work=$(mktemp -d)
pids=()
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, container $p"
    return
  fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  docker rm -fv "$p" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

fail=0
check() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
same() { # what, got, want (the same text)
  if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
psql() { docker exec -i "$p" psql -v ON_ERROR_STOP=1 -U postgres -At "$@"; }

go build -o "$work/lapilli" ./cmd/lapilli

docker run -d --name "$p" -e POSTGRES_PASSWORD="$password" -e POSTGRES_DB=lapilli \
  -p "127.0.0.1:$pgport:5432" "$image" >/dev/null
# The image starts a server to set itself up, stops it, and starts again.
for _ in $(seq 90); do
  docker logs "$p" 2>&1 | grep -q 'PostgreSQL init process complete' && psql -c 'SELECT 1' lapilli >/dev/null 2>&1 && break
  sleep 1
done
psql -c 'SELECT version()' lapilli
psql -c 'CREATE DATABASE lapilli_copy' postgres >/dev/null

# Over loopback, without TLS; a server across a network takes
# sslmode=verify-full.
printf 'postgres://postgres:%s@127.0.0.1:%s/lapilli?sslmode=disable\n' "$password" "$pgport" >"$work/ledger-url"
printf 'postgres://postgres:%s@127.0.0.1:%s/lapilli_copy?sslmode=disable\n' "$password" "$pgport" >"$work/copy-url"
chmod 600 "$work/ledger-url" "$work/copy-url"
printf 'firings-token-0123456789\n' >"$work/firings"

# serve NAME PORT LEDGER-FLAGS...: started in the background, ready once
# /healthz answers; its pid in $serving.
serving=
serve() {
  local name=$1 at=$2
  shift 2
  "$work/lapilli" serve "$@" --listen "127.0.0.1:$at" --incidents-listen "127.0.0.1:$((at + 1))" \
    --firings-token-file "$work/firings" --tz UTC --match service >>"$work/$name.log" 2>&1 &
  serving=$!
  pids+=("$serving")
  for _ in $(seq 60); do
    kill -0 "$serving" 2>/dev/null || break
    curl -fs "http://127.0.0.1:$at/healthz" >/dev/null && return
    sleep 1
  done
  cat "$work/$name.log"
  return 1
}
stop() { kill "$1"; wait "$1" 2>/dev/null || true; }

day=$(date -u -d '-2 days' +%F 2>/dev/null || date -u -v-2d +%F)
# put AT SOURCE: three firings of one day, from a cloud's alarm history.
put() {
  printf '%s\n' \
    '{"rule": "DiskFull", "labels": {"service": "cart"}, "start": "'"$day"'T04:00:00Z", "end": "'"$day"'T04:20:00Z"}' \
    '{"rule": "DiskFull", "labels": {"service": "cart"}, "start": "'"$day"'T09:00:00Z", "end": "'"$day"'T09:05:00Z"}' \
    '{"rule": "CPUHigh", "labels": {"service": "search", "zone": "ü-1"}, "start": "'"$day"'T11:00:00Z"}' |
    curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $(cat "$work/firings")" \
      -H 'Content-Type: application/x-ndjson' --data-binary @- \
      "http://127.0.0.1:$(($1 + 1))/api/v1/firings/$2?from=${day}T00:00:00Z&to=${day}T23:59:59Z"
}
sources() { curl -fsS "http://127.0.0.1:$1/api/v1/sources" | jq -c '[.[] | {name, firings, rules}]'; }
report() { curl -fsS "http://127.0.0.1:$1/api/v1/report?from=${day}T00:00:00Z&to=${day}T23:59:59Z" | jq -cS '[.rules[] | {rule, firings, white, black}]'; }

# serve on Postgres: firings copied in are kept and judged.
serve pg "$port" --ledger-url-file "$work/ledger-url"
pg=$serving
check "firings copied into the Postgres ledger" "$(put "$port" cloud-e2e)" '^200$'
check "the source has them" "$(sources "$port")" '^\[\{"name":"cloud-e2e","firings":3,"rules":2\}\]$'
before=$(report "$port")
check "and they are judged" "$before" '"rule":"DiskFull[^"]*","white":0'
check "the schema is the ledger's" "$(psql -c 'SELECT version, writers FROM schema_version' lapilli)" '^17\|1$'
check "every text compares as SQLite's" "$(psql -c "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND data_type = 'text' AND collation_name IS DISTINCT FROM 'C'" lapilli)" '^0$'

# The connection string is on no command line, and in no log.
check "the password isn't on serve's command line" "$(ps -o args= -p "$pg" | grep -c "$password" || true)" '^0$'
check "nor in its log" "$(grep -c "$password" "$work/pg.log" || true)" '^0$'
check "the log says the ledger is in Postgres, and no more" "$(grep -o 'msg=serving.*' "$work/pg.log")" 'ledger=postgres$'

# A second serve on the same ledger is refused, and the first goes on.
second=$((port + 3))
"$work/lapilli" serve --ledger-url-file "$work/ledger-url" --listen "127.0.0.1:$second" --incidents-listen "127.0.0.1:$((second + 1))" \
  --firings-token-file "$work/firings" --tz UTC >"$work/second.log" 2>&1 &
pids+=($!)
code=0
wait $! || code=$?
check "a second serve on the ledger is refused" "$code $(cat "$work/second.log")" '^1 .*in use by another process: Postgres session [0-9]+'
check "saying nothing of the connection string" "$(grep -c "$password" "$work/second.log" || true)" '^0$'
check "the first still writes" "$(put "$port" cloud-e2e)" '^200$'

# The writer's connection dropped: serve connects again, takes the lock
# again, and goes on.
dropped=$(psql -c "SELECT count(pg_terminate_backend(l.pid)) FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted" lapilli)
check "the writer's session ended" "$dropped" '^1$'
got=$(put "$port" cloud-e2e)
[ "$got" = 200 ] || got=$(put "$port" cloud-e2e)
check "after the connection dropped, a write goes through" "$got" '^200$'
check "and the lock is held again" "$(psql -c "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted" lapilli)" '^1$'
check "and serve runs on" "$(kill -0 "$pg" 2>/dev/null && echo running || echo stopped)" '^running$'

# A restart keeps what was written.
stop "$pg"
check "the lock goes with serve" "$(psql -c "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'" lapilli)" '^0$'
serve pg "$port" --ledger-url-file "$work/ledger-url"
pg=$serving
check "after a restart, the source's firings" "$(sources "$port")" '^\[\{"name":"cloud-e2e","firings":3,"rules":2\}\]$'
same "and their stones" "$(report "$port")" "$before"
stop "$pg"

# A SQLite ledger, written by serve, copied into an empty Postgres
# database, and read back alike.
serve sqlite "$port" --ledger "$work/sqlite"
lite=$serving
check "firings copied into a SQLite ledger" "$(put "$port" sqlite-e2e)" '^200$'
curl -fsS -X POST -H 'Content-Type: application/json' -d '{"name": "storage", "rules": "^Disk"}' "http://127.0.0.1:$port/api/v1/teams" >/dev/null
lite_sources=$(sources "$port")
lite_report=$(report "$port")
lite_teams=$(curl -fsS "http://127.0.0.1:$port/api/v1/teams" | jq -c '[.[] | {name, rules}]')
stop "$lite"
"$work/lapilli" ledger copy --ledger "$work/sqlite" --to-url-file "$work/copy-url" >"$work/copy.out" 2>"$work/copy.err"
check "lapilli ledger copy" "$(cat "$work/copy.out")" '^copied [0-9]+ tables, [0-9]+ rows, and read them back alike$'
check "table by table" "$(grep -c '^copied ' "$work/copy.err")" '^[1-9][0-9]$'
code=0
"$work/lapilli" ledger copy --ledger "$work/sqlite" --to-url-file "$work/copy-url" >/dev/null 2>"$work/again.err" || code=$?
check "a second copy into it is refused" "$code $(cat "$work/again.err")" "^1 .*isn't empty"
check "nor do the copy's errors say the connection string" "$(cat "$work/copy.err" "$work/again.err" | grep -c "$password" || true)" '^0$'
serve copied "$port" --ledger-url-file "$work/copy-url"
copied=$serving
same "the copy's sources, as SQLite had them" "$(sources "$port")" "$lite_sources"
same "its stones" "$(report "$port")" "$lite_report"
same "its teams" "$(curl -fsS "http://127.0.0.1:$port/api/v1/teams" | jq -c '[.[] | {name, rules}]')" "$lite_teams"
check "and it is written to as any" "$(put "$port" sqlite-e2e)" '^200$'
stop "$copied"
exit $fail
