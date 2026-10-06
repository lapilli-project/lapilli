#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
. ../lib.sh

# node-local tuning file, managed outside Kubernetes (as a config-management tool would)
for n in rcabench-worker rcabench-worker2; do docker exec "$n" sh -c 'mkdir -p /etc/ledger-tuning && echo "max_open_files=65536" > /etc/ledger-tuning/tuning.conf'; done
kubectl create namespace ledger
kubectl -n ledger create configmap ledger-code --from-file=ledger_api.py=code/ledger_api.py
kubectl create namespace infra-agents
kubectl -n infra-agents create configmap node-agent-code --from-file=node_agent.py=code/node_agent.py
kubectl apply -f manifest.yaml
kubectl -n ledger rollout status deploy/ledger-api --timeout=240s
kubectl -n infra-agents rollout status ds/node-agent --timeout=240s
sleep 45
# today's rollout (decoy): version bump, completes healthy on both nodes
kubectl -n ledger set env deploy/ledger-api APP_VERSION=1.4.2
kubectl -n ledger rollout status deploy/ledger-api --timeout=240s
sleep 75
# drift appears on one node only, a couple of minutes after the rollout finished
docker exec rcabench-worker2 sh -c 'echo "max_open_files=64" > /etc/ledger-tuning/tuning.conf'
for i in $(seq 1 40); do
  nr=$(kubectl -n ledger get pods -l app=ledger-api --no-headers | awk '$2=="0/1"' | wc -l | tr -d ' ')
  if [ "$nr" -ge 3 ]; then sleep 40; echo "symptom present: $nr pods NotReady"; kubectl -n ledger get pods -o wide --no-headers | awk '{print $1, $2, $7}'; exit 0; fi
  sleep 5
done
echo "symptom did not form"; exit 1
