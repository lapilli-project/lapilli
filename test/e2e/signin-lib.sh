# shellcheck shell=bash
# A browser played with curl, for the sign-in's end-to-end tests
# (signin.sh, serve-lb.sh): it signs in at a real Dex through its password
# form, keeping cookies in a jar, as a person's browser would. Extra curl
# arguments (--connect-to, for names only the test can resolve) go in the
# array CURL_EXTRA.

CURL_EXTRA=(${CURL_EXTRA[@]+"${CURL_EXTRA[@]}"})

# browse JAR ARGS...: curl with the jar and CURL_EXTRA.
browse() {
  local jar=$1
  shift
  curl -sS -c "$jar" -b "$jar" ${CURL_EXTRA[@]+"${CURL_EXTRA[@]}"} "$@"
}

# origin URL: the scheme and host of a URL.
origin() { printf '%s' "$1" | sed -E 's#^(https?://[^/]+).*#\1#'; }

# dex_form JAR BASE EMAIL PASSWORD: from lapilli at BASE to Dex's password
# form, filled in and posted; prints where Dex sends the browser next.
dex_form() {
  local jar=$1 base=$2 email=$3 password=$4 auth page action
  auth=$(browse "$jar" -o /dev/null -w '%{redirect_url}' "$base/auth/login?next=%2Frules")
  case $auth in
    http*/auth\?*) ;;
    *) echo "lapilli didn't send the browser to sign in: '$auth'" >&2; return 1 ;;
  esac
  page=$(browse "$jar" -L "$auth")
  action=$(printf '%s' "$page" | sed -n 's/.*<form method="post" action="\([^"]*\)".*/\1/p' | sed 's/&amp;/\&/g')
  if [ -z "$action" ]; then
    echo "no password form at $auth" >&2
    return 1
  fi
  browse "$jar" -o /dev/null -w '%{redirect_url}' --data-urlencode "login=$email" \
    --data-urlencode "password=$password" "$(origin "$auth")$action"
}

# signin JAR BASE EMAIL PASSWORD: signs in, and prints the code and the URL
# the browser ended on, after coming back to the page it asked for.
signin() {
  local next
  next=$(dex_form "$@") || return 1
  browse "$1" -L -o /dev/null -w '%{http_code} %{url_effective}' "$next"
}

# callback_url JAR BASE EMAIL PASSWORD: signs in at Dex, and prints the
# URL Dex sends the browser back to lapilli with, without going there.
callback_url() {
  local jar=$1 base=$2 next hops=0
  next=$(dex_form "$@") || return 1
  until [ "${next#"$base"/auth/callback}" != "$next" ]; do
    hops=$((hops + 1))
    if [ "$hops" -gt 5 ] || [ -z "$next" ]; then
      echo "Dex didn't send the browser back to $base" >&2
      return 1
    fi
    next=$(browse "$jar" -o /dev/null -w '%{redirect_url}' "$next")
  done
  printf '%s' "$next"
}
