# Shared by export.sh (S3) and kms.sh (KMS): stage the pinned LocalStack image into the kind
# node and run one instance in a namespace of the caller's choosing. Sourced, not executed.
. "$(dirname "${BASH_SOURCE[0]}")/images.env"

localstack_stage() { # pull by digest if absent, check the pin, load into the node
  docker image inspect "$LOCALSTACK" >/dev/null 2>&1 || docker pull -q "$LOCALSTACK@$LOCALSTACK_DIGEST" >/dev/null
  docker image inspect "$LOCALSTACK@$LOCALSTACK_DIGEST" >/dev/null 2>&1 \
    || { echo "FAIL: $LOCALSTACK in the Docker cache does not carry $LOCALSTACK_DIGEST" >&2; return 1; }
  docker tag "$LOCALSTACK@$LOCALSTACK_DIGEST" "$LOCALSTACK" 2>/dev/null || true
  kind load docker-image "$LOCALSTACK" --name "${CLUSTER:-lapilli}" >/dev/null
}

localstack_deploy() { # namespace, SERVICES value (e.g. s3 or kms) → Service <ns>/localstack:4566
  local ns=$1 services=$2
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Namespace
metadata: { name: $ns }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: localstack, namespace: $ns }
spec:
  selector: { matchLabels: { app: localstack } }
  template:
    metadata: { labels: { app: localstack } }
    spec:
      containers:
        - name: localstack
          image: $LOCALSTACK
          imagePullPolicy: Never # loaded into the node by localstack_stage
          env: [{ name: SERVICES, value: $services }]
          readinessProbe: { httpGet: { path: /_localstack/health, port: 4566 } }
---
apiVersion: v1
kind: Service
metadata: { name: localstack, namespace: $ns }
spec:
  selector: { app: localstack }
  ports: [{ port: 4566 }]
YAML
  kubectl -n "$ns" rollout status deploy/localstack --timeout=300s >/dev/null
  # Ready means the process answers, not that the service is up: wait for the health verdict.
  local i
  for i in $(seq 1 60); do
    kubectl -n "$ns" exec deploy/localstack -- python3 -c \
      "import urllib.request;print(urllib.request.urlopen('http://localhost:4566/_localstack/health').read().decode())" 2>/dev/null \
      | grep -q "\"$services\": \"\(available\|running\)\"" && return 0
    sleep 2
  done
  echo "FAIL: LocalStack $services never became available in $ns" >&2
  return 1
}

awslocal_in() { # namespace, shell command run inside the LocalStack pod (awslocal is on its PATH)
  kubectl -n "$1" exec deploy/localstack -- sh -c "$2"
}
