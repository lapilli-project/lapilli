#!/usr/bin/env bash
# Kairn kind E2E. `kairn demo` *is* the harness (DESIGN §8): it stages a bad rollout, fires
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
  kubectl -n "$1" get pods -l app.kubernetes.io/name=kairn \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | head -1
}

CLUSTER=kairn
IMAGE=kairn-controller:dev
# Pinned by digest (round-3 requirement): the node image kind v0.33.0 defaults to.
# Override with NODE_IMAGE=… (the release gate runs the oldest tested minor too).
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}
NS=kairn-system
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
fail() { echo "FAIL: $*"; kubectl -n "$NS" logs deploy/kairn --tail=50 || true; exit 1; }

step "build kairn CLI (host)"
cargo build -q -p kairn-cli
KAIRN="$ROOT/target/debug/kairn"

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
helm install kairn charts/kairn -n "$NS" --create-namespace \
  --set image.repository=kairn-controller --set image.tag=dev \
  --set clusterId=kind-kairn \
  --set metrics.prometheusUrl=http://prometheus.monitoring.svc:9090 --set metrics.stepSeconds=5 \
  --wait --timeout 180s

step "every chart render survives the API server's STRICT decoding"
# Server-side, not client-side: only the real API server reports a field it would PRUNE, and a
# pruned field is how a NetworkPolicy written as a tight allowlist became allow-all while lint,
# install and a values review all passed. The CRDs are installed by now, so CaptureProfile
# validates too. Runs once per tested Kubernetes minor, which also catches version-specific fields.
strict_render() { # description, extra helm args…
  local what=$1; shift
  helm template kairn charts/kairn "$@" \
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
    | kubectl create --raw "/api/v1/namespaces/$NS/services/kairn-webhook:webhook/proxy/webhook" -f - >/dev/null 2>&1; then
  fail "the webhook accepted a request without a token"
fi
CTRL=$(ctrl_pod "$NS")
if echo '{"alerts":[{"status":"firing","labels":{"alertname":"WrongToken","namespace":"default","pod":"x"}}]}' \
    | kubectl -n "$NS" exec -i "$CTRL" -c controller -- /usr/local/bin/kairn post-alert --wrong-token >/dev/null 2>&1; then
  fail "the webhook accepted a wrong token"
fi
[ "$(kubectl -n "$NS" get incidentcaptures --no-headers 2>/dev/null | wc -l)" = "$BEFORE" ] \
  || fail "a rejected request created a capture"
echo "  ok: 401 without a token and with a wrong one; no capture created"

step "kairn demo --scenario crashloop"
"$KAIRN" demo --scenario crashloop --out "$OUT/crashloop" | tee "$OUT/crashloop.txt" \
  || fail "demo crashloop exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/crashloop.txt" || fail "crashloop bundle not OK/100%"
grep -q "FATAL: cache warmup failed" "$OUT/crashloop.txt" || fail "previous-instance logs not recovered"
grep -q "revision 1 → 2, [0-9]*s before the alert, by demo-deployer" "$OUT/crashloop.txt" \
  || fail "diff headline (revision 1 → 2, before the alert, by demo-deployer) missing"
grep -q "env\[name=CACHE_WARMUP\].value: lazy → eager" "$OUT/crashloop.txt" \
  || fail "diff line CACHE_WARMUP: lazy → eager missing"

step "kairn demo --scenario oomkill"
"$KAIRN" demo --scenario oomkill --out "$OUT/oomkill" | tee "$OUT/oomkill.txt" \
  || fail "demo oomkill exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100%" "$OUT/oomkill.txt" || fail "oomkill bundle not OK/100%"
grep -q "OOMKilled (exit 137)" "$OUT/oomkill.txt" || fail "OOMKilled termination not in bundle"
grep -q "memory (metrics/):.*MiB of 64 MiB limit" "$OUT/oomkill.txt" || fail "memory curve not captured from Prometheus"

step "redaction: planted credentials never reach a bundle; useful values stay"
CANARY=kairnDemoCanary7Qx2Lp9w
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
  test/e2e/diffs.sh "$KAIRN" "$OUT"

suite export "object-store export: MinIO with object lock" \
  test/e2e/export.sh "$KAIRN"

suite kms "KMS signing: LocalStack KMS, key fetch, outage + restart" \
  test/e2e/kms.sh "$KAIRN"

suite notify "notification: receiver pod, grouping, no workload content, failure path" \
  test/e2e/notify.sh "$KAIRN"

step "negative: tamper one byte in an unpacked bundle (expect FAILED, exit 1)"
BUNDLE_DIR=$(find "$OUT/crashloop" -mindepth 1 -maxdepth 1 -type d | head -1)
# changes.json is in every bundle; a log file may be absent (kubelet GC), and appending to a
# missing file would test "extra file" instead of "modified byte".
printf 'x' >> "$BUNDLE_DIR/changes.json"
if "$KAIRN" verify "$BUNDLE_DIR"; then fail "verify accepted a tampered bundle"; fi
echo "  correctly rejected the tampered bundle"

step "signing: kairn keygen -> Secret -> helm upgrade signing.mode=static"
"$KAIRN" keygen --out-dir "$OUT/keys"
kubectl -n "$NS" create secret generic kairn-signing-key --from-file=key.pem="$OUT/keys/kairn.key"
helm upgrade kairn charts/kairn -n "$NS" --reuse-values \
  --set signing.mode=static --set signing.keySecret=kairn-signing-key --wait --timeout 120s

step "kairn demo --key (expect a bundle signed by the trusted key)"
"$KAIRN" demo --scenario crashloop --key "$OUT/keys/kairn.pub" --out "$OUT/signed" | tee "$OUT/signed.txt" \
  || fail "signed demo exited non-zero"
grep -q "OK  hash_ok=true context_ok=true coverage=100% signed:trusted-key" "$OUT/signed.txt" \
  || fail "bundle not signed by the trusted key"

step "negative: signed bundle checked against a different key (expect FAILED)"
"$KAIRN" keygen --out-dir "$OUT/other-keys" >/dev/null
SIGNED_IEB=$(find "$OUT/signed" -maxdepth 1 -name '*.ieb' | head -1)
if "$KAIRN" verify "$SIGNED_IEB" --key "$OUT/other-keys/kairn.pub"; then fail "accepted a bundle signed by another key"; fi
echo "  correctly rejected: not signed by the trusted key"

step "negative: unsigned bundle checked with --key (expect FAILED)"
UNSIGNED_IEB=$(find "$OUT/oomkill" -maxdepth 1 -name '*.ieb' | head -1)
if "$KAIRN" verify "$UNSIGNED_IEB" --key "$OUT/keys/kairn.pub"; then fail "accepted an unsigned bundle under --key"; fi
echo "  correctly rejected: unsigned bundle where a signature was required"

step "bundles survive a controller restart (PVC, not emptyDir)"
kubectl -n "$NS" rollout restart deploy/kairn
kubectl -n "$NS" rollout status deploy/kairn --timeout=120s

step "in-cluster kairn verify (distroless binary) — happy path + wrong context"
IC=$(kubectl -n "$NS" get incidentcapture -o jsonpath='{.items[0].metadata.name}')
BUNDLE=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.status.bundlePath}')
CID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.clusterId}')
IID=$(kubectl -n "$NS" get incidentcapture "$IC" -o jsonpath='{.spec.incidentId}')
POD=$(ctrl_pod "$NS")
kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --cluster "$CID" --incident "$IID" \
  || fail "in-cluster verify rejected a good bundle (lost across the restart?)"
if kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn verify "$BUNDLE" --incident WRONG; then
  fail "verify accepted wrong incident (not fail-closed)"
fi
echo "  correctly fail-closed on wrong incident"

step "metrics: /metrics serves the documented series"
# Counters are per-process and earlier steps restarted the pod (helm upgrades, the restart
# check), so make the traffic this pod should count: one capture and one rejected webhook
# request.
echo '{"alerts":[]}' | kubectl -n "$NS" exec -i "$POD" -c controller -- \
  /usr/local/bin/kairn post-alert --wrong-token >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: kairn.dev/v1alpha1
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
kubectl -n "$NS" port-forward deploy/kairn 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
# The state-derived gauges appear after the first poll (30 s).
for _ in $(seq 1 60); do
  curl -sf localhost:18081/metrics 2>/dev/null | grep -q '^kairn_captures{' && break; sleep 2
done
METRICS=$(curl -sf localhost:18081/metrics) || fail "/metrics is not served"
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
# Every series docs/metrics.md documents (except the KMS key, checked in kms.sh).
for series in \
  'kairn_build_info{version=' \
  'kairn_captures_total{result="sealed"}' \
  'kairn_captures_total{result="refused"}' \
  'kairn_captures_total{result="failed"}' \
  'kairn_partial_captures_total' \
  'kairn_collector_failures_total' \
  'kairn_capture_seconds_bucket{le="+Inf"}' \
  'kairn_bundle_bytes_bucket{le="1073741824"}' \
  'kairn_bundle_bytes_count' \
  'kairn_seal_attempts_total{result="ok"}' \
  'kairn_seal_attempts_total{result="failed"}' \
  'kairn_seal_pack_failures_total' \
  'kairn_reconcile_errors_total' \
  'kairn_export_attempts_total{result="ok"}' \
  'kairn_export_attempts_total{result="failed"}' \
  'kairn_export_destinations{state="uploaded"}' \
  'kairn_exports_unsettled' \
  'kairn_captures{phase="sealing"}' \
  'kairn_captures{phase="exported"}' \
  'kairn_captures_awaiting_seal' \
  'kairn_webhook_requests_total{result="accepted"}' \
  'kairn_webhook_requests_total{result="duplicate"}' \
  'kairn_webhook_requests_total{result="rejected"}' \
  'kairn_webhook_requests_total{result="error"}' \
  'kairn_apiserver_poll_ok' \
  'kairn_apiserver_polls_total{result="ok"}' \
  'kairn_apiserver_polls_total{result="forbidden"}' \
  'kairn_apiserver_polls_total{result="unreachable"}' \
  'kairn_apiserver_last_success_timestamp_seconds'; do
  echo "$METRICS" | grep -qF "$series" || fail "/metrics is missing $series"
done
SEALED=$(echo "$METRICS" | awk -F' ' '/^kairn_captures_total\{result="sealed"\}/ {print $2}')
[ "${SEALED:-0}" -ge 1 ] || fail "kairn_captures_total sealed is $SEALED after a capture"
REJECTED=$(echo "$METRICS" | awk -F' ' '/^kairn_webhook_requests_total\{result="rejected"\}/ {print $2}')
[ "${REJECTED:-0}" -ge 1 ] || fail "the rejected webhook request was not counted ($REJECTED)"
# The gauge has to say 1 here: this controller has plainly been using the API server all suite.
# A 0 would mean the poller is reporting on something else entirely.
REACH=$(echo "$METRICS" | awk -F' ' '/^kairn_apiserver_poll_ok/ {print $2}')
[ "${REACH:-0}" = "1" ] || fail "kairn_apiserver_poll_ok is $REACH on a working cluster"
LAST_OK=$(echo "$METRICS" | awk -F' ' '/^kairn_apiserver_last_success_timestamp_seconds/ {print $2}')
# Read "now" from inside the cluster, not from the host: on a laptop the Docker VM's clock drifts
# from the host across sleep, which would fail this assertion for a reason that has nothing to do
# with the metric. The node, not the pod — the controller image is distroless and has no `date`,
# and the node shares its kernel clock with every container on it.
NOW=$(docker exec "$(kind get nodes --name "$CLUSTER" | head -1)" date +%s)
AGE=$(( NOW - ${LAST_OK:-0} ))
# One poll interval is 30 s; allow two plus the scrape, and refuse a timestamp from the future.
[ "$AGE" -ge 0 ] && [ "$AGE" -le 75 ] \
  || fail "the last API-server success is ${AGE}s old, which no 30s poller should report"
POLLS_OK=$(echo "$METRICS" | awk -F' ' '/^kairn_apiserver_polls_total\{result="ok"\}/ {print $2}')
[ "${POLLS_OK:-0}" -ge 1 ] || fail "no successful API-server poll was counted ($POLLS_OK)"
BYTES=$(echo "$METRICS" | awk -F' ' '/^kairn_bundle_bytes_sum/ {print $2}')
[ "${BYTES:-0}" -gt 1000 ] || fail "kairn_bundle_bytes_sum looks wrong ($BYTES)"
echo "$METRICS" | grep -qE '^kairn_bundle_bytes_bucket\{le="1048576"\} [1-9]' \
  || fail "bundle sizes are not landing in the byte buckets"
echo "  ok: sealed=$SEALED, rejected webhook=$REJECTED, bundle bytes bucketed, all series present"
echo "  ok: the API server reads as reachable, last seen ${AGE}s ago over $POLLS_OK polls"

step "negative: a controller that cannot use the API server says so, and is NOT restarted"
# The whole point of these series, and until now only the happy path was checked — a hardcoded
# "the poll succeeded" would have passed the entire suite. Revoke the poller's own permission:
# that is a 403, which must read as `forbidden` (the API server answered) and NOT `unreachable`,
# because those two send an operator to completely different places.
# Specifically the namespace Role that grants `kairn.dev` verbs — NOT the `-collector` ClusterRole,
# which the poller does not use. Revoking the wrong one would make this step assert nothing.
ROLE=kairn
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
kubectl -n "$NS" port-forward deploy/kairn 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
# Two poll intervals plus slack: the gauge holds its previous value until the next poll returns,
# which is exactly why docs/egress.md tells operators to wait before believing it.
BLIND=""
for _ in $(seq 1 24); do
  M2=$(curl -sf localhost:18081/metrics || true)
  if echo "$M2" | grep -q '^kairn_apiserver_poll_ok 0$'; then BLIND=$M2; break; fi
  sleep 5
done
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ -n "$BLIND" ] || fail "the poller never reported kairn_apiserver_poll_ok 0 after its RBAC was revoked"
echo "$BLIND" | grep -qE '^kairn_apiserver_polls_total\{result="forbidden"\} [1-9]' \
  || fail "a 403 must be counted as result=forbidden: $(echo "$BLIND" | grep '^kairn_apiserver_polls_total')"
echo "$BLIND" | grep -q '^kairn_apiserver_polls_total{result="unreachable"} 0$' \
  || fail "a 403 was miscounted as unreachable, which sends an operator to the network"
# The last-success timestamp must survive the outage: it is how long the controller has been blind.
echo "$BLIND" | grep -q '^kairn_apiserver_last_success_timestamp_seconds ' \
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
      /usr/local/bin/kairn post-alert >/dev/null 2>&1 || true
kubectl -n "$NS" port-forward deploy/kairn 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
ERRS=$(curl -sf localhost:18081/metrics | awk -F' ' '/^kairn_webhook_requests_total\{result="error"\}/ {print $2}')
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ "${ERRS:-0}" -ge 1 ] \
  || fail "an alert the API server refused to record was counted nowhere (result=error is $ERRS)"
echo "  ok: the refused capture was counted as webhook result=error, not swallowed"

step "negative: … and it recovers when the permission comes back"
restore_role || fail "could not restore role/$ROLE, so the recovery assertion would prove nothing"
[ "$(kubectl -n "$NS" get "role/$ROLE" -o jsonpath='{.rules}')" = "$SAVED_RULES" ] \
  || fail "role/$ROLE was not restored to its original rules"
kubectl -n "$NS" port-forward deploy/kairn 18081:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18081/metrics >/dev/null 2>&1 && break; sleep 1; done
BACK=""
for _ in $(seq 1 24); do
  M3=$(curl -sf localhost:18081/metrics || true)
  if echo "$M3" | grep -q '^kairn_apiserver_poll_ok 1$'; then BACK=$M3; break; fi
  sleep 5
done
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
[ -n "$BACK" ] || fail "the poller never recovered to kairn_apiserver_poll_ok 1 after RBAC was restored"
trap cleanup EXIT
echo "  ok: back to 1 without a restart — the gauge tracks the fault, not the process"

step "negative: captures the controller refuses (another cluster, unsafe id, an id in use)"
BEFORE=$(kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn cat-bundle "$BUNDLE" | shasum -a 256 | cut -c1-64)
refused() { # name, cluster, incident → prints the Failed message
  kubectl apply -f - >/dev/null <<EOF
apiVersion: kairn.dev/v1alpha1
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
refused ref-cluster other-cluster ref-cluster-1 | grep -q "^Failed cluster-mismatch" \
  || fail "a capture for another cluster was not refused"
refused ref-traversal "$CID" "/../../x" | grep -q "^Failed invalid-incident-id" \
  || fail "an unsafe incident id was not refused"
refused ref-reserved "$CID" "$IID" | grep -q "^Failed reserved-incident-id" \
  || fail "a capture claiming the webhook's incident id was not refused"
refused ref-dup "$CID" export-e2e-ok | grep -q "^Failed incident-id-in-use" \
  || fail "a second capture for an existing incident id was not refused"
AFTER=$(kubectl -n "$NS" exec "$POD" -c controller -- /usr/local/bin/kairn cat-bundle "$BUNDLE" | shasum -a 256 | cut -c1-64)
[ "$BEFORE" = "$AFTER" ] || fail "the existing bundle changed ($BEFORE -> $AFTER)"
kubectl -n "$NS" delete incidentcapture ref-cluster ref-traversal ref-reserved ref-dup metrics-ok >/dev/null
echo "  refused: cluster-mismatch, invalid-incident-id, reserved-incident-id, incident-id-in-use"
echo "  (original bundle intact)"

echo; if [ -n "${SKIP:-}" ]; then
  echo "E2E OK *** with SKIP=$SKIP — this is NOT a full run ***"
else
  echo "E2E OK"
fi
