#!/usr/bin/env bash
# `lapilli mcp` where the bundles are: the chart's `mcp.enabled` container in the controller pod,
# reached over streamable HTTP with the generated token, driven by the reference MCP client.
# Proves the in-cluster shape the design chose after two critics refused the laptop-only one:
#   token       — no token and a wrong token are 401; the right one lists five tools
#   find        — the demo's crash-loop capture is found by the alert's namespace + pod prefix
#   evidence    — read_file returns resources/pod.json with the pod's name, and the rollout diff
#   logs        — logs/** is refused (mcp.allowLogs is off)
#
# Usage: test/e2e/mcp.sh <path-to-lapilli-binary> <scratch-dir>   (called by run.sh)
set -euo pipefail

LAPILLI=$1
OUT=$2/mcp
KNS=lapilli-system
mkdir -p "$OUT"

step() { echo; echo "==> mcp: $*"; }
fail() { echo "FAIL (mcp): $*"; kubectl -n "$KNS" logs deploy/lapilli -c mcp --tail=40 2>/dev/null || true; exit 1; }
PF_PID=""
cleanup() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

step "helm upgrade --set mcp.enabled=true (a second container, the volume read-only)"
helm upgrade lapilli charts/lapilli -n "$KNS" --reuse-values --set mcp.enabled=true \
  --wait --timeout 180s >/dev/null
kubectl -n "$KNS" rollout status deploy/lapilli --timeout=120s >/dev/null
TOKEN=$(kubectl -n "$KNS" get secret lapilli-mcp-token -o jsonpath='{.data.token}' | base64 -d)
[ "${#TOKEN}" -ge 32 ] || fail "generated token is too short"
kubectl -n "$KNS" port-forward svc/lapilli-mcp 18084:8082 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do curl -sf localhost:18084/healthz >/dev/null 2>&1 && break; sleep 1; done
curl -sf localhost:18084/healthz >/dev/null || fail "the mcp container never became reachable"

URL=http://localhost:18084/mcp
# One request per inspector run. A JSON-RPC error (a refused name, a missing file) is printed
# to STDERR as {"error":…} with a non-zero exit; an answer goes to stdout. `tool` folds both into
# one JSON line so refusals can be asserted on as easily as answers.
OUTERR=$(mktemp)
mcp() { # method [args…]
  npx -y @modelcontextprotocol/inspector@2.8.0 --cli "$URL" --transport http \
    --header "Authorization: Bearer $TOKEN" --method "$@"
}
tool() { local name=$1; shift; local args=(); for kv in "$@"; do args+=(--tool-arg "$kv"); done
  local out err
  out=$(mcp tools/call --tool-name "$name" "${args[@]}" 2>"$OUTERR") || true
  err=$(cat "$OUTERR")
  python3 - "$out" "$err" <<'PY'
import json, sys
out, err = sys.argv[1], sys.argv[2]
if out.strip():
    d = json.loads(out)
    print(json.dumps({"isError": True, "text": d["content"][0]["text"]}) if d.get("isError") else d["content"][0]["text"])
else:
    msg = err.strip()
    try:
        msg = json.loads(err.strip().splitlines()[-1])["error"]["message"]
    except Exception:
        pass
    print(json.dumps({"isError": True, "text": msg}))
PY
}

step "token: no token and a wrong token are 401"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL" -H 'Content-Type: application/json' -d '{}')" = 401 ] \
  || fail "a request without a token was not 401"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL" -H 'Authorization: Bearer wrong' -H 'Content-Type: application/json' -d '{}')" = 401 ] \
  || fail "a request with a wrong token was not 401"
LIST=$(mcp tools/list) || fail "tools/list with the right token failed"
for t in find_bundles verify summary read_file postmortem; do grep -q "\"name\": *\"$t\"" <<<"$LIST" || fail "tools/list lacks $t"; done
echo "  ok: 401 twice, five tools with the token"

step "find: the demo's crash-loop capture, by the alert's rule, namespace and pod prefix"
# run.sh ran the crashloop demo and then the oomkill demo, both on lapilli-demo/checkout-*, so
# the newest match by namespace+pod alone is the OOM one; the rule is what an alert carries too.
tool find_bundles rule=KubePodCrashLooping namespace=lapilli-demo 'pod=checkout-*' > "$OUT/find.json"
# The deferred suite runs before this one and leaves perishable captures of the same alert on
# the volume — no `logs` collector, so no logs/index.json in their hash tree, and read_file
# would refuse it correctly. The steps below need a full capture: pick the newest match whose
# collectors_run includes logs, which is what an agent would do with the same field.
python3 - "$OUT/find.json" <<'EOF' > "$OUT/pick.json" || fail "find_bundles did not return a full demo capture"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["matched"] >= 1, d
full = [b for b in d["bundles"] if "logs" in b["collectors_run"]]
assert full, {"matched": d["matched"], "collectors_run": [b["collectors_run"] for b in d["bundles"]]}
b = full[0]
assert b["target"]["namespace"] == "lapilli-demo" and b["target"]["pod"].startswith("checkout-"), b
assert b["rule"] == "KubePodCrashLooping" and b["path"].endswith(".ieb"), b
print(json.dumps(b))
EOF
BUNDLE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["path"])' "$OUT/pick.json")
POD=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["target"]["pod"])' "$OUT/pick.json")
echo "  ok: $BUNDLE about $POD"

step "evidence: verify, then read_file resources/pod.json and the rollout diff"
tool verify "path=$BUNDLE" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["verdict"] in ("OK","PARTIAL"), d' || fail "verify"
tool read_file "path=$BUNDLE" file=resources/pod.json > "$OUT/pod.json"
python3 - "$OUT/pod.json" "$POD" <<'EOF' || fail "resources/pod.json did not come back as the object body"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["file"] == "resources/pod.json" and d["untrusted"] is False and d["redacted_at_capture"] is True, {k: d[k] for k in d if k != "content"}
pod = d["content"]
assert pod["metadata"]["name"] == sys.argv[2], pod["metadata"]["name"]
env = {e["name"]: e.get("value") for c in pod["spec"]["containers"] for e in c.get("env", [])}
assert env.get("CACHE_WARMUP") == "eager", env   # the object body: the value the diff is about
EOF
tool read_file "path=$BUNDLE" file=diffs/index.json | python3 -c '
import json,sys; d=json.load(sys.stdin); e=d["content"]["entries"]
assert any(x.get("status")=="ok" and "CACHE_WARMUP" in " ".join(x.get("summary",[])) for x in e), e
' || fail "diffs/index.json did not carry the CACHE_WARMUP change"
echo "  ok: object body and diff, both redacted at capture, both from a verified bundle"

step "logs: refused while mcp.allowLogs is off"
tool read_file "path=$BUNDLE" file=logs/app-previous.log | grep -q '"isError": true' || fail "a log file was served with allowLogs off"
tool read_file "path=$BUNDLE" file=logs/index.json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert "containers" in d["content"], d' || fail "logs/index.json (not a log) should be served"
echo "  ok"

step "cleanup: mcp.enabled back off"
cleanup; PF_PID=""
helm upgrade lapilli charts/lapilli -n "$KNS" --reuse-values --set mcp.enabled=false --wait --timeout 180s >/dev/null
echo "  mcp scenarios done"
