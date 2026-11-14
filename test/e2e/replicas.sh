#!/usr/bin/env bash
# Three replicas of lapilli serve on one Postgres ledger, on a kind cluster
# (docs/design/replicas.md): one leads, and its process is killed (SIGKILL:
# no lease given up); another leads within the lease and a campaign, at a
# later term; every notice of what people did, through the Service on any
# replica, before the kill and after it, goes to the webhook once; a
# rolling restart keeps the API answering and leaves one leader. Needs
# Docker, Go, kubectl, helm and jq; deletes the cluster afterwards unless
# KEEP is set. A run that fails says what the cluster held, each pod's log
# among it.
# shellcheck disable=SC2329 # dump and done_ run on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."
cluster=lapilli-e2e-replicas
ns=e2e
arch=$(go env GOARCH)
pgimage=postgres:18.6@sha256:e3d24f6906d0760bf4da0dae872415391ededefad23fd471c47dc53b0ddf3c42
# The lease (15 s) and a campaign (2 s), and what polling the pods adds.
bound=${TAKEOVER_BOUND:-30}

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/image/lapilli-linux-$arch" ./cmd/lapilli
docker buildx build --load --platform "linux/$arch" -t lapilli:e2e .

go install sigs.k8s.io/kind@v0.27.0
kind="$(go env GOPATH)/bin/kind"
"$kind" get clusters | grep -qx "$cluster" || "$kind" create cluster --name "$cluster" --wait 2m
# dump: what the cluster holds of lapilli, for a run that failed: its pods,
# the Service and its endpoints, and every pod's log.
dump() {
  echo "::group::what the cluster holds"
  kubectl -n "$ns" get pods,svc,endpointslices,pdb -o wide || true
  kubectl -n "$ns" describe deploy/lapilli || true
  for p in $(kubectl -n "$ns" get pods -l app.kubernetes.io/instance=lapilli -o name 2>/dev/null); do
    echo "--- $p"
    kubectl -n "$ns" logs "$p" --all-containers --tail=300 || true
    kubectl -n "$ns" logs "$p" --all-containers --previous --tail=100 2>/dev/null || true
  done
  echo "--- fakeslack"
  kubectl -n "$ns" logs deploy/fakeslack --tail=100 || true
  echo "::endgroup::"
}
done_() {
  local rc=$?
  if [ "$rc" != 0 ] || [ "${fail:-0}" != 0 ]; then dump; fi
  [ -n "${KEEP:-}" ] || "$kind" delete cluster --name "$cluster"
}
trap done_ EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR
"$kind" load docker-image lapilli:e2e --name "$cluster"
kubectl config use-context "kind-$cluster"

kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" apply -f test/e2e/stack.yaml
password=e2e-Replicas-pw-$RANDOM$RANDOM
kubectl -n "$ns" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: $pgimage
          env:
            - {name: POSTGRES_PASSWORD, value: "$password"}
            - {name: POSTGRES_DB, value: lapilli}
          ports: [{containerPort: 5432}]
          readinessProbe:
            exec: {command: [pg_isready, -h, 127.0.0.1, -U, postgres, -d, lapilli]}
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec: {selector: {app: postgres}, ports: [{port: 5432}]}
---
apiVersion: v1
kind: Secret
metadata: {name: ledger-db}
stringData:
  # Inside the cluster, without TLS: serve warns, and takes it.
  url: postgres://postgres:$password@postgres:5432/lapilli?sslmode=disable
---
apiVersion: v1
kind: Secret
metadata: {name: hook}
stringData:
  url: http://fakeslack:8080/hook
YAML
kubectl -n "$ns" rollout status deploy/prometheus deploy/pushgateway deploy/fakeslack deploy/postgres --timeout=3m

helm upgrade --install lapilli charts/lapilli -n "$ns" --wait --timeout 5m \
  --set mode=serve --set image.repository=lapilli --set image.tag=e2e --set image.pullPolicy=Never \
  --set prometheus.url=http://prometheus:9090 --set serve.backfill=1h \
  --set 'rules.configMaps={alert-rules}' --set serve.userHeader=X-Forwarded-Email \
  --set 'serve.roles.editors={e2e@example.com}' --set serve.networkPolicy.othersKeptOut=true \
  --set serve.replicas=3 --set serve.ledger.postgres.secret.name=ledger-db \
  --set serve.notify.webhookSecret.name=hook

fail=0
# The ports the in-cluster Service gives the UI and the metrics: the
# chart's serve.port and serve.metricsPort, not serve.service.port, which is
# the -ui Service's alone.
http=$(kubectl -n "$ns" get svc lapilli -o jsonpath='{.spec.ports[?(@.name=="http")].port}')
metrics=$(kubectl -n "$ns" get svc lapilli -o jsonpath='{.spec.ports[?(@.name=="metrics")].port}')
ui=http://lapilli:$http
echo "the Service: http $http, metrics $metrics"
check() { # what, text, pattern
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1"; echo "$2"; fail=1; fi
}
# incluster: a command in the cluster, from the pushgateway's pod, which
# has wget.
incluster() { kubectl -n "$ns" exec deploy/pushgateway -- "$@"; }
pods() { kubectl -n "$ns" get pods -l app.kubernetes.io/instance=lapilli,app.kubernetes.io/component=serve -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}'; }
# leaders: "POD REPLICA TERM" for each pod whose readiness says it leads.
leaders() {
  pods | while read -r pod ip; do
    [ -n "$ip" ] || continue
    incluster wget -qO- -T 3 "http://$ip:$metrics/readyz" 2>/dev/null |
      sed -nE "s/^replica ([^ ]+) leads, term ([0-9]+).*/$pod \1 \2/p" || true
  done
}
# task: one made through the Service, on whichever replica it sends it to;
# how long it took (ms) kept in $waited, the longest in $longest.
waited=0
longest=0
task() {
  local began=$EPOCHREALTIME
  if ! incluster wget -qO- -T 30 --header 'Content-Type: application/json' --header 'X-Forwarded-Email: e2e@example.com' \
    --post-data "{\"title\": \"$1\", \"rule\": \"LateStart\"}" "$ui/api/v1/tasks" >/dev/null; then
    echo "FAILED: the task \"$1\", through the Service at $ui"
    fail=1
    return 1
  fi
  waited=$(awk -v a="$began" -v b="$EPOCHREALTIME" 'BEGIN { printf "%d", (b - a) * 1000 }')
  [ "$waited" -le "$longest" ] || longest=$waited
}
unsent() { incluster wget -qO- -T 5 "http://lapilli:$metrics/metrics" | sed -nE 's/^lapilli_serve_notices_unsent ([0-9]+)$/\1/p'; }
wait_sent() {
  for _ in $(seq 60); do
    [ "$(unsent)" = 0 ] && return 0
    sleep 2
  done
  echo "FAILED: notices still unsent: $(unsent)"
  fail=1
}
# told: how many times each notice reached the webhook, by its id.
told() { kubectl -n "$ns" logs deploy/fakeslack | grep -oE '"id":"lapilli-[0-9]+"' | sort | uniq -c; }

check "three pods" "$(pods | wc -l | tr -d ' ')" '^3$'
check "a PodDisruptionBudget, one pod at a time" "$(kubectl -n "$ns" get pdb lapilli -o jsonpath='{.spec.maxUnavailable}')" '^1$'
lead=
for _ in $(seq 60); do
  lead=$(leaders)
  [ -n "$lead" ] && break
  sleep 2
done
# Lines with something in them: an empty here-string is one line to wc.
check "one replica leads" "$(grep -c . <<<"$lead" || true)" '^1$'
read -r leader replica term <<<"$lead"
echo "leader: pod $leader, replica $replica, term $term"
for i in 1 2 3 4 5 6; do task "before $i"; done
wait_sent

# The leader's process killed: no lease given up, as when a node dies.
node=$(kubectl -n "$ns" get pod "$leader" -o jsonpath='{.spec.nodeName}')
cid=$(docker exec "$node" crictl ps -q --name '^lapilli$' --label "io.kubernetes.pod.name=$leader")
pid=$(docker exec "$node" crictl inspect "$cid" | jq -r .info.pid)
killed=$(date +%s)
docker exec "$node" kill -9 "$pid"
next=
for _ in $(seq $((bound * 2))); do
  next=$(leaders | awk -v r="$replica" '$2 != r')
  [ -n "$next" ] && break
  sleep 1
done
took=$(($(date +%s) - killed))
check "another replica leads once the leader is killed" "$next" '.'
read -r _ replica2 term2 <<<"$next"
echo "took over in ${took}s: replica $replica2, term $term2"
check "within ${bound}s" "$([ "$took" -le "$bound" ] && echo within || echo "${took}s")" '^within$'
check "at a later term" "$([ "${term2:-0}" -gt "$term" ] && echo later || echo "$term2 after $term")" '^later$'
check "and one leads" "$(leaders | wc -l | tr -d ' ')" '^1$'
kubectl -n "$ns" rollout status deploy/lapilli --timeout=3m
for i in 1 2 3 4 5 6; do task "after $i"; done
wait_sent

# A rolling restart: the API answers throughout, to a client that tries a
# request a second time (one may reach a pod as it stops), and one leads
# after.
runs() { incluster wget -qO- -T 5 "$ui/api/v1/runs" >/dev/null 2>&1; }
kubectl -n "$ns" rollout restart deploy/lapilli
refused=0
while ! kubectl -n "$ns" rollout status deploy/lapilli --timeout=2s >/dev/null 2>&1; do
  runs || runs || refused=$((refused + 1))
done
check "the API answered through a rolling restart" "$refused" '^0$'
for _ in $(seq 30); do
  [ "$(leaders | wc -l | tr -d ' ')" = 1 ] && break
  sleep 2
done
check "one leads after it" "$(leaders | wc -l | tr -d ' ')" '^1$'
task "rolled"
wait_sent

# A write through the Service, kubectl exec's own time included, waits on no
# leader's job for long.
echo "the longest write through the Service: ${longest} ms"
check "no write waited long" "$([ "$longest" -le 5000 ] && echo short || echo "${longest} ms")" '^short$'

sleep 5 # a notice sent twice, after its claim, would be by now
told=$(told)
echo "$told"
check "every notice reached the webhook" "$(grep -c . <<<"$told" || true)" '^(1[3-9]|[2-9][0-9])$'
check "and none twice" "$(awk '$1 > 1' <<<"$told" | wc -l | tr -d ' ')" '^0$'
check "the tasks made, each told once" "$(kubectl -n "$ns" logs deploy/fakeslack | grep -c '"event":"task.created"')" '^13$'
for p in $(pods | cut -d' ' -f1); do
  if kubectl -n "$ns" logs "$p" | grep -q "$password"; then echo "FAILED: the ledger's password is in $p's log"; fail=1; fi
done
exit $fail
