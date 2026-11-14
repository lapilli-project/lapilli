#!/usr/bin/env bash
# Takes the README's screenshots of lapilli serve, assets/serve-*.png, from
# the demo ledger TestDemoServe serves (internal/serve/demo_test.go): the
# overview and a noisy rule's page, 1240×980, in English, light and dark.
#
# In the Playwright image of the version test/a11y/package.json pins, as
# axe.sh runs it, which needs Go and Docker; with --local, with Go, Node and
# the browser of that version as this machine has them (npx playwright
# install chromium-headless-shell, once), without Docker.
#
# PORT is the port the demo is served on (18431).
set -euo pipefail
cd "$(dirname "$0")/../.."
root=$PWD

# The Playwright image of the version package.json pins, by digest.
image=mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f
port=${PORT:-18431}
local=false
case "${1:-}" in
--local) local=true ;;
"") ;;
*) echo "usage: $0 [--local]" >&2; exit 2 ;;
esac
name=lapilli-shots-$$
work=$(mktemp -d)
server=

cleanup() {
  [ -z "$server" ] || kill "$server" 2>/dev/null || true
  $local || docker rm -fv "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

go test -c -o "$work/serve.test" ./internal/serve
LAPILLI_DEMO_SERVE=127.0.0.1:$port "$work/serve.test" -test.run '^TestDemoServe$' -test.timeout 0 >"$work/serve.log" 2>&1 &
server=$!
for _ in $(seq 120); do
  curl -fs -o /dev/null "http://127.0.0.1:$port/healthz" && break
  kill -0 "$server" 2>/dev/null || { cat "$work/serve.log"; exit 1; }
  sleep 1
done

cp test/a11y/package.json test/a11y/package-lock.json test/screenshots/shots.mjs "$work/"
if $local; then
  (cd "$work" && npm ci --ignore-scripts --no-audit --no-fund --loglevel=error &&
    BASE="http://127.0.0.1:$port" OUT="$root/assets" node shots.mjs)
  exit
fi

# Docker Desktop reaches the host's loopback as host.docker.internal; on
# Linux the container shares the host's network instead. The PNGs are
# copied out of the container, so they are this user's.
if [ "$(uname -s)" = Linux ]; then
  net=(--network host)
  base=http://127.0.0.1:$port
else
  net=()
  base=http://host.docker.internal:$port
fi
docker run --name "$name" --init ${net[@]+"${net[@]}"} -e BASE="$base" -e npm_config_update_notifier=false \
  -v "$work:/shots:ro" "$image" \
  sh -c 'mkdir -p /tmp/w/out && cp /shots/package.json /shots/package-lock.json /shots/shots.mjs /tmp/w/ && cd /tmp/w &&
    npm ci --ignore-scripts --no-audit --no-fund --loglevel=error && OUT=/tmp/w/out node shots.mjs'
docker cp "$name:/tmp/w/out/." assets/
ls -l assets/serve-*.png
