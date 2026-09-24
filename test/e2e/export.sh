#!/usr/bin/env bash
# Object-store export E2E (docs/design-export.md) against MinIO with an object-lock bucket:
#   upload lands under <prefix>/<cluster>/<incident>.ieb with the bundle's exact bytes;
#   a re-reconcile does not upload again; demo captures stay local; an unknown destination
#   is refused; an object pre-created with different bytes is a conflict (never overwritten).
#
# Usage: test/e2e/export.sh <lapilli-binary>   (called by run.sh, Lapilli installed in lapilli-system)
set -euo pipefail

ctrl_pod() { # the controller pod that is not terminating
  kubectl -n "$1" get pods -l app.kubernetes.io/name=lapilli \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | awk 'NR==1'
}

LAPILLI=$1
KNS=lapilli-system
# Pinned by digest, as before — but since 2026-09 quay.io answers an anonymous manifest request
# for these with 401, so the kind node can no longer pull them itself. The pins stay the
# identity; the bytes come from the host's Docker cache (or a `docker login quay.io` pull),
# are checked against the pin, retagged with a name that carries it, and loaded into the node,
# the way run.sh already loads Prometheus. A digest reference cannot be loaded by name (kind
# stores it nameless and the kubelet cannot resolve it), which is why the retag exists.
MINIO_PIN=quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e
MC_PIN=quay.io/minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727
MINIO_IMAGE=lapilli-e2e/minio:14cea493
MC_IMAGE=lapilli-e2e/mc:a7fe349e
stage_image() { # pinned-ref local-tag
  local pin=$1 tag=$2
  if ! docker image inspect "$pin" >/dev/null 2>&1; then
    docker pull "$pin" >/dev/null 2>&1 || {
      echo "FAIL (export): $pin is not in the local Docker cache and cannot be pulled anonymously" >&2
      echo "  quay.io has answered 401 for minio images since 2026-09: \`docker login quay.io\` (or" >&2
      echo "  pull the digest once from a mirror you trust) and rerun. The pin is not changed here." >&2
      exit 1
    }
  fi
  # The retag must carry exactly the pinned bytes.
  docker image inspect "$pin" --format '{{join .RepoDigests "\n"}}' | grep -qF "${pin#*@}" \
    || { echo "FAIL (export): cached image for $pin does not carry that digest" >&2; exit 1; }
  docker tag "$pin" "$tag"
  kind load docker-image "$tag" --name "${CLUSTER:-lapilli}" >/dev/null 2>&1
}
stage_image "$MINIO_PIN" "$MINIO_IMAGE"
stage_image "$MC_PIN" "$MC_IMAGE"
USER=lapilli-e2e
PASS=lapilli-e2e-secret

step() { echo; echo "==> export: $*"; }
fail() { echo "FAIL (export): $*"; kubectl -n $KNS logs deploy/lapilli --tail=40 || true; exit 1; }

mc() { # run an mc command against the in-cluster MinIO; prints its stdout, keeps its status
  # (Not `kubectl run --rm -i`: when the container exits before attach, its output is lost.)
  local name="mc-$RANDOM$RANDOM" phase=""
  kubectl -n minio run "$name" --restart=Never --image="$MC_IMAGE" --image-pull-policy=Never \
    --env="MC_HOST_m=http://$USER:$PASS@minio.minio:9000" --command -- sh -c "$1" >/dev/null
  for _ in $(seq 1 120); do
    phase=$(kubectl -n minio get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = Succeeded ] || [ "$phase" = Failed ] && break
    sleep 1
  done
  kubectl -n minio logs "$name" 2>/dev/null
  kubectl -n minio delete pod "$name" --wait=false >/dev/null 2>&1
  [ "$phase" = Succeeded ]
}

step "MinIO with an object-lock bucket"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata: { name: minio }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: minio, namespace: minio }
spec:
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: $MINIO_IMAGE
          imagePullPolicy: Never # loaded into the node by stage_image
          args: ["server", "/data"]
          env:
            - { name: MINIO_ROOT_USER, value: $USER }
            - { name: MINIO_ROOT_PASSWORD, value: $PASS }
          readinessProbe: { httpGet: { path: /minio/health/ready, port: 9000 } }
---
apiVersion: v1
kind: Service
metadata: { name: minio, namespace: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 9000 }]
EOF
kubectl -n minio rollout status deploy/minio --timeout=180s >/dev/null
mc "mc mb --ignore-existing --with-lock m/evidence" >/dev/null

step "admin defines the destination; credentials by resourceNames-scoped Secret"
kubectl -n $KNS create secret generic lapilli-minio \
  --from-literal=access_key_id=$USER --from-literal=secret_access_key=$PASS \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set-json \
  'export.destinations=[{"name":"evidence","url":"s3://evidence/e2e","region":"us-east-1","endpoint":"http://minio.minio:9000","allowHttp":true,"credentialsSecret":"lapilli-minio"}]' \
  --wait --timeout 180s >/dev/null

kubectl create namespace export-e2e >/dev/null 2>&1 || true
kubectl -n export-e2e run crash --image=busybox:1.36 --restart=Always \
  --command -- sh -c 'echo export-e2e; sleep 2; exit 1' >/dev/null
for _ in $(seq 1 60); do
  rc=$(kubectl -n export-e2e get pod crash -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)
  [ "${rc:-0}" -ge 1 ] && break; sleep 2
done

capture() { # IncidentCapture name, incident id, profile → waits until the capture is Exported
  kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: $1, namespace: $KNS }
spec:
  profile: $3
  incidentId: $2
  clusterId: kind-lapilli
  trigger: { rule: ExportE2E, firingTs: "$(date -u +%Y-%m-%dT%H:%M:%SZ)" }
  target: { namespace: export-e2e, pod: crash }
EOF
  for _ in $(seq 1 60); do
    [ "$(kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.phase}')" = Exported ] && return
    sleep 1
  done
  fail "$1 never exported"
}
export_state() { # capture, destination → state (waits until it's settled)
  local s=""
  for _ in $(seq 1 60); do
    s=$(kubectl -n $KNS get incidentcapture "$1" -o jsonpath="{.status.exports.$2.state}")
    [ -n "$s" ] && [ "$s" != pending ] && break
    sleep 2
  done
  echo "$s"
}

step "a capture is uploaded with its exact bytes"
capture exp-ok export-e2e-ok default
[ "$(export_state exp-ok evidence)" = uploaded ] || fail "exp-ok not uploaded: $(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports}')"
URL=$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.url}')
[ "$URL" = "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" ] || fail "unexpected object url $URL"
CTRL=$(ctrl_pod "$KNS")
hash64() { grep -oE '[0-9a-f]{64}' | head -1; }
LOCAL=$(kubectl -n $KNS exec "$CTRL" -c controller -- /usr/local/bin/lapilli cat-bundle /var/lib/lapilli/bundles/export-e2e-ok.ieb | shasum -a 256 | hash64 || true)
REMOTE=$(mc "mc cat m/evidence/e2e/kind-lapilli/export-e2e-ok.ieb | sha256sum" | hash64 || true)
[ -n "$LOCAL" ] && [ "$LOCAL" = "$REMOTE" ] || fail "remote bytes differ from the local bundle (local=$LOCAL remote=$REMOTE)"
echo "  ok: $URL holds the bundle (sha256 ${LOCAL:0:16}…)"

step "a re-reconcile does not upload again"
BEFORE=$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.lastAttemptAt}')
kubectl -n $KNS annotate incidentcapture exp-ok e2e/poke="$(date +%s)" --overwrite >/dev/null
kubectl -n $KNS patch incidentcapture exp-ok --type=merge -p '{"spec":{"trigger":{"rule":"Edited"}}}' >/dev/null
sleep 5
AFTER=$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.lastAttemptAt}')
[ "$BEFORE" = "$AFTER" ] || fail "the export was attempted again ($BEFORE → $AFTER)"
[ "$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.state}')" = uploaded ] \
  || fail "a spec edit disturbed the uploaded export"
echo "  ok: no new attempt after a poke and a spec edit"

step "an unknown destination is refused"
kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: CaptureProfile
metadata: { name: rogue, namespace: $KNS }
spec:
  collectors: [logs]
  export: { destinations: [elsewhere] }
EOF
capture exp-rogue export-e2e-rogue rogue
[ "$(export_state exp-rogue elsewhere)" = refused ] || fail "unknown destination was not refused"
[ "$(kubectl -n $KNS get incidentcapture exp-rogue -o jsonpath='{.status.exports.elsewhere.reason}')" = not-allowed ] \
  || fail "unexpected refusal reason"
echo "  ok: refused (not-allowed)"

step "an existing object with different bytes is a conflict, never overwritten"
mc "echo not-the-bundle | mc pipe m/evidence/e2e/kind-lapilli/export-e2e-conflict.ieb" >/dev/null
capture exp-conflict export-e2e-conflict default
[ "$(export_state exp-conflict evidence)" = conflict ] || fail "pre-existing object was not reported as conflict"
[ "$(mc "mc cat m/evidence/e2e/kind-lapilli/export-e2e-conflict.ieb")" = not-the-bundle ] \
  || fail "the pre-existing object was overwritten"
[ -n "$(kubectl -n $KNS get events --field-selector reason=ExportConflict -o name)" ] \
  || fail "no ExportConflict event"
echo "  ok: conflict, original object intact, Event emitted"

step "lapilli verify reads the evidence straight from the bucket"
kubectl -n minio port-forward svc/minio 19100:9000 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do (echo >/dev/tcp/127.0.0.1/19100) 2>/dev/null && break; sleep 1; done
rverify() { # lapilli verify against the port-forwarded MinIO; prints output, returns the exit code
  AWS_ACCESS_KEY_ID=$USER AWS_SECRET_ACCESS_KEY=$PASS AWS_REGION=us-east-1 \
    AWS_ENDPOINT_URL=http://127.0.0.1:19100 AWS_ALLOW_HTTP=true "$LAPILLI" verify "$@" 2>&1
}
expect_rc() { # expected exit code, description, args…
  local want=$1 what=$2 out rc; shift 2
  set +e; out=$(rverify "$@"); rc=$?; set -e
  [ "$rc" = "$want" ] || fail "$what: exit $rc, want $want: $out"
}
SHA=$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.sha256}')
VID=$(kubectl -n $KNS get incidentcapture exp-ok -o jsonpath='{.status.exports.evidence.versionId}')
[ "$SHA" = "$LOCAL" ] || fail "status.exports.evidence.sha256 ($SHA) is not the bundle's ($LOCAL)"
[ -n "$VID" ] || fail "no versionId recorded for an object-lock (versioned) bucket"
set +e
OUT=$(rverify "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" --expect-sha256 "$SHA"); RC=$?
set -e
[ "$RC" = 0 ] || fail "remote verify of the uploaded bundle exited $RC: $OUT"
grep -q "sha256=$LOCAL" <<<"$OUT" || fail "remote verify reported another sha256: $OUT"
grep -q "version=$VID .*history=versions:1,delete-markers:0" <<<"$OUT" || fail "unexpected version/history: $OUT"
grep -q "identity: cluster=kind-lapilli incident=export-e2e-ok (from the object key" <<<"$OUT" \
  || fail "the identity was not taken from the key: $OUT"
set +e
JSON=$(rverify "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" --output json); RC=$?
set -e
[ "$RC" = 0 ] || fail "--output json exited $RC: $JSON"
python3 - "$JSON" "$SHA" "$VID" <<'PYEOF' || fail "unexpected verify-result document: $JSON"
import json, sys
d = json.loads(sys.argv[1])
assert d["schema"] == "lapilli.dev/verify-result/v1" and d["verdict"] == "OK", d
assert d["input"]["sha256"] == sys.argv[2] and d["input"]["version_id"] == sys.argv[3], d
assert d["input"]["history"]["state"] == "listed" and d["input"]["history"]["versions"] == 1, d
assert d["expected"]["source"] == "object-key" and d["bundle"]["incident_id"] == "export-e2e-ok", d
PYEOF
expect_rc 0 "the recorded version" "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" --version-id "$VID" --expect-sha256 "$SHA"
expect_rc 1 "the conflicting object" "s3://evidence/e2e/kind-lapilli/export-e2e-conflict.ieb"
expect_rc 1 "a bundle stored under another incident's key" "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" --incident export-e2e-conflict
expect_rc 3 "a missing object" "s3://evidence/e2e/kind-lapilli/does-not-exist.ieb"
expect_rc 1 "another bundle's sha256" "s3://evidence/e2e/kind-lapilli/export-e2e-ok.ieb" --expect-sha256 "$(printf '0%.0s' $(seq 64))"
kill $PF 2>/dev/null; wait $PF 2>/dev/null || true
echo "  ok: remote verify OK (sha256 and versionId from status, history 1 version, identity from the key);"
echo "      conflict, wrong identity and wrong sha256 FAILED; missing object exit 3"

step "a forged status can't make the controller upload another file"
kubectl apply -f - >/dev/null <<EOF
apiVersion: lapilli.dev/v1alpha1
kind: IncidentCapture
metadata: { name: exp-forged, namespace: $KNS }
spec:
  profile: does-not-exist
  incidentId: export-e2e-forged
  clusterId: kind-lapilli
  trigger: { rule: ExportE2E, firingTs: "2026-09-19T00:00:00Z" }
  target: { namespace: export-e2e, pod: crash }
EOF
for _ in $(seq 1 30); do
  [ "$(kubectl -n $KNS get incidentcapture exp-forged -o jsonpath='{.status.phase}')" = Failed ] && break; sleep 1
done
kubectl -n $KNS patch incidentcapture exp-forged --subresource=status --type=merge -p \
  '{"status":{"phase":"Exported","observedGeneration":1,"bundlePath":"/etc/passwd","exports":{"evidence":{"state":"pending","attempts":0}}}}' >/dev/null
[ "$(export_state exp-forged evidence)" = refused ] || fail "forged status was not refused"
[ "$(kubectl -n $KNS get incidentcapture exp-forged -o jsonpath='{.status.exports.evidence.reason}')" = not-a-verified-bundle ] \
  || fail "unexpected reason for the forged status"
mc "mc stat m/evidence/e2e/kind-lapilli/export-e2e-forged.ieb" >/dev/null 2>&1 && fail "the forged file reached the bucket"
echo "  ok: refused (not-a-verified-bundle); nothing uploaded"

step "lapilli demo captures stay local while destinations are configured"
DEMO=$("$LAPILLI" demo --scenario crashloop --out "$(mktemp -d)" | grep -o 'IncidentCapture ic-[0-9a-f]*' | cut -d' ' -f2)
[ -n "$DEMO" ] || fail "demo did not report its IncidentCapture"
[ -z "$(kubectl -n $KNS get incidentcapture "$DEMO" -o jsonpath='{.status.exports}')" ] \
  || fail "a demo capture was exported remotely"
[ "$(kubectl -n $KNS get incidentcapture "$DEMO" -o jsonpath='{.status.exportSummary}')" = local-only ] \
  || fail "a local-only capture is not marked as such"
echo "  ok: $DEMO has no remote exports"

step "cleanup: back to no destinations"
kubectl -n $KNS delete incidentcapture exp-ok exp-rogue exp-conflict exp-forged >/dev/null
kubectl -n $KNS delete captureprofile rogue >/dev/null
kubectl delete namespace export-e2e --wait=false >/dev/null
helm upgrade lapilli charts/lapilli -n $KNS --reuse-values --set-json 'export.destinations=[]' \
  --wait --timeout 180s >/dev/null
echo; echo "export scenarios OK"
