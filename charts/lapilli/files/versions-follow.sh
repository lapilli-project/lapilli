#!/bin/sh
# Keeps a history of the rules, as git.sh writes one, up to date with the
# branch: at each look, the versions of the commits on it since the newest
# version's are written as git.sh writes them (AFTER that commit) and added
# beside it. lapilli serve, given --rules-history without --rules, records
# each version added as of its own time and replays the hours since then
# again. It fetches nothing: fetching needs the repository's credentials,
# which RENDER mustn't see, so run git fetch apart from it (the chart does,
# in a container of its own).
#
#   HISTORY the directory to keep, which lapilli serve reads. With no
#           version in it yet, git.sh writes it whole, from SINCE
#   SINCE   from when, for a HISTORY with no version yet, as git.sh takes it
#   EVERY   seconds between looks (default 300); 0 looks once, and fails
#           when the look does. It is the fetch's too, as the chart has it:
#           a commit is named no earlier than when it was fetched, less
#           EVERY and a minute (git.sh's LATE)
#   KEEP    seconds (default 0, all): versions older than this are taken
#           away after each look, but the newest of them, in effect then.
#           The chart gives serve.backfill and a day
#   REPO, PATHS, RENDER, RENDER_PREPARE, MAX, MAX_TREE_FILES, MAX_TREE_BYTES
#           as git.sh takes them. git.sh runs following (FOLLOWING=1): a
#           commit that can't be rendered is passed over, saying why, one
#           dated after now is named now, and of more than MAX new commits
#           the newest MAX are written, so one commit never stops it
#   GIT_SH  git.sh (default: the one beside this)
#
# A version is added by renaming it into HISTORY from a directory in it
# whose name starts with a dot, which lapilli passes over, so lapilli never
# reads one half written. HISTORY/.head is the commit the last look got to,
# so a commit passed over isn't rendered again at each look. A look that
# fails says why and leaves HISTORY as it was; the next tries again, but a
# branch that hasn't moved since a look that went through isn't looked at
# again.
#
#   while :; do git -C ~/src/monitoring pull -q --ff-only; sleep 300; done &
#   REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 HISTORY=/var/lib/lapilli/history ./follow.sh
#   lapilli serve --rules-history /var/lib/lapilli/history ...
set -eu
say() { printf 'follow.sh: %s\n' "$*" >&2; }
# git as the clone has it, nothing more.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_ATTR_NOSYSTEM=1
: "${HISTORY:?the history to keep, which lapilli serve reads}"
every=${EVERY:-300}
keep=${KEEP:-0}
case $every$keep in *[!0-9]* | '') say "EVERY $every, KEEP $keep: want seconds"; exit 1 ;; esac
export FOLLOWING=1 LATE="${LATE:-$((every + 60))}"
git_sh=${GIT_SH:-$(dirname "$0")/git.sh}
repo=${REPO:-.}
mkdir -p "$HISTORY"
# What a look cut short left.
rm -rf "$HISTORY"/.follow.*

# look: add the versions since the newest, if the branch has moved since
# the last look that went through.
looked=
look() {
  if ! head=$(git -C "$repo" rev-parse --verify -q 'HEAD^{commit}' 2>/dev/null); then
    say "REPO $repo has no branch to follow yet"
    return 1
  fi
  [ "$head" != "$looked" ] || return 0
  # The newest version, as git.sh names them: a glob is in order.
  newest=
  for v in "$HISTORY"/*; do
    case ${v##*/} in
      [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z) newest=${v##*/} ;;
    esac
  done
  out=$(mktemp -d "$HISTORY/.follow.XXXXXX")
  if [ -z "$newest" ]; then
    if [ -z "${SINCE:-}" ]; then
      say "HISTORY $HISTORY has no version yet: give SINCE, from when to write it"
      rm -rf "$out"
      return 1
    fi
    if ! OUT=$out/v sh "$git_sh"; then
      rm -rf "$out"
      return 1
    fi
  else
    if ! after=$(cat "$HISTORY/$newest/.commit" 2>/dev/null); then
      say "$HISTORY/$newest names no commit (.commit, as git.sh writes it): write the history afresh"
      rm -rf "$out"
      return 1
    fi
    # From where the last look got to, when that is still on the branch:
    # commits passed over then aren't rendered again.
    if h=$(cat "$HISTORY/.head" 2>/dev/null) && git -C "$repo" merge-base --is-ancestor "$after" "$h" 2>/dev/null &&
      git -C "$repo" merge-base --is-ancestor "$h" HEAD 2>/dev/null; then
      after=$h
    fi
    since=$(printf '%s' "$newest" | sed -E 's/^(....)(..)(..)T(..)(..)(..)Z$/\1-\2-\3T\4:\5:\6Z/')
    if ! AFTER=$after SINCE=$since OUT=$out/v sh "$git_sh"; then
      rm -rf "$out"
      return 1
    fi
  fi
  added=0
  last=
  for v in "$out"/v/*; do
    [ -d "$v" ] || continue
    last=${v##*/}
    mv "$v" "$HISTORY/$last"
    added=$((added + 1))
  done
  rm -rf "$out"
  [ "$added" -eq 0 ] || say "$added versions added to $HISTORY, the newest $last"
  printf '%s\n' "$head" >"$HISTORY/.head.new" && mv "$HISTORY/.head.new" "$HISTORY/.head"
  looked=$head
  prune
}

# prune: the versions from before KEEP seconds ago go, but the newest of
# them, the one in effect then.
prune() {
  [ "$keep" -gt 0 ] || return 0
  edge=$(date -u -d "@$(($(date -u +%s) - keep))" +%Y%m%dT%H%M%SZ 2>/dev/null || date -u -r "$(($(date -u +%s) - keep))" +%Y%m%dT%H%M%SZ)
  old=
  for v in "$HISTORY"/*; do
    case ${v##*/} in
      [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z) ;;
      *) continue ;;
    esac
    # Names in the basic form, as numbers, are in their times' order.
    n=${v##*/}
    [ "$(printf '%s' "$n" | tr -d TZ)" -lt "$(printf '%s' "$edge" | tr -d TZ)" ] || break
    [ -z "$old" ] || rm -rf "$old"
    old=$v
  done
}

while :; do
  if ! look && [ "$every" -eq 0 ]; then
    exit 1
  fi
  [ "$every" -gt 0 ] || exit 0
  sleep "$every"
done
