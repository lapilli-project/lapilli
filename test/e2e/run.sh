#!/usr/bin/env bash
# Runs the chart's CronJob on a kind cluster against a real Prometheus, and
# checks the report, the Pushgateway and the Slack post. Needs Docker, Go,
# kubectl and helm; deletes the cluster afterwards unless KEEP is set.
set -euo pipefail
cd "$(dirname "$0")/../.."
cluster=lapilli-e2e
ns=e2e
arch=$(go env GOARCH)

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/image/lapilli-linux-$arch" ./cmd/lapilli
docker buildx build --load --platform "linux/$arch" -t lapilli:e2e .

go install sigs.k8s.io/kind@v0.27.0
kind="$(go env GOPATH)/bin/kind"
"$kind" get clusters | grep -qx "$cluster" || "$kind" create cluster --name "$cluster" --wait 2m
trap '[ -n "${KEEP:-}" ] || "$kind" delete cluster --name "$cluster"' EXIT
"$kind" load docker-image lapilli:e2e --name "$cluster"
kubectl config use-context "kind-$cluster"

kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" apply -f test/e2e/stack.yaml
kubectl -n "$ns" rollout status deploy/prometheus deploy/pushgateway deploy/fakeslack --timeout=3m

# The incident starts when LateStart's condition does; wait until it has
# fired and a little history has built up.
query='http://prometheus:9090/api/v1/query?query=process_start_time_seconds%7Bjob%3D%22prometheus%22%7D'
started=
until [ -n "$started" ]; do
  sleep 5
  started=$({ kubectl -n "$ns" exec deploy/pushgateway -- wget -qO- "$query" || true; } |
    sed -nE 's/.*"value":\[[0-9.]+,"([0-9]+)(\.[0-9]*)?"\].*/\1/p')
done
at=$((started + 90))
incident=$(date -u -d "@$at" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$at" +%Y-%m-%dT%H:%M:%SZ)
until [ "$(date +%s)" -gt $((started + 240)) ]; do sleep 10; done

helm upgrade --install lapilli charts/lapilli -n "$ns" --wait \
  --set image.repository=lapilli --set image.tag=e2e --set image.pullPolicy=Never \
  --set prometheus.url=http://prometheus:9090 --set since=30m \
  --set 'rules.configMaps={alert-rules}' \
  --set report.pushgateway=http://pushgateway:9091 \
  --set report.slackWebhookSecret.name=slack \
  -f - <<VALUES
incidents:
  - {id: INC-E2E, title: late start, start: "$incident"}
VALUES

kubectl -n "$ns" delete job lapilli-now --ignore-not-found
kubectl -n "$ns" create job lapilli-now --from=cronjob/lapilli
if ! kubectl -n "$ns" wait --for=condition=complete job/lapilli-now --timeout=5m; then
  kubectl -n "$ns" describe job/lapilli-now
  kubectl -n "$ns" logs job/lapilli-now || true
  exit 1
fi

fail=0
check() { # what, text, pattern
  if grep -qE "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1"; echo "$2"; fail=1; fi
}
report=$(kubectl -n "$ns" logs job/lapilli-now)
check "report: LateStart caught the incident" "$report" 'INC-E2E late start .* LateStart [+-]'
check "report: Never never fired" "$report" '1 rules never fired'
if grep -q e2e-secret <<<"$report"; then echo "FAILED: the webhook URL is in the log"; fail=1; fi
metrics=$(kubectl -n "$ns" exec deploy/pushgateway -- wget -qO- http://localhost:9091/metrics)
check "pushgateway: LateStart's white stone, in this release's group" "$metrics" 'lapilli_rule_caught\{instance="e2e-lapilli",job="lapilli",rule="LateStart"\} 1'
check "pushgateway: one incident caught" "$metrics" 'lapilli_incidents\{.*source="written".*state="caught".*\} 1'
posted=$(kubectl -n "$ns" logs deploy/fakeslack)
check "slack: the report was posted" "$posted" 'POST .*INC-E2E late start'

# A change to the rules, judged before it is merged: LateStart made to wait
# an hour no longer catches the incident, and Never is taken away.
go build -o dist/lapilli ./cmd/lapilli
kubectl -n "$ns" get configmap alert-rules -o jsonpath='{.data.alerts\.yaml}' >dist/rules-now.yaml
cat >dist/rules-proposed.yaml <<'RULES'
groups:
  - name: e2e
    rules:
      - alert: LateStart
        expr: time() - process_start_time_seconds{job="prometheus"} > 90
        for: 1h
RULES
printf 'incidents:\n  - {id: INC-E2E, title: late start, start: "%s"}\n' "$incident" >dist/incidents.yaml
kubectl -n "$ns" port-forward svc/prometheus 19090:9090 >/dev/null &
forward=$!
for _ in $(seq 30); do curl -fs http://localhost:19090/-/ready >/dev/null && break; sleep 1; done
compared=$(dist/lapilli verdict --rules dist/rules-proposed.yaml --compare dist/rules-now.yaml \
  --prometheus http://localhost:19090 --since 30m --incidents dist/incidents.yaml) || fail=1
check "compare: one rule changed, one taken away" "$compared" 'Rules compared: 1 changed, 0 added, 1 taken away, 0 the same'
check "compare: LateStart caught the incident before" "$compared" '^  before  LateStart [+-][0-9a-z]+$'
check "compare: and doesn't after" "$compared" '^  after   ✗ nothing fired$'

# The rules Prometheus itself has loaded, read through its API.
loaded=$(dist/lapilli verdict --loaded-rules --prometheus http://localhost:19090 --since 30m \
  --incidents dist/incidents.yaml) || fail=1
kill "$forward" || true
check "loaded rules: LateStart caught the incident" "$loaded" 'INC-E2E late start .* LateStart [+-]'
check "loaded rules: Never never fired" "$loaded" '1 rules never fired'
exit $fail
