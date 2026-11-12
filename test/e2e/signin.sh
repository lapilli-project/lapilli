#!/usr/bin/env bash
# lapilli serve's own sign-in against a real OpenID Connect provider, Dex,
# in Docker (docs/design/signin.md): people sign in through the
# authorization code flow, with PKCE, state and nonce, at Dex's password
# form, played with curl; their roles come from their names and Dex's
# groups; a callback can't be replayed; an ID token from Dex works as a
# bearer token for the API and MCP; signing out ends the session for good,
# across a restart too. No Prometheus: lapilli serve runs on firings copied
# in, so it starts at once.
#
# Needs Docker, Go and curl. Leaves everything up when KEEP is set.
# shellcheck disable=SC2016,SC2329 # a bcrypt hash; cleanup runs on EXIT
set -euo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/e2e/signin-lib.sh
. test/e2e/signin-lib.sh

dex=ghcr.io/dexidp/dex:v2.46.1@sha256:65b72ca0f4b0bf6d4ada84ec21da8851782563404ac7b8f926923d27ceced9ab
p=lapilli-e2e-signin
port=${PORT:-28580}   # lapilli; port+1 takes firings
dexport=${DEX_PORT:-28556}
base=http://127.0.0.1:$port
issuer=http://127.0.0.1:$dexport/dex

work=$(mktemp -d)
serving=
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "kept: $work, container $p-dex"
    return
  fi
  [ -z "$serving" ] || kill "$serving" 2>/dev/null || true
  docker rm -fv "$p-dex" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'echo "stopped at line $LINENO: $BASH_COMMAND" >&2' ERR

fail=0
check() { # what, got, want (an extended regexp)
  if grep -qE -- "$3" <<<"$2"; then echo "ok: $1"; else echo "FAILED: $1: got $2, want $3"; fail=1; fi
}

go build -o "$work/lapilli" ./cmd/lapilli

# Dex: three people, by the password form; boss is in the group ops. The
# hash is bcrypt's of "password", as in Dex's own examples.
hash='$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W'
cat >"$work/dex.yaml" <<EOF
issuer: $issuer
storage: {type: memory}
web: {http: 0.0.0.0:5556}
oauth2: {skipApprovalScreen: true, passwordConnector: local}
staticClients:
  - id: lapilli
    name: lapilli
    secret: lapilli-e2e-client-secret
    redirectURIs: [$base/auth/callback]
enablePasswordDB: true
staticPasswords:
  - {email: boss@example.com, hash: "$hash", username: boss, userID: "1", groups: [ops]}
  - {email: ed@example.com, hash: "$hash", username: ed, userID: "2"}
  - {email: viewer@example.com, hash: "$hash", username: viewer, userID: "3"}
EOF
chmod 644 "$work/dex.yaml"
docker run -d --name "$p-dex" -p "127.0.0.1:$dexport:5556" -v "$work/dex.yaml:/etc/dex/config.yaml:ro" "$dex" dex serve /etc/dex/config.yaml >/dev/null
for _ in $(seq 60); do curl -fs "$issuer/.well-known/openid-configuration" >/dev/null && break; sleep 1; done

printf 'lapilli-e2e-client-secret\n' >"$work/secret"
printf 'firings-token-0123456789\n' >"$work/firings"
serve() {
  "$work/lapilli" serve --ledger "$work/ledger" --listen "127.0.0.1:$port" \
    --incidents-listen "127.0.0.1:$((port + 1))" --firings-token-file "$work/firings" \
    --oidc-issuer "$issuer" --oidc-client-id lapilli --oidc-client-secret-file "$work/secret" \
    --oidc-scopes email,profile,groups --external-url "$base" \
    --admins group:ops --editors user:ed@example.com >>"$work/serve.log" 2>&1 &
  serving=$!
  for _ in $(seq 60); do
    # This one's, not another listening there.
    kill -0 "$serving" 2>/dev/null || break
    curl -fs "$base/healthz" >/dev/null && return
    sleep 1
  done
  cat "$work/serve.log"
  return 1
}
serve

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# Not signed in: a page sends the browser to sign in; the API says 401.
check "a page, not signed in, sends the browser to sign in" \
  "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$base/rules")" "^303 $base/auth/login\?next=%2Frules$"
check "the API, not signed in, is refused" "$(code "$base/api/v1/me")" '^401$'
check "the health checks are anyone's" "$(code "$base/healthz")" '^200$'

# boss signs in, and is an admin by Dex's group.
signin "$work/boss" "$base" boss@example.com password >"$work/out" || fail=1
check "boss signs in through Dex, and comes back to the page" "$(cat "$work/out")" "^200 $base/rules$"
me=$(browse "$work/boss" "$base/api/v1/me")
check "boss is an admin, by the group ops" "$me" '"role":"admin".*"user":"boss@example.com"|"user":"boss@example.com".*"role":"admin"'
check "with Dex's groups" "$me" '"groups":\["ops"\]'
page=$(browse "$work/boss" "$base/rules")
check "the page says who, and offers to sign out" "$page" 'action="/auth/logout"'

# ed is an editor by name, and writes; viewer may only look.
signin "$work/ed" "$base" ed@example.com password >/dev/null || fail=1
check "ed is an editor" "$(browse "$work/ed" "$base/api/v1/me")" '"role":"editor"'
written=$(browse "$work/ed" -X POST -H 'Content-Type: application/json' \
  -d '{"title": "written by ed", "start": "2026-11-02T06:00:00Z"}' "$base/api/v1/incidents")
check "ed writes an incident, under the name Dex gave" "$written" '"by": ?"ed@example.com"'
signin "$work/viewer" "$base" viewer@example.com password >/dev/null || fail=1
check "viewer is a viewer" "$(browse "$work/viewer" "$base/api/v1/me")" '"role":"viewer"'
check "viewer may not write" "$(browse "$work/viewer" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"title": "x", "start": "2026-11-02T06:00:00Z"}' "$base/api/v1/incidents")" '^403$'

# A callback replayed, cookie and all, is refused.
back=$(callback_url "$work/replay" "$base" ed@example.com password) || fail=1
cp "$work/replay" "$work/replay-copy"
check "the callback, once" "$(browse "$work/replay" -o /dev/null -w '%{http_code}' "$back")" '^303$'
check "the same callback again" "$(browse "$work/replay-copy" -o /dev/null -w '%{http_code}' "$back")" '^400$'

# A cookie changed by hand is no session.
sed -E 's/(lapilli-session[[:space:]]+)([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)$/\1\2x.\3/' "$work/ed" >"$work/tampered"
check "a session cookie changed by hand" "$(browse "$work/tampered" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^401$'

# An ID token from Dex, as an API client or an agent has one, as a bearer
# token.
token=$(curl -s -u lapilli:lapilli-e2e-client-secret \
  --data-urlencode grant_type=password --data-urlencode username=ed@example.com --data-urlencode password=password \
  --data-urlencode 'scope=openid email groups' "$issuer/token" | sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p')
check "an ID token as a bearer token" "$(curl -s -H "Authorization: Bearer $token" "$base/api/v1/me")" '"role":"editor"'
mcp=$(curl -s -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}' "$base/mcp")
check "an agent over MCP, with it" "$mcp" '"serverInfo"'
check "a token with a byte changed" "$(code -H "Authorization: Bearer ${token%?}x" "$base/api/v1/me")" '^401$'

# boss signs out: the session ends, and a copy of its cookie with it.
cp "$work/boss" "$work/boss-copy"
check "signing out from another site is refused" \
  "$(browse "$work/boss" -o /dev/null -w '%{http_code}' -X POST -H 'Sec-Fetch-Site: cross-site' "$base/auth/logout")" '^403$'
check "and boss is still signed in" "$(browse "$work/boss" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^200$'
check "signing out" "$(browse "$work/boss" -o /dev/null -w '%{http_code} %{redirect_url}' -X POST "$base/auth/logout")" "^303 $base/auth/signed-out$"
check "signed out" "$(browse "$work/boss" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^401$'
check "a copy of the cookie, too" "$(browse "$work/boss-copy" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^401$'

# A restart: the session signed out of stays ended; ed's goes on.
kill "$serving"
wait "$serving" 2>/dev/null || true
serve
check "after a restart, the copy is still refused" "$(browse "$work/boss-copy" -o /dev/null -w '%{http_code}' "$base/api/v1/me")" '^401$'
check "and ed is still signed in" "$(browse "$work/ed" "$base/api/v1/me")" '"user":"ed@example.com"'
if grep -q lapilli-e2e-client-secret "$work/serve.log"; then echo "FAILED: the client secret is in the log"; fail=1; fi
exit $fail
