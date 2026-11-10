#!/bin/sh
# Writes the versions of rule files that git keeps, as lapilli's
# --rules-history reads them: one directory each, named by when it took
# effect, holding the files as they stood then. A first lapilli serve start
# then replays each day of its backfill with that day's rules.
#
#   REPO    a clone with its history (default: the directory this runs in)
#   PATHS   the rule files or directories in it, space-separated, as
#           --rules reads them: alerts/ or rules/shop.yaml
#   SINCE   from when: 2026-10-06 or 2026-10-06T00:00:00Z. The rules as they
#           stood then are written too, from the commit that last changed
#           them before then
#   OUT     the directory to write, which must not exist yet or be empty
#   RENDER  optional: a command that writes the rules on its stdout, run in
#           each commit's files, for rules rendered from a chart:
#           RENDER='helm template monitoring charts/monitoring'
#   MAX     the most commits taken (default 500)
#
# A version is named by its commit's time, in UTC: for a merge, the merge's,
# as only the commits on the branch are taken (git log --first-parent). A
# commit's time is what its committer's clock said, so each version is
# named at least a second after the one before it: one dated back can't
# reorder them. A version holding a symbolic link is refused: a link could
# lead to any file this can read. RENDER runs without TOKEN_FILE and with a
# HOME of its own, so a chart's commands can't reach a token the clone was
# made with. It reads the clone and changes nothing in it. Nothing is
# written to OUT until every version has been: what fails leaves OUT as it
# was.
#
#   REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-06 OUT=/tmp/history ./git.sh
#   lapilli serve --rules ~/src/monitoring/alerts --rules-history /tmp/history ...
set -eu
say() { printf 'git.sh: %s\n' "$*" >&2; }
repo=${REPO:-.}
: "${PATHS:?the rule files or directories, such as alerts/}"
: "${SINCE:?from when, such as 2026-10-06}"
: "${OUT:?the directory to write}"
max=${MAX:-500}
case $max in *[!0-9]* | '' | 0) say "MAX $max: want a number of commits"; exit 1 ;; esac
# grep reads lines: a second line could carry anything past it.
c=$SINCE
case $SINCE in *[!0-9TZ:+-]* | '') c=x ;; esac
if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}(:[0-9]{2})?(Z|[+-][0-9]{2}:[0-9]{2}))?$'; then
  say "SINCE $SINCE: want a date or a time, such as 2026-10-06 or 2026-10-06T00:00:00Z"
  exit 1
fi
for p in $PATHS; do
  # Paths in the clone, as git names them: nothing outside it, no options.
  case $p in
    -* | /* | *[!A-Za-z0-9._/-]*) say "PATHS: $p isn't a path in the clone"; exit 1 ;;
  esac
  case /$p/ in */../*) say "PATHS: $p isn't a path in the clone"; exit 1 ;; esac
done
if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT")" ]; then
  say "OUT $OUT holds something: give a new or empty directory"
  exit 1
fi
if [ "$(git -C "$repo" rev-parse --is-inside-work-tree 2>/dev/null)" != true ]; then
  say "REPO $repo isn't a git clone"
  exit 1
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
mkdir "$tmp/out" "$tmp/home"
# basic SECONDS: a time in UTC, in the basic form; date's flags for it
# differ between GNU and BusyBox (-d) and BSD (-r).
basic() { date -u -d "@$1" +%Y%m%dT%H%M%SZ 2>/dev/null || date -u -r "$1" +%Y%m%dT%H%M%SZ; }
prev=
now=$(date -u +%s)
# links DIR COMMIT: refuse a version with a symbolic link in it.
links() {
  if [ -n "$(find "$1" -type l | head -n 1)" ]; then
    say "$2 holds a symbolic link among PATHS: a version is files, not links to elsewhere"
    exit 1
  fi
}
# write COMMIT: the files PATHS name as COMMIT left them, or what RENDER
# writes from them, into a directory named by its time.
write() {
  # Its commit's time, at least a second after the version before: kept
  # here, not in a $(...), whose assignments don't outlast it.
  t=$(git -C "$repo" log -1 --format=%ct "$1")
  # A committer's clock can be wrong: a commit dated after now would name
  # every later version after it too, and lapilli passes those over.
  if [ "$t" -gt "$now" ]; then
    say "$1 is dated after now ($(basic "$t")): a committer's clock was wrong; nothing is written"
    exit 1
  fi
  if [ -n "$prev" ] && [ "$t" -le "$prev" ]; then
    t=$((prev + 1))
  fi
  prev=$t
  name=$(basic "$t")
  dir=$tmp/out/$name
  mkdir "$dir"
  if [ -n "${RENDER:-}" ]; then
    rm -rf "$tmp/tree"
    mkdir "$tmp/tree"
    git -C "$repo" archive --format=tar "$1" | tar -x -C "$tmp/tree"
    # Links among PATHS only: elsewhere in the repository they are its own.
    for p in $PATHS; do
      [ ! -e "$tmp/tree/${p%/}" ] || links "$tmp/tree/${p%/}" "$1"
    done
    if ! (cd "$tmp/tree" && env -u TOKEN_FILE HOME="$tmp/home" GIT_CONFIG_GLOBAL=/dev/null sh -c "$RENDER") >"$dir/rules.yaml"; then
      say "RENDER failed at $1"
      exit 1
    fi
    return
  fi
  there=
  for p in $PATHS; do
    if git -C "$repo" cat-file -e "$1:${p%/}" 2>/dev/null; then
      there="$there ${p%/}"
    fi
  done
  if [ -z "$there" ]; then
    # The rules weren't there then: nothing stood to replay.
    rmdir "$dir"
    say "$1 ($name) has none of PATHS: passed over"
    return
  fi
  # shellcheck disable=SC2086 # $there is the paths, one word each
  git -C "$repo" archive --format=tar "$1" -- $there | tar -x -C "$dir"
  links "$dir" "$1"
}
if [ "$(git -C "$repo" rev-parse --is-shallow-repository)" = true ]; then
  say "REPO $repo is a shallow clone, with no history: only the rules as they are now are written. Clone it whole, or with --shallow-since"
  write HEAD
else
  # shellcheck disable=SC2086 # PATHS are the paths, one word each
  before=$(git -C "$repo" log --first-parent -1 --format=%H --before="$SINCE" HEAD -- $PATHS)
  # shellcheck disable=SC2086
  git -C "$repo" log --first-parent --reverse --format=%H --since="$SINCE" HEAD -- $PATHS >"$tmp/commits"
  n=$(($(wc -l <"$tmp/commits")))
  if [ "$n" -gt "$max" ]; then
    say "$n commits changed PATHS since $SINCE, more than MAX ($max): give a later SINCE"
    exit 1
  fi
  [ -z "$before" ] || write "$before"
  while read -r commit; do
    write "$commit"
  done <"$tmp/commits"
fi
written=$(($(find "$tmp/out" -mindepth 1 -maxdepth 1 | wc -l)))
if [ "$written" -eq 0 ]; then
  say "no version of PATHS since $SINCE: nothing written"
  exit 1
fi
mkdir -p "$OUT"
for v in "$tmp/out"/*; do
  mv "$v" "$OUT/"
done
say "$written versions of $PATHS into $OUT"
