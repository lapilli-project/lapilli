# shellcheck shell=sh
# What every change recipe does, sourced by each host's script, which sets
# recipe and defines four things for its host:
#
#   host_auth      writes the host's auth header line(s) to stdout
#   find_pr B [anyone]
#                  writes the link of the open pull request from branch B,
#                  if there is one of the recipe's own: from the repository
#                  itself, never a fork, into $BASE, opened by the account
#                  the token is (host_me); none when the host can't say who
#                  that is. With anyone, opened by anyone: the host refused
#                  to open another from B, and only one from the repository
#                  itself can be on B, which a fork can't push to
#   create_pr B J  opens a pull request from branch B into $BASE, J a file
#                  holding {title, body}, and writes its link
#   pr_state N     writes open, merged or closed for pull request N
#
# It asks lapilli serve, on --incidents-listen with --changes-token-file's
# token, for the changes approved and checked for a pull request; clones
# the repository; finds the file each was made against by the hash of the
# document Lapilli read; writes the change Lapilli wrote; pushes a branch
# of Lapilli's own (lapilli/proposal-ID) and opens a pull request with the
# evidence as its description; and tells Lapilli its link. It also looks at
# the pull requests open and tells Lapilli which were merged or closed.
#
#   LAPILLI_URL         where --incidents-listen is, such as http://lapilli:8082
#   LAPILLI_TOKEN_FILE  a file holding the token in --changes-token-file
#   CLONE               the repository to clone, as git takes it; git pushes
#                       with the credentials it has (a CI job's, a credential
#                       helper), never ones written in CLONE
#   RULES_PATHS         where in the repository the rules are, space-separated
#                       directories or files (alerts, rules/shop.yaml): a
#                       change is written to a file under one of them, never
#                       elsewhere, and never to what a CI system runs
#                       (.github/, .gitlab-ci.yml and the like)
#   BASE                the branch to open pull requests into (default main)
#   GIT_NAME, GIT_EMAIL who the commits are by (default Lapilli,
#                       lapilli@localhost)
#
# A document no file in the clone is (rules rendered from a chart, read
# back from a cluster) gets no edit: Lapilli is told it failed, and why, and
# the proposal's page keeps its diff for a person. Tokens are read from
# files and given to curl on stdin, never on a command line. A link Lapilli
# or the host gives is never fetched: a pull request is asked about by its
# number, checked, at the API given. Nothing is told to Lapilli when the
# host fails: the next run tries again. What Lapilli sends is not trusted
# to name the file: a Lapilli someone took over could otherwise push a
# workflow to the repository, which a push runs with its secrets before
# anyone reviews it.

set -eu
say() { printf '%s: %s\n' "${recipe:-lib.sh}" "$*" >&2; }
: "${LAPILLI_URL:?where --incidents-listen is, such as http://lapilli:8082}"
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --changes-token-file}"
: "${CLONE:?the repository to clone}"
: "${RULES_PATHS:?where in the repository the rules are, such as alerts}"
BASE=${BASE:-main}
set -f
rps=0
for rp in $RULES_PATHS; do
  case /$rp/ in
    //* | */../* | */./* | /-*) say "RULES_PATHS $rp: want a directory or file in the repository, such as alerts"; exit 1 ;;
  esac
  rps=$((rps + 1))
done
set +f
[ "$rps" -gt 0 ] || { say "RULES_PATHS: want where in the repository the rules are, such as alerts"; exit 1; }
case $CLONE in
  -*) say "CLONE $CLONE: want a repository, not an option"; exit 1 ;;
  *://*:*@*) say "CLONE: give git its credentials as your CI does, not in the URL, where ps shows them"; exit 1 ;;
esac
case $BASE in
  -* | *[!A-Za-z0-9._/-]* | '') say "BASE $BASE: want a branch name"; exit 1 ;;
esac
case $LAPILLI_URL in
  http://* | https://*) ;;
  *) say "LAPILLI_URL $LAPILLI_URL: want an http or https URL"; exit 1 ;;
esac

ltoken=$(cat "$LAPILLI_TOKEN_FILE")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
out=$tmp/out

# lapilli METHOD PATH [FILE]: asks Lapilli, the answer in $out; writes the
# HTTP status.
lapilli() {
  if [ $# -ge 3 ]; then
    printf 'Authorization: Bearer %s\n' "$ltoken" |
      curl -sS --max-time 30 --proto '=http,https' -X "$1" -H @- -H 'Content-Type: application/json' \
        --data-binary "@$3" -o "$out" -w '%{http_code}' "$LAPILLI_URL$2" || echo failed
  else
    printf 'Authorization: Bearer %s\n' "$ltoken" |
      curl -sS --max-time 30 --proto '=http,https' -X "$1" -H @- -o "$out" -w '%{http_code}' "$LAPILLI_URL$2" || echo failed
  fi
}

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

# tell ID JSON: tells Lapilli what became of a change.
tell() {
  printf '%s' "$2" >"$tmp/tell"
  code=$(lapilli PATCH "/api/v1/changes/$1" "$tmp/tell")
  case $code in
    2??) ;;
    *) say "Lapilli didn't take what became of change $1 ($code): $(head -c 300 "$out")"; failed=1 ;;
  esac
}


# in_rules PATH: whether a tracked file is one a change may be written to:
# under RULES_PATHS, and not one a CI system runs, even there, at any depth
# (GitLab's and Azure Pipelines' can be anywhere a project says).
in_rules() {
  case /$1 in
    */.github/* | */.gitea/* | */.forgejo/* | */.gitlab/* | */.azuredevops/* | */.azure-pipelines/* | \
      */.circleci/* | */.buildkite/* | */.woodpecker/* | *gitlab-ci.yml | *gitlab-ci.yaml | \
      */azure-pipelines*.yml | */azure-pipelines*.yaml | */bitbucket-pipelines.yml | */Jenkinsfile | \
      */.drone.yml | */.woodpecker.yml)
      return 1 ;;
  esac
  set -f
  for rp in $RULES_PATHS; do
    rp=${rp%/}
    case $1 in
      "$rp" | "$rp"/*) set +f; return 0 ;;
    esac
  done
  set +f
  return 1
}

# sha256 FILE: its hash, as Lapilli writes it.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

# link_ok LINK: a link the host gave is an http or https URL.
link_ok() {
  case $1 in
    https://* | http://*) [ ${#1} -le 2048 ] ;;
    *) return 1 ;;
  esac
}

main() {
  failed=0
  code=$(lapilli GET /api/v1/changes)
  case $code in
    2??) ;;
    *) say "Lapilli didn't give its changes ($code): $(head -c 300 "$out")"; exit 1 ;;
  esac
  jq -e '.changes | type == "array"' "$out" >/dev/null 2>&1 || {
    say 'Lapilli gave something other than a list of changes'
    exit 1
  }
  cp "$out" "$tmp/changes"
  work=$tmp/clone
  cloned=
  n=$(jq '.changes | length' "$tmp/changes")
  i=0
  while [ "$i" -lt "$n" ]; do
    jq -c ".changes[$i]" "$tmp/changes" >"$tmp/one"
    i=$((i + 1))
    id=$(jq -r '.id' "$tmp/one")
    case $id in *[!0-9]* | '') say "a change has no id: $(jq -c '.id' "$tmp/one")"; failed=1; continue ;; esac
    state=$(jq -r '.state' "$tmp/one")
    case $state in
      opened)
        link=$(jq -r '.link // ""' "$tmp/one")
        num=${link##*/}
        case $num in *[!0-9]* | '') say "change $id: its link doesn't end in a pull request's number"; failed=1; continue ;; esac
        st=$(pr_state "$num") || { failed=1; continue; }
        case $st in
          merged) tell "$id" '{"state":"merged"}' ;;
          closed) tell "$id" '{"state":"closed"}' ;;
        esac
        continue
        ;;
      ready) ;;
      *) continue ;;
    esac
    if [ -z "$cloned" ]; then
      git clone --quiet --branch "$BASE" -- "$CLONE" "$work"
      cloned=1
    fi
    want=$(jq -r '.doc_hash' "$tmp/one")
    case $want in *[!0-9a-f]* | '') say "change $id: no hash to find its file by"; failed=1; continue ;; esac
    # The tracked files, links left out: one could lead anywhere.
    found=
    many=
    git -C "$work" checkout --quiet "$BASE"
    git -C "$work" -c core.quotePath=false ls-files -s >"$tmp/files"
    while IFS= read -r line; do
      mode=${line%% *}
      path=${line#*	}
      [ "$mode" = 120000 ] && continue
      [ -f "$work/$path" ] || continue
      in_rules "$path" || continue
      if [ "$(sha256 "$work/$path")" = "$want" ]; then
        if [ -n "$found" ]; then many=1; fi
        found=$path
      fi
    done <"$tmp/files"
    if [ -n "$many" ]; then
      tell "$id" '{"state":"failed","reason":"more than one file in the repository is the document the rule was read from: make the change by hand"}'
      continue
    fi
    if [ -z "$found" ]; then
      tell "$id" '{"state":"failed","reason":"no file in the repository under RULES_PATHS is the document the rule was read from (rules rendered from a chart, or read back from a cluster): make the change where it is written; its diff is on the proposal'"'"'s page"}'
      continue
    fi
    branch=lapilli/proposal-$id
    git -C "$work" checkout --quiet -B "$branch" "$BASE"
    jq -j '.document' "$tmp/one" >"$work/$found"
    jq -r '.title' "$tmp/one" >"$tmp/msg"
    printf '\n' >>"$tmp/msg"
    jq -r '.body' "$tmp/one" >>"$tmp/msg"
    git -C "$work" add -- "$found"
    git -C "$work" -c user.name="${GIT_NAME:-Lapilli}" -c user.email="${GIT_EMAIL:-lapilli@localhost}" \
      commit --quiet --no-verify -F "$tmp/msg"
    # The branch is Lapilli's own: pushed again over a run that stopped.
    git -C "$work" push --quiet --force origin "HEAD:refs/heads/$branch"
    link=$(find_pr "$branch") || { failed=1; continue; }
    if [ -z "$link" ]; then
      jq '{title, body}' "$tmp/one" >"$tmp/pr"
      if ! link=$(create_pr "$branch" "$tmp/pr"); then
        # Refused, as when one is open from the branch already (a run that
        # stopped after opening it, with a token the host can't say whose
        # it is): that one is the change's, whoever opened it, since it is
        # from Lapilli's branch in the repository itself, into BASE.
        link=$(find_pr "$branch" anyone) || { failed=1; continue; }
        if [ -z "$link" ]; then
          failed=1
          continue
        fi
        say "change $id: the pull request open from $branch already is taken as its own"
      fi
    fi
    if ! link_ok "$link"; then
      say "change $id: the host gave no link for its pull request"
      failed=1
      continue
    fi
    tell "$id" "$(jq -cn --arg link "$link" '{state: "opened", link: $link}')"
  done
  [ "$failed" = 0 ]
}
