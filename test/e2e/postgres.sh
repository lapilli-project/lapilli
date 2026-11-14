#!/usr/bin/env bash
# The ledger in a real Postgres, in Docker (docs/design/postgres.md):
# lapilli serve started on it, firings copied in and judged, kept across a
# restart and a dropped connection; a second serve on the same ledger a
# replica that follows, and a copy out of it refused while they hold it
# (docs/design/replicas.md); the connection string on no command line and
# in no log; a
# SQLite ledger copied in with lapilli ledger copy and read back as SQLite
# reads it, and a second copy into it refused. No Prometheus: serve runs
# on firings copied in, so it starts at once.
#
# Three replicas with the leader's process killed, another leading within
# the lease. Needs Docker, Go, curl, jq and python3. POSTGRES_IMAGE picks
# another Postgres.
# Leaves everything up when KEEP is set.
# shellcheck disable=SC2329 # cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."

image=${POSTGRES_IMAGE:-postgres:18.6@sha256:e3d24f6906d0760bf4da0dae872415391ededefad23fd471c47dc53b0ddf3c42}
p=lapilli-e2e-postgres
pgport=${PG_PORT:-28632}
port=${PORT:-28640} # serve takes port to port+12
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
# Replicas don't count themselves among the processes that wrote it alone.
check "the schema is the ledger's" "$(psql -c 'SELECT version, writers FROM schema_version' lapilli)" '^17\|0$'
check "every text compares as SQLite's" "$(psql -c "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND data_type = 'text' AND collation_name IS DISTINCT FROM 'C'" lapilli)" '^0$'

# The connection string is on no command line, and in no log.
check "the password isn't on serve's command line" "$(ps -o args= -p "$pg" | grep -c "$password" || true)" '^0$'
check "nor in its log" "$(grep -c "$password" "$work/pg.log" || true)" '^0$'
check "the log says the ledger is in Postgres, and no more" "$(grep -o 'msg=serving.*' "$work/pg.log")" 'ledger=postgres replica=[^ ]+$'

# A second serve on the same ledger is a replica: the first leads, the
# second follows, and each writes and reads what the other wrote. A copy
# out of the ledger, which would read it alone, is refused while they hold
# it.
second=$((port + 3))
serve second "$second" --ledger-url-file "$work/ledger-url"
two=$serving
leads() { curl -fsS "http://127.0.0.1:$1/metrics" | sed -nE 's/^lapilli_serve_leader ([01])$/\1/p'; }
for _ in $(seq 30); do [ "$(leads "$port")$(leads "$second")" = 10 ] && break; sleep 1; done
check "the first leads, the second follows" "$(leads "$port")$(leads "$second")" '^10$'
check "two replicas live" "$(curl -fsS "http://127.0.0.1:$second/metrics" | grep '^lapilli_serve_replicas ')" ' 2$'
check "the second writes" "$(put "$second" cloud-e2e-2)" '^200$'
check "and the first reads it at once" "$(sources "$port")" '"name":"cloud-e2e-2","firings":3'
code=0
"$work/lapilli" ledger copy --ledger-url-file "$work/ledger-url" --to "$work/out" >/dev/null 2>"$work/out.err" || code=$?
check "a copy out, while replicas hold the ledger, is refused" "$code $(cat "$work/out.err")" '^1 .*in use by another process: Postgres session [0-9]+'
check "saying nothing of the connection string" "$(grep -c "$password" "$work/second.log" "$work/out.err" | grep -vc ':0$' || true)" '^0$'
# The first stopped: it gives the lease up, and the second leads at once.
stop "$pg"
for _ in $(seq 20); do [ "$(leads "$second")" = 1 ] && break; sleep 1; done
check "the second leads once the first stops" "$(leads "$second")" '^1$'
stop "$two"
serve pg "$port" --ledger-url-file "$work/ledger-url"
pg=$serving
check "the first still writes" "$(put "$port" cloud-e2e)" '^200$'

# Its connections dropped, the writer's and the lease's: serve connects
# again, takes the lock again, and goes on.
dropped=$(psql -c "SELECT count(pg_terminate_backend(l.pid)) FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())" lapilli)
check "its sessions ended" "$dropped" '^[1-9]$'
got=$(put "$port" cloud-e2e)
[ "$got" = 200 ] || got=$(put "$port" cloud-e2e)
check "after the connection dropped, a write goes through" "$got" '^200$'
check "and the lock is held again" "$(psql -c "SELECT count(*) FROM pg_locks l WHERE l.locktype = 'advisory' AND l.granted AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())" lapilli)" '^[1-9]$'
check "and serve runs on" "$(kill -0 "$pg" 2>/dev/null && echo running || echo stopped)" '^running$'

# A restart keeps what was written.
kept=$(sources "$port")
before=$(report "$port")
check "both sources" "$kept" '"name":"cloud-e2e","firings":3.*"name":"cloud-e2e-2","firings":3'
stop "$pg"
check "the lock goes with serve" "$(psql -c "SELECT count(*) FROM pg_locks l WHERE l.locktype = 'advisory' AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())" lapilli)" '^0$'
serve pg "$port" --ledger-url-file "$work/ledger-url"
pg=$serving
same "after a restart, the sources' firings" "$(sources "$port")" "$kept"
same "and their stones" "$(report "$port")" "$before"
stop "$pg"

# Three replicas, and the leader's process killed (SIGKILL: no lease given
# up): another leads within the lease (15 s) and a campaign (2 s), and the
# notices of tasks made through each, before and after, reach the webhook
# once each.
python3 -c '
import http.server, sys
class Hook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(sys.argv[1], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[2])), Hook).serve_forever()
' "$work/told" "$((port + 12))" &
pids+=($!)
printf 'http://127.0.0.1:%s/hook\n' "$((port + 12))" >"$work/hook"
reps=()
for i in 0 1 2; do
  serve "replica$i" "$((port + 3 + 3 * i))" --ledger-url-file "$work/ledger-url" --notify-webhook-file "$work/hook"
  reps+=("$serving")
done
at() { echo $((port + 3 + 3 * $1)); }
leader=
for _ in $(seq 30); do
  for i in 0 1 2; do [ "$(leads "$(at "$i")")" = 1 ] && leader=$i; done
  [ -n "$leader" ] && break
  sleep 1
done
check "one of three replicas leads" "$leader" '^[012]$'
mktask() { curl -fsS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"title\": \"$2\"}" "http://127.0.0.1:$(at "$1")/api/v1/tasks"; }
for i in 0 1 2; do check "a task made on replica $i" "$(mktask "$i" "before $i")" '^201$'; done
unsent() { curl -fsS "http://127.0.0.1:$(at "$1")/metrics" | sed -nE 's/^lapilli_serve_notices_unsent ([0-9]+)$/\1/p'; }
for _ in $(seq 30); do [ "$(unsent "$leader")" = 0 ] && break; sleep 1; done
killed=$EPOCHREALTIME
kill -9 "${reps[$leader]}"
next=
for _ in $(seq 150); do
  for i in 0 1 2; do [ "$i" != "$leader" ] && [ "$(leads "$(at "$i")")" = 1 ] && next=$i; done
  [ -n "$next" ] && break
  sleep 0.2
done
took=$(awk -v a="$killed" -v b="$EPOCHREALTIME" 'BEGIN { printf "%.1f", b - a }')
echo "took over in ${took}s"
check "another leads once the leader is killed" "$next" '^[012]$'
check "within the lease and a campaign, and a little" "$(awk -v t="$took" 'BEGIN { print (t <= 20) ? "within" : t }')" '^within$'
for i in 0 1 2; do [ "$i" = "$leader" ] || check "a task made on replica $i after" "$(mktask "$i" "after $i")" '^201$'; done
for _ in $(seq 30); do [ "$(unsent "$next")" = 0 ] && break; sleep 1; done
sleep 3
check "every task told" "$(grep -c '"event":"task.created"' "$work/told")" '^5$'
check "and no notice twice" "$(grep -oE '"id":"lapilli-[0-9]+"' "$work/told" | sort | uniq -d | wc -l | tr -d ' ')" '^0$'
for i in 0 1 2; do [ "$i" = "$leader" ] || stop "${reps[$i]}"; done

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
