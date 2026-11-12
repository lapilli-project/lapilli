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
# of Lapilli's own (lapilli/LEDGER/proposal-ID; lapilli/proposal-ID from a
# Lapilli before 0.28.1, which names no ledger) and opens a pull request
# with the evidence as its description; and tells Lapilli its link. It
# also looks at the pull requests open and tells Lapilli which were merged
# or closed.
#
#   LAPILLI_URL         where --incidents-listen is, such as http://lapilli:8082
#   LAPILLI_TOKEN_FILE  a file holding the token in --changes-token-file
#   CLONE               the repository to clone, as git takes it; git pushes
#                       with the credentials it has (a CI job's, a credential
#                       helper), never ones written in CLONE
#   RULES_PATHS         where in the repository the rules are, space-separated
#                       directories or files (alerts, rules/shop.yaml): a
#                       change is written to a file under one of them, never
#                       elsewhere, and only to a rule file (rule_file),
#                       never to what a CI system runs (.github/,
#                       .gitlab-ci.yml and the like)
#   BASE                the branch to open pull requests into (default main)
#   GIT_NAME, GIT_EMAIL who the commits are by (default Lapilli,
#                       lapilli@localhost)
#   MAX_CHANGES         the most branches a run pushes (default 10); the
#                       changes past it wait for the next run
#
# A document no file in the clone is (rules rendered from a chart, read
# back from a cluster) gets no edit: Lapilli is told it failed, and why, and
# the proposal's page keeps its diff for a person. Tokens are read from
# files and given to curl on stdin, never on a command line. A link Lapilli
# or the host gives is never fetched: a pull request is asked about by its
# number, checked, at the API given. Nothing is told to Lapilli when the
# host fails: the next run tries again. What Lapilli sends is not trusted
# to name the file, or to be a rule file: a Lapilli someone took over could
# otherwise push a workflow, a script or a build's configuration to the
# repository, which a push runs with its secrets before anyone reviews it.
# Nor to name a branch that may be pushed over: a branch is pushed only
# where there is none, so a pull request people reviewed never changes
# under them. What Lapilli or the host says is shown as JSON, so no escape
# sequence reaches a terminal or a CI log.

set -eu
say() { printf '%s: %s\n' "${recipe:-lib.sh}" "$*" >&2; }
# shown FILE: what a server answered, as a JSON string of 300 bytes at
# most, to be said.
shown() { head -c 300 "$1" | jq -Rsc . 2>/dev/null || echo '"(unreadable)"'; }
: "${LAPILLI_URL:?where --incidents-listen is, such as http://lapilli:8082}"
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --changes-token-file}"
: "${CLONE:?the repository to clone}"
: "${RULES_PATHS:?where in the repository the rules are, such as alerts}"
BASE=${BASE:-main}
MAX_CHANGES=${MAX_CHANGES:-10}
case $MAX_CHANGES in
  '' | *[!0-9]* | 0*) say "MAX_CHANGES $MAX_CHANGES: want how many branches a run may push, such as 10"; exit 1 ;;
esac

# ci PATH: whether a path in the repository is, or is under, what a CI
# system or a tool a push sets off runs, in any letter case, at any depth:
# never written, whatever else holds. rule_file keeps the rest out; this
# is a second lock.
ci() {
  set -- "/$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  case $1 in
    */.github/* | */.gitea/* | */.forgejo/* | */.gitlab/* | */.azuredevops/* | */.azure-pipelines/* | */.circleci/* | \
      */.buildkite/* | */.woodpecker/* | */.tekton/* | */.drone/* | */.travis/* | */.semaphore/* | */.harness/* | \
      */.teamcity/* | */.devcontainer/* | */.husky/* | */.githooks/* | */.git/* | \
      */.github | */.gitea | */.forgejo | */.gitlab | */.azuredevops | */.azure-pipelines | */.circleci | */.buildkite | \
      */.woodpecker | */.tekton | */.drone | */.travis | */.semaphore | */.harness | */.teamcity | */.devcontainer | \
      */.husky | */.githooks | */.git | \
      *gitlab-ci.yml | *gitlab-ci.yaml | */azure-pipelines*.yml | */azure-pipelines*.yaml | */bitbucket-pipelines.yml | \
      */jenkinsfile* | */.drone.yml | */.drone.yaml | */.woodpecker.yml | */.woodpecker.yaml | */.travis.yml | \
      */cloudbuild*.yaml | */cloudbuild*.yml | */cloudbuild*.json | */buildspec*.yml | */buildspec*.yaml | \
      */appveyor.yml | */.appveyor.yml | */.cirrus.yml | */codemagic.yaml | */bitrise.yml | */codefresh.yml | \
      */wercker.yml | */.pre-commit-config.yaml | */.pre-commit-hooks.yaml | */package.json | */renovate.json | \
      */.renovaterc* | */taskfile.yml | */taskfile.yaml | */skaffold.yaml | */.goreleaser.yml | */.goreleaser.yaml | \
      */makefile | */*.sh)
      return 0 ;;
  esac
  return 1
}

set -f
rps=0
for rp in $RULES_PATHS; do
  case /$rp/ in
    //* | */../* | */./* | /-*) say "RULES_PATHS $rp: want a directory or file in the repository, such as alerts"; exit 1 ;;
  esac
  if ci "${rp%/}"; then
    say "RULES_PATHS $rp: it is what a CI system runs, which is never written"
    exit 1
  fi
  rps=$((rps + 1))
done
set +f
[ "$rps" -gt 0 ] || { say "RULES_PATHS: want where in the repository the rules are, such as alerts"; exit 1; }
# The URL's authority, user and password and all. An http URL with a user
# in it holds a token (https://TOKEN@host/...), which ps shows and git keeps
# in the clone's .git/config; ssh's user is no secret.
authority=
case $CLONE in
  *://*) authority=${CLONE#*://}; authority=${authority%%/*} ;;
esac
case $CLONE in
  -*) say "CLONE $CLONE: want a repository, not an option"; exit 1 ;;
  [Hh][Tt][Tt][Pp]://* | [Hh][Tt][Tt][Pp][Ss]://*)
    case $authority in
      *@*) say "CLONE: give git its credentials as your CI does, not in the URL, where ps shows them"; exit 1 ;;
    esac
    ;;
esac
case $authority in
  *:*@*) say "CLONE: give git its credentials as your CI does, not in the URL, where ps shows them"; exit 1 ;;
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
    *) say "Lapilli didn't take what became of change $1 ($code): $(shown "$out")"; failed=1 ;;
  esac
}


# in_rules PATH: whether a tracked file is under RULES_PATHS.
in_rules() {
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

# writable PATH MODE: whether a change may be written to a tracked file
# under RULES_PATHS, as git keeps it: a regular file (mode 100644, not one
# that runs), named as rules are (*.yml, *.yaml, *.json, *.rules), and not
# what a CI system runs (ci), at any depth (GitLab's and Azure Pipelines'
# can be anywhere a project says), in any letter case. What it holds is
# rule_file's.
writable() {
  [ "$2" = 100644 ] || return 1
  case $(printf '%s' "${1##*/}" | tr '[:upper:]' '[:lower:]') in
    *.yml | *.yaml | *.json | *.rules) ;;
    *) return 1 ;;
  esac
  ! ci "$1"
}

# rule_file FILE NAME: whether FILE holds alert rules and nothing else, read
# as JSON when NAME is *.json, as YAML otherwise: what Lapilli edits. Each
# document is, by the keys at its top, a Prometheus rule file (groups, a
# list, and a ruler's namespace), a Grafana alerting provisioning file
# (apiVersion and groups, and what else such a file provisions), a
# PrometheusRule or Google Cloud's Rules, ClusterRules or GlobalRules
# (apiVersion, kind, metadata, spec), or a ConfigMap (apiVersion, kind,
# metadata, data). A CI system's file needs keys of its own (on, jobs,
# steps, script, stages, pipelines), and GitLab refuses a pipeline whose
# groups is a list. YAML that can be read in more ways than one is refused
# rather than read: a key at the top that is quoted, in braces, merged,
# anchored or tagged; a directive; a document that is text; a key twice; a
# line that starts with a tab; a NUL; and the line breaks YAML 1.1 has
# besides LF and CRLF (CR, NEL, LS, PS), which awk reads as within a line
# and a CI system's parser as a new one.
rule_file() {
  case $(printf '%s' "$2" | tr '[:upper:]' '[:lower:]') in
    *.json)
      jq -e -s '
        def only($allowed): (keys - $allowed) == [];
        length == 1 and (.[0] | type == "object" and (
          if has("kind") then
            ((.kind == "PrometheusRule" or .kind == "Rules" or .kind == "ClusterRules" or .kind == "GlobalRules")
              and has("spec") and only(["apiVersion", "kind", "metadata", "spec"]))
            or (.kind == "ConfigMap" and has("data") and only(["apiVersion", "kind", "metadata", "data"]))
          else
            (.groups | type == "array") and only(["groups", "namespace", "apiVersion", "deleteRules", "contactPoints",
              "deleteContactPoints", "policies", "resetPolicies", "templates", "deleteTemplates", "muteTimes", "deleteMuteTimes"])
          end))' "$1" >/dev/null 2>&1
      return
      ;;
  esac
  [ "$(tr -d '\000' <"$1" | wc -c)" -eq "$(wc -c <"$1")" ] || return 1
  LC_ALL=C awk -v q="'" '
    BEGIN { nel = "\302\205"; ls = "\342\200\250"; ps = "\342\200\251" }
    function done(   k, allowed, need) {
      if (nkeys == 0) return
      docs++
      if (kind == "PrometheusRule" || kind == "Rules" || kind == "ClusterRules" || kind == "GlobalRules") {
        allowed = " apiVersion kind metadata spec "; need = "spec"
      } else if (kind == "ConfigMap") {
        allowed = " apiVersion kind metadata data "; need = "data"
      } else if (kind == "" && list) {
        allowed = " groups namespace apiVersion deleteRules contactPoints deleteContactPoints policies resetPolicies templates deleteTemplates muteTimes deleteMuteTimes "
        need = "groups"
      } else {
        bad = 1; return
      }
      for (k in keys) if (index(allowed, " " k " ") == 0) bad = 1
      if (!(need in keys)) bad = 1
      split("", keys); nkeys = 0; kind = ""; list = 0
    }
    { sub(/\r$/, "") }
    index($0, "\r") || index($0, nel) || index($0, ls) || index($0, ps) { bad = 1; next }
    /^(---|\.\.\.)[ \t]*(#.*)?$/ { done(); expect = 0; seq = 0; next }
    /^[ \t]*(#.*)?$/ { next }
    expect {
      expect = 0
      if ($0 ~ /^ *-([ \t]|$)/) list = 1; else bad = 1
    }
    /^-([ \t]|$)/ { if (!seq) bad = 1; next }
    /^\t/ { bad = 1; next }
    /^ / { next }
    {
      seq = 0
      if ($0 !~ /^[A-Za-z][A-Za-z0-9_]*:([ \t]|$)/) { bad = 1; next }
      key = substr($0, 1, index($0, ":") - 1)
      if (key in keys) bad = 1
      keys[key] = 1; nkeys++
      rest = substr($0, index($0, ":") + 1)
      sub(/^[ \t]+/, "", rest)
      if (rest ~ /^#/) rest = ""
      if (rest ~ /^[&*!<]/) bad = 1
      if (rest == "") seq = 1
      if (key == "groups") {
        if (rest == "") expect = 1
        else if (rest ~ /^\[/) list = 1
        else bad = 1
      }
      if (key == "kind") {
        sub(/[ \t]+#.*$/, "", rest); sub(/[ \t]+$/, "", rest)
        if (rest ~ /^".*"$/ || substr(rest, 1, 1) == q && substr(rest, length(rest), 1) == q) rest = substr(rest, 2, length(rest) - 2)
        kind = rest
      }
    }
    END { if (expect) bad = 1; done(); exit !(docs > 0 && !bad) }
  ' "$1"
}

# sha256 FILE: its hash, as Lapilli writes it.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

# on_remote BRANCH: the tree the branch holds on the remote; nothing when
# there is no such branch.
on_remote() {
  if ! git -C "$work" ls-remote --exit-code --heads origin "refs/heads/$1" >/dev/null 2>&1; then
    return 0
  fi
  git -C "$work" fetch --quiet --no-tags origin "refs/heads/$1" && git -C "$work" rev-parse 'FETCH_HEAD^{tree}'
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
  pushed=0
  left=0
  code=$(lapilli GET /api/v1/changes)
  case $code in
    2??) ;;
    *) say "Lapilli didn't give its changes ($code): $(shown "$out")"; exit 1 ;;
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
    if [ "$pushed" -ge "$MAX_CHANGES" ]; then
      left=$((left + 1))
      continue
    fi
    ledger=$(jq -r '.ledger // ""' "$tmp/one")
    case $ledger in
      '') branch=lapilli/proposal-$id ;;
      *[!0-9a-f]*) say "change $id: its ledger isn't one: $(jq -c '.ledger' "$tmp/one")"; failed=1; continue ;;
      *) branch=lapilli/$ledger/proposal-$id ;;
    esac
    if [ ${#ledger} -gt 32 ]; then
      say "change $id: its ledger isn't one: $(jq -c '.ledger' "$tmp/one")"
      failed=1
      continue
    fi
    if [ -z "$cloned" ]; then
      git clone --quiet --branch "$BASE" -- "$CLONE" "$work"
      cloned=1
    fi
    want=$(jq -r '.doc_hash' "$tmp/one")
    case $want in *[!0-9a-f]* | '') say "change $id: no hash to find its file by"; failed=1; continue ;; esac
    # The tracked files, links left out: one could lead anywhere. Only a
    # rule file may be written: one whose hash is the document's but isn't
    # (a script, a CI system's file, one that runs) is never taken.
    found=
    many=
    unfit=
    git -C "$work" checkout --quiet "$BASE"
    git -C "$work" -c core.quotePath=false ls-files -s >"$tmp/files"
    while IFS= read -r line; do
      mode=${line%% *}
      path=${line#*	}
      [ "$mode" = 120000 ] && continue
      [ -f "$work/$path" ] || continue
      in_rules "$path" || continue
      [ "$(sha256 "$work/$path")" = "$want" ] || continue
      if writable "$path" "$mode" && rule_file "$work/$path" "$path"; then
        if [ -n "$found" ]; then many=1; fi
        found=$path
      else
        unfit=1
      fi
    done <"$tmp/files"
    if [ -n "$many" ]; then
      tell "$id" '{"state":"failed","reason":"more than one file in the repository is the document the rule was read from: make the change by hand"}'
      continue
    fi
    if [ -z "$found" ] && [ -n "$unfit" ]; then
      tell "$id" '{"state":"failed","reason":"the file in the repository that is the document the rule was read from is not one a change is written to: a rule file alone (a Prometheus rule file, a Grafana provisioning file, a PrometheusRule or a ConfigMap), named *.yml, *.yaml, *.json or *.rules, not executable, and not what a CI system runs. Make the change by hand"}'
      continue
    fi
    if [ -z "$found" ]; then
      tell "$id" '{"state":"failed","reason":"no file in the repository under RULES_PATHS is the document the rule was read from (rules rendered from a chart, or read back from a cluster): make the change where it is written; its diff is on the proposal'"'"'s page"}'
      continue
    fi
    jq -j '.document' "$tmp/one" >"$tmp/document"
    if ! rule_file "$tmp/document" "$found"; then
      tell "$id" '{"state":"failed","reason":"the change Lapilli wrote is not a rule file alone, as the file it changes is: nothing was pushed"}'
      continue
    fi
    pushed=$((pushed + 1))
    git -C "$work" checkout --quiet -B "$branch" "$BASE"
    cp "$tmp/document" "$work/$found"
    jq -r '.title' "$tmp/one" >"$tmp/msg"
    printf '\n' >>"$tmp/msg"
    jq -r '.body' "$tmp/one" >>"$tmp/msg"
    git -C "$work" add -- "$found"
    git -C "$work" -c user.name="${GIT_NAME:-Lapilli}" -c user.email="${GIT_EMAIL:-lapilli@localhost}" \
      commit --quiet --no-verify -F "$tmp/msg"
    # A branch is pushed only where there is none: never over one, which a
    # pull request people reviewed, approved or set to merge may be from.
    # One holding this very change is a run's that stopped: its pull
    # request is found, or opened. A recipe before 0.28.1 pushed to
    # lapilli/proposal-ID, the ledger unnamed: such a branch holding this
    # change is taken; holding another, it is left as it is.
    mine=$(git -C "$work" rev-parse 'HEAD^{tree}')
    there=$(on_remote "$branch") || { failed=1; continue; }
    if [ -z "$there" ] && [ "$branch" != "lapilli/proposal-$id" ]; then
      old=$(on_remote "lapilli/proposal-$id") || { failed=1; continue; }
      if [ -n "$old" ] && [ "$old" = "$mine" ]; then
        branch=lapilli/proposal-$id there=$old
      elif [ -n "$old" ]; then
        say "change $id: lapilli/proposal-$id, from a recipe before 0.28.1 or another ledger, holds another change and is left as it is; this one goes on $branch"
      fi
    fi
    if [ -z "$there" ]; then
      if ! git -C "$work" push --quiet --force-with-lease="refs/heads/$branch:" origin "HEAD:refs/heads/$branch"; then
        say "change $id: $branch wasn't pushed"
        failed=1
        continue
      fi
    elif [ "$there" != "$mine" ]; then
      tell "$id" "$(jq -cn --arg b "$branch" '{state: "failed", reason: ("the branch " + $b + " holds another change already, which a pull request may be open from and people may have reviewed: it is never pushed over. Close its pull request and delete the branch, then propose the change again")}')"
      continue
    fi
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
  if [ "$left" -gt 0 ]; then
    say "$left changes ready are left for the next run: a run pushes $MAX_CHANGES branches at most (MAX_CHANGES)"
  fi
  [ "$failed" = 0 ]
}
