#!/usr/bin/env bash
# Deferred collectors (docs/design-record-and-seal.md, phase A) and permissions that follow the
# profiles (docs/design-permissions-by-profile.md), proven on a real cluster:
#   cel         — a profile naming one collector in both `collectors` and `deferred` is refused
#                 by the API server, not discovered per capture
#   perishable  — a profile that intends resources+changes and defers logs+events seals a bundle
#                 that verifies OK, says `(deferred: …)` on the verdict line, carries no logs/ and
#                 no events.json, and the postmortem names the deferred set
#   denied      — with `pods/log` removed from the collector ClusterRole and a full profile, the
#                 bundle is PARTIAL and the capture carries a CollectorDenied Event naming the
#                 check that stopped `logs`
#   not-needed  — the same tightened Role under the perishable profile: OK, no new Event, and
#                 `lapilli_permissions_denied` returns to 0 because no profile needs pods/log
#
# Usage: test/e2e/deferred.sh <path-to-lapilli-binary> <scratch-dir>   (called by run.sh)
set -euo pipefail

LAPILLI=$1
OUT=$2/deferred
KNS=lapilli-system
mkdir -p "$OUT"

step() { echo; echo "==> deferred: $*"; }
fail() { echo "FAIL (deferred): $*"; kubectl -n "$KNS" logs deploy/lapilli --tail=40 || true; exit 1; }

# Everything below mutates the default profile and the collector ClusterRole; both are put back
# exactly, whatever happens, so the suites after this one start from what they expect.
kubectl -n "$KNS" get captureprofile default -o json > "$OUT/profile.before.json"
kubectl get clusterrole lapilli-collector -o json > "$OUT/clusterrole.before.json"
PF_PID=""
restore() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  kubectl apply -f "$OUT/clusterrole.before.json" >/dev/null 2>&1 || true
  kubectl apply -f "$OUT/profile.before.json" >/dev/null 2>&1 || true
  kubectl -n "$KNS" set env deploy/lapilli LAPILLI_PERMS_RECHECK_SECONDS- >/dev/null 2>&1 || true
  kubectl -n "$KNS" rollout status deploy/lapilli --timeout=120s >/dev/null 2>&1 || true
}
trap restore EXIT

# Write the default profile with the given collectors/deferred lists (JSON arrays as strings).
set_profile() { # collectors-json, deferred-json
  python3 - "$OUT/profile.before.json" "$1" "$2" <<'EOF' | kubectl apply -f - >/dev/null
import json, sys
p = json.load(open(sys.argv[1]))
for k in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
    p["metadata"].pop(k, None)
p.pop("status", None)
p["spec"]["collectors"] = json.loads(sys.argv[2])
p["spec"]["deferred"] = json.loads(sys.argv[3])
print(json.dumps(p))
EOF
}

metrics() { curl -sf localhost:18082/metrics; }
wait_metric() { # regex, seconds
  for _ in $(seq 1 "$2"); do metrics 2>/dev/null | grep -qE "$1" && return 0; sleep 1; done
  echo "--- /metrics did not show: $1"; metrics | grep -E "^lapilli_permission|^lapilli_deferred" || true
  return 1
}
kubectl -n "$KNS" port-forward deploy/lapilli 18082:8081 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do metrics >/dev/null 2>&1 && break; sleep 1; done

step "cel: a collector in both lists is refused by the API server"
if set_profile '["logs","resources"]' '["logs"]' 2> "$OUT/cel.err"; then
  fail "the API server accepted a profile with logs in both collectors and deferred"
fi
grep -q "both in" "$OUT/cel.err" || { cat "$OUT/cel.err"; fail "refused, but not by the CEL rule"; }
echo "  ok: refused with the rule's own message"

step "perishable: intend resources+changes(+metrics), defer logs+events"
set_profile '["resources","changes","metrics"]' '["logs","events"]'
"$LAPILLI" demo --scenario crashloop --out "$OUT/perishable" | tee "$OUT/perishable.txt" \
  || fail "demo exited non-zero under the perishable profile (expected OK: nothing failed)"
grep -q "OK  hash_ok=true context_ok=true coverage=100% (deferred: logs,events)" "$OUT/perishable.txt" \
  || fail "verdict line does not carry the deferred set"
DIR=$(find "$OUT/perishable" -mindepth 1 -maxdepth 1 -type d | head -1)
[ -n "$DIR" ] || fail "no unpacked bundle under $OUT/perishable"
[ ! -e "$DIR/logs" ] || fail "a deferred collector wrote logs/"
[ ! -e "$DIR/events.json" ] || fail "a deferred collector wrote events.json"
[ -e "$DIR/resources/pod.json" ] || fail "resources/pod.json missing: the perishable core was not captured"
[ -e "$DIR/diffs/index.json" ] || fail "diffs/index.json missing: the perishable core was not captured"
python3 - "$DIR/manifest.json" <<'EOF' || fail "manifest coverage is not the perishable shape"
import json, sys
c = json.load(open(sys.argv[1]))["coverage"]
assert sorted(c["deferred"]) == ["events", "logs"], c
assert "logs" not in c["collectors_intended"] and "events" not in c["collectors_intended"], c
assert set(c["collectors_run"]) == set(c["collectors_intended"]), c
EOF
"$LAPILLI" verify "$DIR" --output json > "$OUT/perishable.json"
python3 - "$OUT/perishable.json" <<'EOF' || fail "verify-result/v1 does not carry the deferred set"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["verdict"] == "OK" and d["bundle"]["deferred"] == ["logs", "events"], d["bundle"]
assert d["bundle"]["coverage_score"] == 1 and d["bundle"]["partial"] is False, d["bundle"]
assert [p["code"] for p in d["problems"]] == ["notice"], d["problems"]
EOF
"$LAPILLI" postmortem "$DIR" > "$OUT/perishable.md" || fail "postmortem did not render the perishable bundle"
grep -q "| Deferred | logs, events" "$OUT/perishable.md" || fail "postmortem header does not name the deferred set"
grep -q "| Collectors deferred | logs, events |" "$OUT/perishable.md" || fail "postmortem inventory does not name the deferred set"
wait_metric '^lapilli_deferred_captures_total [1-9]' 20 || fail "lapilli_deferred_captures_total did not move"
echo "  ok: OK + (deferred: logs,events); no logs/ or events.json; postmortem and /metrics say so"

step "denied: remove pods/log from the collector ClusterRole; a full profile's capture names the cause"
set_profile '["logs","resources","events","changes","metrics"]' '[]'
kubectl -n "$KNS" set env deploy/lapilli LAPILLI_PERMS_RECHECK_SECONDS=5 >/dev/null
kubectl -n "$KNS" rollout status deploy/lapilli --timeout=120s >/dev/null
# The rollout replaced the pod: re-attach the port-forward.
kill "$PF_PID" 2>/dev/null || true
kubectl -n "$KNS" port-forward deploy/lapilli 18082:8081 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do metrics >/dev/null 2>&1 && break; sleep 1; done
python3 - "$OUT/clusterrole.before.json" <<'EOF' | kubectl apply -f - >/dev/null
import json, sys
r = json.load(open(sys.argv[1]))
for k in ("resourceVersion", "uid", "creationTimestamp", "managedFields"):
    r["metadata"].pop(k, None)
for rule in r["rules"]:
    rule["resources"] = [x for x in rule["resources"] if x != "pods/log"]
print(json.dumps(r))
EOF
wait_metric '^lapilli_permissions_denied 1$' 60 || fail "the self-check never noticed pods/log was removed"
BEFORE_EVENTS=$(kubectl -n "$KNS" get events --field-selector reason=CollectorDenied -o name | wc -l)
set +e
"$LAPILLI" demo --scenario crashloop --out "$OUT/denied" | tee "$OUT/denied.txt"
RC=${PIPESTATUS[0]}
set -e
[ "$RC" = 2 ] || fail "expected PARTIAL (exit 2) with pods/log denied, got exit $RC"
grep -q "^PARTIAL " "$OUT/denied.txt" || fail "verdict is not PARTIAL"
DIR=$(find "$OUT/denied" -mindepth 1 -maxdepth 1 -type d | head -1)
python3 - "$DIR/manifest.json" <<'EOF' || fail "logs should be intended-but-not-run"
import json, sys
c = json.load(open(sys.argv[1]))["coverage"]
assert "logs" in c["collectors_intended"] and "logs" not in c["collectors_run"], c
EOF
# The denial itself is sealed: the index names it, and no log file was written.
grep -q "unavailable" "$DIR/logs/index.json" || fail "logs/index.json does not record the denial"
[ -z "$(ls "$DIR/logs/" | grep -v index.json)" ] || fail "a log file was sealed under a denied read"
for _ in $(seq 1 30); do
  [ "$(kubectl -n "$KNS" get events --field-selector reason=CollectorDenied -o name | wc -l)" -gt "$BEFORE_EVENTS" ] && break
  sleep 1
done
kubectl -n "$KNS" get events --field-selector reason=CollectorDenied -o jsonpath='{.items[*].message}' > "$OUT/denied.events"
grep -q "logs did not run" "$OUT/denied.events" || { cat "$OUT/denied.events"; fail "no CollectorDenied event naming logs"; }
grep -q "pod-logs" "$OUT/denied.events" || fail "the event does not name the pod-logs check"
grep -qE "the permission self-check at [0-9]{4}-" "$OUT/denied.events" || fail "the event does not carry the check's time"
echo "  ok: PARTIAL, denial sealed in logs/index.json, CollectorDenied names pod-logs and when"

step "not-needed: the same tightened Role under the perishable profile"
set_profile '["resources","changes","metrics"]' '["logs","events"]'
wait_metric '^lapilli_permissions_denied 0$' 60 || fail "pods/log still counted as denied although no profile needs it"
wait_metric '^lapilli_permission_checks_total\{result="not_needed"\} [1-9]' 20 || fail "not_needed never counted"
BEFORE_EVENTS=$(kubectl -n "$KNS" get events --field-selector reason=CollectorDenied -o name | wc -l)
"$LAPILLI" demo --scenario crashloop --out "$OUT/notneeded" | tee "$OUT/notneeded.txt" \
  || fail "demo exited non-zero under the perishable profile with pods/log removed (expected OK)"
grep -q "OK  hash_ok=true context_ok=true coverage=100% (deferred: logs,events)" "$OUT/notneeded.txt" \
  || fail "perishable capture under a tightened Role should be OK with the deferred set"
sleep 3
AFTER_EVENTS=$(kubectl -n "$KNS" get events --field-selector reason=CollectorDenied -o name | wc -l)
[ "$AFTER_EVENTS" = "$BEFORE_EVENTS" ] || fail "a CollectorDenied event was published for a collector no profile intends"
echo "  ok: OK, denied=0, not_needed counted, no new CollectorDenied"

step "cleanup"
restore
trap - EXIT
echo "  deferred scenarios done"
