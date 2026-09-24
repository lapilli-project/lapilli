#!/usr/bin/env bash
# `lapilli mcp` over stdio, driven by the reference MCP client (@modelcontextprotocol/inspector
# --cli) against the released fixtures — the laptop shape. The in-cluster HTTP shape is proven
# by test/e2e/mcp.sh. Both assert on what an agent would receive, not on exit codes alone.
#
# Usage: test/mcp/check.sh [path-to-lapilli]   (default: target/debug/lapilli)
set -euo pipefail
cd "$(dirname "$0")/../.."
LAPILLI=${1:-target/debug/lapilli}
FIX=test/fixtures/ieb/v0.1.0
[ -x "$LAPILLI" ] || { echo "no binary at $LAPILLI (cargo build -p lapilli-cli)"; exit 1; }

fail() { echo "FAIL (mcp): $*"; exit 1; }
# One request per run, as the inspector CLI is designed: connect, call, print, exit.
mcp() { # method [args…]
  npx -y @modelcontextprotocol/inspector@2.8.0 --cli "$LAPILLI" mcp --root "$FIX" --method "$@" 2>/dev/null
}
# A tool result is JSON text inside content[0].text; unwrap it.
tool() { # name key=value…
  local name=$1; shift
  local args=()
  for kv in "$@"; do args+=(--tool-arg "$kv"); done
  mcp tools/call --tool-name "$name" "${args[@]}" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["content"][0]["text"] if not d.get("isError") else json.dumps({"isError": True, "text": d["content"][0]["text"]}))'
}

echo "==> mcp: tools/list names the five tools"
LIST=$(mcp tools/list)
for t in find_bundles verify summary read_file postmortem; do
  grep -q "\"name\": *\"$t\"" <<<"$LIST" || fail "tools/list lacks $t"
done
echo "  ok"

echo "==> mcp: verify returns the verify-result/v1 document, verdict first"
tool verify path=ok-deferred.ieb | python3 -c '
import json,sys; d=json.load(sys.stdin)
assert d["schema"]=="lapilli.dev/verify-result/v1", d["schema"]
assert d["verdict"]=="OK" and d["bundle"]["deferred"]==["events","metrics"], d
' || fail "ok-deferred did not verify OK with its deferred set"
tool verify path=partial.ieb | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["verdict"]=="PARTIAL", d' || fail "partial.ieb"
tool verify path=fail-modified.ieb | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["verdict"]=="FAILED", d' || fail "fail-modified.ieb"
echo "  ok"

echo "==> mcp: read_file serves hash-tree names from a verified bundle and refuses the rest"
tool read_file path=ok-unsigned.ieb file=logs/index.json | python3 -c '
import json,sys; d=json.load(sys.stdin)
assert d["verdict"]=="OK" and d["file"]=="logs/index.json" and "containers" in d["content"], d
assert d["untrusted"] is False, d
' || fail "read_file logs/index.json"
tool read_file path=ok-unsigned.ieb file=logs/app-previous.log | grep -q '"isError": true' || fail "a log file was served without --allow-logs"
tool read_file path=ok-unsigned.ieb file=../../etc/passwd | grep -q '"isError": true' || fail "a traversal name was not refused"
tool read_file path=ok-unsigned.ieb file=nope.json | grep -q '"isError": true' || fail "a name outside the hash tree was not refused"
tool read_file path=fail-modified.ieb file=logs/index.json | grep -q '"isError": true' || fail "a FAILED bundle served a file as evidence"
echo "  ok"

echo "==> mcp: paths outside --root are refused"
tool verify path=../keys/fixture.pub | grep -q '"isError": true' || fail "a path outside the root was served"
tool verify path=/etc/hosts | grep -q '"isError": true' || fail "an absolute path outside the root was served"
echo "  ok"

echo "==> mcp: summary and postmortem carry no log line"
tool summary path=ok-unsigned.ieb | python3 -c '
import json,sys; d=json.load(sys.stdin)
assert d["verdict"]=="OK" and d["summary"]["last_line"] is None, d
' || fail "summary carried a log line or no verdict"
tool postmortem path=partial.ieb | python3 -c '
import json,sys; d=json.load(sys.stdin)
assert d["verdict"]=="PARTIAL" and d["exit_code"]==2 and "# Incident" in d["markdown"], d
assert "panic: boom" not in d["markdown"], "the fixture log line leaked into the postmortem"
' || fail "postmortem"
echo "  ok"

echo "==> mcp: find_bundles scans manifests and filters"
tool find_bundles rule=KubePodCrashLooping | python3 -c '
import json,sys; d=json.load(sys.stdin)
assert d["matched"]>=1 and all(b["rule"]=="KubePodCrashLooping" for b in d["bundles"]), d
' || fail "find_bundles by rule"
tool find_bundles rule=NoSuchRule | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["matched"]==0, d' || fail "find_bundles negative"
echo "  ok"

echo "MCP CHECK OK"
