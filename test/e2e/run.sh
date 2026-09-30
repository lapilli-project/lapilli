#!/usr/bin/env bash
# Lapilli kind E2E. `lapilli demo` *is* the harness (DESIGN §8): it stages a bad rollout, fires
# the Alertmanager webhook, waits for export, pulls the .ieb, and verifies it offline. On
# top of that this script asserts the negative paths the integrity claim rests on:
#   - one tampered byte                     -> verify FAILED (non-zero)
#   - signed bundle vs. a different key      -> FAILED (authenticity comes from --key only)
#   - unsigned bundle where --key is given   -> FAILED
#   - wrong incident context                 -> FAILED (fail-closed), with the in-cluster binary
#
# Usage: test/e2e/run.sh            (creates & tears down a kind cluster)
#        KEEP=1 test/e2e/run.sh     (leave the cluster up for debugging)
#        SKIP="diffs export kms" test/e2e/run.sh
#            skip named sub-suites while iterating on another one. Development only: the
#            release gate runs with SKIP unset, and the script says loudly what it skipped so
#            a green run with SKIP set cannot be mistaken for a full one.
#            NOTE: the later steps are not independent of the skipped ones — the
#            `incident-id-in-use` check below reuses an incident id that export.sh created, so
#            `SKIP=export` makes it fail. That is the switch telling the truth, not a bug: a
#            SKIP run proves only the suites it ran.
set -euo pipefail

ctrl_pod() { # the controller pod that is not terminating
  kubectl -n "$1" get pods -l app.kubernetes.io/name=lapilli \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | awk 'NR==1'
}

CLUSTER=lapilli
IMAGE=lapilli-controller:dev
# Pinned by digest (round-3 requirement): the node image kind v0.33.0 defaults to.
# Override with NODE_IMAGE=… (the release gate runs the oldest tested minor too).
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}
NS=lapilli-system
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

OUT="$(mktemp -d)"
# Pin kubectl and helm to THIS kind cluster, in a kubeconfig of our own, for the whole run —
# sub-scripts inherit it. Without this every call goes to whatever context happens to be current:
# on a laptop with production clusters in ~/.kube/config that is a real cluster, and one run did
# send its commands at a GKE cluster when the current context changed underneath it. It was
# refused there for lack of permission, which is luck, not a safeguard. This also leaves the
# user's own kubeconfig untouched.
export KUBECONFIG="$OUT/kubeconfig"
cleanup() {
  rm -rf "$OUT"
  if [ "${KEEP:-0}" = "1" ]; then
    # The kubeconfig lives in $OUT, which was just removed: say how to reach the kept cluster.
    echo "cluster kept; to use it: kind export kubeconfig --name $CLUSTER"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

step() { echo; echo "==> $*"; }
fail() { echo "FAIL: $*"; kubectl -n "$NS" logs deploy/lapilli --tail=50 || true; exit 1; }

step "build lapilli CLI (host)"
cargo build -q -p lapilli
LAPILLI="$ROOT/target/debug/lapilli"

step "create kind cluster"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s \
  --kubeconfig "$KUBECONFIG"
# Refuse to go on if we are not pointed at the cluster we just made: every later step mutates
# whatever this resolves to.
ctx=$(kubectl config current-context)
[ "$ctx" = "kind-$CLUSTER" ] || { echo "kubectl points at $ctx, not kind-$CLUSTER"; exit 1; }

step "build + load controller image (SHA-independent :dev tag, IfNotPresent)"
docker build -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"

step "minimal Prometheus (cAdvisor via the API-server node proxy, 5s scrape)"
kind load docker-image prom/prometheus:v3.5.0 --name "$CLUSTER" 2>/dev/null || true
kubectl apply -f test/e2e/prometheus.yaml
kubectl -n monitoring rollout status deploy/prometheus --timeout=180s

step "helm install (the same chart users install; local image, PVC on kind's default StorageClass)"
helm install lapilli charts/lapilli -n "$NS" --create-namespace \
  --set image.repository=lapilli-controller --set image.tag=dev \
  --set clusterId=kind-lapilli \
  --set metrics.prometheusUrl=http://prometheus.monitoring.svc:9090 --set metrics.stepSeconds=5 \
  --wait --timeout 180s

step "every chart render survives the API server's STRICT decoding"
# Server-side, not client-side: only the real API server reports a field it would PRUNE, and a
# pruned field is how a NetworkPolicy written as a tight allowlist became allow-all while lint,
# install and a values review all passed. The CRDs are installed by now, so CaptureProfile
# validates too. Runs once per tested Kubernetes minor, which also catches version-specific fields.
strict_render() { # description, extra helm args…
  local what=$1; shift
  helm template lapilli charts/lapilli "$@" \
    | kubectl apply --dry-run=server --validate=strict -f - >/dev/null \
    || fail "strict decoding rejected the render: $what"
}
strict_render defaults
strict_render "static signing" --set signing.mode=static --set signing.keySecret=k
strict_render "webhook NetworkPolicy" --set webhook.networkPolicy.enabled=true \
  --set-json 'webhook.networkPolicy.from=[{"podSelector":{}}]'
strict_render "notification route" \
  --set-json 'notify.routes=[{"name":"platform","host":"hooks.slack.com","pathSecret":"s"}]' \
  --set profile.notifyRoute=platform
# Conditional, and it says so when it skips: `strict_render` exits the script on failure, so a
# trailing `|| echo` would never run, and a silent skip is how a check stops meaning anything.
if kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
  strict_render "telemetry ServiceMonitor" --set telemetry.serviceMonitor.enabled=true
else
  echo "  note: ServiceMonitor render NOT strict-checked — the Prometheus operator CRD is absent"
fi
echo "  ok: every render decodes strictly against this API server"

step "webhook authentication: no token and a wrong token are both rejected"
BEFORE=$(kubectl -n "$NS" get incidentcaptures --no-headers 2>/dev/null | wc -l)
if echo '{"alerts":[{"status":"firing","labels":{"alertname":"NoToken","namespace":"default","pod":"x"}}]}' \
    | kubectl create --raw "/api/v1/namespaces/$NS/services/lapilli-webhook:webhook/proxy/webhook" -f - >/dev/null 2>&1; then
  fail "the webhook accepted a request without a token"
fi
CTRL=$(ctrl_pod "$NS")
if echo '{"alerts":[{"status":"firing","labels":{"alertname":"WrongToken","namespace":"default","pod":"x"}}]}' \
    | kubectl -n "$NS" exec -i "$CTRL" -c controller -- /usr/local/bin/lapilli post-alert --wrong-token >/dev/null 2>&1; then
  fail "the webhook accepted a wrong token"
fi
[ "$(kubectl -n "$NS" get incidentcaptures --no-headers 2>/dev/null | wc -l)" = "$BEFORE" ] \
  || fail "a rejected request created a capture"
echo "  ok: 401 without a token and with a wrong one; no capture created"

step "lapilli demo --scenario crashloop"
"$LAPILLI" demo --scenario crashloop --out "$OUT/crashloop" | tee "$OUT/crashloop.txt" \
  || fail "demo crashloop exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/crashloop.txt" || fail "crashloop bundle not OK/100%"
grep -q "FATAL: cache warmup failed" "$OUT/crashloop.txt" || fail "previous-instance logs not recovered"
grep -q "revision 1 → 2, [0-9]*s before the alert, by demo-deployer" "$OUT/crashloop.txt" \
  || fail "diff headline (revision 1 → 2, before the alert, by demo-deployer) missing"
grep -q "env\[name=CACHE_WARMUP\].value: lazy → eager" "$OUT/crashloop.txt" \
  || fail "diff line CACHE_WARMUP: lazy → eager missing"

step "lapilli demo --scenario oomkill"
"$LAPILLI" demo --scenario oomkill --out "$OUT/oomkill" | tee "$OUT/oomkill.txt" \
  || fail "demo oomkill exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/oomkill.txt" || fail "oomkill bundle not OK/100%"
grep -q "OOMKilled (exit 137)" "$OUT/oomkill.txt" || fail "OOMKilled termination not in bundle"
grep -q "memory (metrics/):.*MiB of 64 MiB limit" "$OUT/oomkill.txt" || fail "memory curve not captured from Prometheus"

step "redaction: planted credentials never reach a bundle; useful values stay"
CANARY=lapilliDemoCanary7Qx2Lp9w
if grep -rl "$CANARY" "$OUT/crashloop" "$OUT/oomkill"; then fail "canary credential leaked into a bundle"; fi
grep -rq '"eager"' "$OUT/crashloop"/*/resources/ || fail "CACHE_WARMUP value was over-redacted"
grep -q '"mode": "default"' "$OUT"/crashloop/*/redaction.json || fail "redaction.json missing or wrong mode"
echo "  canary absent from every file; CACHE_WARMUP still readable"

suite() { # name, description, command…
  local name=$1 desc=$2; shift 2
  case " ${SKIP:-} " in
    *" $name "*) echo; echo "==> SKIPPED (SKIP=$SKIP): $desc"; return 0 ;;
  esac
  step "$desc"
  "$@" || fail "$name scenarios"
}

suite diffs "diffs/ scenarios: rollback, scale canary, paused, recreate" \
  test/e2e/diffs.sh "$LAPILLI" "$OUT"

suite export "object-store export: LocalStack S3 with object lock" \
  test/e2e/export.sh "$LAPILLI"

suite kms "KMS signing: LocalStack KMS, key fetch, outage + restart" \
  test/e2e/kms.sh "$LAPILLI"

suite notify "notification: receiver pod, grouping, no workload content, failure path" \
  test/e2e/notify.sh "$LAPILLI"

suite deferred "deferred collectors: perishable profile, CEL refusal, a denied read named at the capture" \
  test/e2e/deferred.sh "$LAPILLI" "$OUT"

suite mcp "lapilli mcp in the controller pod: token, find by alert, object body and diff, logs refused" \
  test/e2e/mcp.sh "$LAPILLI" "$OUT"

step "negative: tamper one byte in an unpacked bundle (expect FAILED, exit 1)"
BUNDLE_DIR=$(find "$OUT/crashloop" -mindepth 1 -maxdepth 1 -type d | head -1)
# changes.json is in every bundle; a log file may be absent (kubelet GC), and appending to a
# missing file would test "extra file" instead of "modified byte".
printf 'x' >> "$BUNDLE_DIR/changes.json"
if "$LAPILLI" verify "$BUNDLE_DIR"; then fail "verify accepted a tampered bundle"; fi
echo "  correctly rejected the tampered bundle"

step "signing: lapilli keygen -> Secret -> helm upgrade signing.mode=static"
"$LAPILLI" keygen --out-dir "$OUT/keys"
kubectl -n "$NS" create secret generic lapilli-signing-key --from-file=key.pem="$OUT/keys/lapilli.key"
helm upgrade lapilli charts/lapilli -n "$NS" --reuse-values \
  --set signing.mode=static --set signing.keySecret=lapilli-signing-key --wait --timeout 120s

step "lapilli demo --key (expect a bundle signed by the trusted key)"
"$LAPILLI" demo --scenario crashloop --key "$OUT/keys/lapilli.pub" --out "$OUT/signed" | tee "$OUT/signed.txt" \
  || fail "signed demo exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100% signed:trusted-key" "$OUT/signed.txt" \
  || fail "bundle not signed by the trusted key"

step "negative: signed bundle checked against a different key (expect FAILED)"
"$LAPILLI" keygen --out-dir "$OUT/other-keys" >/dev/null
SIGNED_IEB=$(find "$OUT/signed" -maxdepth 1 -name '*.ieb' | head -1)
if "$LAPILLI" verify "$SIGNED_IEB" --key "$OUT/other-keys/lapilli.pub"; then fail "accepted a bundle signed by another key"; fi
echo "  correctly rejected: not signed by the trusted key"

step "negative: unsigned bundle checked with --key (expect FAILED)"
UNSIGNED_IEB=$(find "$OUT/oomkill" -maxdepth 1 -name '*.ieb' | head -1)
if "$LAPILLI" verify "$UNSIGNED_IEB" --key "$OUT/keys/lapilli.pub"; then fail "accepted an unsigned bundle under --key"; fi
echo "  correctly rejected: unsigned bundle where a signature was required"

step "postmortem: a real capture renders a draft whose facts came from the bundle"
# The fixtures are minimal — one collector, no timeline, no pod.json — so the sections that carry
# the incident never run against them. This is a real crashloop capture: it has a termination, a
# restart count, events and a rollout.
PM=$(mktemp); "$LAPILLI" postmortem "$UNSIGNED_IEB" > "$PM" || fail "postmortem failed on a verified bundle"
pm() { grep -qE "$1" "$PM" || { echo "--- rendered draft ---"; cat "$PM"; fail "$2"; }; }
pm '^\| Verdict \| \*\*(OK|PARTIAL)\*\* \|' "the verdict is not the first thing in the table"
pm '^\| Bundle SHA-256 \| `[0-9a-f]{64}` \|' "no digest, so a reader cannot confirm they hold these bytes"
pm '^lapilli verify ' "no reproducing command"
pm '^## (Impact|Root cause)' "the headings a human must fill are missing"
# The digest has to be this file's, or the self-checkable claim is decoration.
grep -qF "$(shasum -a 256 "$UNSIGNED_IEB" | cut -d' ' -f1)" "$PM" \
  || fail "the rendered SHA-256 is not this bundle's"
# The last log line is workload content and must stay out unless asked for.
CANARY=$(grep -oE 'lapilliDemo[A-Za-z0-9]+' "$PM" | head -1 || true)
[ -z "$CANARY" ] || fail "postmortem leaked workload content by default: $CANARY"
echo "  ok: draft renders, digest matches the file, and the log line stays out by default"

step "postmortem: a bundle that cannot be read still reports the verdict, not a different one"
# `unpack` refuses a traversal entry; the rendering path used to die with it and exit 3 while
# `lapilli verify` said FAILED and exited 1. Two commands contradicting each other on the same
# bytes is worse than either being wrong alone.
TRAV="$ROOT/test/fixtures/ieb/v0.1.0/fail-traversal.ieb"
# `cmd; RC=$?` does not survive `set -e`: the non-zero exit — which is the whole point here —
# kills the script before the assignment runs. Both of these are EXPECTED to fail.
VC=0; "$LAPILLI" verify "$TRAV" >/dev/null 2>&1 || VC=$?
PC=0; "$LAPILLI" postmortem "$TRAV" >/dev/null 2>&1 || PC=$?
[ "$VC" = "$PC" ] || fail "verify exited $VC but postmortem exited $PC on the same bundle"
echo "  ok: both exited $VC"
rm -f "$PM"

step "bundles survive a controller restart (PVC, not emptyDir)"
kubectl -n "$NS" rollout restart deploy/lapilli
kubectl -n "$NS" rollout status deploy/lapilli --timeout=120s

step "in-cluster lapilli verify (distroless binary) — happy path + wrong context"
# The crashloop demo's capture, asserted OK/100% above — not `items[0]`: the deferred suite
# leaves a capture that is PARTIAL on purpose (pods/log denied), and which name sorts first
# differs between clusters. The 1.30 release gate picked that one once (2026-09-26).
IC=$(grep -o 'IncidentCapture ic-[0-9a-f]*' "$OUT/crashloop.txt" | head -1 | cut -d' ' -f2)
[ -n "$IC" ] || fail "the crashloop demo did not report its IncidentCapture"
BUNDLE=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.bundlePath}')
CID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.clusterId}')
IID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.incidentId}')
POD=$(ctrl_pod "$NS")
kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/lapilli verify "$BUNDLE" --cluster "$CID" --incident "$IID" \
  || fail "in-cluster verify rejected a good bundle (lost across the restart?)"
if kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/lapilli verify "$BUNDLE" --incident WRONG; then
  fail "verify accepted wrong incident (not fail-closed)"
fi
echo "  correctly fail-closed on wrong incident"

step "metrics: /metrics serves the documented series"
# Counters are per-process and earlier steps restarted the pod (helm upgrades, the restart
# check), so make the traffic this pod should count: one capture and one rejected webhook
# request.
echo '{"alerts":[]}' | kubectl -n "$NS" exec -i "$POD" -c controller -- \
  /usr/local/bin/lapilli post-alert --wrong-token >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: metrics-ok, namespace: $NS }
spec:
  profile: default
  incidentId: metrics-e2e-ok
  clusterId: "$CID"
  trigger: { rule: MetricsE2E, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: $NS, pod: $POD }
EOF
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get incidentcapture metrics-ok -o jsonpath='{.status.phase}')" = Exported ] && break
  sleep 2
done
[ "$(kubectl -n "$NS" get incidentcapture metrics-ok -o jsonpath='{.status.phase}')" = Exported ] \
  || fail "the capture for the metrics check never exported"
kubectl -n "$NS" port-forward deploy/lapilli 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
# The state-derived gauges appear after the first poll (30 s).
for _ in $(seq 1 60); do
  grep -q '^lapilli_captures{' <<<"$(curl -sf localhost:18081/metrics 2>/dev/null)" && break; sleep 2
done
METRICS=$(curl -sf localhost:18081/metrics) || fail "/metrics is not served"
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
# Every series docs/metrics.md documents (except the KMS key, checked in kms.sh).
for series in \
  'lapilli_build_info{version=' \
  'lapilli_captures_total{result="sealed"}' \
  'lapilli_captures_total{result="refused"}' \
  'lapilli_captures_total{result="failed"}' \
  'lapilli_partial_captures_total' \
  'lapilli_collector_failures_total' \
  'lapilli_capture_seconds_bucket{le="+Inf"}' \
  'lapilli_bundle_bytes_bucket{le="1073741824"}' \
  'lapilli_bundle_bytes_count' \
  'lapilli_seal_attempts_total{result="ok"}' \
  'lapilli_seal_attempts_total{result="failed"}' \
  'lapilli_seal_pack_failures_total' \
  'lapilli_reconcile_errors_total' \
  'lapilli_captures_watched' \
  'lapilli_captures_retired' \
  'lapilli_captures_exported_unretired' \
  'lapilli_captures_retired_total' \
  'lapilli_export_attempts_total{result="ok"}' \
  'lapilli_export_attempts_total{result="failed"}' \
  'lapilli_export_destinations{state="uploaded"}' \
  'lapilli_exports_unsettled' \
  'lapilli_captures{phase="sealing"}' \
  'lapilli_captures{phase="exported"}' \
  'lapilli_captures_awaiting_seal' \
  'lapilli_webhook_requests_total{result="accepted"}' \
  'lapilli_webhook_requests_total{result="duplicate"}' \
  'lapilli_webhook_requests_total{result="rejected"}' \
  'lapilli_webhook_requests_total{result="error"}' \
  'lapilli_apiserver_poll_ok' \
  'lapilli_apiserver_polls_total{result="ok"}' \
  'lapilli_apiserver_polls_total{result="forbidden"}' \
  'lapilli_apiserver_polls_total{result="unreachable"}' \
  'lapilli_apiserver_last_success_timestamp_seconds' \
  'lapilli_permission_checks_total{result="held"}' \
  'lapilli_permission_checks_total{result="denied"}' \
  'lapilli_permission_checks_total{result="unknown"}' \
  'lapilli_permissions_denied' \
  'lapilli_permissions_unknown' \
  'lapilli_bundle_fs_bytes{state="free"}' \
  'lapilli_bundle_fs_bytes{state="used"}' \
  'lapilli_retention_sweeps_total{result="ok"}'; do
  grep -qF "$series" <<<"$METRICS" || fail "/metrics is missing $series"
done
SEALED=$(echo "$METRICS" | awk -F' ' '/^lapilli_captures_total\{result="sealed"\}/ {print $2}')
[ "${SEALED:-0}" -ge 1 ] || fail "lapilli_captures_total sealed is $SEALED after a capture"
REJECTED=$(echo "$METRICS" | awk -F' ' '/^lapilli_webhook_requests_total\{result="rejected"\}/ {print $2}')
[ "${REJECTED:-0}" -ge 1 ] || fail "the rejected webhook request was not counted ($REJECTED)"
# The gauge has to say 1 here: this controller has plainly been using the API server all suite.
# A 0 would mean the poller is reporting on something else entirely.
REACH=$(echo "$METRICS" | awk -F' ' '/^lapilli_apiserver_poll_ok/ {print $2}')
[ "${REACH:-0}" = "1" ] || fail "lapilli_apiserver_poll_ok is $REACH on a working cluster"

# The conventional process series, cross-checked against a number this controller did not
# produce. A `/proc/self/statm` parse that used a hardcoded 4096-byte page would under-report by
# four on a 16 KiB-page arm64 kernel — which is what a Mac running kind actually is — and a unit
# test on the developer's macOS host cannot catch it, because there is no /proc there to parse.
RSS=$(awk '/^process_resident_memory_bytes /{print $2}' <<<"$METRICS")
[ -n "$RSS" ] || fail "process_resident_memory_bytes is missing; /proc metrics did not render"
[ "$RSS" -gt 8000000 ] || fail "resident memory reads $RSS bytes, which is too small to be real"
LIMIT=268435456   # the chart's 256Mi
[ "$RSS" -lt "$LIMIT" ] || fail "resident memory $RSS is at or over the pod's $LIMIT limit"
# Independent source, in a DIFFERENT UNIT. `/proc/<pid>/status` reports `VmRSS` in kilobytes,
# so it carries no page-size assumption at all — which is the whole point, because the bug worth
# catching is `/proc/self/statm` (in PAGES) parsed with a hardcoded 4096 on a 16 KiB-page arm64
# kernel. That mistake reads four times too small and nothing on a macOS host can see it.
#
# An earlier version of this compared against the container's cgroup `memory.current` on the
# theory that it must be >= RSS. It is not: `statm`'s resident count includes shared file-backed
# pages charged to whichever cgroup faulted them in first, so the controller legitimately
# reported 16.7 MiB against a 6.4 MiB cgroup charge. The assumption was wrong, not the parse —
# and a ratio that far from 1 could not have caught a 4x error anyway.
# NOT `CID`: that name already holds the cluster id earlier in this script, and clobbering it
# made the `ref-traversal` refusal fail as `cluster-mismatch` instead of `invalid-incident-id` —
# the step still saw "refused", just for the wrong reason.
CTR_ID=$(kubectl -n "$NS" get pod "$POD" \
  -o jsonpath='{.status.containerStatuses[?(@.name=="controller")].containerID}' | sed 's|.*/||')
NODE=$(kind get nodes --name "$CLUSTER" | awk 'NR==1')
CGDIR=$(docker exec "$NODE" sh -c \
  "find /sys/fs/cgroup -name cgroup.procs -path \"*${CTR_ID}*\" 2>/dev/null | head -1" || true)
VMRSS_KB=""
if [ -n "$CGDIR" ]; then
  VMRSS_KB=$(docker exec "$NODE" sh -c \
    "p=\$(head -1 \"$CGDIR\"); [ -n \"\$p\" ] && awk '/^VmRSS:/{print \$2}' /proc/\$p/status" \
    2>/dev/null || true)
fi
if [ -n "$VMRSS_KB" ]; then
  EXPECT=$((VMRSS_KB * 1024))
  # Sampled a moment apart, so allow drift — but a page-size error is 4x, far outside this band.
  LO=$((EXPECT / 2)); HI=$((EXPECT * 2))
  [ "$RSS" -ge "$LO" ] && [ "$RSS" -le "$HI" ] \
    || fail "reported RSS ${RSS}B is outside [${LO},${HI}] around the node's VmRSS ${EXPECT}B — the /proc/self/statm parse is wrong (page size?)"
  echo "  ok: RSS ${RSS}B agrees with the node's VmRSS ${EXPECT}B (independent, in kB)"
else
  echo "  NOTE: the controller's VmRSS was NOT readable from the node, so the independent"
  echo "        cross-check DID NOT RUN. RSS was checked for plausibility only — a 4x page-size"
  echo "        error would still have passed this step."
fi
grep -q '^process_cpu_seconds_total ' <<<"$METRICS" || fail "process_cpu_seconds_total is missing"
grep -q '^process_open_fds ' <<<"$METRICS" || fail "process_open_fds is missing"
LAST_OK=$(echo "$METRICS" | awk -F' ' '/^lapilli_apiserver_last_success_timestamp_seconds/ {print $2}')
# Read "now" from inside the cluster, not from the host: on a laptop the Docker VM's clock drifts
# from the host across sleep, which would fail this assertion for a reason that has nothing to do
# with the metric. The node, not the pod — the controller image is distroless and has no `date`,
# and the node shares its kernel clock with every container on it.
NOW=$(docker exec "$(kind get nodes --name "$CLUSTER" | head -1)" date +%s)
AGE=$(( NOW - ${LAST_OK:-0} ))
# One poll interval is 30 s; allow two plus the scrape, and refuse a timestamp from the future.
[ "$AGE" -ge 0 ] && [ "$AGE" -le 75 ] \
  || fail "the last API-server success is ${AGE}s old, which no 30s poller should report"
POLLS_OK=$(echo "$METRICS" | awk -F' ' '/^lapilli_apiserver_polls_total\{result="ok"\}/ {print $2}')
[ "${POLLS_OK:-0}" -ge 1 ] || fail "no successful API-server poll was counted ($POLLS_OK)"
BYTES=$(echo "$METRICS" | awk -F' ' '/^lapilli_bundle_bytes_sum/ {print $2}')
[ "${BYTES:-0}" -gt 1000 ] || fail "lapilli_bundle_bytes_sum looks wrong ($BYTES)"
grep -qE '^lapilli_bundle_bytes_bucket\{le="1048576"\} [1-9]' <<<"$METRICS" \
  || fail "bundle sizes are not landing in the byte buckets"
echo "  ok: sealed=$SEALED, rejected webhook=$REJECTED, bundle bytes bucketed, all series present"
echo "  ok: the API server reads as reachable, last seen ${AGE}s ago over $POLLS_OK polls"
# Every permission check must read 1 on a chart install that has not been tampered with. A 0 here
# means the chart's RBAC and the controller's idea of what it needs have drifted apart — which is
# the whole reason the check exists, and it would otherwise be found by a bundle coming out empty.
DENIED=$(echo "$METRICS" | awk -F' ' '/^lapilli_permissions_denied/ {print $2}')
UNKNOWN=$(echo "$METRICS" | awk -F' ' '/^lapilli_permissions_unknown/ {print $2}')
[ "${DENIED:-1}" = "0" ] || fail "a default chart install reports $DENIED missing permission(s); see the controller log"
[ "${UNKNOWN:-1}" = "0" ] || fail "$UNKNOWN permission checks could not be answered on a healthy cluster"
HELD=$(echo "$METRICS" | awk -F' ' '/^lapilli_permission_checks_total\{result="held"\}/ {print $2}')
# Twelve on a bare default install; the export suite has already added a credentials Secret by now,
# which adds its own check, so this run sees thirteen.
[ "${HELD:-0}" -ge 12 ] || fail "only $HELD permission checks were held; the self-check did not run"
echo "  ok: $HELD permission checks held, 0 denied, 0 unanswerable on a default install"
# The bundle volume, from statvfs on the always-on poller. This must be present on a DEFAULT install
# — retention is off there, and that is exactly the install whose disk fills.
FREE=$(echo "$METRICS" | awk -F' ' '/^lapilli_bundle_fs_bytes\{state="free"\}/ {print $2}')
USED=$(echo "$METRICS" | awk -F' ' '/^lapilli_bundle_fs_bytes\{state="used"\}/ {print $2}')
[ "${FREE:-0}" -gt 0 ] || fail "lapilli_bundle_fs_bytes free is $FREE; statvfs of the bundle root failed"
[ "${USED:-0}" -gt 0 ] || fail "lapilli_bundle_fs_bytes used is $USED"
# Retention's BOUNDS are off by default, and the property that matters is that nothing which is
# evidence is reclaimed. This used to assert `lapilli_retention_sweeps_total{result="ok"} == 0`,
# which was a proxy: the sweep did not run at all, so a zero counter and "nothing was reclaimed"
# were the same observation. They are not the same property, and the proxy froze the weaker one —
# the same way the `captureprofiles` RBAC assertion once froze "get only" in place of "no write".
#
# The sweep now always runs, because two of its reasons need no policy: work abandoned by a capture
# that is no longer live, and a notification hand-off no dispatcher can still send (that one carries
# workload content, so it is not kept forever). So assert the reason, not the sweep: with no bound
# set, no bundle may be taken for the byte ceiling, for age, or for a missing IncidentCapture.
for r in max-bytes age orphan; do
  n=$(echo "$METRICS" | awk -F' ' -v r="$r" '$0 ~ "^lapilli_bundles_reclaimed_total\\{reason=\""r"\"\\}" {print $2}')
  [ "${n:-0}" = "0" ] \
    || fail "retention reclaimed $n bundle(s) for reason=$r on a default install, where no bound is set"
done
# The companion half — that a real bundle survives a sweep — is asserted in the retention section
# below, which has the `retention-probe` pod that can read the volume. `probe` does not exist yet
# here, and neither does that pod.
grep -q '^lapilli_bundles_reclaimed_total' <<<"$METRICS" \
  && fail "nothing may be reclaimed on a default install"
echo "  ok: the bundle volume is measured with retention off (free=${FREE}B), and nothing was reclaimed"

step "negative: a controller that cannot use the API server says so, and is NOT restarted"
# The whole point of these series, and until now only the happy path was checked — a hardcoded
# "the poll succeeded" would have passed the entire suite. Revoke the poller's own permission:
# that is a 403, which must read as `forbidden` (the API server answered) and NOT `unreachable`,
# because those two send an operator to completely different places.
# Specifically the namespace Role that grants `lapilli.dev` verbs — NOT the `-collector` ClusterRole,
# which the poller does not use. Revoking the wrong one would make this step assert nothing.
ROLE=lapilli
# Capture the RULES ONLY, and put them back with a merge patch. A `get -o yaml` backup plus
# `kubectl apply` does NOT work here: the YAML carries metadata.resourceVersion, which the API
# server treats as an optimistic-concurrency precondition, so re-applying after the revoke fails
# with `Operation cannot be fulfilled … the object has been modified`. The first version of this
# step did that and swallowed the error with `|| true`, which is how a restore silently failed and
# took the recovery assertion with it.
SAVED_RULES=$(kubectl -n "$NS" get "role/$ROLE" -o jsonpath='{.rules}') \
  || fail "cannot read role/$ROLE to revoke it"
case "$SAVED_RULES" in *incidentcaptures*) ;; *)
  fail "role/$ROLE does not grant incidentcaptures; this step would prove nothing" ;; esac
restore_role() { kubectl -n "$NS" patch "role/$ROLE" --type merge -p "{\"rules\":$SAVED_RULES}" >/dev/null; }
trap 'restore_role || true; cleanup' EXIT
# Drop every rule. The poller's list is refused from the next poll onwards.
kubectl -n "$NS" patch "role/$ROLE" --type merge -p '{"rules":[]}' >/dev/null
RESTARTS_BEFORE=$(kubectl -n "$NS" get pod "$(ctrl_pod "$NS")" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
kubectl -n "$NS" port-forward deploy/lapilli 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
# Two poll intervals plus slack: the gauge holds its previous value until the next poll returns,
# which is exactly why docs/egress.md tells operators to wait before believing it.
BLIND=""
for _ in $(seq 1 24); do
  M2=$(curl -sf localhost:18081/metrics || true)
  if grep -q '^lapilli_apiserver_poll_ok 0$' <<<"$M2"; then BLIND=$M2; break; fi
  sleep 5
done
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ -n "$BLIND" ] || fail "the poller never reported lapilli_apiserver_poll_ok 0 after its RBAC was revoked"
grep -qE '^lapilli_apiserver_polls_total\{result="forbidden"\} [1-9]' <<<"$BLIND" \
  || fail "a 403 must be counted as result=forbidden: $(echo "$BLIND" | grep '^lapilli_apiserver_polls_total')"
grep -q '^lapilli_apiserver_polls_total{result="unreachable"} 0$' <<<"$BLIND" \
  || fail "a 403 was miscounted as unreachable, which sends an operator to the network"
# The last-success timestamp must survive the outage: it is how long the controller has been blind.
grep -q '^lapilli_apiserver_last_success_timestamp_seconds ' <<<"$BLIND" \
  || fail "a failed poll erased the last-success timestamp"
# And the premise: the pod is still Ready, unrestarted, with /healthz answering ok. This is what
# makes the metric necessary rather than a duplicate of pod status.
kubectl -n "$NS" wait --for=condition=Ready "pod/$(ctrl_pod "$NS")" --timeout=30s >/dev/null \
  || fail "the controller pod went unready; /healthz must not depend on the API server"
RESTARTS_AFTER=$(kubectl -n "$NS" get pod "$(ctrl_pod "$NS")" \
  -o jsonpath='{.status.containerStatuses[0].restartCount}')
[ "$RESTARTS_AFTER" = "$RESTARTS_BEFORE" ] \
  || fail "the blind controller was restarted ($RESTARTS_BEFORE -> $RESTARTS_AFTER); restarting cannot fix RBAC"
echo "  ok: poll_ok=0, counted as forbidden (not unreachable), pod still Ready with $RESTARTS_AFTER restarts"

# And the twin hole: an authenticated alert that the API server will not let become a capture. The
# rules are still revoked, so `create` is refused. This used to land in NO bucket — the caller got a
# 500 and every series stayed flat while capture was impossible.
echo '{"alerts":[{"status":"firing","labels":{"alertname":"BlindE2E","namespace":"default","pod":"x"}}]}' \
  | kubectl -n "$NS" exec -i "$(ctrl_pod "$NS")" -c controller -- \
      /usr/local/bin/lapilli post-alert >/dev/null 2>&1 || true
kubectl -n "$NS" port-forward deploy/lapilli 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
ERRS=$(curl -sf localhost:18081/metrics | awk -F' ' '/^lapilli_webhook_requests_total\{result="error"\}/ {print $2}')
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ "${ERRS:-0}" -ge 1 ] \
  || fail "an alert the API server refused to record was counted nowhere (result=error is $ERRS)"
echo "  ok: the refused capture was counted as webhook result=error, not swallowed"

# The permission self-check must see the same revocation. It re-runs every 10 minutes, which is too
# long to wait here, so restart the pod: the check runs at startup. A fresh pod with the Role
# emptied must report create-captures and patch-capture-status as 0 while the collector checks stay
# 1 — the ClusterRole was never touched, and a check that went to 0 for everything would be
# reporting "something is wrong" rather than what.
kubectl -n "$NS" rollout restart deploy/lapilli >/dev/null
kubectl -n "$NS" rollout status deploy/lapilli --timeout=120s >/dev/null
kubectl -n "$NS" port-forward deploy/lapilli 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
PERMS=""
for _ in $(seq 1 24); do
  P=$(curl -sf localhost:18081/metrics || true)
  if grep -qE '^lapilli_permissions_denied [1-9]' <<<"$P"; then PERMS=$P; break; fi
  sleep 5
done
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ -n "$PERMS" ] \
  || fail "the permission self-check did not report a denial after the Role was emptied"
DEN=$(echo "$PERMS" | awk -F' ' '/^lapilli_permissions_denied/ {print $2}')
# Exactly the four checks that Role backs: captures, capture-status, profile (apiGroup lapilli.dev)
# and recorded-events (events.k8s.io). The collector ClusterRole was never touched, so its eight
# checks must still be held — a count that swallowed everything would be reporting "something is
# wrong" rather than how much.
[ "$DEN" = "4" ] \
  || fail "expected 4 denied checks (the emptied Role backs lapilli.dev and events.k8s.io), got $DEN"
[ "$(echo "$PERMS" | awk -F' ' '/^lapilli_permissions_unknown/ {print $2}')" = "0" ] \
  || fail "a denial must not read as unanswerable"
[ "$(echo "$PERMS" | awk -F' ' '/^lapilli_permission_checks_total\{result="held"\}/ {print $2}')" -ge 8 ] \
  || fail "the untouched collector ClusterRole's checks must still be held"
# And the log names them, which is where the detail deliberately lives — not on this endpoint.
grep -q "missing permission" <<<"$(kubectl -n "$NS" logs deploy/lapilli --tail=300)" \
  || fail "the log must name each missing permission; that is where the detail lives"
# The controller logs without ANSI on purpose (main.rs): colour codes wrap every field name and make
# `kubectl logs | grep` useless, which would defeat the decision to keep this detail in the log
# rather than on the unauthenticated metrics endpoint.
# tracing quotes string field values, so the line reads `check="captures"`. Matched exactly, quotes
# included: `check=captures` matches nothing, which is how the first version of this step failed.
for want in captures capture-status profile recorded-events; do
  grep -q "check=\"$want\"" <<<"$(kubectl -n "$NS" logs deploy/lapilli --tail=500)" \
    || fail "the log does not name the $want check in a greppable form"
done
echo "  ok: $DEN denied, 0 unanswerable, and the log names each one"

step "negative: … and it recovers when the permission comes back"
restore_role || fail "could not restore role/$ROLE, so the recovery assertion would prove nothing"
[ "$(kubectl -n "$NS" get "role/$ROLE" -o jsonpath='{.rules}')" = "$SAVED_RULES" ] \
  || fail "role/$ROLE was not restored to its original rules"
kubectl -n "$NS" port-forward deploy/lapilli 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
BACK=""
for _ in $(seq 1 24); do
  M3=$(curl -sf localhost:18081/metrics || true)
  if grep -q '^lapilli_apiserver_poll_ok 1$' <<<"$M3"; then BACK=$M3; break; fi
  sleep 5
done
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ -n "$BACK" ] || fail "the poller never recovered to lapilli_apiserver_poll_ok 1 after RBAC was restored"
trap cleanup EXIT
echo "  ok: back to 1 without a restart — the gauge tracks the fault, not the process"

step "retention: abandoned staging is reclaimed; a claim file and an archived key never are"
# The controller image is distroless, so file surgery goes through a helper pod sharing the PVC.
# kind is one node, so a second pod can mount the same ReadWriteOnce volume.
PVC=$(kubectl -n "$NS" get pvc -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: retention-probe, namespace: $NS }
spec:
  containers:
    - name: sh
      image: busybox:1.37
      command: ["sleep", "3600"]
      volumeMounts: [{ name: bundles, mountPath: /b }]
  volumes:
    - name: bundles
      persistentVolumeClaim: { claimName: $PVC }
EOF
kubectl -n "$NS" wait --for=condition=Ready pod/retention-probe --timeout=120s >/dev/null
probe() { kubectl -n "$NS" exec retention-probe -- sh -c "$1"; }
# Plant: an abandoned staging directory whose uid no capture owns, both claim files, and an archived
# signing key. Only the first may disappear.
# chown to the controller's uid: the probe runs as root, and `remove_dir_all` needs write on the
# directory it is emptying, so a root-owned staging dir would fail for a permission reason that has
# nothing to do with the logic. The controller creates its own staging dirs as 65532.
probe 'mkdir -p /b/.staging-plant-deadbeefuid /b/keys &&
       head -c 200000 /dev/zero > /b/.staging-plant-deadbeefuid/logs.txt &&
       : > /b/plant.notified && : > /b/plant.ieb.owner &&
       : > /b/keys/0000000000000000000000000000000000000000000000000000000000000000.pub &&
       chown -R 65532:65532 /b/.staging-plant-deadbeefuid' >/dev/null

# Retention on, but deliberately with bounds no real bundle can meet: a ten-year window and no byte
# ceiling. Abandoned staging is reclaimed regardless of both bounds, which is the whole point of that
# rule — and this suite's own bundles must survive, which the assertions below check.
#
# An earlier version of this step used maxBytes=1 to force the ceiling, and retention correctly
# reclaimed the suite's real bundle, breaking the step after it. The feature was right; the test was
# greedy. The byte ceiling is covered by the unit tests instead.
helm upgrade lapilli charts/lapilli -n "$NS" --reuse-values \
  --set retention.maxBytes=0 --set retention.days=3650 >/dev/null
kubectl -n "$NS" rollout status deploy/lapilli --timeout=180s >/dev/null
POD=$(ctrl_pod "$NS"); CTRL=$POD

LEFT=""
for _ in $(seq 1 30); do
  L=$(probe 'ls -1a /b' 2>/dev/null || true)
  if ! grep -qx '.staging-plant-deadbeefuid' <<<"$L"; then LEFT=$L; break; fi
  sleep 5
done
[ -n "$LEFT" ] || fail "the abandoned staging directory was never reclaimed:
$(probe 'ls -1a /b' || true)
$(kubectl -n "$NS" logs deploy/lapilli --tail=20 | grep -i reclaim || true)"
grep -qx 'plant.notified' <<<"$LEFT" \
  || fail "the notification CLAIM was reclaimed; that re-announces old incidents (design-notify.md)"
grep -qx 'plant.ieb.owner' <<<"$LEFT" \
  || fail "the incident-id claim was reclaimed; a resent alert could then rebuild that bundle"
grep -qx '0\{64\}.pub' <<<"$(probe 'ls -1 /b/keys')" \
  || fail "an archived signing key was reclaimed; every bundle it signed becomes unverifiable"
grep -q '"reason":"abandoned"' <<<"$(probe 'cat /b/reclaimed.jsonl')" \
  || fail "the reclaim is not in the journal; an Event expires within the hour and status dies with the CR"
# And a real sealed bundle — the only copy, since this install has no destination — is untouched.
grep -q '\.ieb$' <<<"$(probe 'ls -1 /b/*.ieb')" \
  || fail "retention removed a sealed bundle that is the only copy of its evidence"
grep -q '\.ieb$' <<<"$LEFT" \
  || fail "no sealed bundle survived the sweep"
echo "  ok: staging gone, both claims, the key and the sealed bundles intact, and the journal recorded it"

# Put it back the way it was, so later steps see the shipped defaults.
kubectl -n "$NS" delete pod retention-probe --wait=false >/dev/null
helm upgrade lapilli charts/lapilli -n "$NS" --reuse-values \
  --set retention.maxBytes=0 --set retention.days=0 >/dev/null
kubectl -n "$NS" rollout status deploy/lapilli --timeout=180s >/dev/null
POD=$(ctrl_pod "$NS"); CTRL=$POD

step "negative: captures the controller refuses (another cluster, unsafe id, an id in use)"
BEFORE=$(kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/lapilli cat-bundle "$BUNDLE" | shasum -a 256 | cut -c1-64)
refused() { # name, cluster, incident → prints the Failed message
  kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: $1, namespace: $NS }
spec:
  profile: default
  incidentId: "$3"
  clusterId: "$2"
  trigger: { rule: Refusal, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: $NS, pod: $POD }
EOF
  for _ in $(seq 1 30); do
    [ "$(kubectl -n "$NS" get incidentcapture "$1" -o jsonpath='{.status.phase}')" = Failed ] && break
    sleep 1
  done
  kubectl -n "$NS" get incidentcapture "$1" -o jsonpath='{.status.phase} {.status.message}'
}
grep -q "^Failed cluster-mismatch" <<<"$(refused ref-cluster other-cluster ref-cluster-1)" \
  || fail "a capture for another cluster was not refused"
grep -q "^Failed invalid-incident-id" <<<"$(refused ref-traversal "$CID" "/../../x")" \
  || fail "an unsafe incident id was not refused"
grep -q "^Failed reserved-incident-id" <<<"$(refused ref-reserved "$CID" "$IID")" \
  || fail "a capture claiming the webhook's incident id was not refused"
grep -q "^Failed incident-id-in-use" <<<"$(refused ref-dup "$CID" export-e2e-ok)" \
  || fail "a second capture for an existing incident id was not refused"

# The webhook takes its target namespace from the alert's labels, so whoever can POST to it picks
# the pod whose logs are sealed. Where the operator named the namespaces this install records, a
# capture outside them is refused — otherwise the webhook token reads any pod in the cluster.
step "negative: a capture outside watchNamespaces is refused (the webhook token is not a cluster-wide log read)"
helm upgrade lapilli charts/lapilli -n "$NS" --reuse-values \
  --set-json "watchNamespaces=[\"$NS\"]" --wait --timeout 180s >/dev/null
POD=$(ctrl_pod "$NS")
kubectl create namespace watch-e2e >/dev/null 2>&1 || true
kubectl -n watch-e2e run bystander --image=busybox:1.36 --restart=Never \
  --command -- sh -c 'sleep 600' >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: ref-unwatched, namespace: $NS }
spec:
  profile: default
  incidentId: ref-unwatched-1
  clusterId: "$CID"
  trigger: { rule: Refusal, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: watch-e2e, pod: bystander }
EOF
for _ in $(seq 1 30); do
  [ "$(kubectl -n "$NS" get incidentcapture ref-unwatched -o jsonpath='{.status.phase}')" = Failed ] && break
  sleep 1
done
grep -q "^Failed target-not-watched" \
  <<<"$(kubectl -n "$NS" get incidentcapture ref-unwatched -o jsonpath='{.status.phase} {.status.message}')" \
  || fail "a capture outside watchNamespaces was not refused: $(kubectl -n "$NS" get incidentcapture ref-unwatched -o jsonpath='{.status.phase} {.status.message}')"
[ -z "$(kubectl -n "$NS" get incidentcapture ref-unwatched -o jsonpath='{.status.bundlePath}')" ] \
  || fail "the refused capture produced a bundle"
echo "  ok: refused target-not-watched, no bundle; the watched list holds captures, not just permission questions"
kubectl -n "$NS" delete incidentcapture ref-unwatched >/dev/null
kubectl delete namespace watch-e2e --wait=false >/dev/null 2>&1 || true
helm upgrade lapilli charts/lapilli -n "$NS" --reuse-values \
  --set-json 'watchNamespaces=[]' --wait --timeout 180s >/dev/null
POD=$(ctrl_pod "$NS"); CTRL=$POD

step "negative: the schema refuses what no controller code can catch"
# The guards this exercises bind a writer that is NOT the controller, so only a real API server
# can show them working (docs/design-status-message.md). The unit tests cover the truncation
# inside the controller; they cannot cover these.
LONGID=$(printf 'c%.0s' $(seq 1 84))            # 84 > the 83 the pattern allows
if kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: ref-longcluster, namespace: $NS }
spec:
  profile: default
  incidentId: "ref-longcluster-1"
  clusterId: "$LONGID"
  trigger: { rule: Refusal, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: $NS, pod: $POD }
EOF
then fail "an 84-character clusterId was admitted; the CRD pattern is not in effect"; fi
echo "  ok: an over-long clusterId is refused at admission, before any object exists"

# The status cap, against the population the newtype cannot reach: a direct status write. The
# controller's own Role grants incidentcaptures/status, which is exactly the hole.
LONGMSG=$(printf 'm%.0s' $(seq 1 1100))         # 1100 > the 1024 the schema allows
if kubectl -n "$NS" patch incidentcapture ref-cluster --subresource=status --type=merge \
     -p "{\"status\":{\"message\":\"$LONGMSG\"}}" >/dev/null 2>&1
then fail "a 1100-byte status.message was accepted; the schema cap is not in effect"; fi
# …and the field still holds what the controller put there, not a truncated forgery.
grep -q "^cluster-mismatch" \
  <<<"$(kubectl -n "$NS" get incidentcapture ref-cluster -o jsonpath='{.status.message}')" \
  || fail "the rejected patch damaged the message the controller wrote"
echo "  ok: an over-long status.message is refused by the API server, and the real one survives"
AFTER=$(kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/lapilli cat-bundle "$BUNDLE" | shasum -a 256 | cut -c1-64)
[ "$BEFORE" = "$AFTER" ] || fail "the existing bundle changed ($BEFORE -> $AFTER)"
kubectl -n "$NS" delete incidentcapture ref-cluster ref-traversal ref-reserved ref-dup metrics-ok >/dev/null
echo "  refused: cluster-mismatch, invalid-incident-id, reserved-incident-id, incident-id-in-use"
echo "  (original bundle intact)"

# Last, because its cleanup deletes every IncidentCapture in the namespace and the steps above
# read theirs. It is also the most realistic place for it: by now the release has been upgraded,
# signing is on, and retention has run, so the storm hits the configuration a user would have
# rather than a fresh install.
#
# What it adds over everything above: every check before this one exercises ONE capture at a time.
# A node dying is not one capture, it is one payload of a hundred, and the two failures that cost
# the most evidence this year were only reachable at that shape — a body limit that rejected a
# whole payload with no series able to see it, and a liveness probe that killed the controller in
# the middle of the captures it was recording (docs/design-trigger-and-load.md §2).
suite storm "alert storm: one payload of 20 alerts — survival, accounting, and the kubelet's verdict" \
  env CLUSTER="$CLUSTER" test/e2e/storm.sh 20

echo; if [ -n "${SKIP:-}" ]; then
  echo "E2E OK *** with SKIP=$SKIP — this is NOT a full run ***"
else
  echo "E2E OK"
fi
