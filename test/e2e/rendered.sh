#!/usr/bin/env bash
# Rules a chart renders, kept in git and followed (docs/design/history.md),
# against real servers. A real Gitea holds a small Helm chart of Grafana
# alert rules, in ConfigMaps labelled for Grafana's sidecar, beside a
# Prometheus rule file in a ConfigMap that isn't, a Deployment, a Service
# and a Secret. Its subchart comes from Gitea's Helm registry, and the
# first commit doesn't vendor it. The chart's own rules history
# containers, as helm template writes them (images, scripts, environment,
# the clone read-only where render runs), run in Docker as the pod would
# run them: the clone and the versions, then the fetch and the follow
# beside lapilli serve.
#
# A real Prometheus holds three days of e2e_errors, written with promtool:
# up for half an hour 60, 36 and 12 hours ago. The rules' threshold is
# raised 48 hours ago and lowered again 24 hours ago, so the rules of each
# day fire 60 and 12 hours ago, and today's would fire all three times.
# Then a commit dated 14 hours ago raises it again: fetched now, it is
# named when it was fetched, not 14 hours back, so the firing 12 hours ago
# stands; serve, following, records it from then.
#
# Needs Docker, Go, git, curl, jq, helm and yq. Leaves everything up when
# KEEP is set.
# shellcheck disable=SC2329 # cleanup runs from the trap
set -euo pipefail
cd "$(dirname "$0")/../.."

prom=prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
gitea=gitea/gitea:28.3.0-rootless@sha256:40c5a68a0463c0940e1ae32626c8300dd0572f3558e390073ad8379ca67c0e1d
p=lapilli-e2e-rnd
port=${PORT:-28280}

work=$(mktemp -d)
pids=()
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, containers $p-*"
    return
  fi
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  docker rm -fv "$p-gitea" "$p-prom" "$p-fetch" "$p-follow" >/dev/null 2>&1 || true
  docker volume rm "$p-repo" "$p-work" "$p-prom" >/dev/null 2>&1 || true
  docker network rm "$p" >/dev/null 2>&1 || true
  # The chart's containers wrote some of it as 65532, which the runner's own
  # user can't remove on Linux: removed from a container, as root.
  if [ -n "${git_image:-}" ]; then
    docker run --rm --user 0 -v "$work:/w" --entrypoint sh "$git_image" -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1 || true
  fi
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

fail=0
check() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}
until_true() { # seconds, command...: run it until it succeeds
  local end=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 2
  done
}
at() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }

go build -o "$work/lapilli" ./cmd/lapilli
# The images the chart runs its rules history in.
git_image=$(yq .serve.rulesHistory.image charts/lapilli/values.yaml)
render_image=$(yq .serve.rulesHistory.renderImage charts/lapilli/values.yaml)
export GIT_CONFIG_GLOBAL=$work/gitconfig GIT_CONFIG_NOSYSTEM=1
printf '[user]\n\tname = Ops\n\temail = ops@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n' >"$GIT_CONFIG_GLOBAL"

now=$(date -u +%s)
now=$((now - now % 60))
h=3600

# Prometheus, with three days of e2e_errors written into its storage first.
{
  echo '# TYPE e2e_errors gauge'
  awk -v from=$((now - 74 * h)) -v to=$((now - 2 * h)) -v a=$((now - 60 * h)) -v b=$((now - 36 * h)) -v c=$((now - 12 * h)) 'BEGIN {
    for (t = from; t <= to; t += 60) {
      v = (t >= a && t < a + 1800) || (t >= b && t < b + 1800) || (t >= c && t < c + 1800) ? 10 : 0
      printf "e2e_errors{service=\"shop\"} %d %d\n", v, t
    }
  }'
  echo '# EOF'
} >"$work/seed.om"
docker network create "$p" >/dev/null
docker volume create "$p-prom" >/dev/null
docker run --rm --user 0 --entrypoint sh -v "$p-prom:/prometheus" -v "$work/seed.om:/seed.om:ro" "$prom" \
  -c 'promtool tsdb create-blocks-from openmetrics /seed.om /prometheus >/dev/null && chown -R 65534:65534 /prometheus'
docker run -d --name "$p-prom" --network "$p" -p 127.0.0.1::9090 -v "$p-prom:/prometheus" "$prom" \
  --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --storage.tsdb.retention.time=30d >/dev/null
docker run -d --name "$p-gitea" --network "$p" -p 127.0.0.1::3000 \
  -e GITEA__security__INSTALL_LOCK=true -e GITEA__database__DB_TYPE=sqlite3 \
  -e GITEA__service__DISABLE_REGISTRATION=true -e GITEA__server__ROOT_URL="http://$p-gitea:3000/" "$gitea" >/dev/null
pu=http://$(docker port "$p-prom" 9090/tcp | head -1)
gt=http://$(docker port "$p-gitea" 3000/tcp | head -1)
until_true 120 curl -sf "$gt/api/v1/version"
until_true 120 curl -sf "$pu/-/ready"
check "prometheus: the series written before it started" \
  "$(curl -sf "$pu/api/v1/query?query=max_over_time(e2e_errors%5B1h%5D)&time=$((now - 12 * h + 1800))" | jq -r '.data.result[0].value[1]')" '^10$'

docker exec "$p-gitea" gitea admin user create --username ops --password e2e-ops-pass-123 \
  --email ops@example.com --must-change-password=false >/dev/null
token=$(docker exec "$p-gitea" gitea admin user generate-access-token --username ops --token-name e2e \
  --scopes write:repository,write:user,write:package --raw | tail -1)
curl -sf -H "Authorization: token $token" -H 'Content-Type: application/json' -X POST "$gt/api/v1/user/repos" \
  -d '{"name":"platform","default_branch":"main"}' >/dev/null
charts=http://$p-gitea:3000/api/packages/ops/helm

# helm, as the render image has it, in a directory of the host's.
helm_in() { # dir, script
  docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp --network "$p" -v "$1:/src" -w /src --entrypoint sh "$render_image" -c "$2"
}

# The subchart, a library, in Gitea's Helm registry.
mkdir -p "$work/common/templates"
cat >"$work/common/Chart.yaml" <<'EOF'
apiVersion: v2
name: common
version: 0.1.0
type: library
EOF
cat >"$work/common/templates/_labels.tpl" <<'EOF'
{{- define "common.labels" -}}
app.kubernetes.io/part-of: shop
{{- end -}}
EOF
helm_in "$work" 'helm package common >/dev/null'
curl -sf --user "ops:$token" -X POST --upload-file "$work/common-0.1.0.tgz" "$gt/api/packages/ops/helm/api/charts" >/dev/null

# The chart: two ConfigMaps of Grafana's rules for its sidecar, a
# Prometheus rule file in one that isn't labelled, and what else it runs.
src=$work/platform
mkdir -p "$src/charts/alerts/templates"
cat >"$src/charts/alerts/Chart.yaml" <<EOF
apiVersion: v2
name: alerts
version: 1.0.0
dependencies:
  - name: common
    version: 0.1.0
    repository: $charts
EOF
cat >"$src/charts/alerts/templates/grafana.yaml" <<'EOF'
{{- range $name, $expr := dict "checkout" "sum(e2e_errors{service=\"shop\"})" "search" "sum(e2e_errors{service=\"search\"})" }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: alerts-{{ $name }}
  labels:
    grafana_alert: "1"
    {{- include "common.labels" $ | nindent 4 }}
data:
  {{ $name }}.yaml: |
    apiVersion: 1
    groups:
      - orgId: 1
        name: {{ $name }}
        folder: shop
        interval: 1m
        rules:
          - uid: e2e-{{ $name }}
            title: E2E {{ $name }} errors
            condition: C
            for: 2m
            noDataState: OK
            execErrState: OK
            data:
              - refId: A
                relativeTimeRange: {from: 300, to: 0}
                datasourceUid: prom
                model: {expr: {{ $expr | quote }}, instant: true}
              - refId: C
                datasourceUid: __expr__
                model:
                  type: threshold
                  expression: A
                  conditions: [{evaluator: {type: gt, params: [{{ $.Values.threshold }}]}}]
{{- end }}
EOF
cat >"$src/charts/alerts/templates/prometheus.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: alerts-prometheus
  labels:
    {{- include "common.labels" . | nindent 4 }}
data:
  shop.yaml: |
    groups:
      - name: shop
        rules:
          - alert: E2EUnlabelled
            expr: e2e_errors > 1
EOF
cat >"$src/charts/alerts/templates/app.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop
  labels:
    {{- include "common.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicas }}
  selector: {matchLabels: {app: shop}}
  template:
    metadata: {labels: {app: shop}}
    spec:
      containers: [{name: shop, image: "registry.example.com/shop:1"}]
---
apiVersion: v1
kind: Service
metadata: {name: shop}
spec:
  selector: {app: shop}
  ports: [{port: 80}]
---
apiVersion: v1
kind: Secret
metadata: {name: shop}
stringData: {password: e2e-not-a-secret}
EOF
values() { printf 'threshold: %s\nreplicas: %s\n' "$1" "$2" >"$src/charts/alerts/values.yaml"; }
commit() { # when, message
  GIT_AUTHOR_DATE="$1 +0000" GIT_COMMITTER_DATE="$1 +0000" git -C "$src" commit -qam "$2"
}
git -C "$src" init -q
printf 'charts/alerts/charts/\n' >"$src/.gitignore"
values 5 1
# Chart.lock, pinning the subchart; the archive isn't vendored yet.
helm_in "$src" "helm repo add common $charts >/dev/null && helm dependency update charts/alerts >/dev/null"
git -C "$src" add -A
commit $((now - 72 * h)) "alerts, threshold 5"
c1=$(git -C "$src" rev-parse HEAD)
# Vendored from here on.
rm "$src/.gitignore"
values 50 1
git -C "$src" add -A
commit $((now - 48 * h)) "threshold 50, the subchart vendored"
values 5 1
commit $((now - 24 * h)) "threshold 5 again"
values 5 3
commit $((now - 23 * h)) "three replicas: no rule changed"
git -C "$src" push -q "http://ops:$token@${gt#http://}/ops/platform.git" main
# Gitea takes a push in a while.
branch() { curl -sf -H "Authorization: token $token" "$gt/api/v1/repos/ops/platform/branches/main" | jq -r .commit.id; }
until_true 60 sh -c "curl -sf -H 'Authorization: token $token' '$gt/api/v1/repos/ops/platform/branches/main' | grep -q $(git -C "$src" rev-parse HEAD)" || true
check "gitea: the branch, four commits" "$(branch)" "^$(git -C "$src" rev-parse HEAD)$"

# The chart's rules history, as helm template writes it.
prepare="ls charts/alerts/charts/*.tgz >/dev/null 2>&1 || { helm repo add common $charts >&2 && helm dependency build charts/alerts >&2; }"
helm template lapilli charts/lapilli --set mode=serve --set prometheus.url=http://prometheus:9090 --set serve.backfill=3d \
  --set serve.rulesHistory.repository="http://$p-gitea:3000/ops/platform.git" --set serve.rulesHistory.paths=charts/alerts/ \
  --set serve.rulesHistory.render="helm template alerts charts/alerts" --set serve.rulesHistory.renderPrepare="$prepare" \
  --set serve.rulesHistory.select=grafana_alert=1 --set serve.rulesHistory.every=30 >"$work/chart.yaml"
mkdir -p "$work/scripts" "$work/history"
chmod 777 "$work/history"
for f in git.sh follow.sh; do
  yq "select(.kind == \"ConfigMap\" and .metadata.name == \"lapilli-rules-history\") | .data[\"$f\"]" "$work/chart.yaml" >"$work/scripts/$f"
done
# yq ends a block of text with a newline of its own.
check "the chart's git.sh is the recipe" "$(diff <(printf '%s\n' "$(cat "$work/scripts/git.sh")") recipes/versions/git.sh && echo same)" '^same$'
docker volume create "$p-repo" >/dev/null
docker volume create "$p-work" >/dev/null
# An emptyDir is the pod's to write (fsGroup).
docker run --rm --user 0 -v "$p-repo:/repo" -v "$p-work:/work" --entrypoint sh "$git_image" -c 'chown 65532:65532 /repo /work'

# container NAME [docker run options...]: runs the container NAME as the
# chart writes it: its image, script and environment; its volumes mounted
# as it mounts them; as the pod's user, with a read-only root and no
# capabilities.
container() {
  local name=$1
  shift
  local c=$work/$name
  yq "select(.kind == \"Deployment\") | (.spec.template.spec.initContainers + .spec.template.spec.containers)[] | select(.name == \"$name\")" \
    "$work/chart.yaml" >"$c.yaml"
  yq '.env[] | .name + "=" + .value' "$c.yaml" >"$c.env"
  local mounts=()
  while read -r vol path ro; do
    local from
    case $vol in
      rules-history) from=$work/history ;;
      rules-history-repo) from=$p-repo ;;
      rules-history-work) from=$p-work ;;
      rules-history-script) from=$work/scripts ;;
      *) echo "$name mounts $vol, which this doesn't stand in for" >&2; return 1 ;;
    esac
    [ "$ro" = true ] && mounts+=(-v "$from:$path:ro") || mounts+=(-v "$from:$path")
  done < <(yq '.volumeMounts[] | .name + " " + .mountPath + " " + (.readOnly // false | tostring)' "$c.yaml")
  docker run "$@" --network "$p" --user 65532:65532 --read-only --cap-drop ALL --security-opt no-new-privileges \
    --env-file "$c.env" "${mounts[@]}" --entrypoint sh "$(yq .image "$c.yaml")" -c "$(yq '.args[0]' "$c.yaml")"
}

container rules-history-clone --rm --name "$p-clone"
container rules-history --rm --name "$p-versions" 2>"$work/versions.log" || { cat "$work/versions.log"; exit 1; }
versions=$(find "$work/history/versions" -mindepth 1 -maxdepth 1 -type d | sort)
check "the history: a version for each of the four commits" "$(wc -l <<<"$versions" | tr -d ' ')" '^4$'
first=$(head -1 <<<"$versions")
check "the first commit, which doesn't vendor the subchart, rendered with renderPrepare" "$(cat "$first/.commit")" "^$c1$"
check "renderPrepare fetched the subchart from Gitea's registry" "$(cat "$work/versions.log")" 'Saving 1 charts|Downloading common'
check "the clone is read-only where render runs" "$(yq '.volumeMounts[] | select(.name == "rules-history-repo") | .readOnly' "$work/rules-history.yaml")" '^true$'

# Each version holds exactly the rule documents --rules-select keeps.
inc=$work/incidents.yaml
echo 'incidents: []' >"$inc"
for v in $versions; do
  out=$("$work/lapilli" verdict --rules "$v" --rules-select grafana_alert=1 --prometheus "$pu" --incidents "$inc" --since 1h --json 2>"$work/said")
  check "$(basename "$v"): its rules" "$(jq -r '[.rules[].rule] | sort | join(",")' <<<"$out")" '^E2E checkout errors,E2E search errors$'
  check "$(basename "$v"): what was passed over" "$(cat "$work/said")" \
    'held 3 documents that hold no rules, and 1 document of rules the selector doesn.t match: passed over'
done

# Replayed with each day's rules: the checkout rule fires 60 and 12 hours
# ago; today's rules alone would fire all three times.
firings() { jq -r '.rules[] | select(.rule == "E2E checkout errors") | .firings'; }
out=$("$work/lapilli" verdict --rules-history "$work/history/versions" --rules-select grafana_alert=1 --prometheus "$pu" \
  --incidents "$inc" --since 70h --json 2>/dev/null)
check "verdict --rules-history: each day's rules, two firings" "$(firings <<<"$out")" '^2$'
check "verdict --rules-history: the versions it changed at" "$(jq -r '.rules_changed | length' <<<"$out")" '^[23]$'
newest=$(tail -1 <<<"$versions")
out=$("$work/lapilli" verdict --rules "$newest" --rules-select grafana_alert=1 --prometheus "$pu" --incidents "$inc" --since 70h --json 2>/dev/null)
check "verdict, today's rules alone: three firings" "$(firings <<<"$out")" '^3$'

# Without renderPrepare, the first commit can't be rendered: said, and
# with UNRENDERED=skip passed over.
docker run --rm --user 65532:65532 --network "$p" -v "$p-repo:/repo:ro" -v "$work/scripts:/scripts:ro" -e HOME=/tmp -e TMPDIR=/tmp \
  -e PATHS=charts/alerts/ -e SINCE="$(at $((now - 96 * h)))" -e RENDER="helm template alerts charts/alerts" -e UNRENDERED=skip \
  --entrypoint sh "$render_image" -c 'REPO=/repo/clone OUT=/tmp/out sh /scripts/git.sh && ls /tmp/out' >"$work/skipped" 2>&1 || true
check "unrendered, skipped: said which and why" "$(cat "$work/skipped")" "$c1 \(.*\) can't be rendered: RENDER failed. Passed over"
check "unrendered, skipped: three versions" "$(grep -cE '^[0-9]{8}T[0-9]{6}Z$' "$work/skipped")" '^3$'

# Following: serve reads the directory alone, the fetch and the follow
# beside it.
container rules-history-fetch -d --name "$p-fetch" >/dev/null
container rules-history-follow -d --name "$p-follow" >/dev/null
"$work/lapilli" serve --ledger "$work/ledger" --listen "127.0.0.1:$port" --prometheus "$pu" --rules-history "$work/history/versions" \
  --rules-select grafana_alert=1 --backfill 3d --every 1m --watch 10s --settle 0 --check-replay=false --check-content=false \
  >"$work/serve.log" 2>&1 &
pids+=($!)
report() { curl -sf "http://127.0.0.1:$port/api/v1/report?from=$(at $((now - 70 * h)))&to=$(at $((now + h)))"; }
until_true 240 sh -c "curl -sf 'http://127.0.0.1:$port/api/v1/report?from=$(at $((now - 70 * h)))&to=$(at $((now + h)))' |
  jq -e '.rules[] | select(.rule == \"E2E checkout errors\") | .firings >= 2'" || true
check "serve, following: each day's rules, two firings" "$(report | firings)" '^2$'
check "serve: only the rules --rules-select keeps" "$(report | jq -r '[.rules[].rule] | sort | join(",")')" \
  '^E2E checkout errors,E2E search errors$'

# A commit dated 14 hours ago raises the threshold again: fetched, its
# version is added, named when the fetch first held it (less every and a
# minute), not 14 hours back, so the firing 12 hours ago stands. serve
# records it from then.
values 50 3
commit $((now - 14 * h)) "threshold 50 again"
pushed=$(date -u +%s)
git -C "$src" push -q "http://ops:$token@${gt#http://}/ops/platform.git" main
c5=$(git -C "$src" rev-parse HEAD)
until_true 150 sh -c "grep -rqx '$c5' '$work/history/versions' --include=.commit"
newest=$(find "$work/history/versions" -name .commit | sort | tail -1)
check "follow: the new commit's version added" "$(cat "$newest")" "^$c5$"
named=$(basename "$(dirname "$newest")")
named=$(date -u -d "$(sed -E 's/^(....)(..)(..)T(..)(..)(..)Z$/\1-\2-\3T\4:\5:\6Z/' <<<"$named")" +%s 2>/dev/null ||
  date -u -j -f %Y%m%dT%H%M%SZ "$named" +%s)
check "follow: named when it was fetched, not by its date" "$(( named >= pushed - 120 && named <= $(date -u +%s) ))" '^1$'
check "follow: said why" "$(docker logs "$p-follow" 2>&1)" 'before the branch held it .*: it is named when it was fetched'
until_true 120 grep -q 'rules changed: a new version, from the history' "$work/serve.log"
# The next run, a minute on, replays from it if it had got past it.
sleep 70
check "serve: the firing 12 hours ago stands, two firings" "$(report | firings)" '^2$'
check "the token-less fetch and follow ran without errors" "$(docker logs "$p-follow" 2>&1 | tail -5)" 'versions added|^$'

if [ "$fail" -ne 0 ]; then
  echo "--- serve"
  tail -40 "$work/serve.log"
  echo "--- follow"
  docker logs "$p-follow" 2>&1 | tail -20
  echo "--- fetch"
  docker logs "$p-fetch" 2>&1 | tail -20
fi
exit "$fail"
