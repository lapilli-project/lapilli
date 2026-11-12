# shellcheck shell=sh
# What every backtest recipe does, sourced by each host's script, which sets
# recipe and PR and defines, for its host:
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
#   LAPILLI_SHA256   optional: the sha256 of the release's archive for this
#                    machine, pinned; SHA256SUMS is checked either way
#   LAPILLI          optional: a lapilli to run instead of installing one
#   LAPILLI_RELEASES optional: where releases are downloaded from (default
#                    https://github.com/lapilli-project/lapilli/releases/download),
#                    a mirror holding the same files, over https
#   PATHS            where the rules are: files or directories in the
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
#   TARGET           the commit the pull request goes into, as the clone has
#                    it (default HEAD^1: the merge commit's first parent, as
#                    GitHub, Gitea and Azure Pipelines check a pull request
#                    out); the rules compared against are those of the
#                    commit HEAD and TARGET share last (git merge-base)
#   TARGET_BRANCH    instead, a branch to fetch from origin, for a CI that
#                    clones the pull request's branch alone
#   REPO             the clone (default: the directory this runs in)
#   KEY              tells apart the comments of several jobs on one pull
#                    request: letters, digits, dots, dashes and underscores
#                    (default: default)
#   FAIL_ON_ERROR    true: the job fails when the backtest does, or the
#                    comment can't be posted, once the comment says so if it
#                    can (default false)
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
# link. What RENDER or the backtest said goes in a fenced block, its last
# lines only, with no backtick (which could end the block), no control or
# format character (which could hide or reorder what is shown), and the
# URLs and the project given in the arguments taken out. A commit holding a
# symbolic link where the rules are read, or anywhere with RENDER, isn't
# read: a link could lead to any file the job can read. The host's token is
# read from a file and given to curl on stdin, never on a command line;
# RENDER and RENDER_PREPARE run without the token's variable, and with a
# HOME of their own. They run what the pull request holds: run this only
# for pull requests from people trusted with what the job can reach (the
# README says how, host by host).

set -eu
say() { printf '%s: %s\n' "${recipe:-lib.sh}" "$*" >&2; }
# shown FILE: what a server answered, as a JSON string of 300 bytes at
# most, to be said.
shown() { head -c 300 "$1" | jq -Rsc . 2>/dev/null || echo '"(unreadable)"'; }

# The settings, checked before anything runs.
case ${PR:-} in
  '' | *[!0-9]* | 0*) say "PR ${PR:-}: want the pull request's number"; exit 1 ;;
esac
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
set -f
for p in ${PATHS:-}; do
  # Paths in the repository, as git names them: nothing outside it, no
  # options.
  case $p in
    -* | /* | *[!A-Za-z0-9._/-]*) say "PATHS: $p isn't a path in the repository"; exit 1 ;;
  esac
  case /$p/ in
    */../* | */./* | //) say "PATHS: $p isn't a path in the repository"; exit 1 ;;
  esac
done
set +f
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
  # Over plain http, whoever is between could give an archive and a
  # SHA256SUMS that agree: https, or this machine's own.
  case ${LAPILLI_RELEASES:-https://} in
    https://* | http://127.0.0.1[:/]* | http://localhost[:/]* | 'http://[::1]'[:/]*) ;;
    *) say "LAPILLI_RELEASES $LAPILLI_RELEASES: want an https URL"; exit 1 ;;
  esac
fi
for a in "$@"; do
  case $a in
    --rules | --rules=* | --compare | --compare=* | --rules-history | --rules-history=* | --changed-only | --markdown | \
      --json | --metrics | --check-replay* | --check-content*)
      say "$a: the recipe gives lapilli the rules and asks for the report itself; give it the rest, such as --prometheus URL and --since 14d"
      exit 1
      ;;
  esac
done
for tool in git curl jq tar; do
  command -v "$tool" >/dev/null 2>&1 || { say "$tool isn't installed: the recipe needs sh, git, curl, jq and tar"; exit 1; }
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
out=$tmp/out
mkdir "$tmp/home" "$tmp/empty"
: >"$tmp/problem.log"

# host METHOD URL [FILE]: asks the host's API, the answer in $out; writes
# the HTTP status.
host() {
  if [ $# -ge 3 ]; then
    host_auth | curl -sS --max-time 30 --proto '=http,https' -X "$1" -H @- -H 'Content-Type: application/json' \
      --data-binary "@$3" -o "$out" -w '%{http_code}' "$2" || echo failed
  else
    host_auth | curl -sS --max-time 30 --proto '=http,https' -X "$1" -H @- -o "$out" -w '%{http_code}' "$2" || echo failed
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
    *) trouble "Lapilli has no release for $(uname -s)"; return 1 ;;
  esac
  case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) trouble "Lapilli has no release for $(uname -m)"; return 1 ;;
  esac
  name=lapilli-$LAPILLI_VERSION-$os-$arch
  from=${LAPILLI_RELEASES:-https://github.com/lapilli-project/lapilli/releases/download}
  mkdir "$tmp/release"
  for f in "$name.tar.gz" SHA256SUMS; do
    # A release's files are redirected to where they are kept: https only.
    if ! curl -fsSL --max-time 300 --proto '=http,https' --proto-redir '=https' -o "$tmp/release/$f" \
      "${from%/}/$LAPILLI_VERSION/$f" 2>"$tmp/curl.log"; then
      trouble "Lapilli $LAPILLI_VERSION's $f couldn't be downloaded" "$tmp/curl.log"
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
    trouble "REPO $repo isn't a git clone" "$tmp/git.log"
    return 1
  fi
  clone=$(cd "$repo" && pwd)
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
    trouble "$what holds a symbolic link, ${l#"$d/"}, which could lead to any file the job can read: not read"
    return 1
  fi
}

# in_tree DIR COMMAND: RENDER or RENDER_PREPARE, in a commit's files,
# without the host's token's variable and with a HOME of its own (what
# Helm caches there is fetched once); REPO is the clone.
in_tree() {
  (cd "$1" && env -u GITHUB_TOKEN_FILE -u GITLAB_TOKEN_FILE -u BITBUCKET_TOKEN_FILE -u AZURE_DEVOPS_TOKEN_FILE \
    -u GITEA_TOKEN_FILE HOME="$tmp/home" REPO="$clone" sh -c "$2")
}

# render SIDE WHAT: RENDER_PREPARE, then RENDER, in $tmp/SIDE, what RENDER
# writes into $tmp/rules/SIDE, a directory, in which a rendering without
# rules is no rules, not an error.
render() {
  mkdir -p "$tmp/rules/$1"
  if [ -n "${RENDER_PREPARE:-}" ]; then
    if ! in_tree "$tmp/$1" "$RENDER_PREPARE" >"$tmp/render.log" 2>&1; then
      cat "$tmp/render.log" >&2
      trouble "RENDER_PREPARE failed in $2" "$tmp/render.log"
      return 1
    fi
    cat "$tmp/render.log" >&2
    l=$(find "$tmp/$1" -type l | head -n 1)
    if [ -n "$l" ]; then
      trouble "RENDER_PREPARE left a symbolic link in $2, ${l#"$tmp/$1/"}: not read"
      return 1
    fi
  fi
  if ! in_tree "$tmp/$1" "$RENDER" >"$tmp/rules/$1/rules.yaml" 2>"$tmp/render.log"; then
    cat "$tmp/render.log" >&2
    trouble "RENDER failed in $2" "$tmp/render.log"
    return 1
  fi
  cat "$tmp/render.log" >&2
}

# hidden ARG...: of lapilli's arguments, what says where the history and
# the rules are read from, a line each, the longest first: the URLs, their
# hosts, a Google Cloud project. Taken out of what goes in the comment.
hidden() {
  while [ $# -gt 0 ]; do
    v=
    case $1 in
      --prometheus | --loki | --grafana | --ruler | --gcp-project | --alarms)
        if [ $# -ge 2 ]; then
          v=$2
          shift
        fi
        ;;
      --prometheus=* | --loki=* | --grafana=* | --ruler=* | --gcp-project=* | --alarms=*) v=${1#*=} ;;
    esac
    shift
    [ -n "$v" ] || continue
    printf '%s\n' "$v"
    case $v in
      *://*)
        h=${v#*://}
        h=${h%%/*}
        h=${h##*@}
        printf '%s\n%s\n' "$h" "${h%:*}"
        ;;
    esac
  done | awk 'length($0) >= 4 { print length($0), $0 }' | sort -rn | cut -d ' ' -f 2-
}

# fenced LOG BYTES: LOG's last lines, BYTES at most, as a fenced block:
# what hidden wrote taken out, with no backtick, which could end the block,
# and no control or format character, line or paragraph separator, which
# could hide or reorder what is shown. Inside one, no host reads Markdown,
# HTML, a mention or a link.
fenced() {
  printf '```text\n'
  tail -n 40 "$1" | tail -c "$2" | jq -Rr --rawfile hide "$tmp/hidden" '
    reduce ($hide | split("\n")[] | select(length > 0)) as $h (.; split($h) | join("[hidden]"))
    | gsub("\t"; " ") | gsub("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]"; "") | gsub("`"; "'"'"'")'
  printf '```\n'
}

# backtest ARG...: lapilli verdict on the rules of both commits, with the
# arguments given; the report in $tmp/report.md, lapilli's exit status in
# $verdict.
verdict=
backtest() {
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
  cat "$tmp/lapilli.log" >&2
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
}

# nothing_changed: whether the report says nothing that decides when a rule
# fires changed, on its first line.
nothing_changed() {
  [ "$verdict" = 0 ] && [ -z "$problem" ] && [ -s "$tmp/report.md" ] &&
    head -n 1 "$tmp/report.md" | sed -n 's/^\[\/\/\]: # (lapilli-compare \({.*}\))$/\1/p' | jq -e '.nothing_changed == true' >/dev/null 2>&1
}

# compose: the comment, in $tmp/comment.md: the marker; the report, if
# lapilli wrote one; what went wrong, if anything did.
compose() {
  # Beside a report of up to 64,000 bytes, a short log keeps the comment
  # under the 65,536 characters GitHub takes.
  logged=4000
  {
    printf '%s\n' "$marker"
    if [ -s "$tmp/report.md" ] && [ "${verdict:-0}" -le 1 ]; then
      cat "$tmp/report.md"
      logged=1000
    fi
    if [ -n "$problem" ]; then
      printf '\n### Lapilli backtest failed\n\n%s.\n' "$(printf '%s' "$problem" | jq -Rr 'gsub("[\\p{Cc}\\p{Cf}`]"; "")')"
      if [ -s "$tmp/problem.log" ]; then
        printf '\n'
        fenced "$tmp/problem.log" "$logged"
      fi
      printf '\nThe rest is in the job'"'"'s log.\n'
    fi
  } >"$tmp/comment.md"
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
  if fetch_lapilli && commits && files "$head" "$tmp/head" "the pull request's commit" &&
    files "$base" "$tmp/base" "the commit it goes into"; then
    if [ -z "${RENDER:-}" ] || { render head "the pull request's commit" && render base "the commit it goes into"; }; then
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
