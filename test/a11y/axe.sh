#!/usr/bin/env bash
# Checks the web UI of lapilli serve against WCAG 2.2 AA with axe-core, in a
# headless Chromium in Docker: every page TestA11yServe serves (those the
# i18n test renders, with data), in each language, in light and in dark,
# and that none scrolls sideways at 320 CSS px. Needs Go and Docker.
#
# PORT is the port to serve on (8399); OUT, a file to write every violation
# to as JSON. Exits 1 when there is a violation.
set -euo pipefail
if [ -n "${OUT:-}" ]; then
  touch "$OUT"
  OUT=$(cd "$(dirname "$OUT")" && pwd)/$(basename "$OUT")
fi
cd "$(dirname "$0")/../.."

# The Playwright image of the version package.json pins, by digest.
image=mcr.microsoft.com/playwright:v1.64.1-noble@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f
port=${PORT:-8399}
name=lapilli-a11y-$$
work=$(mktemp -d)
server=

cleanup() {
  [ -z "$server" ] || kill "$server" 2>/dev/null || true
  docker rm -fv "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

go test -c -o "$work/serve.test" ./internal/serve
LAPILLI_A11Y_SERVE=127.0.0.1:$port "$work/serve.test" -test.run '^TestA11yServe$' -test.timeout 0 >"$work/serve.log" 2>&1 &
server=$!
for _ in $(seq 120); do
  curl -fs -o /dev/null "http://127.0.0.1:$port/a11y-paths" && break
  kill -0 "$server" 2>/dev/null || { cat "$work/serve.log"; exit 1; }
  sleep 1
done

# Docker Desktop reaches the host's loopback as host.docker.internal; on
# Linux the container shares the host's network instead.
if [ "$(uname -s)" = Linux ]; then
  net=(--network host)
  base=http://127.0.0.1:$port
else
  net=()
  base=http://host.docker.internal:$port
fi
out=()
[ -z "${OUT:-}" ] || out=(-e OUT=/out/axe.json -v "$OUT:/out/axe.json")
docker run --name "$name" --init ${net[@]+"${net[@]}"} ${out[@]+"${out[@]}"} -e BASE="$base" -e npm_config_update_notifier=false \
  -v "$PWD/test/a11y:/a11y:ro" "$image" \
  sh -c 'mkdir /tmp/w && cp /a11y/package.json /a11y/package-lock.json /a11y/axe.mjs /tmp/w/ && cd /tmp/w &&
    npm ci --ignore-scripts --no-audit --no-fund --loglevel=error && node axe.mjs'
