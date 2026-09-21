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
# Strict decoding is NOT checked here: it needs the API server's openapi, so it cannot run
# without a cluster. It lives in test/e2e/run.sh instead, as a SERVER-side dry run against
# the kind cluster — which is stronger anyway, because that is what reveals a field the API
# server would silently prune. (A first attempt at it here talked to whatever kubectl
# context happened to be current, which is the same defect it was added to catch.)
echo "helm: lint and renders OK"
