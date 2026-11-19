#!/usr/bin/env bash
# The ledger's, serve's and the command's tests in two shards, as CI runs
# them on Postgres (.github/workflows/ci.yml, postgres), where serve's alone
# take some twenty minutes in one job.
#
# Shard 2 runs every second of serve's tests, in name order. Shard 1 runs
# the ledger's, the command's and serve's, skipping what shard 2 runs: a
# name the ledger's or the command's tests have too stays in shard 1, or
# the skip would take theirs. Before running, the shard checks that the
# pattern shard 2 runs by lists exactly its tests among serve's and none of
# the others': so every test runs in one shard, and one only.
#
#   test/shard.sh 1|2 [go test flags]
#
# With LAPILLI_TEST_POSTGRES naming a server, the ledgers are kept there
# (CONTRIBUTING.md); without, in SQLite.
set -euo pipefail
cd "$(dirname "$0")/.."

shard=${1:-}
case $shard in
  1 | 2) shift ;;
  *)
    echo "usage: test/shard.sh 1|2 [go test flags]" >&2
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
others=$(names . ./internal/ledger ./cmd/lapilli)
two=$(LC_ALL=C comm -23 <(printf '%s\n' "$serve") <(printf '%s\n' "$others") | awk 'NF && NR % 2 == 0')
re="^($(printf '%s\n' "$two" | paste -sd '|' -))\$"

listed=$(names "$re" ./internal/serve)
if [ "$listed" != "$two" ]; then
  echo "FAILED: shard 2's pattern lists other tests of serve's than shard 2's:" >&2
  diff <(printf '%s\n' "$two") <(printf '%s\n' "$listed") >&2 || true
  exit 1
fi
taken=$(names "$re" ./internal/ledger ./cmd/lapilli)
if [ -n "$taken" ]; then
  echo "FAILED: shard 1 would skip these tests of the ledger's or the command's:" >&2
  printf '%s\n' "$taken" >&2
  exit 1
fi

total=$(printf '%s\n' "$serve" | grep -c . || true)
n2=$(printf '%s\n' "$two" | grep -c . || true)
echo "serve's $total tests: shard 1 runs $((total - n2)), and the ledger's and the command's; shard 2 runs $n2"

case $shard in
  1) exec go test -skip "$re" "$@" ./internal/ledger ./internal/serve ./cmd/lapilli ;;
  2) exec go test -run "$re" "$@" ./internal/serve ;;
esac
