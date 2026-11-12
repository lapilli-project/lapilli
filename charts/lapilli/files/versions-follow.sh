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
#           when the look does
#   REPO, PATHS, RENDER, RENDER_PREPARE, UNRENDERED, MAX
#           as git.sh takes them
#   GIT_SH  git.sh (default: the one beside this)
#
# A version is added by renaming it into HISTORY from a directory in it
# whose name starts with a dot, which lapilli passes over, so lapilli never
# reads one half written. A look that fails says why and leaves HISTORY as
# it was; the next tries again, but a branch that hasn't moved since a look
# that went through isn't looked at again.
#
#   while :; do git -C ~/src/monitoring pull -q --ff-only; sleep 300; done &
#   REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 HISTORY=/var/lib/lapilli/history ./follow.sh
#   lapilli serve --rules-history /var/lib/lapilli/history ...
set -eu
say() { printf 'follow.sh: %s\n' "$*" >&2; }
: "${HISTORY:?the history to keep, which lapilli serve reads}"
every=${EVERY:-300}
case $every in *[!0-9]* | '') say "EVERY $every: want seconds"; exit 1 ;; esac
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
  looked=$head
}

while :; do
  if ! look && [ "$every" -eq 0 ]; then
    exit 1
  fi
  [ "$every" -gt 0 ] || exit 0
  sleep "$every"
done
