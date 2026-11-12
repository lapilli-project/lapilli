#!/bin/sh
# Writes the versions of rule files that git keeps, as lapilli's
# --rules-history reads them: one directory each, named by when it took
# effect, holding the files as they stood then. A first lapilli serve start
# then replays each day of its backfill with that day's rules.
#
#   REPO    a clone with its history (default: the directory this runs in)
#   PATHS   the rule files or directories in it, space-separated, as
#           --rules reads them: alerts/ or rules/shop.yaml
#   SINCE   from when: 2026-10-06 (the start of that day, in UTC) or
#           2026-10-06T00:00:00Z. The rules as they stood then are written
#           too, from the commit that last changed them before then
#   OUT     the directory to write, which must not exist yet or be empty
#   RENDER  optional: a command that writes the rules on its stdout, run in
#           each commit's files, for rules rendered from a chart:
#           RENDER='helm template monitoring charts/monitoring', or from a
#           Kustomize overlay: RENDER='kustomize build overlays/prod'. What
#           else it writes (Deployments, Services) lapilli passes over, and
#           --rules-select keeps only labelled rules
#   RENDER_PREPARE
#           optional: a command run in each commit's files before RENDER,
#           for what a commit lacks, such as a chart's dependencies that
#           older commits don't vendor: 'helm dependency build
#           charts/monitoring' (with 'helm repo add' first for a chart
#           repository; Chart.lock pins the versions). Its output is shown,
#           not taken as rules. Both run with REPO the clone's full path
#   UNRENDERED
#           what to do with a commit that can't be rendered: RENDER_PREPARE
#           or RENDER fails, or RENDER writes nothing. fail (the default)
#           writes nothing and says which; skip passes it over, saying
#           which and why, and the version before stays in effect until the
#           next commit that renders
#   MAX     the most versions written (default 500): commits, or with
#           DEPLOYS the deploys that changed the rules
#   DEPLOYS optional: a file of the revisions deployed and when, "REVISION
#           TIME" a line, as argocd.sh and flux.sh write it. The versions
#           are then the rules each deploy put in place, named by when: when
#           they reached Prometheus, not when they were committed
#   AFTER   optional, as follow.sh gives it: a commit; only the commits on
#           the branch after it are written, each named after SINCE, the
#           time of the version it wrote. With none, it says so, writes
#           nothing and succeeds. If the branch was rewritten since, from
#           where the two parted
#
# Each version holds .commit, the full name of the commit it is (lapilli
# passes names starting with a dot over), for follow.sh to go on from.
#
# A version is named by its commit's time, in UTC: for a merge, the merge's,
# as only the commits on the branch are taken (git log --first-parent). A
# commit's time is what its committer's clock said, so each version is
# named at least a second after the one before it: one dated back can't
# reorder them. A version holding a symbolic link is refused: a link could
# lead to any file this can read. RENDER and RENDER_PREPARE run without
# TOKEN_FILE and with a HOME of their own, kept from one commit to the next
# (what Helm caches there is fetched once), so a chart's commands can't
# reach a token the clone was made with. It reads the clone and changes
# nothing in it. Nothing is written to OUT until every version has been:
# what fails leaves OUT as it was.
#
# With DEPLOYS, the deploys are walked in time, from the last at or before
# SINCE (the rules in effect then) on: each that changed the rules from
# what the deploy before it put in place is a version, a rollback too, and
# commits deployed together are one, the others never in effect. A deploy
# is never dated before its revision's commit (a Flux snapshot brought back
# by a revert keeps its first time). With no deploy at or before SINCE,
# the rules as they stood then are the commit's, as without DEPLOYS. A
# revision is taken only as the commit whose full name starts with it,
# found among the clone's objects, not its refs: a tag or a branch named
# like it isn't it, and two commits that start alike are passed over. A
# deploy that changed nothing (Flux before 2.7 writes one each reconcile)
# isn't a version, and MAX doesn't count it.
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
unrendered=${UNRENDERED:-fail}
case $unrendered in
  fail | skip) ;;
  *) say "UNRENDERED $unrendered: want fail or skip"; exit 1 ;;
esac
if [ -n "${RENDER_PREPARE:-}" ] && [ -z "${RENDER:-}" ]; then
  say "RENDER_PREPARE prepares each commit's files for RENDER: give RENDER too"
  exit 1
fi
# grep reads lines: a second line could carry anything past it.
c=$SINCE
case $SINCE in *[!0-9TZ:+-]* | '') c=x ;; esac
if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}(:[0-9]{2})?(Z|[+-][0-9]{2}:[0-9]{2}))?$'; then
  say "SINCE $SINCE: want a date or a time, such as 2026-10-06 or 2026-10-06T00:00:00Z"
  exit 1
fi
# A date is the start of that day, in UTC: git would read it at the time of
# day it is now, where this runs.
case $SINCE in
  ????-??-??) SINCE=${SINCE}T00:00:00Z ;;
esac
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
after=
if [ -n "${AFTER:-}" ]; then
  if [ -n "${DEPLOYS:-}" ]; then
    say "AFTER goes on from a commit, and DEPLOYS names versions by deploys: give one"
    exit 1
  fi
  # Checked before git sees it: a commit's name, not an option or a ref.
  case $AFTER in *[!0-9a-f]* | '') c= ;; *) c=$AFTER ;; esac
  if [ "${#c}" -lt 7 ] || [ "${#c}" -gt 64 ] || ! after=$(git -C "$repo" rev-parse --verify -q "$c^{commit}"); then
    say "AFTER $AFTER isn't a commit in the clone"
    exit 1
  fi
  if [ "$(git -C "$repo" rev-parse --is-shallow-repository)" = true ]; then
    say "REPO $repo is a shallow clone, with no history to go on from AFTER: clone it whole"
    exit 1
  fi
  # A branch rewritten since: AFTER..HEAD is the commits since where the
  # two parted.
  if ! git -C "$repo" merge-base --is-ancestor "$after" HEAD; then
    if ! base=$(git -C "$repo" merge-base "$after" HEAD); then
      say "AFTER $AFTER shares no history with the branch"
      exit 1
    fi
    say "AFTER $AFTER isn't on the branch any more (it was rewritten): the commits since where they parted, $base, are written"
  fi
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
skipped=0
# links DIR COMMIT: refuse a version with a symbolic link in it.
links() {
  if [ -n "$(find "$1" -type l | head -n 1)" ]; then
    say "$2 holds a symbolic link among PATHS: a version is files, not links to elsewhere"
    exit 1
  fi
}
# run COMMAND: RENDER or RENDER_PREPARE, in the commit's files, without the
# token and with a HOME of its own; REPO is the clone, wherever this runs.
clone=$(cd "$repo" && pwd)
run() {
  (cd "$tmp/tree" && env -u TOKEN_FILE HOME="$tmp/home" GIT_CONFIG_GLOBAL=/dev/null REPO="$clone" sh -c "$1")
}
# unrendered COMMIT NAME WHY: a commit that can't be rendered, passed over
# with UNRENDERED=skip, or else the end.
unrendered() {
  rm -rf "${tmp:?}/out/$2"
  if [ "$unrendered" = skip ]; then
    skipped=$((skipped + 1))
    say "$1 ($2) can't be rendered: $3. Passed over (UNRENDERED=skip): the version before stays in effect until the next"
    return
  fi
  say "$1 ($2) can't be rendered: $3. Nothing is written; UNRENDERED=skip passes such a commit over"
  exit 1
}
# write COMMIT [SECONDS]: the files PATHS name as COMMIT left them, or what
# RENDER writes from them, into a directory named by its time: SECONDS, when
# it was deployed, or its commit's.
write() {
  # Its time, at least a second after the version before: kept here, not in
  # a $(...), whose assignments don't outlast it.
  t=${2:-$(git -C "$repo" log -1 --format=%ct "$1")}
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
    # What prepares the files shows its output; only RENDER's is rules.
    if [ -n "${RENDER_PREPARE:-}" ] && ! run "$RENDER_PREPARE" >&2; then
      unrendered "$1" "$name" "RENDER_PREPARE failed"
    elif ! run "$RENDER" >"$dir/rules.yaml"; then
      unrendered "$1" "$name" "RENDER failed"
    elif [ ! -s "$dir/rules.yaml" ]; then
      unrendered "$1" "$name" "RENDER wrote nothing"
    else
      git -C "$repo" rev-parse --verify "$1^{commit}" >"$dir/.commit"
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
  git -C "$repo" rev-parse --verify "$1^{commit}" >"$dir/.commit"
}
# The deploys, as SECONDS COMMIT, the earliest first: each line checked
# before git sees it, so a revision can't be an option or a path and a time
# can't be anything git would guess at.
: >"$tmp/deploys"
if [ -n "${DEPLOYS:-}" ]; then
  if [ ! -f "$DEPLOYS" ]; then
    say "DEPLOYS $DEPLOYS isn't a file"
    exit 1
  fi
  if grep -Ev '^[0-9a-f]{7,64} [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' "$DEPLOYS" | grep -q .; then
    say "DEPLOYS $DEPLOYS has a line that isn't REVISION TIME, such as 4f2a9c1 2026-10-14T16:40:31Z"
    exit 1
  fi
  if [ "$(($(wc -l <"$DEPLOYS")))" -gt 10000 ]; then
    say "DEPLOYS $DEPLOYS has more than 10,000 deploys: keep the last ones"
    exit 1
  fi
  elsewhere=0
  late=0
  # The last line is read too when the file doesn't end with a newline.
  alike=0
  while read -r rev at || [ -n "$rev" ]; do
    # The commit whose name starts with the revision, among the objects:
    # a tag or a branch named like it isn't it, and doesn't hide it.
    full=$(git -C "$repo" rev-parse --disambiguate="$rev" 2>/dev/null |
      git -C "$repo" cat-file --batch-check='%(objecttype) %(objectname)' 2>/dev/null |
      awk '$1 == "commit" { print $2 }')
    case $(printf '%s' "$full" | grep -c .) in
      0)
        elsewhere=$((elsewhere + 1))
        continue
        ;;
      1) ;;
      *)
        alike=$((alike + 1))
        continue
        ;;
    esac
    s=$(git -C "$repo" rev-parse --since="$at")
    s=${s#--max-age=}
    case $s in *[!0-9]* | '') say "DEPLOYS: $at isn't a time"; exit 1 ;; esac
    if [ "$s" -gt "$now" ]; then
      say "DEPLOYS: a deploy at $at is after now; nothing is written"
      exit 1
    fi
    # Not before its commit: Flux dates a snapshot brought back by a revert
    # by when it was first applied.
    c=$(git -C "$repo" log -1 --format=%ct "$full")
    if [ "$s" -lt "$c" ]; then
      s=$c
      late=$((late + 1))
    fi
    printf '%s %s\n' "$s" "$full" >>"$tmp/deploys"
  done <"$DEPLOYS"
  [ "$late" -eq 0 ] || say "$late deploys dated before their commits (a Flux snapshot a revert brought back) are dated by their commits"
  [ "$elsewhere" -eq 0 ] || say "$elsewhere deploys of revisions this clone doesn't have (another branch, a shallow clone) are passed over"
  [ "$alike" -eq 0 ] || say "$alike deploys of revisions that more than one commit's name starts with are passed over: give more of them"
  sort -n -o "$tmp/deploys" "$tmp/deploys"
  if [ ! -s "$tmp/deploys" ]; then
    say "no deploy in DEPLOYS is of this clone: versions are named by their commits"
  fi
fi
# rules COMMIT: what the rules are at COMMIT, as git names the files and
# trees PATHS name there: the same for two commits whose rules are the same.
rules() {
  for p in $PATHS; do
    git -C "$repo" rev-parse --verify -q "$1:${p%/}" 2>/dev/null || echo -
  done | tr '\n' ' '
}
if [ -n "$after" ]; then
  # shellcheck disable=SC2086 # PATHS are the paths, one word each
  git -C "$repo" log --first-parent --reverse --format=%H "$after..HEAD" -- $PATHS >"$tmp/commits"
  n=$(($(wc -l <"$tmp/commits")))
  if [ "$n" -gt "$max" ]; then
    say "$n commits changed PATHS since $AFTER, more than MAX ($max): write the history afresh, from a later SINCE"
    exit 1
  fi
  # Each named after the version AFTER is, at SINCE.
  prev=$(git -C "$repo" rev-parse --since="$SINCE")
  prev=${prev#--max-age=}
  while read -r commit; do
    write "$commit"
  done <"$tmp/commits"
elif [ "$(git -C "$repo" rev-parse --is-shallow-repository)" = true ]; then
  say "REPO $repo is a shallow clone, with no history: only the rules as they are now are written. Clone it whole, or with --shallow-since"
  write HEAD
elif [ -s "$tmp/deploys" ]; then
  since=$(git -C "$repo" rev-parse --since="$SINCE")
  since=${since#--max-age=}
  # From the last deploy at or before SINCE, the rules in effect then; with
  # none, the rules as committed then stand for them.
  from=$(awk -v s="$since" '$1 <= s { n = NR } END { print n + 0 }' "$tmp/deploys")
  : >"$tmp/plan"
  # was is the rules the deploy before put in place (prev is write's: the
  # time of the version before).
  was=
  if [ "$from" -eq 0 ]; then
    # shellcheck disable=SC2086 # PATHS are the paths, one word each
    before=$(git -C "$repo" log --first-parent -1 --format=%H --before="$SINCE" HEAD -- $PATHS)
    if [ -n "$before" ]; then
      say "no deploy at or before SINCE: the rules as committed then stand for those deployed"
      printf '%s %s\n' "$before" "$(git -C "$repo" log -1 --format=%ct "$before")" >>"$tmp/plan"
      was=$(rules "$before")
    fi
    from=1
  fi
  # Each deploy that changed the rules, a rollback too; the same revision
  # deployed again, one after another, changes nothing and isn't looked at.
  tail -n "+$from" "$tmp/deploys" | awk '$2 != last { print; last = $2 }' >"$tmp/walk"
  while read -r d rev; do
    r=$(rules "$rev")
    [ "$r" != "$was" ] || continue
    was=$r
    printf '%s %s\n' "$rev" "$d" >>"$tmp/plan"
  done <"$tmp/walk"
  n=$(($(wc -l <"$tmp/plan")))
  if [ "$n" -gt "$max" ]; then
    say "$n deploys since $SINCE changed the rules, more than MAX ($max): give a later SINCE"
    exit 1
  fi
  # write sets t and commit's like: these are the plan's own.
  while read -r c when; do
    write "$c" "$when"
  done <"$tmp/plan"
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
case $skipped in
  0) ;;
  1) say "1 commit that couldn't be rendered was passed over" ;;
  *) say "$skipped commits that couldn't be rendered were passed over" ;;
esac
if [ "$written" -eq 0 ] && [ -n "$after" ]; then
  say "nothing new on the branch since $AFTER: nothing written"
  exit 0
fi
if [ "$written" -eq 0 ]; then
  say "no version of PATHS since $SINCE: nothing written"
  exit 1
fi
mkdir -p "$OUT"
for v in "$tmp/out"/*; do
  mv "$v" "$OUT/"
done
say "$written versions of $PATHS into $OUT"
