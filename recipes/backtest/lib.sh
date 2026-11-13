# shellcheck shell=sh
# What every backtest recipe does, sourced by each host's script, which sets
# recipe, PR and host_api (its API's URL) and defines, for its host:
#
#   host_auth            writes the host's auth header line(s) to stdout
#   host_me              writes the account the token is, or fails
#   find_comment ME      writes the id of the oldest comment on pull
#                        request PR by ME whose first line is $marker, or
#                        nothing; fails when the host does
#   post_comment FILE    posts the Markdown in FILE as a comment on PR
#   edit_comment ID FILE puts the Markdown in FILE in comment ID's place
#
# In a clone of the repository with the pull request checked out, it
# installs a pinned Lapilli release, checked against the release's
# SHA256SUMS; takes the rules of the pull request's commit and of the commit
# it goes into, as they are kept or as RENDER renders them; backtests the
# one against the other (lapilli verdict --compare --changed-only
# --markdown, with the arguments it was given: --prometheus URL, --since
# 14d, ...); and posts the report as one comment on the pull request, found
# again by a hidden first line, $marker, and edited on later runs. When
# nothing that decides when a rule fires changed and there is no comment
# yet, it posts none.
#
#   PR               the pull request's number (each host's script finds
#                    it in its CI's variables)
#   LAPILLI_VERSION  the Lapilli release to install, such as v0.32.0
#   LAPILLI_SHA256   the sha256 of the release's archive for this machine,
#                    pinned: optional from the release itself, which its
#                    SHA256SUMS is checked against either way; needed with
#                    LAPILLI_RELEASES
#   LAPILLI          optional: a lapilli to run instead of installing one
#   LAPILLI_RELEASES optional: a mirror holding the release's files (default
#                    https://github.com/lapilli-project/lapilli/releases/download),
#                    over https, with LAPILLI_SHA256
#   PATHS            where the rules are: directories or files in the
#                    repository, space-separated (alerts, rules/shop.yaml),
#                    as --rules reads them; or
#   RENDER           a command that writes the rules on its stdout, run in
#                    each commit's files, for rules rendered from a chart:
#                    RENDER='helm template monitoring charts/monitoring', or
#                    RENDER='kustomize build overlays/prod'. What else it
#                    writes, lapilli passes over; add --rules-select to the
#                    arguments to keep only labelled objects
#   RENDER_PREPARE   optional: a command run in each commit's files before
#                    RENDER, for what they lack, such as a chart's
#                    dependencies. Both run with REPO the clone's full path
#   INCIDENTS        optional: a file of incidents in the repository, read
#                    from the commit the pull request goes into, never the
#                    pull request's own: --incidents. A file in the checkout
#                    is the pull request's, and isn't taken as an argument
#   TARGET           the commit the pull request goes into, as the clone has
#                    it (default HEAD^1: the merge commit's first parent, as
#                    GitHub checks a pull request out); the rules compared
#                    against are those of the commit HEAD and TARGET share
#                    last (git merge-base)
#   TARGET_BRANCH    instead, a branch to fetch from origin, for a CI that
#                    clones the pull request's branch alone
#   REPO             the clone (default: the directory this runs in)
#   KEY              tells apart the comments of several jobs on one pull
#                    request: letters, digits, dots, dashes and underscores
#                    (default: default)
#   FAIL_ON_ERROR    true: the job fails when the backtest does, or a rule
#                    that differs couldn't be replayed, or the comment can't
#                    be posted, once the comment says so if it can (default
#                    false)
#   MAX_TREE_FILES, MAX_TREE_BYTES
#                    the most files (20000) and bytes (256 MiB) a commit is
#                    read with: with RENDER all of its files, without, those
#                    PATHS names
#
# What can fail once it runs (the install, the commits, RENDER, the
# backtest) is said in the comment, and the job succeeds unless
# FAIL_ON_ERROR=true; a setting that is wrong fails it at once.
#
# What the pull request holds is untrusted, and the comment is read by
# everyone who can read the pull request. Lapilli writes what comes from the
# rules as code spans, which no host reads as Markdown, HTML, a mention or a
# link. What went wrong, and what RENDER or the backtest said of it, goes in
# a fenced block, nothing outside it: its last lines only, with no backtick
# (which could end the block), no control or format character (which could
# hide or reorder what is shown), and where the history is (the URLs, user
# names, hosts, tenants and project in the arguments, any IP address) taken
# out. What is copied to the job's log is each line after a "| ", and
# Azure's ##vso[ made inert, so none of it is a CI's command. A commit
# holding a symbolic link where the rules are read, or anywhere with RENDER,
# isn't read: a link could lead to any file the job can read. The host's
# token is read from a file and given to curl on stdin, never on a command
# line. RENDER and RENDER_PREPARE run what the pull request holds, with the
# job's rights: they run without the variables CIs keep tokens in, and with a
# HOME of their own for each commit, the base first, but can read whatever
# the job can, the token's file included. Run this only for pull requests
# from people trusted with what the job can reach (the README says how,
# host by host).

set -eu
# inert: text as a CI's log can take it: no control character but the tab
# and the line break, and Azure Pipelines' ##vso[, which it reads anywhere
# in a line, made inert.
inert() { tr -d '\000-\010\013-\037\177' | sed 's/##\([vV][sS][oO]\)\[/##\1(/g'; }
say() { printf '%s: %s\n' "${recipe:-lib.sh}" "$(printf '%s' "$*" | tr '\n\t' '  ' | inert)" >&2; }
# logged FILE: FILE in the job's log, each line after a "| ": GitHub's and
# Gitea's ::commands are read only at a line's start.
logged() { inert <"$1" | sed 's/^/  | /' >&2; }
# shown FILE: what a server answered, as a JSON string of 300 bytes at
# most, to be said.
shown() { head -c 300 "$1" | jq -Rsc . 2>/dev/null || echo '"(unreadable)"'; }
# safe_url URL: https, or plain http to this machine itself, with no user
# in its authority: a token or a release goes nowhere else in the clear.
safe_url() {
  case $1 in
    https://*) rest=${1#https://} ;;
    http://*) rest=${1#http://} ;;
    *) return 1 ;;
  esac
  auth=${rest%%/*}
  case $auth in
    *@* | '' | *[!]A-Za-z0-9.:_[-]*) return 1 ;;
  esac
  case $1 in https://*) return 0 ;; esac
  case $auth in
    127.0.0.1 | 127.0.0.1:* | localhost | localhost:* | '[::1]' | '[::1]':*) return 0 ;;
  esac
  return 1
}

# The settings, checked before anything runs.
case ${PR:-} in
  '' | *[!0-9]* | 0*) say "PR ${PR:-}: want the pull request's number"; exit 1 ;;
esac
if ! safe_url "${host_api:-}"; then
  say "the host's API ${host_api:-}: want an https URL"
  exit 1
fi
KEY=${KEY:-default}
case $KEY in
  '' | *[!A-Za-z0-9._-]*) say "KEY $KEY: want letters, digits, dots, dashes and underscores"; exit 1 ;;
esac
[ "${#KEY}" -le 64 ] || { say "KEY $KEY: want 64 characters at most"; exit 1; }
# The comment's first line: a link reference definition, which every
# host's Markdown shows nothing of.
marker="[//]: # (lapilli-backtest $KEY)"
case ${FAIL_ON_ERROR:-false} in
  true) strict=1 ;;
  false | '') strict= ;;
  *) say "FAIL_ON_ERROR $FAIL_ON_ERROR: want true or false"; exit 1 ;;
esac
repo=${REPO:-.}
TARGET=${TARGET:-HEAD^1}
case $TARGET in
  -* | *[!A-Za-z0-9._/^~-]*) say "TARGET $TARGET: want a commit or a ref, such as HEAD^1 or origin/main"; exit 1 ;;
esac
case ${TARGET_BRANCH:-} in
  -* | *[!A-Za-z0-9._/-]*) say "TARGET_BRANCH $TARGET_BRANCH: want a branch's name"; exit 1 ;;
esac
tree_files=${MAX_TREE_FILES:-20000}
tree_bytes=${MAX_TREE_BYTES:-268435456}
for n in "MAX_TREE_FILES $tree_files" "MAX_TREE_BYTES $tree_bytes"; do
  case ${n#* } in *[!0-9]* | '') say "$n: want a number"; exit 1 ;; esac
done
if [ -n "${RENDER:-}" ] && [ -n "${PATHS:-}" ]; then
  say "RENDER renders all of each commit: leave out PATHS"
  exit 1
fi
if [ -z "${RENDER:-}" ] && [ -z "$(printf '%s' "${PATHS:-}" | tr -d ' ')" ]; then
  say "say where the rules are: PATHS, files or directories in the repository, or RENDER, a command that writes them"
  exit 1
fi
if [ -n "${RENDER_PREPARE:-}" ] && [ -z "${RENDER:-}" ]; then
  say "RENDER_PREPARE prepares each commit's files for RENDER: give RENDER too"
  exit 1
fi
# in_repo PATH WHAT: whether PATH is a path in the repository, as git names
# it: nothing outside it, no options.
in_repo() {
  case $1 in
    -* | /* | *[!A-Za-z0-9._/-]*) return 1 ;;
  esac
  case /$1/ in
    */../* | */./* | //) return 1 ;;
  esac
}
set -f
for p in ${PATHS:-}; do
  in_repo "$p" || { say "PATHS: $p isn't a path in the repository"; exit 1; }
done
set +f
if [ -n "${INCIDENTS:-}" ] && ! in_repo "$INCIDENTS"; then
  say "INCIDENTS $INCIDENTS: want a file in the repository"
  exit 1
fi
if [ -z "${LAPILLI:-}" ]; then
  case ${LAPILLI_VERSION:-} in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) say "LAPILLI_VERSION ${LAPILLI_VERSION:-}: want the Lapilli release to install, such as v0.32.0"; exit 1 ;;
  esac
  case $LAPILLI_VERSION in
    *[!a-z0-9.-]*) say "LAPILLI_VERSION $LAPILLI_VERSION: want a release's tag, such as v0.32.0"; exit 1 ;;
  esac
  case ${LAPILLI_SHA256:-} in
    '') ;;
    *[!0-9a-f]*) say "LAPILLI_SHA256: want a sha256 in lowercase hex"; exit 1 ;;
    *) [ "${#LAPILLI_SHA256}" -eq 64 ] || { say "LAPILLI_SHA256: want a sha256, 64 hex digits"; exit 1; } ;;
  esac
  if [ -n "${LAPILLI_RELEASES:-}" ]; then
    # Over plain http, or from a mirror, whoever gives the archive could
    # give a SHA256SUMS that agrees: the sum pinned is what holds.
    if ! safe_url "$LAPILLI_RELEASES"; then
      say "LAPILLI_RELEASES $LAPILLI_RELEASES: want an https URL"
      exit 1
    fi
    if [ -z "${LAPILLI_SHA256:-}" ]; then
      say "LAPILLI_RELEASES: a mirror's archive is taken only as LAPILLI_SHA256, pinned, says: give it"
      exit 1
    fi
  fi
fi
# The clone, and where this runs: a file an argument names in the clone is
# the pull request's, which is judged by it.
clone=$(cd "$repo" 2>/dev/null && pwd -P) || { say "REPO $repo isn't a directory"; exit 1; }
here_now=$(pwd -P)
# in_clone PATH: whether PATH, as lapilli opens it from here, is in the
# clone.
in_clone() {
  case $1 in /*) f=$1 ;; *) f=$here_now/$1 ;; esac
  d=$(cd "$(dirname -- "$f")" 2>/dev/null && pwd -P) || return 1
  case $d/ in "$clone"/*) return 0 ;; esac
  return 1
}
# Each argument in turn, to the end and around: the same, in order, after.
n=$#
while [ "$n" -gt 0 ]; do
  a=$1
  shift
  n=$((n - 1))
  case $a in
    --rules | --rules=* | --compare | --compare=* | --rules-history | --rules-history=* | --changed-only | --markdown | \
      --json | --metrics | --check-replay* | --check-content*)
      say "$a: the recipe gives lapilli the rules and asks for the report itself; give it the rest, such as --prometheus URL and --since 14d"
      exit 1
      ;;
    --incidents | --on-call | --bearer-token-file | --grafana-token-file | --ruler-token-file | --loki-token-file | --slack-webhook-file)
      if [ "$n" -gt 0 ] && in_clone "$1"; then
        say "$a $1: a file in the checkout is the pull request's, which it would be judged by: give it from outside the checkout, or INCIDENTS"
        exit 1
      fi
      ;;
    --incidents=* | --on-call=* | --bearer-token-file=* | --grafana-token-file=* | --ruler-token-file=* | --loki-token-file=* | --slack-webhook-file=*)
      if in_clone "${a#*=}"; then
        say "${a%%=*} ${a#*=}: a file in the checkout is the pull request's, which it would be judged by: give it from outside the checkout, or INCIDENTS"
        exit 1
      fi
      ;;
  esac
  set -- "$@" "$a"
done
for tool in git curl jq tar; do
  command -v "$tool" >/dev/null 2>&1 || { say "$tool isn't installed: the recipe needs sh, git, curl, jq and tar"; exit 1; }
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
out=$tmp/out
mkdir "$tmp/empty"
: >"$tmp/problem.log"

# host METHOD URL [FILE]: asks the host's API, the answer in $out; writes
# the HTTP status. curl reads no .curlrc (-q), follows no redirect, and
# speaks only HTTP and HTTPS.
host() {
  if [ $# -ge 3 ]; then
    host_auth | curl -q -sS --max-time 30 --max-filesize 16777216 --proto '=http,https' -X "$1" -H @- -H 'Content-Type: application/json' \
      --data-binary "@$3" -o "$out" -w '%{http_code}' "$2" || echo failed
  else
    host_auth | curl -q -sS --max-time 30 --max-filesize 16777216 --proto '=http,https' -X "$1" -H @- -o "$out" -w '%{http_code}' "$2" || echo failed
  fi
}

# trouble WHAT [LOG]: the backtest can't be what the comment shows: WHAT,
# said in the comment, with LOG's last lines.
problem=
trouble() {
  problem=$1
  : >"$tmp/problem.log"
  if [ $# -ge 2 ] && [ -s "$2" ]; then
    cp "$2" "$tmp/problem.log"
  fi
  say "$1"
}

# sha256 FILE: its sha256, with whichever tool the machine has.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  else
    openssl dgst -sha256 -r "$1" | cut -d ' ' -f 1
  fi
}

# fetch_lapilli: the lapilli to run, in $bin: LAPILLI, or the release
# LAPILLI_VERSION for this machine, downloaded and checked against the
# release's SHA256SUMS (and LAPILLI_SHA256, when given) before it is
# unpacked.
fetch_lapilli() {
  if [ -n "${LAPILLI:-}" ]; then
    bin=$LAPILLI
    return 0
  fi
  case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) trouble "Lapilli has no release for this machine's system"; return 1 ;;
  esac
  case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) trouble "Lapilli has no release for this machine's processor"; return 1 ;;
  esac
  name=lapilli-$LAPILLI_VERSION-$os-$arch
  from=${LAPILLI_RELEASES:-https://github.com/lapilli-project/lapilli/releases/download}
  mkdir "$tmp/release"
  for f in "$name.tar.gz 268435456" "SHA256SUMS 65536"; do
    # A release's files are redirected to where they are kept: https only.
    if ! curl -q -fsSL --max-time 300 --max-filesize "${f#* }" --proto '=http,https' --proto-redir '=https' -o "$tmp/release/${f% *}" \
      "${from%/}/$LAPILLI_VERSION/${f% *}" 2>"$tmp/curl.log"; then
      trouble "Lapilli $LAPILLI_VERSION's ${f% *} couldn't be downloaded" "$tmp/curl.log"
      return 1
    fi
  done
  want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/release/SHA256SUMS")
  case $want in
    *[!0-9a-f]* | '') trouble "Lapilli $LAPILLI_VERSION's SHA256SUMS has no sum for $name.tar.gz"; return 1 ;;
  esac
  if [ "${#want}" -ne 64 ]; then
    trouble "Lapilli $LAPILLI_VERSION's SHA256SUMS has no sum for $name.tar.gz"
    return 1
  fi
  if [ -n "${LAPILLI_SHA256:-}" ] && [ "$LAPILLI_SHA256" != "$want" ]; then
    trouble "LAPILLI_SHA256 isn't the sum Lapilli $LAPILLI_VERSION's SHA256SUMS gives $name.tar.gz ($want): not run"
    return 1
  fi
  got=$(sha256 "$tmp/release/$name.tar.gz")
  if [ "$got" != "$want" ]; then
    trouble "$name.tar.gz isn't the file SHA256SUMS names (its sha256 is $got, not $want): not run"
    return 1
  fi
  if ! tar -xzf "$tmp/release/$name.tar.gz" -C "$tmp/release" "$name/lapilli" 2>"$tmp/tar.log"; then
    trouble "$name.tar.gz couldn't be unpacked" "$tmp/tar.log"
    return 1
  fi
  bin=$tmp/release/$name/lapilli
  say "installed Lapilli $LAPILLI_VERSION ($name.tar.gz, sha256 $got)"
}

# commits: the pull request's commit, $head, and the commit its rules are
# compared against, $base: the last HEAD and TARGET share.
commits() {
  if ! gitdir=$(git -C "$repo" rev-parse --absolute-git-dir 2>"$tmp/git.log"); then
    trouble "REPO isn't a git clone" "$tmp/git.log"
    return 1
  fi
  if [ -n "${TARGET_BRANCH:-}" ]; then
    if ! git -C "$repo" fetch --quiet --no-tags origin "refs/heads/$TARGET_BRANCH" 2>"$tmp/git.log"; then
      trouble "the branch $TARGET_BRANCH couldn't be fetched from origin" "$tmp/git.log"
      return 1
    fi
    TARGET=FETCH_HEAD
  fi
  if ! head=$(git -C "$repo" rev-parse --verify -q "HEAD^{commit}"); then
    trouble "the clone has no commit checked out"
    return 1
  fi
  if ! target=$(git -C "$repo" rev-parse --verify -q "$TARGET^{commit}"); then
    trouble "TARGET $TARGET isn't a commit the clone has: fetch it, or more history (the merge commit and its parents, a depth of 2)"
    return 1
  fi
  if ! base=$(git -C "$repo" merge-base "$head" "$target" 2>"$tmp/git.log"); then
    trouble "HEAD and TARGET $TARGET share no commit in the clone: fetch more history" "$tmp/git.log"
    return 1
  fi
  say "the rules of $head, beside those of $base"
  if [ -z "${RENDER:-}" ]; then
    # A path neither commit has is a mistake in PATHS: both sides would
    # have no rules, nothing would change, and nothing would be said.
    found=0
    set -f
    for p in $PATHS; do
      p=${p%/}
      if git -C "$repo" cat-file -e "$head:$p" 2>/dev/null || git -C "$repo" cat-file -e "$base:$p" 2>/dev/null; then
        found=$((found + 1))
      else
        say "PATHS: $p is in neither commit"
      fi
    done
    set +f
    if [ "$found" = 0 ]; then
      say "none of PATHS is in either commit: say where the rules are"
      exit 1
    fi
  fi
}

# files COMMIT DIR WHAT: COMMIT's files into DIR, as git keeps them: all of
# them with RENDER, those PATHS names without, an empty directory in place
# of each it hasn't. With the attributes of an empty work tree and an empty
# index, run from it, so the commit's own .gitattributes don't leave a file
# out (export-ignore), rewrite one (export-subst) or run a filter on it.
# WHAT names the commit, to say what went wrong.
files() {
  c=$1 d=$2 what=$3
  mkdir -p "$d"
  set -f
  set --
  if [ -z "${RENDER:-}" ]; then
    for p in $PATHS; do
      p=${p%/}
      if git -C "$repo" cat-file -e "$c:$p" 2>/dev/null; then
        set -- "$@" "$p"
      else
        mkdir -p "$d/$p"
      fi
    done
    set +f
    [ $# -gt 0 ] || return 0
  fi
  set +f
  big=$(git -C "$repo" ls-tree -r -l "$c" -- "$@" | awk -v files="$tree_files" -v bytes="$tree_bytes" '
    { n++; if ($4 != "-") s += $4 }
    END { if (n > files || s > bytes) printf "%d files of %d bytes, more than MAX_TREE_FILES (%d) or MAX_TREE_BYTES (%d)", n, s, files, bytes }')
  if [ -n "$big" ]; then
    trouble "$what is too big to read: $big"
    return 1
  fi
  if ! (cd "$tmp/empty" && GIT_INDEX_FILE=$tmp/index git --git-dir="$gitdir" --work-tree=. archive --worktree-attributes \
    --format=tar -o "$tmp/c.tar" "$c" -- "$@") 2>"$tmp/git.log" || ! tar -x -f "$tmp/c.tar" -C "$d" 2>>"$tmp/git.log"; then
    trouble "$what couldn't be read" "$tmp/git.log"
    return 1
  fi
  l=$(find "$d" -type l | head -n 1)
  if [ -n "$l" ]; then
    # Its path is the pull request's: in the job's log, not the comment.
    say "a symbolic link in $what: ${l#"$d/"}"
    trouble "$what holds a symbolic link, which could lead to any file the job can read: not read (the job's log says where)"
    return 1
  fi
}

# incidents: INCIDENTS, as the commit the pull request goes into has it,
# into $tmp/incidents.yaml: a file, not a link, of 8 MiB at most.
incidents() {
  [ -n "${INCIDENTS:-}" ] || return 0
  mode=$(git -C "$repo" ls-tree "$base" -- "$INCIDENTS" | awk '{ print $1 }')
  case $mode in
    100644 | 100755) ;;
    '') trouble "INCIDENTS $INCIDENTS isn't in the commit the pull request goes into"; return 1 ;;
    *) trouble "INCIDENTS $INCIDENTS isn't a file in the commit the pull request goes into"; return 1 ;;
  esac
  if [ "$(git -C "$repo" cat-file -s "$base:$INCIDENTS")" -gt 8388608 ]; then
    trouble "INCIDENTS $INCIDENTS is more than 8 MiB"
    return 1
  fi
  git -C "$repo" cat-file blob "$base:$INCIDENTS" >"$tmp/incidents.yaml"
}

# in_tree SIDE COMMAND: RENDER or RENDER_PREPARE, in a commit's files, with
# a HOME of that commit's own (what Helm caches there is fetched once a
# commit), without the variables CIs keep tokens in; REPO is the clone. It
# runs what the pull request holds, with the job's rights.
in_tree() {
  mkdir -p "$tmp/$1-home"
  (cd "$tmp/$1" && env -u GITHUB_TOKEN_FILE -u GITLAB_TOKEN_FILE -u BITBUCKET_TOKEN_FILE -u AZURE_DEVOPS_TOKEN_FILE \
    -u GITEA_TOKEN_FILE -u LAPILLI_INPUT_TOKEN -u GITHUB_TOKEN -u GH_TOKEN -u GITEA_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_TOKEN \
    -u ACTIONS_ID_TOKEN_REQUEST_URL -u ACTIONS_RUNTIME_TOKEN -u CI_JOB_TOKEN -u CI_JOB_JWT -u CI_JOB_JWT_V1 -u CI_JOB_JWT_V2 \
    -u CI_REGISTRY_PASSWORD -u CI_DEPLOY_PASSWORD -u SYSTEM_ACCESSTOKEN -u BITBUCKET_STEP_OIDC_TOKEN \
    HOME="$tmp/$1-home" REPO="$clone" sh -c "$2")
}

# render SIDE WHAT: RENDER_PREPARE, then RENDER, in $tmp/SIDE, what RENDER
# writes into $tmp/rules/SIDE, a directory, in which a rendering without
# rules is no rules, not an error.
render() {
  mkdir -p "$tmp/rules/$1"
  if [ -n "${RENDER_PREPARE:-}" ]; then
    if ! in_tree "$1" "$RENDER_PREPARE" >"$tmp/render.log" 2>&1; then
      logged "$tmp/render.log"
      trouble "RENDER_PREPARE failed in $2" "$tmp/render.log"
      return 1
    fi
    logged "$tmp/render.log"
    l=$(find "$tmp/$1" -type l | head -n 1)
    if [ -n "$l" ]; then
      say "a symbolic link RENDER_PREPARE left in $2: ${l#"$tmp/$1/"}"
      trouble "RENDER_PREPARE left a symbolic link in $2: not read (the job's log says where)"
      return 1
    fi
  fi
  if ! in_tree "$1" "$RENDER" >"$tmp/rules/$1/rules.yaml" 2>"$tmp/render.log"; then
    logged "$tmp/render.log"
    trouble "RENDER failed in $2" "$tmp/render.log"
    return 1
  fi
  logged "$tmp/render.log"
}

# hidden ARG...: of lapilli's arguments, what says where the history and
# the rules are read from, a line each, the longest first: the URLs, the
# user names in them, their hosts, the tenants, a Google Cloud project.
# Taken out of what goes in the comment.
hidden() {
  while [ $# -gt 0 ]; do
    v=
    case $1 in
      --prometheus | --loki | --grafana | --ruler | --gcp-project | --alarms | --ruler-tenant | --loki-tenant)
        if [ $# -ge 2 ]; then
          v=$2
          shift
        fi
        ;;
      --prometheus=* | --loki=* | --grafana=* | --ruler=* | --gcp-project=* | --alarms=* | --ruler-tenant=* | --loki-tenant=*) v=${1#*=} ;;
    esac
    shift
    [ -n "$v" ] || continue
    printf '%s\n' "$v"
    case $v in
      *://*)
        h=${v#*://}
        h=${h%%/*}
        case $h in
          *@*)
            u=${h%%@*}
            printf '%s\n' "${u%%:*}"
            h=${h##*@}
            ;;
        esac
        printf '%s\n%s\n' "$h" "${h%:*}"
        ;;
    esac
  done | awk 'length($0) >= 2 { print length($0), $0 }' | sort -rn | cut -d ' ' -f 2-
}

# fenced WHAT LOG BYTES: WHAT, then LOG's last lines, BYTES of them at most,
# as a fenced block: what hidden wrote taken out (each a whole word), and
# URLs' user names and IP addresses, with no backtick, which could end the
# block, and no control or format character, line or paragraph separator,
# which could hide or reorder what is shown. Inside one, no host reads
# Markdown, HTML, a mention or a link.
fenced() {
  printf '```text\n'
  {
    printf '%s.\n' "$1"
    [ ! -s "$2" ] || tail -n 40 "$2" | tail -c "$3"
  } | jq -Rr --rawfile hide "$tmp/hidden" '
    def esc: gsub("(?<c>[\\\\^$.|?*+()\\[\\]{}/-])"; "\\\(.c)");
    reduce ($hide | split("\n")[] | select(length > 0)) as $h (.;
      gsub("(?<![A-Za-z0-9._-])" + ($h | esc) + "(?![A-Za-z0-9_-])"; "[hidden]"))
    | gsub("(?<s>[A-Za-z][A-Za-z0-9+.-]*://)[^/@\\s\"]*@"; "\(.s)")
    | gsub("\\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\\](:[0-9]+)?"; "[address]")
    | gsub("\\b([0-9]{1,3}\\.){3}[0-9]{1,3}(:[0-9]+)?\\b"; "[address]")
    | gsub("\t"; " ") | gsub("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]"; "") | gsub("`"; "'"'"'")' | head -c "$(($3 + 1000))"
  printf '\n```\n'
}

# summary FIELD: the report's summary's FIELD, as its first line says.
summary() {
  head -n 1 "$tmp/report.md" | sed -n 's/^\[\/\/\]: # (lapilli-compare \({.*}\))$/\1/p' | jq -r ".$1 // empty" 2>/dev/null
}

# backtest ARG...: lapilli verdict on the rules of both commits, with the
# arguments given; the report in $tmp/report.md, lapilli's exit status in
# $verdict.
verdict=
backtest() {
  if [ -n "${INCIDENTS:-}" ]; then
    set -- --incidents "$tmp/incidents.yaml" "$@"
  fi
  if [ -n "${RENDER:-}" ]; then
    set -- --rules "$tmp/rules/head" --compare "$tmp/rules/base" "$@"
  else
    set -f
    for p in $PATHS; do
      p=${p%/}
      set -- --rules "$tmp/head/$p" --compare "$tmp/base/$p" "$@"
    done
    set +f
  fi
  verdict=0
  "$bin" verdict "$@" --changed-only --markdown >"$tmp/report.md" 2>"$tmp/lapilli.log" || verdict=$?
  logged "$tmp/lapilli.log"
  case $verdict in
    0) ;;
    1) trouble "the backtest failed, or didn't finish (lapilli exited 1)" "$tmp/lapilli.log" ;;
    2)
      if grep -q -e '--markdown needs a value' -e 'unknown flag --markdown' "$tmp/lapilli.log" && [ -n "${LAPILLI:-}" ]; then
        trouble "the lapilli LAPILLI names has no --markdown: give a later release" "$tmp/lapilli.log"
      elif grep -q -e '--markdown needs a value' -e 'unknown flag --markdown' "$tmp/lapilli.log"; then
        trouble "Lapilli $LAPILLI_VERSION has no --markdown: give LAPILLI_VERSION a later release" "$tmp/lapilli.log"
      else
        trouble "lapilli refused its arguments (it exited 2)" "$tmp/lapilli.log"
      fi
      ;;
    *) trouble "lapilli didn't run (exit status $verdict)" "$tmp/lapilli.log" ;;
  esac
  # Asked to fail when it does, a backtest of rules that couldn't be
  # replayed (the history out of reach, say) is one that failed.
  if [ "$verdict" = 0 ] && [ -n "$strict" ]; then
    n=$(summary unreplayed)
    case $n in
      '' | *[!0-9]*) trouble "the report has no summary line" "$tmp/lapilli.log" ;;
      0) ;;
      *) trouble "$n of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)" "$tmp/lapilli.log" ;;
    esac
  fi
}

# nothing_changed: whether the report says nothing that decides when a rule
# fires changed, on its first line, from a run that went well.
nothing_changed() {
  [ "$verdict" = 0 ] && [ -z "$problem" ] && [ -s "$tmp/report.md" ] && [ "$(summary nothing_changed)" = true ]
}

# compose: the comment, in $tmp/comment.md: the marker; the report, if
# lapilli wrote one; what went wrong, if anything did, in a fenced block.
# Under the 65,536 characters GitHub takes, the least of the hosts: a
# shorter log, or none, beside a long report.
compose() {
  for logged_bytes in 4000 1000 0; do
    {
      printf '%s\n' "$marker"
      if [ -s "$tmp/report.md" ] && [ "${verdict:-0}" -le 1 ]; then
        cat "$tmp/report.md"
      fi
      if [ -n "$problem" ]; then
        printf '\n### Lapilli backtest failed\n\n'
        if [ "$logged_bytes" -gt 0 ]; then
          fenced "$problem" "$tmp/problem.log" "$logged_bytes"
        else
          fenced "$problem" /dev/null 0
        fi
        printf '\nThe rest is in the job'"'"'s log.\n'
      fi
    } >"$tmp/comment.md"
    [ "$(wc -c <"$tmp/comment.md")" -gt 65000 ] || return 0
  done
}

# post: the comment on the pull request, in place of the one posted
# before, if there is one.
post() {
  me=
  if ! me=$(host_me) || [ -z "$me" ]; then
    say "the host didn't say who the token is: an earlier comment can't be told from someone else's, so none is looked for"
    me=
  fi
  id=
  if [ -n "$me" ] && ! id=$(find_comment "$me"); then
    return 1
  fi
  if nothing_changed && [ -z "$id" ]; then
    say "nothing that decides when a rule fires changed, and there is no comment to update: none posted"
    return 0
  fi
  if [ -n "$id" ]; then
    edit_comment "$id" "$tmp/comment.md" || return 1
    say "updated comment $id on pull request $PR"
  else
    post_comment "$tmp/comment.md" || return 1
    say "commented on pull request $PR"
  fi
}

main() {
  hidden "$@" >"$tmp/hidden"
  # The base first, then the pull request's: what the pull request runs
  # comes last.
  if commits && fetch_lapilli && files "$base" "$tmp/base" "the commit it goes into" &&
    files "$head" "$tmp/head" "the pull request's commit" && incidents; then
    if [ -z "${RENDER:-}" ] || { render base "the commit it goes into" && render head "the pull request's commit"; }; then
      backtest "$@"
    fi
  fi
  compose
  if ! post; then
    say "the comment couldn't be posted"
    [ -z "$strict" ] || exit 1
    exit 0
  fi
  if [ -n "$problem" ] && [ -n "$strict" ]; then
    exit 1
  fi
  exit 0
}
