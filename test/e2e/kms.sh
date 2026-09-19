#!/usr/bin/env bash
# KMS signing E2E (docs/design-kms.md) with LocalStack KMS in the cluster:
#   the controller signs every bundle with the KMS key; `kairn key fetch` gets the public
#   key from the KMS and the bundle verifies with it (signed:trusted-key);
#   during a KMS outage a capture waits in Sealing (no bundle), survives a controller
#   restart, and seals once KMS is back, from the data collected before the restart.
#
# Usage: test/e2e/kms.sh <kairn-binary>   (called by run.sh, Kairn installed in kairn-system)
set -euo pipefail

KAIRN=$1
KNS=kairn-system
LOCALSTACK=localstack/localstack:4.12
LOCALSTACK_DIGEST=sha256:0df3a97da57de03a588c05d9b8f390f15c7033fc7c4512f94619d344bc3cd317
PORT=14567

step() { echo; echo "==> kms: $*"; }
fail() { echo "FAIL (kms): $*"; kubectl -n $KNS logs deploy/kairn --tail=40 || true; exit 1; }
ctrl_pod() {
  kubectl -n "$KNS" get pods -l app.kubernetes.io/name=kairn \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | head -1
}
PF=""
trap '[ -n "$PF" ] && kill $PF 2>/dev/null || true' EXIT

step "LocalStack KMS in the cluster (pinned $LOCALSTACK@$LOCALSTACK_DIGEST)"
docker image inspect "$LOCALSTACK" >/dev/null 2>&1 || docker pull -q "$LOCALSTACK@$LOCALSTACK_DIGEST" >/dev/null
docker tag "$LOCALSTACK@$LOCALSTACK_DIGEST" "$LOCALSTACK" 2>/dev/null || true
kind load docker-image "$LOCALSTACK" --name kairn >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Namespace
metadata: { name: localstack }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: localstack, namespace: localstack }
spec:
  selector: { matchLabels: { app: localstack } }
  template:
    metadata: { labels: { app: localstack } }
    spec:
      containers:
        - name: localstack
          image: $LOCALSTACK
          imagePullPolicy: IfNotPresent
          env: [{ name: SERVICES, value: kms }]
          readinessProbe: { httpGet: { path: /_localstack/health, port: 4566 } }
---
apiVersion: v1
kind: Service
metadata: { name: localstack, namespace: localstack }
spec:
  selector: { app: localstack }
  ports: [{ port: 4566 }]
YAML
kubectl -n localstack rollout status deploy/localstack --timeout=300s >/dev/null
kubectl -n localstack port-forward svc/localstack $PORT:4566 >/dev/null 2>&1 &
PF=$!
for _ in $(seq 1 60); do
  curl -sf "localhost:$PORT/_localstack/health" 2>/dev/null | grep -q '"kms": "\(available\|running\)"' && break
  sleep 2
done
ARN=$(python3 - "$PORT" <<'PY'
import json, sys, urllib.request
req = urllib.request.Request(f"http://localhost:{sys.argv[1]}/", data=json.dumps(
    {"KeySpec": "ECC_NIST_P256", "KeyUsage": "SIGN_VERIFY"}).encode(), headers={
    "Content-Type": "application/x-amz-json-1.1", "X-Amz-Target": "TrentService.CreateKey",
    "Authorization": "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/kms/aws4_request, SignedHeaders=host, Signature=x"})
print(json.load(urllib.request.urlopen(req))["KeyMetadata"]["Arn"])
PY
)
echo "  key $ARN"

step "helm upgrade: signing.mode=kms (the admin's key; profiles can't change it)"
helm upgrade kairn charts/kairn -n $KNS --reuse-values --set signing.mode=kms \
  --set signing.kms.key="$ARN" --set-json 'extraEnv=[
    {"name":"AWS_ENDPOINT_URL_KMS","value":"http://localstack.localstack.svc.cluster.local:4566"},
    {"name":"AWS_ACCESS_KEY_ID","value":"test"},{"name":"AWS_SECRET_ACCESS_KEY","value":"test"}]' \
  --wait --timeout 180s >/dev/null
for _ in $(seq 1 60); do
  kubectl -n $KNS logs deploy/kairn 2>/dev/null | grep -q "KMS signing key pinned" && break; sleep 2
done
kubectl -n $KNS logs deploy/kairn | grep -q "KMS signing key pinned" || fail "the controller never pinned the KMS key"

step "kairn key fetch asks the KMS for the public key"
TMP=$(mktemp -d)
OUT=$(AWS_ENDPOINT_URL_KMS="http://localhost:$PORT" AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
  AWS_REGION=us-east-1 "$KAIRN" key fetch --kms "$ARN" --out "$TMP/kms.pub")
KEY_ID=$(echo "$OUT" | awk '/^key_id/ {print $2}')
[ ${#KEY_ID} = 64 ] || fail "key fetch printed no key_id: $OUT"
kubectl -n $KNS logs deploy/kairn | grep -q "$KEY_ID" || fail "the controller pinned another key_id"
echo "  key_id $KEY_ID"

CTRL=$(ctrl_pod)
capture() { # name, incident
  kubectl apply -f - >/dev/null <<YAML
apiVersion: kairn.dev/v1alpha1
kind: IncidentCapture
metadata: { name: $1, namespace: $KNS }
spec:
  profile: default
  incidentId: $2
  clusterId: kind-kairn
  trigger: { rule: KmsE2E, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: $KNS, pod: $CTRL }
YAML
}
phase() { kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.phase}'; }
wait_phase() { # name, phase, seconds
  for _ in $(seq 1 "$3"); do [ "$(phase "$1")" = "$2" ] && return 0; sleep 1; done
  fail "$1 never reached $2 (phase $(phase "$1"): $(kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.message}'))"
}
verify_bundle() { # name, incident → verify output
  local path
  path=$(kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.bundlePath}')
  kubectl -n $KNS exec "$(ctrl_pod)" -c controller -- /usr/local/bin/kairn cat-bundle "$path" > "$TMP/$2.ieb"
  "$KAIRN" verify "$TMP/$2.ieb" --key "$TMP/kms.pub" --cluster kind-kairn --incident "$2"
}

step "a capture is signed with KMS and verifies with the fetched key"
capture kms-ok kms-e2e-ok
wait_phase kms-ok Exported 120
verify_bundle kms-ok kms-e2e-ok | grep -q "^OK .*signed:trusted-key" || fail "the KMS-signed bundle did not verify with the fetched key"
[ "$(kubectl -n $KNS get incidentcapture kms-ok -o jsonpath='{.status.seal.keyId}')" = "$KEY_ID" ] || fail "status.seal.keyId"
[ -n "$(kubectl -n $KNS get incidentcapture kms-ok -o jsonpath='{.status.seal.manifestSha256}')" ] || fail "no manifestSha256"
[ -n "$(kubectl -n $KNS get incidentcapture kms-ok -o jsonpath='{.status.seal.requestId}')" ] || fail "no request id"
echo "  ok: signed:trusted-key; status.seal records key_id, manifest digest and request id"

step "several captures at once: each sealed once, no spurious failures"
for i in 1 2 3 4 5; do capture "kms-many-$i" "kms-e2e-many-$i"; done
for i in 1 2 3 4 5; do wait_phase "kms-many-$i" Exported 180; done
if kubectl -n $KNS get events --field-selector reason=SealFailed -o name | grep -q .; then
  kubectl -n $KNS get events --field-selector reason=SealFailed
  fail "SealFailed events for captures that sealed fine"
fi
for i in 1 2 3 4 5; do
  verify_bundle "kms-many-$i" "kms-e2e-many-$i" | grep -q "^OK .*signed:trusted-key" || fail "kms-many-$i did not verify"
done
echo "  ok: 5 concurrent captures sealed and verified, no SealFailed"

step "metrics: the pinned signing key is exposed"
kubectl -n $KNS port-forward deploy/kairn 18082:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18082/metrics >/dev/null 2>&1 && break; sleep 1; done
curl -sf localhost:18082/metrics | grep -qF "kairn_signing_key_info{key_id=\"$KEY_ID\"} 1" \
  || fail "kairn_signing_key_info does not name the pinned key"
curl -sf localhost:18082/metrics | grep -qE '^kairn_seal_attempts_total\{result="ok"\} [1-9]' \
  || fail "successful seal attempts were not counted"
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
echo "  ok: kairn_signing_key_info and kairn_seal_attempts_total"

step "KMS outage: the capture waits in Sealing and never produces an unsigned bundle"
# Drop traffic to and from the LocalStack pod on the node. A Service change isn't enough:
# established keep-alive connections survive it (conntrack).
LS_IP=$(kubectl -n localstack get pods -l app=localstack -o jsonpath='{.items[0].status.podIP}')
outage() { # add|del
  local op=-I; [ "$1" = del ] && op=-D
  docker exec kairn-control-plane iptables $op FORWARD -d "$LS_IP" -j DROP
  docker exec kairn-control-plane iptables $op FORWARD -s "$LS_IP" -j DROP
}
outage add
capture kms-outage kms-e2e-outage
capture kms-tamper kms-e2e-tamper
wait_phase kms-outage Sealing 120
wait_phase kms-tamper Sealing 120
for _ in $(seq 1 60); do
  [ "$(kubectl -n $KNS get incidentcapture kms-outage -o jsonpath='{.status.seal.reason}')" = signing-unavailable ] && break; sleep 1
done
[ "$(kubectl -n $KNS get incidentcapture kms-outage -o jsonpath='{.status.seal.reason}')" = signing-unavailable ] \
  || fail "no signing-unavailable while KMS is down"
if kubectl -n $KNS exec "$(ctrl_pod)" -c controller -- /usr/local/bin/kairn cat-bundle \
    /var/lib/kairn/bundles/kms-e2e-outage.ieb >/dev/null 2>&1; then
  fail "a bundle was written while KMS was down"
fi
kubectl -n $KNS get events --field-selector reason=SealDelayed -o name | grep -q . || fail "no SealDelayed event"
kubectl -n $KNS port-forward deploy/kairn 18082:8081 >/dev/null 2>&1 &
MPF=$!
for _ in $(seq 1 30); do curl -sf localhost:18082/metrics >/dev/null 2>&1 && break; sleep 1; done
# The gauge comes from a 30 s poll of the API.
for _ in $(seq 1 45); do
  curl -sf localhost:18082/metrics 2>/dev/null | grep -qE '^kairn_captures_awaiting_seal [1-9]' && break
  sleep 2
done
curl -sf localhost:18082/metrics | grep -qE '^kairn_captures_awaiting_seal [1-9]' \
  || fail "kairn_captures_awaiting_seal is 0 while captures wait for KMS"
curl -sf localhost:18082/metrics | grep -qE '^kairn_captures\{phase="sealing"\} [1-9]' \
  || fail "kairn_captures{phase=sealing} is 0 while captures wait for KMS"
kill $MPF 2>/dev/null; wait $MPF 2>/dev/null || true
echo "  ok: Sealing (signing-unavailable), no bundle, SealDelayed event, awaiting_seal > 0"

step "someone writes into a waiting capture's staged data (from the node)"
TUID=$(kubectl -n $KNS get incidentcapture kms-tamper -o jsonpath='{.metadata.uid}')
NODE_STAGE=$(docker exec kairn-control-plane sh -c "ls -d /var/local-path-provisioner/*/.staging-kms-e2e-tamper-$TUID" 2>/dev/null | head -1)
[ -n "$NODE_STAGE" ] || fail "could not find the staging directory on the node"
docker exec kairn-control-plane sh -c "echo planted > '$NODE_STAGE/planted.txt'"

step "the controller restarts during the outage; the capture resumes without collecting again"
RESTART=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl -n $KNS rollout restart deploy/kairn >/dev/null
kubectl -n $KNS rollout status deploy/kairn --timeout=120s >/dev/null
sleep 5
[ "$(phase kms-outage)" = Sealing ] || fail "after the restart the capture is $(phase kms-outage), not Sealing"
outage del
wait_phase kms-outage Exported 300
verify_bundle kms-outage kms-e2e-outage | grep -q "^OK .*signed:trusted-key" || fail "the resumed bundle did not verify"
"$KAIRN" unpack "$TMP/kms-e2e-outage.ieb" "$TMP/outage" >/dev/null
STARTED=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["timing"]["capture_started"])' "$TMP/outage/manifest.json")
python3 - "$STARTED" "$RESTART" <<'PY' || fail "the capture was collected again after the restart ($STARTED >= $RESTART)"
import sys
from datetime import datetime
started, restart = (datetime.fromisoformat(t.replace("Z", "+00:00")) for t in sys.argv[1:])
sys.exit(0 if started < restart else 1)
PY
echo "  ok: sealed after KMS came back, from data collected before the restart ($STARTED < $RESTART)"

step "the tampered capture fails (staging-modified), stays failed, and keeps its data"
wait_phase kms-tamper Failed 300
kubectl -n $KNS get incidentcapture kms-tamper -o jsonpath='{.status.message}' | grep -q "^staging-modified" \
  || fail "kms-tamper failed for another reason: $(kubectl -n $KNS get incidentcapture kms-tamper -o jsonpath='{.status.message}')"
sleep 30
[ "$(phase kms-tamper)" = Failed ] || fail "the failed capture moved on to $(phase kms-tamper) (collected again?)"
docker exec kairn-control-plane test -f "$NODE_STAGE/planted.txt" || fail "the staged data (with the planted file) was wiped"
kubectl -n $KNS annotate incidentcapture kms-tamper kairn.dev/retry-seal=1 --overwrite >/dev/null
sleep 10
wait_phase kms-tamper Failed 60
docker exec kairn-control-plane test -f "$NODE_STAGE/planted.txt" || fail "retry-seal wiped the staged data"
if kubectl -n $KNS exec "$(ctrl_pod)" -c controller -- /usr/local/bin/kairn cat-bundle \
    /var/lib/kairn/bundles/kms-e2e-tamper.ieb >/dev/null 2>&1; then
  fail "a bundle was sealed over modified staged data"
fi
echo "  ok: Failed (staging-modified), stays Failed, data kept; retry-seal fails again; no bundle"

step "cleanup: signing back to none"
kubectl -n $KNS delete incidentcapture kms-ok kms-outage kms-tamper kms-many-1 kms-many-2 kms-many-3 kms-many-4 kms-many-5 >/dev/null
helm upgrade kairn charts/kairn -n $KNS --reuse-values --set signing.mode=none \
  --set-json 'extraEnv=[]' --wait --timeout 180s >/dev/null
kubectl delete namespace localstack --wait=false >/dev/null
rm -rf "$TMP"
echo; echo "kms scenarios OK"
