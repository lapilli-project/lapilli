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
#                       elsewhere, only to a file of alert rules
#                       (rule_file), never to what a CI system runs
#                       (.github/, .gitlab-ci.yml and the like), and only
#                       its rules (fits)
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
# never written, whatever else holds. fits keeps the rest out; this is a
# second lock.
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
# rule_file's, and what a change may do to it fits'.
writable() {
  [ "$2" = 100644 ] || return 1
  case $(printf '%s' "${1##*/}" | tr '[:upper:]' '[:lower:]') in
    *.yml | *.yaml | *.json | *.rules) ;;
    *) return 1 ;;
  esac
  ! ci "$1"
}

# What a change may write is read here document by document, as YAML reads
# a file apart, with nothing but sh, awk and jq: a document a CI system, a
# GitOps controller or kubectl reads must be read the same way here, or
# refused. fits says whether what Lapilli wrote may be written over the
# file it was made against.

# nobom IN OUT: IN without a UTF-8 byte order mark at its start, which
# YAML and JSON readers pass over.
nobom() {
  if [ "$(head -c 3 "$1" | od -An -tx1 | tr -d ' \n')" = efbbbf ]; then
    tail -c +4 "$1" >"$2"
  else
    cp "$1" "$2"
  fi
}

# plain_text FILE: whether FILE has no NUL, and breaks lines only where awk
# does: YAML 1.1 also breaks them at a CR alone, NEL, LS and PS.
plain_text() {
  [ "$(tr -d '\000' <"$1" | wc -c)" -eq "$(wc -c <"$1")" ] || return 1
  LC_ALL=C awk '
    BEGIN { nel = "\302\205"; ls = "\342\200\250"; ps = "\342\200\251" }
    { sub(/\r$/, "") }
    index($0, "\r") || index($0, nel) || index($0, ls) || index($0, ps) { bad = 1; exit }
    END { exit bad }' "$1"
}

# split_docs FILE DIR: FILE's documents as YAML reads them apart, into
# DIR/0, DIR/1 and on, and writes how many (more than 100: refused). A
# line of --- or ..., alone or followed by a space or a tab, is YAML's
# marker wherever it is, in a block or a quoted text too: --- starts the
# next document, the line in it; after ..., the next line does. ---# is
# no marker.
split_docs() {
  mkdir -p "$2"
  LC_ALL=C awk -v dir="$2" '
    function next_doc() { close(f); n++; f = dir "/" n; printf "" > f }
    BEGIN { n = 0; f = dir "/0"; printf "" > f }
    /^---([ \t]|\r?$)/ { next_doc() }
    { print > f }
    /^\.\.\.([ \t]|\r?$)/ { next_doc() }
    END { close(f); if (n >= 100) print -1; else print n + 1 }' "$1"
}

# doc_read DOC [FRAME]: one document, as split_docs wrote it, read by the
# keys at its top, line by line. Writes what it is, then those keys:
#
#   bad    what YAML could read in more than one way, or awk not as YAML
#          does: a document whose first line is indented (its top unseen
#          here), a key at the top quoted, in braces, merged, anchored,
#          tagged or twice, a value at the top quoted or in brackets over
#          more than one line, a directive, a tab leading a line, a
#          marker followed by more than a comment
#   empty  comments, or nothing
#   other  keys, but not alert rules
#   plain  a Prometheus rule file: groups, a list (and a ruler's
#          namespace), or a Grafana alerting provisioning file
#   crd    a PrometheusRule, or Google Cloud's Rules, ClusterRules or
#          GlobalRules: apiVersion, kind, metadata, spec
#   cm     a ConfigMap: apiVersion, kind, metadata, data, immutable
#
# With FRAME, for one of the last three, also writes the document with its
# rules taken out (FRAME): the lines under groups, spec, or each of data's
# entries, each such run as one line \001. A ConfigMap's entries go to
# FRAME.N, one a file, and how each is written (|, |-, a value) to
# FRAME.list, N and it a line.
doc_read() {
  LC_ALL=C awk -v q="'" -v frame="${2:-}" '
    function value(v) {
      sub(/^[ \t]+/, "", v); sub(/[ \t]+#.*$/, "", v); sub(/[ \t]+$/, "", v)
      if (v ~ /^"[A-Za-z0-9._\/-]*"$/ || substr(v, 1, 1) == q && substr(v, length(v), 1) == q && length(v) > 1 &&
        substr(v, 2, length(v) - 2) ~ /^[A-Za-z0-9._\/-]*$/) return substr(v, 2, length(v) - 2)
      if (v ~ /^[A-Za-z0-9][A-Za-z0-9._\/-]*$/) return v
      bad = 1
      return ""
    }
    function balanced(v,   s, o, c) {
      s = v; gsub(/"[^"]*"/, "", s); gsub(q "[^" q "]*" q, "", s)
      if (index(s, "\"") || index(s, q)) return 0
      o = gsub(/[[{]/, "", s); c = gsub(/[]}]/, "", s)
      return o == c
    }
    function metaflow(v,   n, i, p, kv) {
      sub(/[ \t]+#.*$/, "", v); sub(/[ \t]+$/, "", v)
      if (v !~ /^\{ *([A-Za-z0-9_.\/-]+ *: *[A-Za-z0-9._\/-]+( *, *[A-Za-z0-9_.\/-]+ *: *[A-Za-z0-9._\/-]+)*)? *\}$/) { bad = 1; return }
      v = substr(v, 2, length(v) - 2)
      n = split(v, p, ",")
      for (i = 1; i <= n; i++) {
        split(p[i], kv, ":"); gsub(/ /, "", kv[1]); gsub(/ /, "", kv[2])
        if (kv[1] == "name" || kv[1] == "namespace") {
          if (kv[1] in meta) bad = 1
          meta[kv[1]] = kv[2]
        }
      }
    }
    function metaline(   n, k) {
      match($0, /^ +/); n = RLENGTH
      if (mind < 0) mind = n
      if (n < mind) { bad = 1; return }
      if (n > mind) return
      if ($0 !~ /^ +[A-Za-z0-9_.\/-]+:([ \t]|$)/) { bad = 1; return }
      k = substr($0, n + 1, index($0, ":") - n - 1)
      if (k == "name" || k == "namespace") {
        if (k in meta) bad = 1
        meta[k] = value(substr($0, index($0, ":") + 1))
      }
    }
    function dataline(   n, k, r) {
      match($0, /^ +/); n = RLENGTH
      if (dind < 0) dind = n
      if (n < dind) { bad = 1; return }
      if (n > dind) { E[NR] = ne; return }
      if ($0 !~ /^ +[A-Za-z0-9._-]+:([ \t]|$)/) { bad = 1; return }
      k = substr($0, n + 1, index($0, ":") - n - 1)
      if (k in entry) bad = 1
      entry[k] = ++ne
      r = substr($0, index($0, ":") + 1); sub(/^[ \t]+/, "", r); sub(/[ \t]+#.*$/, "", r); sub(/^#.*$/, "", r)
      style[ne] = r
      E[NR] = "key"
    }
    function only(set,   k) { for (k in seen) if (index(set, " " k " ") == 0) return 0; return 1 }
    BEGIN { mind = -1; dind = -1 }
    { sub(/\r$/, ""); L[NR] = $0 }
    NR == 1 && /^(---|\.\.\.)([ \t]|$)/ { if ($0 !~ /^(---|\.\.\.)[ \t]*(#.*)?$/) bad = 1; O[NR] = "!"; next }
    /^\.\.\.([ \t]|$)/ { if ($0 !~ /^\.\.\.[ \t]*(#.*)?$/) bad = 1; O[NR] = "!"; next }
    /^[ \t]*(#.*)?$/ { O[NR] = cur; if (cur == "data" && ne) E[NR] = ne; next }
    expect { expect = 0; if ($0 ~ /^ *-([ \t]|$)/) list = 1 }
    /^-([ \t]|$)/ { if (!seq || cur == "metadata" || cur == "data") bad = 1; O[NR] = cur; next }
    /^\t/ { bad = 1; next }
    /^ / {
      if (nkeys == 0) { bad = 1; next }
      O[NR] = cur
      if (cur == "metadata") metaline()
      if (cur == "data") dataline()
      next
    }
    {
      if ($0 !~ /^[A-Za-z][A-Za-z0-9_]*:([ \t]|$)/) { bad = 1; next }
      key = substr($0, 1, index($0, ":") - 1)
      if (key in seen) bad = 1
      seen[key] = 1; K[++nkeys] = key; cur = key; O[NR] = key ":"
      rest = substr($0, index($0, ":") + 1); sub(/^[ \t]+/, "", rest)
      if (rest ~ /^#/) rest = ""
      seq = rest == ""
      c = substr(rest, 1, 1)
      if (c ~ /[&*!<%@`]/) bad = 1
      if (c == "\"" || c == q) { v = rest; sub(/[ \t]+#.*$/, "", v); sub(/[ \t]+$/, "", v); if (length(v) < 2 || substr(v, length(v), 1) != c) bad = 1 }
      if ((c == "[" || c == "{") && !balanced(rest)) bad = 1
      if (key == "groups") { if (rest == "") expect = 1; else if (c == "[") list = 1 }
      if (key == "kind") kind = value(rest)
      if (key == "apiVersion") api = value(rest)
      if (key == "metadata" && rest != "") metaflow(rest)
      if (key == "data" && rest != "") datainline = 1
    }
    END {
      if (bad) { print "bad"; exit }
      if (nkeys == 0) { print "empty"; exit }
      type = "other"
      if (kind ~ /^(PrometheusRule|Rules|ClusterRules|GlobalRules)$/ && only(" apiVersion kind metadata spec ") && ("spec" in seen)) {
        type = "crd"; block = "spec"
      } else if (kind == "ConfigMap" && only(" apiVersion kind metadata data immutable ") && ("data" in seen) && !datainline) {
        type = "cm"; block = "data"
      } else if (!("kind" in seen) && list && ("groups" in seen) &&
        only(" groups namespace apiVersion deleteRules contactPoints deleteContactPoints policies resetPolicies templates deleteTemplates muteTimes deleteMuteTimes ")) {
        type = "plain"; block = "groups"
      }
      s = type
      for (i = 1; i <= nkeys; i++) s = s " " K[i]
      print s
      if (frame == "" || type == "other") exit
      printf "" > (frame ".list")
      for (i = 1; i <= NR; i++) {
        if (O[i] != block) { print L[i] > frame; was = ""; continue }
        if (type != "cm") { if (was != "rules") print "\001" > frame; was = "rules"; continue }
        if (E[i] == "key") { print L[i] > frame; was = ""; continue }
        if (E[i] == "") { print L[i] > frame; was = ""; continue }
        if (was != E[i]) { print "\001" > frame; printf "" > (frame "." E[i]); was = E[i] }
        print L[i] > (frame "." E[i])
      }
      for (i = 1; i <= ne; i++) print i "\t" style[i] > (frame ".list")
    }' "$1"
}

# is_json FILE NAME: whether FILE, named NAME, is read as JSON: NAME is
# *.json, or FILE is one JSON object (JSON is YAML too).
is_json() {
  case $(printf '%s' "$2" | tr '[:upper:]' '[:lower:]') in *.json) return 0 ;; esac
  case $(tr -d ' \t\r\n' <"$1" | head -c 1) in
    '{') jq -e -s 'length == 1 and (.[0] | type == "object")' "$1" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

# json_read FILE: what one JSON object is, as doc_read writes it, with no
# key twice anywhere in it (jq keeps the last, other readers the first).
json_read() {
  jq -n --stream -e '[inputs | if length == 2 then .[0] else .[0][:-1] end | select(length > 0)] |
    length == (unique | length)' "$1" >/dev/null 2>&1 || { echo bad; return; }
  jq -s -r 'def only($allowed): (keys - $allowed) == [];
    if length != 1 or (.[0] | type) != "object" then "bad" else .[0] |
    if (.kind | type) == "string" and (.kind | test("^(PrometheusRule|Rules|ClusterRules|GlobalRules)$"))
      and has("spec") and only(["apiVersion", "kind", "metadata", "spec"]) then "crd"
    elif .kind == "ConfigMap" and (.data | type) == "object" and only(["apiVersion", "kind", "metadata", "data", "immutable"]) then "cm"
    elif (has("kind") | not) and (.groups | type) == "array" and only(["groups", "namespace", "apiVersion", "deleteRules",
      "contactPoints", "deleteContactPoints", "policies", "resetPolicies", "templates", "deleteTemplates", "muteTimes",
      "deleteMuteTimes"]) then "plain"
    else "other" end end' "$1" 2>/dev/null || echo bad
}

# rule_file FILE NAME: whether a change may be written to FILE, named NAME:
# each of its documents read here as YAML reads it (none bad), and one at
# least alert rules. Its others (a ServiceMonitor, a Deployment) are left
# as they are: fits holds a change to that.
rule_file() {
  nobom "$1" "$tmp/rf"
  if is_json "$tmp/rf" "$2"; then
    case $(json_read "$tmp/rf") in plain | crd | cm) return 0 ;; *) return 1 ;; esac
  fi
  plain_text "$tmp/rf" || return 1
  rm -rf "$tmp/rf.d"
  rf_n=$(split_docs "$tmp/rf" "$tmp/rf.d")
  [ "$rf_n" -gt 0 ] || return 1
  rf_rules=
  rf_i=0
  while [ "$rf_i" -lt "$rf_n" ]; do
    case $(doc_read "$tmp/rf.d/$rf_i") in
      bad*) return 1 ;;
      plain* | crd* | cm*) rf_rules=1 ;;
    esac
    rf_i=$((rf_i + 1))
  done
  [ -n "$rf_rules" ]
}

# rules_only FILE: whether FILE, YAML, holds alert rules and nothing else:
# every document a rule file's, or empty. A ConfigMap's entry is held to it.
rules_only() {
  plain_text "$1" || return 1
  rm -rf "$1.d"
  ro_n=$(split_docs "$1" "$1.d")
  [ "$ro_n" -gt 0 ] || return 1
  ro_rules=
  ro_i=0
  while [ "$ro_i" -lt "$ro_n" ]; do
    case $(doc_read "$1.d/$ro_i") in
      empty) ;;
      plain*) ro_rules=1 ;;
      *) return 1 ;;
    esac
    ro_i=$((ro_i + 1))
  done
  [ -n "$ro_rules" ]
}

# entry FILE OUT: a ConfigMap's entry written as a literal block (|), its
# lines less their indentation, as the file a pod mounts holds it.
entry() {
  LC_ALL=C awk '
    { sub(/\r$/, ""); L[NR] = $0 }
    ind == "" && /[^ ]/ { match($0, /^ */); ind = RLENGTH }
    END { for (i = 1; i <= NR; i++) print substr(L[i], ind + 1) }' "$1" >"$2"
}

# fits OLD NEW NAME: whether NEW may be written over OLD, a file named NAME.
# Each document of NEW is OLD's, in the same order and as many: as it was,
# byte for byte, whatever it is; or, both alert rules of one kind, the
# same but for its rules (doc_read's frame): a rule file's groups, a
# PrometheusRule's spec, a ConfigMap's entries, each as it was or, written
# as a literal block in both, a file of alert rules alone in both. What a
# document is, its kind, apiVersion, name and namespace, its other keys
# and what they hold, a ConfigMap's entries and their names, never change.
fits() {
  nobom "$1" "$tmp/fit-old"
  nobom "$2" "$tmp/fit-new"
  if is_json "$tmp/fit-old" "$3" || is_json "$tmp/fit-new" "$3"; then
    json_fits "$tmp/fit-old" "$tmp/fit-new"
    return
  fi
  plain_text "$tmp/fit-new" || return 1
  rm -rf "$tmp/fit-o" "$tmp/fit-n"
  fi_n=$(split_docs "$tmp/fit-old" "$tmp/fit-o")
  [ "$fi_n" -gt 0 ] && [ "$fi_n" = "$(split_docs "$tmp/fit-new" "$tmp/fit-n")" ] || return 1
  fi_i=0
  while [ "$fi_i" -lt "$fi_n" ]; do
    fi_o=$tmp/fit-o/$fi_i
    fi_m=$tmp/fit-n/$fi_i
    fi_i=$((fi_i + 1))
    cmp -s "$fi_o" "$fi_m" && continue
    fi_a=$(doc_read "$fi_o" "$fi_o.frame")
    case $fi_a in plain* | crd* | cm*) ;; *) return 1 ;; esac
    [ "$fi_a" = "$(doc_read "$fi_m" "$fi_m.frame")" ] || return 1
    cmp -s "$fi_o.frame" "$fi_m.frame" || return 1
    cmp -s "$fi_o.frame.list" "$fi_m.frame.list" || return 1
    while IFS='	' read -r fi_e fi_how; do
      cmp -s "$fi_o.frame.$fi_e" "$fi_m.frame.$fi_e" && continue
      case $fi_how in '|' | '|-' | '|+') ;; *) return 1 ;; esac
      entry "$fi_o.frame.$fi_e" "$fi_o.e$fi_e"
      entry "$fi_m.frame.$fi_e" "$fi_m.e$fi_e"
      rules_only "$fi_o.e$fi_e" && rules_only "$fi_m.e$fi_e" || return 1
    done <"$fi_o.frame.list"
  done
}

# json_fits OLD NEW: fits, for a JSON object: alert rules of one kind in
# both, the same but for groups, spec, or data's entries, each a file of
# alert rules alone in both where it changed.
json_fits() {
  jf_a=$(json_read "$1")
  case $jf_a in plain | crd | cm) ;; *) return 1 ;; esac
  [ "$jf_a" = "$(json_read "$2")" ] || return 1
  case $jf_a in plain) jf_block=groups ;; crd) jf_block=spec ;; *) jf_block=data ;; esac
  [ "$(jq -cS --arg b "$jf_block" 'del(.[$b])' "$1")" = "$(jq -cS --arg b "$jf_block" 'del(.[$b])' "$2")" ] || return 1
  [ "$jf_a" = cm ] || return 0
  [ "$(jq -c '.data | keys_unsorted' "$1")" = "$(jq -c '.data | keys_unsorted' "$2")" ] || return 1
  jq -r '.data | keys_unsorted[]' "$1" >"$tmp/fit-keys"
  jf_k=0
  while IFS= read -r jf_e; do
    jf_k=$((jf_k + 1))
    [ "$(jq -c --arg e "$jf_e" '.data[$e]' "$1")" = "$(jq -c --arg e "$jf_e" '.data[$e]' "$2")" ] && continue
    jq -e -r --arg e "$jf_e" '.data[$e] | strings' "$1" >"$tmp/fit-je$jf_k-o" 2>/dev/null || return 1
    jq -e -r --arg e "$jf_e" '.data[$e] | strings' "$2" >"$tmp/fit-je$jf_k-n" 2>/dev/null || return 1
    rules_only "$tmp/fit-je$jf_k-o" && rules_only "$tmp/fit-je$jf_k-n" || return 1
  done <"$tmp/fit-keys"
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
    # file of alert rules may be written: one whose hash is the document's
    # but isn't (a script, a CI system's file, one that runs) is never
    # taken, and in it only the rules may change.
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
      tell "$id" '{"state":"failed","reason":"the file in the repository that is the document the rule was read from is not one a change is written to: a file of alert rules (a Prometheus rule file, a Grafana provisioning file, a PrometheusRule or a ConfigMap of rules, beside other documents or not), each document in it read one way only, named *.yml, *.yaml, *.json or *.rules, not executable, and not what a CI system runs. Make the change by hand"}'
      continue
    fi
    if [ -z "$found" ]; then
      tell "$id" '{"state":"failed","reason":"no file in the repository under RULES_PATHS is the document the rule was read from (rules rendered from a chart, or read back from a cluster): make the change where it is written; its diff is on the proposal'"'"'s page"}'
      continue
    fi
    jq -j '.document' "$tmp/one" >"$tmp/document"
    if cmp -s "$tmp/document" "$work/$found"; then
      tell "$id" '{"state":"failed","reason":"the change Lapilli wrote changes nothing in the file: nothing was pushed"}'
      continue
    fi
    if ! fits "$work/$found" "$tmp/document" "$found"; then
      tell "$id" '{"state":"failed","reason":"the change Lapilli wrote changes more than the alert rules in the file: only a rule file'"'"'s groups, a PrometheusRule'"'"'s spec or a rule file in a ConfigMap'"'"'s data may change, every other line and document as it was. Nothing was pushed"}'
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
