#!/usr/bin/env bash
# The ledger's, serve's and the command's tests in three shards, as CI runs
# them on Postgres (.github/workflows/ci.yml, postgres), where serve's alone
# take some twenty minutes in one job, and the ledger's and the command's
# with half of serve's came to the go test limit.
#
# Shard 1 runs the ledger's and the command's. Shard 2 runs every second of
# serve's tests, in name order; shard 3 runs the rest of serve's, skipping
# what shard 2 runs. Before running, the shard checks that the pattern shard
# 2 runs by lists exactly its tests among serve's: so every test runs in one
# shard, and one only.
#
#   test/shard.sh 1|2|3 [go test flags]
#
# With LAPILLI_TEST_POSTGRES naming a server, the ledgers are kept there
# (CONTRIBUTING.md); without, in SQLite.
set -euo pipefail
cd "$(dirname "$0")/.."

shard=${1:-}
case $shard in
  1 | 2 | 3) shift ;;
  *)
    echo "usage: test/shard.sh 1|2|3 [go test flags]" >&2
    exit 2
    ;;
esac

# The tests, examples and fuzz tests of the packages that a pattern lists,
# in name order: -list matches a name as -run and -skip do one without a
# slash.
names() {
  local pattern=$1
  shift
  go test -list "$pattern" "$@" | { grep -E '^(Test|Example|Fuzz)' || true; } | LC_ALL=C sort -u
}

serve=$(names . ./internal/serve)
two=$(printf '%s\n' "$serve" | awk 'NF && NR % 2 == 0')
re="^($(printf '%s\n' "$two" | paste -sd '|' -))\$"

listed=$(names "$re" ./internal/serve)
if [ "$listed" != "$two" ]; then
  echo "FAILED: shard 2's pattern lists other tests of serve's than shard 2's:" >&2
  diff <(printf '%s\n' "$two") <(printf '%s\n' "$listed") >&2 || true
  exit 1
fi

total=$(printf '%s\n' "$serve" | grep -c . || true)
n2=$(printf '%s\n' "$two" | grep -c . || true)
echo "serve's $total tests: shard 2 runs $n2, shard 3 runs $((total - n2)); shard 1 runs the ledger's and the command's"

case $shard in
  1) exec go test "$@" ./internal/ledger ./cmd/lapilli ;;
  2) exec go test -run "$re" "$@" ./internal/serve ;;
  3) exec go test -skip "$re" "$@" ./internal/serve ;;
esac
