#!/usr/bin/env bash
# The whole service, as an operations team runs it, on one kind cluster:
#
#   a shop (test/e2e/service.yaml) whose share of failed requests the test
#     sets, scraped by Prometheus;
#   Grafana, with the shop's alert rule provisioned, sending its
#     notifications, signed, to Lapilli's notifications port, and showing
#     the dashboard the Lapilli chart ships;
#   the Lapilli chart in serve mode, from this checkout, with an image built
#     from it: the rules read from Grafana's API, user impact from an
#     expression over the shop's errors, the shop's deploys from a
#     ConfigMap, people signed in through Dex by their roles, the UI at a
#     LoadBalancer.
#
# Then what a team lives through, in about seven minutes: errors creep in
# and the alert fires; a minute later users are hurt for two; it's fixed,
# and the alert resolves; a deploy goes out whose new version fails some
# requests, and the alert fires again, within the hour and during the
# deploy; a customer reports it, an editor writes it down, and it's fixed.
# Lapilli must then say so, read through the load balancer by someone
# signed in at Dex: the user impact caught with a lead and the incident
# written down caught, on /incidents; the rule re-fired; its firing during
# the deploy marked; /api/v1/summary agreeing with the pages; each firing
# Grafana sent a notification for recorded and tied to the firing Lapilli
# replayed, which starts and ends when Grafana's did; /metrics holding
# every series the dashboard reads; and, asked through Grafana's API,
# every panel of that dashboard returning data. Last, a headless Chromium
# signs in at Dex and opens the main pages, saving a screenshot of each to
# SHOTS (by default the work directory's screenshots/).
#
# What makes it fast, and why it still means what it would in production:
# - Prometheus scrapes every 5s and Grafana evaluates every 10s, with a
#   30s for: the alert fires within a minute. Lapilli replays at
#   --step=10s, so it evaluates the rule every 10s as Grafana does (a step
#   that divides the group's interval): its firings start and end at
#   Grafana's evaluations, which the test holds it to.
# - serve.every=1m (1h by default; a minute is the least) runs it every
#   minute, and serve.settle=15m replays the last 15 minutes again each
#   run, as the 1h default replays points that came late: each run judges
#   the whole scenario again, so what it ends with doesn't depend on when
#   the runs fell. serve.backfill=1h is the least a first start replays.
# - --impact-for=1m (5m by default): two minutes of users hurt make a
#   window, as twenty would with 5m.
# - --lead=2m (15m by default): a firing up to two minutes before an
#   incident catches it. The scenario's incidents are minutes apart, not
#   hours, so the first firing, five minutes before the incident written
#   down, would catch that too with 15m; 2m keeps each to its own, as 15m
#   does for incidents an hour apart.
# - Grafana's policy sends at once (group_wait 1s): notifications come
#   seconds after a firing, not half a minute.
# --refire (1h) and --deploy-after (10m) keep their defaults: the re-fire
# and the firing during the deploy come within them as they would.
#
# Needs Docker, Go, kubectl, helm, curl and jq, on Linux (where a
# LoadBalancer's address is reachable from the host); deletes the cluster
# afterwards unless KEEP is set.
# shellcheck disable=SC2016,SC2329 # PromQL and jq, not the shell's; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/e2e/signin-lib.sh
. test/e2e/signin-lib.sh

cluster=lapilli-service
arch=$(go env GOARCH)
host=lapilli.localhost
base=http://$host
issuer=http://dex.dex.svc.cluster.local:5556/dex
playwright=mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f
work=$(mktemp -d)
shots=${SHOTS:-$work/screenshots}
mkdir -p "$shots"
cpk=

# The browser's image is large: it is pulled while the cluster comes up.
docker pull -q "$playwright" >"$work/pull.log" 2>&1 &
pulling=$!

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/image/lapilli-linux-$arch" ./cmd/lapilli
docker buildx build --load --platform "linux/$arch" -t lapilli:e2e .
# As serve-lb.sh: cloud-provider-kind for LoadBalancers, on a kind and a
# node new enough for it.
go install sigs.k8s.io/kind@v0.33.0
go install sigs.k8s.io/cloud-provider-kind@v0.12.0
node=kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
bin=$(go env GOPATH)/bin

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "::group::what the cluster held"
    kubectl get all -A -o wide || true
    kubectl -n monitoring describe pods -l app.kubernetes.io/name=lapilli || true
    kubectl -n monitoring logs deploy/lapilli --tail=300 || true
    kubectl -n monitoring logs deploy/grafana --tail=100 || true
    kubectl -n dex logs deploy/dex --tail=30 || true
    grep -iE 'error|fail' "$work/cpk.log" | tail -n 50 || true
    echo "::endgroup::"
  fi
  [ -z "$cpk" ] || kill "$cpk" 2>/dev/null || true
  kill "$pulling" 2>/dev/null || true
  if [ -z "${KEEP:-}" ]; then
    "$bin/kind" delete cluster --name "$cluster" || true
    rm -rf "$work"
  fi
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

"$bin/kind" get clusters | grep -qx "$cluster" || "$bin/kind" create cluster --name "$cluster" --image "$node" --wait 3m
"$bin/kind" load docker-image lapilli:e2e --name "$cluster"
kubectl config use-context "kind-$cluster"
# A single node: load balancers may send to it.
kubectl label node "$cluster-control-plane" node.kubernetes.io/exclude-from-external-load-balancers- || true
"$bin/cloud-provider-kind" >"$work/cpk.log" 2>&1 &
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
# until_true SECONDS COMMAND...: run it until it succeeds.
until_true() {
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$end" ]; then
      echo "gave up waiting for: $*" >&2
      return 1
    fi
    sleep 3
  done
}
now() { date -u +%FT%TZ; }
say() { printf '%s  %s\n' "$(date -u +%T)" "$*"; }

# --- The cluster's services ------------------------------------------------

# The secrets Lapilli shares with Grafana (the webhook's signing secret)
# and with Dex (the client's), and the shop's deploys, none yet.
kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
kubectl -n monitoring create secret generic grafana-webhook \
  --from-literal=secret="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n monitoring create secret generic lapilli-oidc --from-literal=client-secret=lapilli-e2e-client-secret \
  --dry-run=client -o yaml | kubectl apply -f -
deploys() { # the deploys file, as a team's pipeline keeps it
  kubectl -n monitoring create configmap shop-deploys --from-file=deploys.yaml=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -
}
printf 'deploys: []\n' | deploys

# Lapilli first, so that its dashboard's ConfigMap is there for Grafana;
# its pod waits for the token Grafana gives it.
values=(
  --set mode=serve --set image.repository=lapilli --set image.tag=e2e --set image.pullPolicy=Never
  --set prometheus.url=http://prometheus:9090
  # The rules as Grafana holds them, read with a Viewer's token: what
  # Lapilli replays is what Grafana evaluates and sends notifications of,
  # by the same names, whether provisioned (as here) or made in its UI.
  --set rules.grafana.url=http://grafana --set rules.grafana.tokenSecret.name=grafana-viewer
  --set-string 'impact=shop_error_ratio > 0.1' --set 'match={service}'
  --set 'extraArgs={--step=10s,--impact-for=1m,--lead=2m}'
  --set serve.every=1m --set serve.settle=15m --set serve.backfill=1h
  --set serve.deploys.configMap.name=shop-deploys
  --set serve.notificationsAPI.grafanaSecret.name=grafana-webhook
  --set dashboard.enabled=true
  --set serve.service.type=LoadBalancer --set "serve.externalURL=$base"
  --set "serve.signIn.oidc.issuer=$issuer" --set serve.signIn.oidc.clientId=lapilli
  --set serve.signIn.oidc.clientSecret.name=lapilli-oidc --set 'serve.signIn.oidc.scopes={email,profile,groups}'
  --set 'serve.roles.admins={group:ops}' --set 'serve.roles.editors={user:ed@example.com}'
  --set 'serve.roles.viewers={viewer@example.com}'
)
helm upgrade --install lapilli charts/lapilli -n monitoring "${values[@]}"
kubectl apply -f test/e2e/service.yaml
kubectl -n shop rollout status deploy/shop --timeout=3m
kubectl -n monitoring rollout status deploy/prometheus deploy/grafana --timeout=5m
kubectl -n dex rollout status deploy/dex --timeout=3m

gfip=$(until_set 480 kubectl -n monitoring get service grafana -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
gf=http://$gfip
gapi() { curl -fsS -u admin:e2e-admin-pass -H 'Content-Type: application/json' "$@"; }
until_true 180 gapi "$gf/api/health"
check "Grafana took the shop's alert rule" "$(gapi "$gf/api/v1/provisioning/alert-rules/shop-errors" | jq -r '.title + " " + .provenance')" '^ShopErrors file$'
check "and Lapilli as its contact point, signing what it sends" \
  "$(gapi "$gf/api/v1/provisioning/contact-points" | jq -r '.[] | select(.name == "lapilli") | .settings.url + " " + .settings.hmacConfig.timestampHeader')" \
  '^http://lapilli.monitoring.svc.cluster.local:8083/v1/grafana X-Grafana-Alerting-Timestamp$'
id=$(gapi -X POST "$gf/api/serviceaccounts" -d '{"name": "lapilli", "role": "Viewer"}' | jq -r .id)
gapi -X POST "$gf/api/serviceaccounts/$id/tokens" -d '{"name": "lapilli"}' | jq -j .key >"$work/viewer-token"
kubectl -n monitoring create secret generic grafana-viewer --from-file=token="$work/viewer-token" --dry-run=client -o yaml | kubectl apply -f -
# The pod that waited for the token is made again, which mounts it at once
# rather than when the kubelet next tries (up to two minutes on).
kubectl -n monitoring delete pod -l app.kubernetes.io/instance=lapilli --wait=false
# Ready once its first run is recorded: the rules read from Grafana, and
# the hour before replayed.
kubectl -n monitoring rollout status deploy/lapilli --timeout=5m

lb=$(until_set 480 kubectl -n monitoring get service lapilli-ui -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
dexip=$(until_set 480 kubectl -n dex get service dex -o 'jsonpath={.status.loadBalancer.ingress[0].ip}')
echo "Lapilli's load balancer $lb, Grafana's $gfip, Dex's $dexip"
# The browser reaches each name at the load balancer in front of it, as
# DNS would send it.
CURL_EXTRA=(--connect-to "$host:80:$lb:80" --connect-to "dex.dex.svc.cluster.local:5556:$dexip:5556")
code() { browse "$work/none" -o /dev/null -w '%{http_code} %{redirect_url}' "$@"; }
healthy() { test "$(code "$base/healthz")" = "200 "; }
until_true 240 healthy
check "the load balancer: a page sends the browser to sign in" "$(code "$base/rules")" "^303 $base/auth/login\?next=%2Frules$"
check "boss signs in at Dex" "$(signin "$work/boss" "$base" boss@example.com password)" "^200 $base/rules$"
check "boss is an admin, by Dex's group" "$(browse "$work/boss" "$base/api/v1/me")" '"role":"admin"'
signin "$work/ed" "$base" ed@example.com password >/dev/null
check "ed is an editor" "$(browse "$work/ed" "$base/api/v1/me")" '"role":"editor"'
check "boss makes the shop's team, of its rules" \
  "$(browse "$work/boss" -X POST -H 'Content-Type: application/json' -d '{"name": "shop", "rules": "^Shop"}' "$base/api/v1/teams")" '"name": ?"shop"'

# --- What happens -----------------------------------------------------------

errors() { # the share of the shop's requests that fail, from now
  kubectl -n shop exec deploy/shop -- sh -c "echo $1 >/tmp/errors"
}
state() { # ShopErrors's state in Grafana: Normal, Pending or Alerting
  gapi "$gf/api/prometheus/grafana/api/v1/alerts" |
    jq -r '[.data.alerts[] | select(.labels.alertname == "ShopErrors") | .state][0] // "Normal" | sub("^Normal.*"; "Normal")'
}
in_state() { test "$(state)" = "$1"; }
until_state() { until_true 180 in_state "$1"; }
day0=$(date -u +%F)

errors 0.04
say "4% of the shop's requests fail"
until_state Alerting
say "Grafana fires ShopErrors"
sleep 60
errors 0.3
say "30% fail: users are hurt"
sleep 120
errors 0
say "fixed"
until_state Normal
say "ShopErrors resolves"

# A deploy, and its record, as the pipeline that deploys would write it.
old=$(kubectl -n shop get pods -l app=shop -o name)
deploy_start=$(now)
kubectl -n shop set env deploy/shop VERSION=v2
kubectl -n shop rollout status deploy/shop --timeout=2m
# shellcheck disable=SC2086 # the pods' names, one a word
kubectl -n shop wait --for=delete $old --timeout=1m
deploy_end=$(now)
printf 'deploys:\n  - labels: {namespace: shop, service: shop}\n    start: %s\n    end: %s\n    revision: v2\n' \
  "$deploy_start" "$deploy_end" | deploys
say "v2 deployed, from $deploy_start to $deploy_end"

errors 0.04
incident_start=$(now)
say "v2 fails 4% of the shop's requests"
until_state Alerting
say "ShopErrors fires again"
sleep 20
errors 0
incident_end=$(now)
say "fixed"
until_state Normal
say "ShopErrors resolves"
written=$(browse "$work/ed" -X POST -H 'Content-Type: application/json' \
  -d '{"title": "Checkout fails for some customers after the v2 deploy", "start": "'"$incident_start"'", "end": "'"$incident_end"'", "labels": {"service": ["shop"]}}' \
  "$base/api/v1/incidents")
check "a customer reports it, and ed writes it down" "$written" '"by": ?"ed@example.com"'
written_id=$(jq -r .id <<<"$written")

# --- What Lapilli says --------------------------------------------------------

api() { browse "$work/boss" -f "$base/api/v1/$1"; }
summary() { api "summary?from=$day0&to=$(date -u +%F)"; }
# Settled once a run has judged it all: two firings, both ended, one
# re-fired, one during the deploy, the impact window and the incident
# written down each caught, and Grafana's four notifications kept.
settled() {
  summary | jq -e '[.rows[]] | (map(.firings) | add) == 2 and (map(.refired.firings) | add) == 1 and
    (map(.during_deploys.firings) | add) == 1 and (map(.impacts.caught) | add) == 1 and (map(.incidents.caught) | add) == 1' &&
    api report | jq -e '(.firings | length) == 2 and all(.firings[]; .end != null)' &&
    api rules | jq -e '.Lines[] | select(.Name == "ShopErrors") | .Notifications == 4'
}
if ! until_true 420 settled; then
  echo "FAILED: Lapilli didn't come to what happened within seven minutes"
  summary | jq -c '.rows[]' || true
  api report | jq -c '.firings' || true
  api rules | jq -c '.Lines[] | del(.Spark, .Content)' || true
  exit 1
fi
say "Lapilli has judged it all"

sum=$(summary)
rule=$(api rules | jq -c '.Lines[] | select(.Name == "ShopErrors") | del(.Spark, .Content)')
incidents=$(api incidents)
tot() { jq "[.rows[] | $1] | add" <<<"$sum"; }
firings=$(tot .firings)
caught=$(tot .caught)
refired=$(tot .refired.firings)
deployed=$(tot .during_deploys.firings)
deployed_caught=$(tot .during_deploys.caught)
impact_lead=$(jq '[.rows[].impacts | select(.total > 0) | .median_lead_seconds][0]' <<<"$sum")
written_lead=$(jq '[.rows[].incidents | select(.total > 0) | .median_lead_seconds][0]' <<<"$sum")
# Lead as the pages write it: 1m, 1m5s or 45s, before or after.
lead_text() {
  local s=${1#-} w=before
  [ "${1#-}" = "$1" ] || w=after
  if [ "$s" -ge 60 ] && [ $((s % 60)) -eq 0 ]; then
    printf '%dm %s' $((s / 60)) "$w"
  elif [ "$s" -ge 60 ]; then
    printf '%dm%ds %s' $((s / 60)) $((s % 60)) "$w"
  else
    printf '%ds %s' "$s" "$w"
  fi
}
page() { # a page's text, its tags and the entities in it undone
  browse "$work/boss" -f "$base$1" | sed -e 's/<[^>]*>/ /g' -e 's/&#43;/+/g' -e "s/&#39;/'/g" -e 's/&quot;/"/g' -e 's/&amp;/\&/g' | tr -s ' \n' ' '
}

# The user impact, caught with a lead; the incident written down, caught.
check "the summary: the user impact caught, by an alert that came before it" "$(tot .impacts.total) $(tot .impacts.caught) $impact_lead" '^1 1 [1-9][0-9]*$'
check "the summary: the incident written down caught" "$(tot .incidents.total) $(tot .incidents.caught)" '^1 1$'
check "the incidents API: the impact window caught by ShopErrors, with the same lead" \
  "$(jq -c '[.[] | select(.origin == "impact" and .counts) | [.caught_by[0].rule, (-.caught_by[0].delay_ns / 1e9)]]' <<<"$incidents")" \
  "^\[\[\"ShopErrors\",$impact_lead\]\]$"
check "the incidents API: the incident written down caught by ShopErrors, with the same lead" \
  "$(jq -c --arg id "$written_id" '[.[] | select(.id == $id) | [.caught_by[0].rule, (-.caught_by[0].delay_ns / 1e9)]]' <<<"$incidents")" \
  "^\[\[\"ShopErrors\",$written_lead\]\]$"
incidents_page=$(page /incidents)
check "/incidents: both judged against, both caught" "$incidents_page" 'Of the 2 judged against: 2 caught, 0 caught only by noisy rules, 0 missed'
check "/incidents: the user impact, caught, and its lead" "$incidents_page" \
  "impact-[0-9T]+Z user impact \(--impact\) [-0-9: ]+ found by --impact ○ ShopErrors [-+0-9ms]+ $(lead_text "$impact_lead")"
check "/incidents: the incident written down, caught, and its lead" "$incidents_page" \
  "$written_id Checkout fails for some customers after the v2 deploy [-0-9: ]+ .* ○ ShopErrors [-+0-9ms]+ $(lead_text "$written_lead")"

# The rule: two firings, both caught; the second re-fired, during the
# deploy; four notifications, none of them of a firing Lapilli hasn't.
check "the summary agrees with the rules API" \
  "$firings $caught $refired $deployed $deployed_caught" \
  "^$(jq -r '"\(.Firings) \(.White) \(.Refired) \(.Deploys) \(.DeploysCaught)"' <<<"$rule")$"
check "which says two firings, both caught, one re-fired, during the deploy" "$firings $caught $refired $deployed $deployed_caught" '^2 2 1 1 1$'
# Its page: the numbers on its summary, the rest on its firings' tab.
rule_page=$(page /rules/ShopErrors)
rule_firings=$(page '/rules/ShopErrors?tab=firings')
check "the rule's page says so" "$rule_page" "$caught of $firings caught something"
check "the rule's page: its re-fire" "$rule_firings" "$refired of its $firings firings re-fired \(50%\)\."
check "the rule's page: the firing during the deploy" "$rule_firings" \
  "Of its firings, $deployed came during a deploy of what they are about, $deployed_caught of them catching something: precision 100% with them"
check "/rules: its re-fire, and its firing during the deploy" "$(page /rules)" \
  "ShopErrors .* $refired 50% of firings $deployed precision 100% without them"
check "/firings: the second firing, re-fired" "$(page /firings)" '○ refired ShopErrors'

# Grafana's notifications: each kept, each of a firing Lapilli replayed,
# and Lapilli's firings start and end when Grafana's alert did.
check "the notifications Grafana sent, kept: two firing, two resolved, to the receiver lapilli" \
  "$(jq -c '[.Notifications, .Untied, .ByReceiver]' <<<"$rule")" '^\[4,0,\{"lapilli":4\}\]$'
check "the rule's page: none of a firing Lapilli hasn't" "$rule_firings" \
  '4 notifications were sent for it: 0 of them repeats.* and 0 tell of no firing Lapilli has'
gapi "$gf/api/annotations?type=alert&limit=100" >"$work/history.json"
grafana_spans=$(jq -c '[.[] | select(.alertName == "ShopErrors") | {t: (.time / 1000 | floor), s: .newState, p: .prevState}] | sort_by(.t) |
  [.[] | select(.s == "Alerting") | .t] as $up | [.[] | select(.p == "Alerting") | .t] as $down | [$up, $down] | transpose' "$work/history.json")
lapilli_spans=$(api report | jq -c '[.firings[] | select(.rule == "ShopErrors") | [(.start | fromdateiso8601), (.end | fromdateiso8601)]] | sort')
check "Grafana fired twice, and Lapilli replayed each firing to Grafana's evaluation (10s)" \
  "$(jq -rn --argjson g "$grafana_spans" --argjson l "$lapilli_spans" \
    'def off: if . < 0 then -. else . end; [$g, $l] | transpose | map(.[0] as $a | .[1] as $b | ($a | length) == 2 and ($b | length) == 2 and
       (($a[0] - $b[0]) | off) <= 10 and (($a[1] - $b[1]) | off) <= 10) | "\(length) \(all)"' 2>&1) Grafana $grafana_spans Lapilli $lapilli_spans" \
  '^2 true '

# /metrics, as Prometheus scrapes it: every series the dashboard reads,
# but two kinds this service can't have. lapilli_rule_refired_weeks counts
# the whole weeks a rule re-fired in, a series only for a rule with one,
# and a cluster minutes old has no whole week (dashboard.sh, over seventeen
# days, has); lapilli_serve_source_* are of firings copied in from
# elsewhere, which nothing here does (the panels that read them read the
# other sources too).
metrics=$(kubectl -n monitoring exec deploy/prometheus -- wget -qO- http://lapilli.monitoring.svc.cluster.local:9091/metrics)
missing=
for m in $(jq -r '.panels[].targets[]?.expr, (.templating.list[].query | .query? // .)' charts/lapilli/dashboards/lapilli.json |
  grep -oE 'lapilli_[a-z_]+' | grep -vE '^(lapilli_rule_refired_weeks|lapilli_serve_source_.*)$' | sort -u); do
  grep -qE "^$m(\{| )" <<<"$metrics" || missing="$missing $m"
done
check "/metrics: every series the dashboard reads" "${missing:-none missing}" '^none missing$'
check "/metrics: Grafana's notifications, none refused" "$(grep -E '^lapilli_serve_notifications_refused_total\{source="grafana"\}' <<<"$metrics")" ' 0$'

# The dashboard, as Grafana loaded it from the chart's ConfigMap: each
# panel's every query, asked of Grafana with the variables filled in, for
# everyone and for the shop's team, must return data, as in dashboard.sh.
gapi "$gf/api/dashboards/uid/lapilli" >"$work/dashboard.json"
check "Grafana loaded the chart's dashboard from its ConfigMap" \
  "$(jq -r --slurpfile want charts/lapilli/dashboards/lapilli.json '"\(.meta.provisioned) \((.dashboard.panels | length) == ($want[0].panels | length))"' "$work/dashboard.json")" '^true true$'
ask() { # EXPR REF INSTANT: Grafana's answer to a query of the dashboard's
  gapi -X POST "$gf/api/ds/query" -d "$(jq -n --arg expr "$1" --arg ref "$2" --argjson instant "$3" \
    '{from: "now-15m", to: "now", queries: [{refId: $ref, datasource: {type: "prometheus", uid: "prom"}, expr: $expr,
      instant: $instant, range: ($instant | not), intervalMs: 15000, maxDataPoints: 200}]}')"
}
points() { jq --arg ref "$1" '[.results[$ref].frames[]?.data.values[1]? // [] | length] | add // 0'; }
# scraped EXPR: true now, and at a step of the range the panels ask for.
# Grafana ends that range at the last step (15s) before now, so a series
# scraped since has a value now but no point in the panels yet: the
# deploys' series, there once Lapilli reads the deploys file, often in
# the run that comes to the rest, came up empty so.
scraped() { test "$(ask "$1" A true | points A)" -gt 0 && test "$(ask "$1" A false | points A)" -gt 0; }
# Once Prometheus has scraped what Lapilli came to: both incidents' leads,
# the firing during the deploy and the re-fire, the shop team's.
until_true 120 scraped 'count(lapilli_incident_lead_seconds) == 2 and on () lapilli_rule_firings_during_deploys{team="shop"} == 1
  and on () lapilli_rule_refired{team="shop"} == 1'
asked=0
empty=0
for team in '.*' shop; do
  while IFS=$'\t' read -r title ref instant expr; do
    if [[ $expr == *lapilli_rule_refired_weeks* ]]; then
      printf 'skip %-4s %-45s %s (whole weeks: see /metrics above)\n' "${team/.\*/all}" "$title" "$ref"
      continue
    fi
    expr=${expr//'$instance'/.*}
    expr=${expr//'$team'/$team}
    expr=${expr//'$severity'/.*}
    expr=${expr//'$critical'/critical}
    out=$(ask "$expr" "$ref" "$instant") || out='{}'
    n=$(points "$ref" <<<"$out")
    asked=$((asked + 1))
    if [ "$n" -gt 0 ]; then
      printf 'ok   %-4s %-45s %s %d\n' "${team/.\*/all}" "$title" "$ref" "$n"
    else
      printf 'NONE %-4s %-45s %s %s\n' "${team/.\*/all}" "$title" "$ref" "$(jq -c --arg ref "$ref" '.results[$ref].error // .message // ""' <<<"$out")"
      empty=$((empty + 1))
    fi
  done < <(jq -r '.dashboard.panels[] | select(.type != "row") | .title as $t | .targets[] | [$t, .refId, (.instant | tostring), .expr] | @tsv' \
    "$work/dashboard.json")
done
check "the dashboard: every panel's query returned data" "$asked queries asked, $empty returned nothing" ' 0 returned nothing$'

# --- A person at the UI -------------------------------------------------------

# Chromium, in the Playwright image, on kind's network: viewer signs in at
# Dex's form and opens the main pages, each saved as a screenshot.
wait "$pulling" || { cat "$work/pull.log"; exit 1; }
pages=$(printf '%s\t%s\n' \
  / 'Your alerts, the last twelve weeks' \
  /rules 'ShopErrors' \
  /rules/ShopErrors 'caught something' \
  '/rules/ShopErrors?tab=firings' 'came during a deploy of what they are about' \
  /incidents 'Of the 2 judged against: 2 caught' \
  /firings 'refired' \
  /teams 'shop' \
  /runs 'hourly')
if docker run --rm --init --network kind -e "BASE=$base" \
  -e "RESOLVE=MAP $host $lb, MAP dex.dex.svc.cluster.local $dexip" \
  -e WHO=viewer@example.com -e PASSWORD=password -e OUT=/shots -e "PAGES=$pages" -e npm_config_update_notifier=false \
  -v "$PWD/test/a11y:/a11y:ro" -v "$PWD/test/e2e/browse.mjs:/e2e/browse.mjs:ro" -v "$shots:/shots" "$playwright" \
  sh -c 'mkdir /tmp/w && cp /a11y/package.json /a11y/package-lock.json /e2e/browse.mjs /tmp/w/ && cd /tmp/w &&
    npm ci --ignore-scripts --no-audit --no-fund --loglevel=error && node browse.mjs'; then
  echo "ok: the browser's pages, saved in $shots"
else
  echo "FAILED: the browser's pages"
  fail=1
fi
exit $fail
