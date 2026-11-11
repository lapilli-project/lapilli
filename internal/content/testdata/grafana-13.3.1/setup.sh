#!/bin/sh
# How the answers beside this file were made: a Grafana 13.3.1
# (grafana/grafana:13.3.1) given a folder, three template groups (one that
# doesn't parse, which Grafana refuses), two contact points (plain-hook with
# two integrations), a policy tree and four rules, then read with a Viewer
# service account's token, as lapilli reads them.
set -eu
G=${G:-http://localhost:3000}
: "${GF_ADMIN_PASSWORD:?}"
here=$(dirname "$0")
api() { curl -s -u "admin:$GF_ADMIN_PASSWORD" -H Content-Type:application/json -H X-Disable-Provenance:true "$@"; }
api -X POST "$G/api/folders" -d '{"title":"shop","uid":"shop"}'
api -X PUT "$G/api/v1/provisioning/templates/shop" -d "$(jq -n --rawfile t "$here/shop.tmpl" '{template: $t}')"
api -X PUT "$G/api/v1/provisioning/templates/broken" -d '{"template":"{{ define \"broken.message\" }}{{ .CommonLabels.x }{{ end }}"}'
api -X PUT "$G/api/v1/provisioning/templates/plain" -d '{"template":"{{ define \"plain.message\" }}{{ len .Alerts }} alerts{{ end }}"}'
api -X POST "$G/api/v1/provisioning/contact-points" -d '{"name":"shop-hook","type":"webhook","settings":{"url":"http://hooks.invalid/shop","title":"{{ template \"shop.title\" . }}","message":"{{ template \"shop.message\" . }}"}}'
api -X POST "$G/api/v1/provisioning/contact-points" -d '{"name":"plain-hook","type":"webhook","settings":{"url":"http://hooks.invalid/plain","message":"{{ template \"plain.message\" . }}"}}'
api -X POST "$G/api/v1/provisioning/contact-points" -d '{"name":"plain-hook","type":"email","settings":{"addresses":"ops@example.invalid","subject":"{{ template \"shop.title\" . }}"}}'
api -X PUT "$G/api/v1/provisioning/policies" -d '{"receiver":"plain-hook","group_by":["grafana_folder","alertname"],"routes":[{"receiver":"shop-hook","object_matchers":[["team","=","shop"]]},{"receiver":"shop-hook","object_matchers":[["service","=~","pay.*"]],"continue":true},{"receiver":"plain-hook","object_matchers":[["grafana_folder","=","shop"],["severity","!=","info"]]}]}'
base='"folderUID":"shop","ruleGroup":"g","for":"1m","condition":"A","noDataState":"OK","execErrState":"Error","data":[{"refId":"A","datasourceUid":"__expr__","relativeTimeRange":{"from":600,"to":0},"model":{"type":"math","expression":"1 > 0","refId":"A"}}]'
for r in '"title":"ShopErrors","labels":{"team":"shop","severity":"page"},"annotations":{"summary":"Shop errors","runbook_url":"https://runbooks.example/shop"}' \
	'"title":"PayLatency","labels":{"service":"payments"},"annotations":{"description":"slow"}' \
	'"title":"Disk","labels":{"severity":"info"},"annotations":{}' \
	'"title":"Routed","labels":{},"annotations":{"summary":"routed"},"notification_settings":{"receiver":"shop-hook"}'; do
	api -X POST "$G/api/v1/provisioning/alert-rules" -d "{$r,$base}"
done
