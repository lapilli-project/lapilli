#!/usr/bin/env bash
# lapilli serve served as a team would serve it, at a URL people open: the
# chart on a kind cluster with cloud-provider-kind, which gives a
# LoadBalancer Service its address and implements Gateway API and Ingress;
# the UI reached through each of them, and people signed in by lapilli
# itself through the authorization code flow against a Dex in the cluster,
# played with curl. Then the chart's NetworkPolicy lets in what the
# Gateway sends from, and keeps a pod in the cluster out.
#
# Needs Docker, Go, kubectl and helm, on Linux (where a LoadBalancer's
# address is reachable from the host); deletes the cluster afterwards
# unless KEEP is set.
# shellcheck disable=SC2016,SC2329 # a bcrypt hash; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/e2e/signin-lib.sh
. test/e2e/signin-lib.sh

cluster=lapilli-lb
ns=e2e
arch=$(go env GOARCH)
dex=ghcr.io/dexidp/dex:v2.46.1@sha256:65b72ca0f4b0bf6d4ada84ec21da8851782563404ac7b8f926923d27ceced9ab
host=lapilli.localhost       # through the Gateway: --external-url's
ihost=ui.lapilli.localhost   # through the Ingress
issuer=http://dex.dex.svc.cluster.local:5556/dex
work=$(mktemp -d)
cpk=

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/image/lapilli-linux-$arch" ./cmd/lapilli
docker buildx build --load --platform "linux/$arch" -t lapilli:e2e .
go install sigs.k8s.io/kind@v0.27.0
go install sigs.k8s.io/cloud-provider-kind@v0.12.0
bin=$(go env GOPATH)/bin

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "::group::what the cluster held"
    kubectl get all,gateway,httproute,ingress,networkpolicy -A -o wide || true
    kubectl -n "$ns" describe pods -l app.kubernetes.io/name=lapilli || true
    kubectl -n "$ns" logs -l app.kubernetes.io/name=lapilli --tail=200 || true
    kubectl -n dex logs deploy/dex --tail=50 || true
    tail -n 100 "$work/cpk.log" || true
    echo "::endgroup::"
  fi
  [ -z "$cpk" ] || kill "$cpk" 2>/dev/null || true
  if [ -z "${KEEP:-}" ]; then
    "$bin/kind" delete cluster --name "$cluster" || true
    rm -rf "$work"
  fi
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

"$bin/kind" get clusters | grep -qx "$cluster" || "$bin/kind" create cluster --name "$cluster" --wait 2m
"$bin/kind" load docker-image lapilli:e2e --name "$cluster"
kubectl config use-context "kind-$cluster"
# A single node: load balancers may send to it.
kubectl label node "$cluster-control-plane" node.kubernetes.io/exclude-from-external-load-balancers- || true
"$bin/cloud-provider-kind" --gateway-channel standard >"$work/cpk.log" 2>&1 &
cpk=$!

fail=0
check() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
# until_set SECONDS COMMAND...: the command's output, once it gives one.
until_set() {
  local end=$(($(date +%s) + $1)) out
  shift
  while :; do
    out=$("$@" 2>/dev/null || true)
    if [ -n "$out" ]; then
      printf '%s' "$out"
      return
    fi
    if [ "$(date +%s)" -ge "$end" ]; then
      echo "gave up waiting for: $*" >&2
      return 1
    fi
    sleep 3
  done
}

kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" apply -f test/e2e/stack.yaml

# Dex, in the cluster, at a LoadBalancer of its own: lapilli reaches it by
# its cluster name, the browser by the load balancer's address. The hash
# is bcrypt's of "password", as in Dex's own examples.
hash='$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W'
kubectl create namespace dex --dry-run=client -o yaml | kubectl apply -f -
kubectl -n dex apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: dex}
data:
  config.yaml: |
    issuer: $issuer
    storage: {type: memory}
    web: {http: 0.0.0.0:5556}
    oauth2: {skipApprovalScreen: true, passwordConnector: local}
    staticClients:
      - id: lapilli
        name: lapilli
        secret: lapilli-e2e-client-secret
        redirectURIs: [http://$host/auth/callback]
    enablePasswordDB: true
    staticPasswords:
      - {email: boss@example.com, hash: "$hash", username: boss, userID: "1", groups: [ops]}
      - {email: ed@example.com, hash: "$hash", username: ed, userID: "2"}
      - {email: viewer@example.com, hash: "$hash", username: viewer, userID: "3"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: dex}
spec:
  selector: {matchLabels: {app: dex}}
  template:
    metadata: {labels: {app: dex}}
    spec:
      containers:
        - name: dex
          image: $dex
          args: [dex, serve, /etc/dex/config.yaml]
          ports: [{containerPort: 5556}]
          readinessProbe: {httpGet: {path: /dex/healthz, port: 5556}}
          volumeMounts: [{name: config, mountPath: /etc/dex}]
      volumes: [{name: config, configMap: {name: dex}}]
---
apiVersion: v1
kind: Service
metadata: {name: dex}
spec:
  type: LoadBalancer
  selector: {app: dex}
  ports: [{port: 5556, targetPort: 5556}]
EOF

# A Gateway of cloud-provider-kind's class, which it makes.
until_set 180 kubectl get gatewayclass cloud-provider-kind -o name >/dev/null
kubectl -n "$ns" apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: web}
spec:
  gatewayClassName: cloud-provider-kind
  listeners:
    - {name: http, protocol: HTTP, port: 80, allowedRoutes: {namespaces: {from: Same}}}
EOF

kubectl -n "$ns" create secret generic lapilli-oidc --from-literal=client-secret=lapilli-e2e-client-secret \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" rollout status deploy/prometheus --timeout=3m
kubectl -n dex rollout status deploy/dex --timeout=3m

values=(
  --set mode=serve --set image.repository=lapilli --set image.tag=e2e --set image.pullPolicy=Never
  --set prometheus.url=http://prometheus:9090 --set serve.backfill=1h --set 'rules.configMaps={alert-rules}'
  --set serve.service.type=LoadBalancer
  --set serve.ingress.enabled=true --set serve.ingress.className=cloud-provider-kind --set "serve.ingress.hosts={$ihost}"
  --set serve.httpRoute.enabled=true --set 'serve.httpRoute.parentRefs[0].name=web' --set "serve.httpRoute.hostnames={$host}"
  --set "serve.externalURL=http://$host"
  --set "serve.signIn.oidc.issuer=$issuer" --set serve.signIn.oidc.clientId=lapilli
  --set serve.signIn.oidc.clientSecret.name=lapilli-oidc --set 'serve.signIn.oidc.scopes={email,profile,groups}'
  --set 'serve.roles.admins={group:ops}' --set 'serve.roles.editors={user:ed@example.com}'
)
helm upgrade --install lapilli charts/lapilli -n "$ns" --wait --timeout 5m "${values[@]}"

lb=$(until_set 180 kubectl -n "$ns" get service lapilli -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
dexip=$(until_set 180 kubectl -n dex get service dex -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
gw=$(until_set 180 kubectl -n "$ns" get gateway web -o 'jsonpath={.status.addresses[0].value}')
# The Ingress's address, as its status says, or as the Gateway
# cloud-provider-kind makes for the namespace's Ingresses does.
ingress_ip() {
  local ip
  ip=$(kubectl -n "$ns" get ingress lapilli -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
  [ -n "$ip" ] || ip=$(kubectl -n "$ns" get gateway kind-ingress-gateway -o 'jsonpath={.status.addresses[0].value}')
  printf '%s' "$ip"
}
ing=$(until_set 180 ingress_ip)
echo "load balancer $lb, Gateway $gw, Ingress $ing, Dex $dexip"
# The browser reaches each name at the address in front of it.
CURL_EXTRA=(--connect-to "$host:80:$gw:80" --connect-to "$ihost:80:$ing:80" --connect-to "dex.dex.svc.cluster.local:5556:$dexip:5556")
base=http://$host
code() { browse "$work/none" -o /dev/null -w '%{http_code} %{redirect_url}' "$@"; }
for _ in $(seq 60); do
  [ "$(code "$base/healthz")" = "200 " ] && [ "$(code "http://$ihost/healthz")" = "200 " ] && break
  sleep 3
done

# The UI through each: the load balancer's address, the Ingress's host and
# the Gateway's; a page sends the browser to sign in at --external-url.
check "the load balancer: the health check" "$(code "http://$lb:8080/healthz")" '^200 $'
check "the load balancer: a page sends the browser to sign in" "$(code "http://$lb:8080/rules")" "^303 $base/auth/login\?next=%2Frules$"
check "the load balancer: the metrics port isn't on it" "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$lb:9091/metrics" || true)" '^000$'
check "the Ingress: a page sends the browser to sign in" "$(code "http://$ihost/rules")" "^303 $base/auth/login\?next=%2Frules$"
check "the Gateway: a page sends the browser to sign in" "$(code "$base/rules")" "^303 $base/auth/login\?next=%2Frules$"
check "the Gateway: the API, not signed in" "$(code "$base/api/v1/me")" '^401 $'
check "another host name is refused" "$(browse "$work/none" -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' "http://$lb:8080/rules")" '^421$'

# Signed in through Dex, by the Gateway.
check "boss signs in, and comes back to the page" "$(signin "$work/boss" "$base" boss@example.com password)" "^200 $base/rules$"
check "boss is an admin, by Dex's group" "$(browse "$work/boss" "$base/api/v1/me")" '"role":"admin"'
signin "$work/ed" "$base" ed@example.com password >/dev/null || fail=1
check "ed is an editor" "$(browse "$work/ed" "$base/api/v1/me")" '"role":"editor"'
written=$(browse "$work/ed" -X POST -H 'Content-Type: application/json' \
  -d '{"title": "written through the Gateway", "start": "2026-11-02T06:00:00Z"}' "$base/api/v1/incidents")
check "ed writes an incident" "$written" '"by": ?"ed@example.com"'
signin "$work/viewer" "$base" viewer@example.com password >/dev/null || fail=1
check "viewer may only look" "$(browse "$work/viewer" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"title": "x", "start": "2026-11-02T06:00:00Z"}' "$base/api/v1/incidents")" '^403$'
token=$(browse "$work/none" -u lapilli:lapilli-e2e-client-secret --data-urlencode grant_type=password \
  --data-urlencode username=ed@example.com --data-urlencode password=password --data-urlencode 'scope=openid email groups' \
  "$issuer/token" | sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p')
check "an ID token as a bearer token, through the load balancer" \
  "$(curl -s -H "Authorization: Bearer $token" "http://$lb:8080/api/v1/me")" '"role":"editor"'
cp "$work/boss" "$work/boss-copy"
check "signing out" "$(browse "$work/boss" -o /dev/null -w '%{http_code} %{redirect_url}' -X POST "$base/auth/logout")" "^303 $base/auth/signed-out$"
check "a copy of the cookie after signing out" "$(browse "$work/boss-copy" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^401$'

# The NetworkPolicy: the UI's port lets in what the Gateway, the Ingress
# and the load balancer send from (kind's Docker network), and nothing in
# the cluster; the kubelet's probes reach the metrics port.
subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -v : | head -1)
helm upgrade lapilli charts/lapilli -n "$ns" --wait --timeout 5m "${values[@]}" \
  --set serve.networkPolicy.enabled=true --set "serve.networkPolicy.from[0].ipBlock.cidr=$subnet" \
  --set "serve.networkPolicy.metricsFrom[0].ipBlock.cidr=$subnet"
# A policy takes a moment to be enforced: a pod in the cluster is kept out
# within a minute, and what is in front is let in all along.
kept=
for _ in $(seq 12); do
  if ! kubectl -n "$ns" exec deploy/pushgateway -- wget -q -T 5 -O- http://lapilli:8080/healthz >/dev/null 2>&1; then
    kept=1
    break
  fi
  sleep 5
done
if [ -n "$kept" ]; then echo "ok: a pod in the cluster is kept out"; else
  echo "FAILED: a pod in the cluster reached the UI's port past the NetworkPolicy"
  fail=1
fi
check "with the NetworkPolicy, the Gateway is let in" "$(browse "$work/ed" "$base/api/v1/me")" '"role":"editor"'
check "and the Ingress" "$(code "http://$ihost/healthz")" '^200 $'
check "and the load balancer" "$(code "http://$lb:8080/healthz")" '^200 $'
check "and the pod is ready, its probes let in" "$(kubectl -n "$ns" get pods -l app.kubernetes.io/name=lapilli -o 'jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}')" '^True$'
exit $fail
