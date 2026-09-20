#!/usr/bin/env bash
# Chart lint and the supported value combinations. Used by CI (helm-lint) and
# scripts/release-check.sh, so both check the same list.
set -euo pipefail
cd "$(dirname "$0")/.."
helm lint charts/kairn >/dev/null
helm template kairn charts/kairn >/dev/null
helm template kairn charts/kairn --set watchNamespaces='{a,b}' >/dev/null
helm template kairn charts/kairn --set signing.mode=static --set signing.keySecret=k >/dev/null
helm template kairn charts/kairn --set persistence.enabled=false >/dev/null
helm template kairn charts/kairn --set metrics.prometheusUrl=http://prom:9090 | grep -q -- '- metrics'
# prometheusUrl: an http(s) base URL with a plain host; credentials, a query string, a
# bracketed IP literal or a stray character are refused by the schema (the controller refuses
# them again at runtime, and refuses plain http to a non-local host — see
# crates/kairn-controller/src/metrics.rs and crates/kairn-net/src/lib.rs)
! helm template kairn charts/kairn --set-string 'metrics.prometheusUrl=http://user:pw@prom:9090' >/dev/null 2>&1
! helm template kairn charts/kairn --set-string 'metrics.prometheusUrl=ftp://prom' >/dev/null 2>&1
! helm template kairn charts/kairn --set-string 'metrics.prometheusUrl=http://prom:9090/api?x=1' >/dev/null 2>&1
! helm template kairn charts/kairn --set-string 'metrics.prometheusUrl=http://[::1]:9090' >/dev/null 2>&1
helm template kairn charts/kairn --set diffs.configMaps=true --set 'watchNamespaces={shop}' | grep -q configmaps
helm template kairn charts/kairn --set clusterId=prod-1 --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' | grep -q destinations.json
# the schema rejects typos and bad values
! helm template kairn charts/kairn --set profile.colectors=x >/dev/null 2>&1
! helm template kairn charts/kairn --set redaction.mode=loose >/dev/null 2>&1
# ConfigMap diffs without a namespace list, or with kube-system, must be refused
! helm template kairn charts/kairn --set diffs.configMaps=true >/dev/null 2>&1
! helm template kairn charts/kairn --set diffs.configMaps=true --set 'watchNamespaces={kube-system}' >/dev/null 2>&1
# static signing without a key Secret must be refused
! helm template kairn charts/kairn --set signing.mode=static >/dev/null 2>&1
# the cluster id is part of every incident id: path-safe, at most 83 characters
! helm template kairn charts/kairn --set clusterId=arn:aws:eks:x:cluster/prod >/dev/null 2>&1
# export destinations need a real, path-safe cluster id
! helm template kairn charts/kairn --set-json 'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' >/dev/null 2>&1
! helm template kairn charts/kairn --set clusterId=arn:x:cluster/prod --set-json \
  'export.destinations=[{"name":"e","url":"s3://bucket/p"}]' >/dev/null 2>&1
# KMS signing: the controller gets the key; the chart's profile keeps a valid CRD value
K=arn:aws:kms:us-east-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab
helm template kairn charts/kairn --set signing.mode=kms --set signing.kms.key=$K | grep -q KAIRN_SIGNING_KMS_KEY
! helm template kairn charts/kairn --set signing.mode=kms --set signing.kms.key=$K | grep -q 'mode: kms'
helm template kairn charts/kairn --set signing.mode=kms \
  --set signing.kms.key=projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1 >/dev/null
! helm template kairn charts/kairn --set signing.mode=kms >/dev/null 2>&1
! helm template kairn charts/kairn --set signing.mode=kms \
  --set signing.kms.key=arn:aws:kms:us-east-1:123456789012:alias/kairn >/dev/null 2>&1
# the controller's own metrics: scrape annotations by default, ServiceMonitor opt-in
helm template kairn charts/kairn | grep -q 'prometheus.io/path: /metrics'
! helm template kairn charts/kairn | grep -q ServiceMonitor
helm template kairn charts/kairn --set telemetry.serviceMonitor.enabled=true | grep -q ServiceMonitor
# `grep -qv` would be vacuous here: -q exits 0 on the first line that does NOT match, and
# almost every line does not. The assertion has to be that no line matches at all.
! helm template kairn charts/kairn --set telemetry.scrapeAnnotations=false | grep -q 'prometheus.io/scrape'
# notification: a route renders a ConfigMap, the secret mount and the env, and nothing when off
R='notify.routes=[{"name":"platform","host":"hooks.slack.com","pathSecret":"kairn-slack-hook"}]'
helm template kairn charts/kairn --set-json "$R" | grep -q KAIRN_NOTIFY_ROUTES_FILE
helm template kairn charts/kairn --set-json "$R" | grep -q 'secretName: kairn-slack-hook'
# `pathSecret` names the mount; it is not a field the controller reads
! helm template kairn charts/kairn --set-json "$R" | grep -q 'pathSecret'
! helm template kairn charts/kairn | grep -q KAIRN_NOTIFY_ROUTES_FILE
# a profile may only name a route the admin defined
helm template kairn charts/kairn --set-json "$R" --set profile.notifyRoute=platform | grep -q 'route: "platform"'
! helm template kairn charts/kairn --set profile.notifyRoute=platform >/dev/null 2>&1
! helm template kairn charts/kairn --set-json "$R" --set profile.notifyRoute=other >/dev/null 2>&1
# a host is a host, not a URL, and plain HTTP needs a cluster-local host
! helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"platform","host":"https://hooks.slack.com","pathSecret":"s"}]' >/dev/null 2>&1
! helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"platform","host":"hooks.slack.com","pathSecret":"s","insecureHttp":true}]' >/dev/null 2>&1
helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"r","host":"receiver.notify-e2e.svc","pathSecret":"s","insecureHttp":true}]' >/dev/null
# …including on a non-80 port: the locality check runs on the host, as the controller's does
helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"r","host":"receiver.notify-e2e.svc:8080","pathSecret":"s","insecureHttp":true}]' >/dev/null
! helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"r","host":"hooks.slack.com:8080","pathSecret":"s","insecureHttp":true}]' >/dev/null 2>&1
# a missing route Secret must not be able to wedge the pod
helm template kairn charts/kairn --set-json "$R" | grep -q 'optional: true'
# the retrieval command a notification prints must name THIS release's Deployment
helm template evidence charts/kairn --set-json "$R" | grep -q 'value: evidence-kairn'
# a route that loads vs a route that is broken must be distinguishable without an incident
helm template kairn charts/kairn --set-json "$R" --show-only templates/notify.yaml | grep -q routes.json
# a missing pathSecret, an unknown key and a bad detail are all refused at install time
! helm template kairn charts/kairn --set-json 'notify.routes=[{"name":"p","host":"h.example"}]' >/dev/null 2>&1
! helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"p","host":"h.example","pathSecret":"s","channel":"#ops"}]' >/dev/null 2>&1
! helm template kairn charts/kairn --set-json \
  'notify.routes=[{"name":"p","host":"h.example","pathSecret":"s","detail":"everything"}]' >/dev/null 2>&1
# Strict decoding is NOT checked here: it needs the API server's openapi, so it cannot run
# without a cluster. It lives in test/e2e/run.sh instead, as a SERVER-side dry run against
# the kind cluster — which is stronger anyway, because that is what reveals a field the API
# server would silently prune. (A first attempt at it here talked to whatever kubectl
# context happened to be current, which is the same defect it was added to catch.)
echo "helm: lint and renders OK"
