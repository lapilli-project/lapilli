#!/usr/bin/env bash
# Chart lint and the supported value combinations. Used by CI (helm-lint) and
# scripts/release-check.sh, so both check the same list.
set -euo pipefail
cd "$(dirname "$0")/.."

# A negative check is NOT `! cmd`. Bash's own manual, on `set -e`: the shell does not exit "if the
# command's return value is being inverted with a !". So every `! helm template … ` line this file
# used to hold passed whether the chart refused the values or not — all 27 of them, which was the
# entire negative coverage of charts/lapilli/values.schema.json. Proved by mutation rather than
# reasoned about: changing `! … | grep -q ServiceMonitor` to `grep -q Deployment`, which the default
# render plainly contains, still left the script exiting 0. (The note further down about `grep -qv`
# shows this shape was on our minds; the `!` was not.)
#
# So negatives go through these two, which fail loudly and say what they mean.
refuses() { # refuses <what must be refused> <command…>   — fails if the command SUCCEEDS
  local what=$1; shift
  if "$@" >/dev/null 2>&1; then
    echo "FAIL (helm-renders): the chart ACCEPTED what it must refuse — $what"
    exit 1
  fi
}
absent() { # absent <fixed string> <rendered text> <what its presence would mean>
  if grep -qF -- "$1" <<<"$2"; then
    echo "FAIL (helm-renders): the render contains '$1' — $3"
    exit 1
  fi
}

helm lint charts/lapilli >/dev/null
DEFAULT_RENDER=$(helm template lapilli charts/lapilli)
helm template lapilli charts/lapilli --set watchNamespaces='{a,b}' >/dev/null
helm template lapilli charts/lapilli --set signing.mode=static --set signing.keySecret=k >/dev/null
helm template lapilli charts/lapilli --set persistence.enabled=false >/dev/null
grep -q -- '- metrics' <<<"$(helm template lapilli charts/lapilli --set metrics.prometheusUrl=http://prom:9090)"
# prometheusUrl: an http(s) base URL with a plain host; credentials, a query string, a
# bracketed IP literal or a stray character are refused by the schema (the controller refuses
# them again at runtime, and refuses plain http to a non-local host — see
# crates/lapilli-controller/src/metrics.rs and crates/lapilli-net/src/lib.rs)
refuses "a prometheusUrl carrying credentials" \
  helm template lapilli charts/lapilli --set-string 'metrics.prometheusUrl=http://user:pw@prom:9090'
refuses "a prometheusUrl with a non-http scheme" \
  helm template lapilli charts/lapilli --set-string 'metrics.prometheusUrl=ftp://prom'
refuses "a prometheusUrl with a query string" \
  helm template lapilli charts/lapilli --set-string 'metrics.prometheusUrl=http://prom:9090/api?x=1'
refuses "a prometheusUrl with a bracketed IP literal" \
  helm template lapilli charts/lapilli --set-string 'metrics.prometheusUrl=http://[::1]:9090'
grep -q configmaps <<<"$(helm template lapilli charts/lapilli --set diffs.configMaps=true --set 'watchNamespaces={shop}')"
grep -q destinations.json <<<"$(helm template lapilli charts/lapilli --set clusterId=prod-1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]')"
# the schema rejects typos and bad values
refuses "a misspelled profile key (profile.colectors)" \
  helm template lapilli charts/lapilli --set profile.colectors=x
refuses "an undefined redaction mode" \
  helm template lapilli charts/lapilli --set redaction.mode=loose
# ConfigMap diffs without a namespace list, or with kube-system, must be refused
refuses "ConfigMap diffs with no namespace list to bound them" \
  helm template lapilli charts/lapilli --set diffs.configMaps=true
refuses "ConfigMap diffs over kube-system" \
  helm template lapilli charts/lapilli --set diffs.configMaps=true --set 'watchNamespaces={kube-system}'
# static signing without a key Secret must be refused
refuses "static signing with no key Secret" \
  helm template lapilli charts/lapilli --set signing.mode=static
# the cluster id is part of every incident id: path-safe, at most 83 characters
refuses "a clusterId with colons and slashes" \
  helm template lapilli charts/lapilli --set clusterId=arn:aws:eks:x:cluster/prod
# The chart and the CRD have to agree on what a profile name is. They did not: values.schema.json
# allowed any non-empty string while spec.profile is an object name, so `profile.name=Prod` would
# install cleanly and then have every capture rejected at admission.
refuses "a profile.name that is not an RFC 1123 subdomain" \
  helm template lapilli charts/lapilli --set profile.name=Prod_Default
# export destinations need a real, path-safe cluster id
refuses "export destinations with the default clusterId" \
  helm template lapilli charts/lapilli --set-json 'export.destinations=[{"name":"e","url":"s3://bucket/p"}]'
refuses "export destinations with a path-unsafe clusterId" \
  helm template lapilli charts/lapilli --set clusterId=arn:x:cluster/prod --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]'
# KMS signing: the controller gets the key; the chart's profile keeps a valid CRD value
K=arn:aws:kms:us-east-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab
KMS_RENDER=$(helm template lapilli charts/lapilli --set signing.mode=kms --set signing.kms.key=$K)
grep -q LAPILLI_SIGNING_KMS_KEY <<<"$KMS_RENDER"
absent 'mode: kms' "$KMS_RENDER" "the CaptureProfile must keep a signing mode the CRD accepts"
helm template lapilli charts/lapilli --set signing.mode=kms \
  --set signing.kms.key=projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1 >/dev/null
refuses "KMS signing with no key" \
  helm template lapilli charts/lapilli --set signing.mode=kms
refuses "a KMS alias instead of a key id" \
  helm template lapilli charts/lapilli --set signing.mode=kms \
  --set signing.kms.key=arn:aws:kms:us-east-1:123456789012:alias/lapilli
# the controller's own metrics: scrape annotations by default, ServiceMonitor opt-in
grep -q 'prometheus.io/path: /metrics' <<<"$DEFAULT_RENDER"
absent ServiceMonitor "$DEFAULT_RENDER" "a ServiceMonitor is opt-in; the default must not need the CRD"
grep -q ServiceMonitor <<<"$(helm template lapilli charts/lapilli --set telemetry.serviceMonitor.enabled=true)"
# `grep -qv` would be vacuous here: -q exits 0 on the first line that does NOT match, and
# almost every line does not. The assertion has to be that no line matches at all.
absent 'prometheus.io/scrape' "$(helm template lapilli charts/lapilli --set telemetry.scrapeAnnotations=false)" \
  "scrapeAnnotations=false must remove them"
# notification: a route renders a ConfigMap, the secret mount and the env, and nothing when off
R='notify.routes=[{"name":"platform","host":"hooks.slack.com","pathSecret":"lapilli-slack-hook"}]'
ROUTE_RENDER=$(helm template lapilli charts/lapilli --set-json "$R")
grep -q LAPILLI_NOTIFY_ROUTES_FILE <<<"$ROUTE_RENDER"
grep -q 'secretName: lapilli-slack-hook' <<<"$ROUTE_RENDER"
# `pathSecret` names the mount; it is not a field the controller reads. The assertion is about
# routes.json and nothing else: the first version searched the whole render, where the word also
# appears in the chart's own comment explaining that it is not a route field — so as soon as the
# check could fail (see the note at the top) it failed, on a comment. Had it been able to fail
# earlier, the likely "fix" would have been deleting that comment.
ROUTES_JSON=$(helm template lapilli charts/lapilli --set-json "$R" --show-only templates/notify.yaml \
  | grep 'routes.json:')
absent 'pathSecret' "$ROUTES_JSON" "routes.json is what the controller reads; pathSecret is chart-only sugar"
# …and the line really is the route's, so the absent() above is not passing on an empty string.
grep -q 'hooks.slack.com' <<<"$ROUTES_JSON" \
  || { echo "FAIL (helm-renders): routes.json does not carry the route's host"; exit 1; }
absent LAPILLI_NOTIFY_ROUTES_FILE "$DEFAULT_RENDER" "notification is off by default"
# a profile may only name a route the admin defined
grep -q 'route: "platform"' <<<"$(helm template lapilli charts/lapilli --set-json "$R" --set profile.notifyRoute=platform)"
refuses "a profile naming a route no route defines" \
  helm template lapilli charts/lapilli --set profile.notifyRoute=platform
refuses "a profile naming a route other than the defined one" \
  helm template lapilli charts/lapilli --set-json "$R" --set profile.notifyRoute=other
# a host is a host, not a URL, and plain HTTP needs a cluster-local host
refuses "a route host given as a URL" \
  helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"platform","host":"https://hooks.slack.com","pathSecret":"s"}]'
refuses "plain HTTP to a host outside the cluster" \
  helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"platform","host":"hooks.slack.com","pathSecret":"s","insecureHttp":true}]'
helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"r","host":"receiver.notify-e2e.svc","pathSecret":"s","insecureHttp":true}]' >/dev/null
# …including on a non-80 port: the locality check runs on the host, as the controller's does
helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"r","host":"receiver.notify-e2e.svc:8080","pathSecret":"s","insecureHttp":true}]' >/dev/null
refuses "plain HTTP to an outside host on a non-80 port" \
  helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"r","host":"hooks.slack.com:8080","pathSecret":"s","insecureHttp":true}]'
# a missing route Secret must not be able to wedge the pod
grep -q 'optional: true' <<<"$ROUTE_RENDER"
# the retrieval command a notification prints must name THIS release's Deployment
grep -q 'value: evidence-lapilli' <<<"$(helm template evidence charts/lapilli --set-json "$R")"
# a route that loads vs a route that is broken must be distinguishable without an incident
grep -q routes.json <<<"$(helm template lapilli charts/lapilli --set-json "$R" --show-only templates/notify.yaml)"
# a missing pathSecret, an unknown key and a bad detail are all refused at install time
refuses "a route with no pathSecret" \
  helm template lapilli charts/lapilli --set-json 'notify.routes=[{"name":"p","host":"h.example"}]'
refuses "a route carrying a key the chart does not define" \
  helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"p","host":"h.example","pathSecret":"s","channel":"#ops"}]'
refuses "a route with a detail level that does not exist" \
  helm template lapilli charts/lapilli --set-json \
  'notify.routes=[{"name":"p","host":"h.example","pathSecret":"s","detail":"everything"}]'
# MCP: opt-in second container, its Service and token Secret; nothing of it in the default render
MCP_RENDER=$(helm template lapilli charts/lapilli --set mcp.enabled=true)
grep -q 'name: lapilli-mcp' <<<"$MCP_RENDER"
grep -q -- '--token-file' <<<"$MCP_RENDER"
absent 'lapilli-mcp' "$DEFAULT_RENDER" "the MCP server is opt-in"
# a private mirror of the image: the pull secret reaches the pod spec, and only when named
grep -q 'imagePullSecrets' <<<"$(helm template lapilli charts/lapilli --set 'imagePullSecrets[0].name=regcred')" \
  || { echo "FAIL (helm-renders): imagePullSecrets does not reach the pod spec"; exit 1; }
grep -q 'name: regcred' <<<"$(helm template lapilli charts/lapilli --set 'imagePullSecrets[0].name=regcred')" \
  || { echo "FAIL (helm-renders): the named pull secret is not the one rendered"; exit 1; }
absent 'imagePullSecrets' "$DEFAULT_RENDER" "an empty list must render no field at all"
refuses "a pull secret entry with no name" \
  helm template lapilli charts/lapilli --set-json 'imagePullSecrets=[{"nam":"regcred"}]'
# The ServiceAccount token reaches the controller container and NOTHING else. The default is
# `automountServiceAccountToken: true`, which mounts it into every container in the pod — so the
# mcp sidecar, reachable from any pod in the cluster and in the business of unpacking tars, held
# the controller's cluster-wide read on pods, pod logs and events plus `get` on the signing-key
# Secret. The three files below are what kube-rs's in-cluster config reads; a rename or a dropped
# source breaks the controller's API access entirely, which is why they are asserted by name.
grep -q 'automountServiceAccountToken: false' <<<"$DEFAULT_RENDER"
grep -q 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount' <<<"$DEFAULT_RENDER"
grep -q 'name: kube-root-ca.crt' <<<"$DEFAULT_RENDER"
grep -q 'fieldPath: metadata.namespace' <<<"$DEFAULT_RENDER"
# …and exactly ONE container mounts it. `grep -c` on the mount path is the assertion that the mcp
# container did not quietly acquire a copy: with mcp.enabled there are two containers and one mount.
MCP_MOUNTS=$(grep -c 'mountPath: /var/run/secrets/kubernetes.io/serviceaccount' \
  <<<"$(helm template lapilli charts/lapilli --set mcp.enabled=true)")
[ "$MCP_MOUNTS" = 1 ] \
  || { echo "FAIL (helm-renders): the SA token is mounted $MCP_MOUNTS times; the mcp container must have none"; exit 1; }
# The controller reads CaptureProfiles and never writes one: a write verb would let a compromised
# controller set `redaction.mode: off` or repoint `notify.route`. The rule must be its own, because
# grouping it with incidentcaptures is how it got create/update/patch in the first place.
#
# The property is NO WRITE, not "get only". This check asserted `verbs: ["get"]` exactly, and that
# spelling was wrong in a way nothing else could see: `perms.rs` *lists* profiles, which is how the
# permission self-check narrows the collector checks to what the installed profiles need. With
# `list` denied the narrowing silently stopped and the install reported a permission no profile
# needs as missing — found by the deferred E2E, not here. So: both read verbs required, every write
# verb refused.
RBAC=$(helm template lapilli charts/lapilli --show-only templates/rbac.yaml)
PROFILE_VERBS=$(grep -A1 'resources: \["captureprofiles"\]' <<<"$RBAC" | sed -n 's/.*verbs: //p')
[ "$PROFILE_VERBS" = '["get", "list"]' ] \
  || { echo "FAIL (helm-renders): captureprofiles verbs are $PROFILE_VERBS; want [\"get\", \"list\"] — both reads (perms.rs lists them) and no write"; exit 1; }
for w in create update patch delete deletecollection; do
  case "$PROFILE_VERBS" in
    *"$w"*) echo "FAIL (helm-renders): captureprofiles must never be granted $w"; exit 1 ;;
  esac
done
absent 'resources: ["incidentcaptures", "incidentcaptures/status", "captureprofiles"]' "$RBAC" \
  "captureprofiles must not share the incidentcaptures write verbs"
# Pinning the image by digest. A tag is mutable, so an evidence recorder installed by tag records
# bundles produced by whatever that tag resolved to; `image.digest` renders repo@digest and drops
# the tag. Empty must stay valid — a private mirror has a different digest for the same image.
DIGEST=sha256:1111111111111111111111111111111111111111111111111111111111111111
DIGEST_RENDER=$(helm template lapilli charts/lapilli --set image.digest=$DIGEST --set mcp.enabled=true)
grep -q "lapilli-controller@$DIGEST" <<<"$DIGEST_RENDER"
# both containers, not just the controller
[ "$(grep -c "image: \"ghcr.io/lapilli-project/lapilli-controller@$DIGEST\"" <<<"$DIGEST_RENDER")" = 2 ] \
  || { echo "FAIL (helm-renders): the digest must pin the mcp container too"; exit 1; }
absent 'lapilli-controller@' "$DEFAULT_RENDER" "no digest is set by default; the tag is what renders"
refuses "an image digest that is not a digest" \
  helm template lapilli charts/lapilli --set-string image.digest=v0.1.0
refuses "an image digest of the wrong length" \
  helm template lapilli charts/lapilli --set-string image.digest=sha256:abc123
# The webhook NetworkPolicy is a pod-wide ingress DENY, so every port the pod serves has to be
# named in it. It carried the webhook rule alone, which cut off 8081 (/healthz, /metrics, the
# ServiceMonitor) and 8082 (mcp) while values.yaml said "restrict ingress to the webhook port".
NP_ARGS=(--set webhook.networkPolicy.enabled=true --set-json 'webhook.networkPolicy.from=[{"podSelector":{}}]')
NP_RENDER=$(helm template lapilli charts/lapilli "${NP_ARGS[@]}" --show-only templates/webhook-auth.yaml)
grep -q 'port: webhook' <<<"$NP_RENDER"
grep -q 'port: health' <<<"$NP_RENDER" \
  || { echo "FAIL (helm-renders): the webhook NetworkPolicy denies the health port, killing probes and scraping"; exit 1; }
# the health rule defaults to no `from` (from anywhere) and takes a peer list when given
grep -q 'kubernetes.io/metadata.name: monitoring' <<<"$(helm template lapilli charts/lapilli \
  "${NP_ARGS[@]}" --set-json 'webhook.networkPolicy.healthFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]' \
  --show-only templates/webhook-auth.yaml)"
absent NetworkPolicy "$DEFAULT_RENDER" "a NetworkPolicy is opt-in: the default install restricts no ingress"
refuses "a webhook NetworkPolicy with no peer list" \
  helm template lapilli charts/lapilli --set webhook.networkPolicy.enabled=true
# With the mcp container on, that same policy has to name the mcp port too — from a list the admin
# gives, since the port serves bundle contents and an empty list would allow every pod.
MCP_NP=$(helm template lapilli charts/lapilli --set mcp.enabled=true "${NP_ARGS[@]}" \
  --set-json 'mcp.networkPolicy.from=[{"podSelector":{"matchLabels":{"app":"agent"}}}]' \
  --show-only templates/webhook-auth.yaml)
grep -q 'port: mcp' <<<"$MCP_NP" \
  || { echo "FAIL (helm-renders): a webhook NetworkPolicy with mcp.enabled must re-open the mcp port"; exit 1; }
refuses "an ingress policy that would silently black-hole the mcp port" \
  helm template lapilli charts/lapilli --set mcp.enabled=true "${NP_ARGS[@]}"
# The mcp port's own policy: until it existed the port could not be restricted at all. On its own
# it must re-open the webhook and health ports, because it denies them like any ingress policy.
MCP_OWN=$(helm template lapilli charts/lapilli --set mcp.enabled=true --set mcp.networkPolicy.enabled=true \
  --set-json 'mcp.networkPolicy.from=[{"podSelector":{"matchLabels":{"app":"agent"}}}]' \
  --show-only templates/mcp.yaml)
grep -q 'name: lapilli-mcp$' <<<"$MCP_OWN"
grep -q 'port: mcp, protocol: TCP' <<<"$MCP_OWN"
for p in webhook health; do
  grep -q "port: $p" <<<"$MCP_OWN" \
    || { echo "FAIL (helm-renders): the mcp NetworkPolicy alone denies the $p port"; exit 1; }
done
absent 'kind: NetworkPolicy' "$(helm template lapilli charts/lapilli --set mcp.enabled=true --show-only templates/mcp.yaml)" \
  "the mcp NetworkPolicy is opt-in"
refuses "an mcp NetworkPolicy with no peer list" \
  helm template lapilli charts/lapilli --set mcp.enabled=true --set mcp.networkPolicy.enabled=true
# Export destinations: plaintext HTTP only to an endpoint that cannot resolve outside the cluster,
# the same rule notify.routes[].insecureHttp gets — otherwise `allowHttp: true` shipped every
# sealed bundle over the wire in the clear, and neither field was constrained at all.
helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"http://minio.minio-e2e.svc:9000","allowHttp":true}]' >/dev/null
refuses "plaintext export to an endpoint outside the cluster" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"https://s3.evil.example","allowHttp":true}]'
refuses "plaintext export to a host that only looks loopback" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"http://127.evil.example","allowHttp":true}]'
refuses "allowHttp with no endpoint at all" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","allowHttp":true}]'
refuses "a plain-http endpoint that allowHttp does not permit" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"http://minio.minio-e2e.svc:9000"}]'
refuses "an export endpoint carrying credentials" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"https://user:pw@minio.example"}]'
refuses "an export endpoint with a query string" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"https://minio.example/?x=1"}]'
refuses "an export endpoint with a bracketed IP literal" \
  helm template lapilli charts/lapilli --set clusterId=p1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://b/p","endpoint":"http://[::1]:9000","allowHttp":true}]'
absent 'allowHttp' "$DEFAULT_RENDER" "no destination, so nothing about plaintext export in the render"
# the perishable profile: deferred collectors reach the CaptureProfile. The overlap refusal
# (a name in both lists) is a CEL rule on the CRD, so it is checked server-side in the E2E,
# not here.
grep -q 'deferred' <<<"$(helm template lapilli charts/lapilli --set 'profile.deferred={logs,metrics}' \
  --set 'profile.collectors={resources,changes}' --show-only templates/captureprofile.yaml)"
# Strict decoding is NOT checked here: it needs the API server's openapi, so it cannot run
# without a cluster. It lives in test/e2e/run.sh instead, as a SERVER-side dry run against
# the kind cluster — which is stronger anyway, because that is what reveals a field the API
# server would silently prune. (A first attempt at it here talked to whatever kubectl
# context happened to be current, which is the same defect it was added to catch.)
echo "helm: lint and renders OK"
