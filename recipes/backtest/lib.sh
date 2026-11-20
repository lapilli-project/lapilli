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
#   LAPILLI_VERSION  the Lapilli release to install, such as v0.33.0
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
#   IMPACT           optional: a PromQL expression that returns something
#                    while users are hurt, such as a 5xx ratio above 1%
#                    (--impact): its windows are incidents too, which the
#                    rules that differ are judged by. Given to lapilli as
#                    one argument, as it is: never split at spaces, nor
#                    expanded
#   IMPACT_FOR       optional: how long impact must last in a series to be
#                    an incident (--impact-for; lapilli's default 5m), a
#                    second or more, as lapilli reads it: 10m, 1.5h, 1d12h
#   MATCH            optional: the labels a firing and an incident must
#                    agree on when both have them, comma-separated, such
#                    as service or service,cluster: a --match each, beside
#                    any among the arguments
#   EXPECT           optional: an expectations file in the repository
#                    (docs/design/expectations.md): the rules that differ
#                    are held to it, the pull request's file for the rules
#                    it proposes and the commit it goes into's for those in
#                    place (--expect, --expect-before), and the comment
#                    says what the change broke
#   FAIL_ON_EXPECTATIONS
#                    true: the job fails when the change breaks an
#                    expectation the rules in place meet, once the comment
#                    says which (default false). With EXPECT_REQUIRED=true,
#                    also when it adds a rule no expectation holds
#   TARGET           the commit the pull request goes into, as the clone has
#                    it (default HEAD^1: the merge commit's first parent, as
#                    GitHub checks a pull request out); the rules compared
#                    against are those of the commit HEAD and TARGET share
#                    last (git merge-base), as are the expectations of the
#                    rules in place; INCIDENTS is TARGET's own
#   TARGET_BRANCH    instead, a branch: origin/BRANCH as the checkout
#                    fetched it, or else fetched from origin, for a CI that
#                    clones the pull request's branch alone
#   REPO             the clone, or a directory of it (default: the
#                    directory this runs in)
#   KEY              tells apart the comments of several jobs on one pull
#                    request: letters, digits, dots, dashes and underscores
#                    (default: default)
#   FAIL_ON_ERROR    true: the job fails when the backtest does, or a rule
#                    that differs couldn't be replayed, for whatever reason
#                    (the summary's unreplayed: the history out of reach, or
#                    a query it refused as written), or the comment can't be
#                    posted, once the comment says so if it can (default
#                    false). lapilli's --fail-unreplayed, which leaves out
#                    the queries refused as written, isn't taken
#   MAX_TREE_FILES, MAX_TREE_BYTES
#                    the most files (20000) and bytes (256 MiB) a commit is
#                    read with: with RENDER all of its files, without, those
#                    PATHS names
#
# Or through lapilli serve (docs/design/backtests.md), which backtests the
# rules with its own access to the history, judged by its record: nothing
# is installed or run here, and the job holds no credential for the
# metrics.
#
#   LAPILLI_SERVE    serve's backtest port (--backtest-listen), such as
#                    https://lapilli-ci.example.com: https, or plain http,
#                    through no proxy, to this machine (127.0.0.1, [::1],
#                    localhost) or to a Service in the cluster,
#                    NAME.NAMESPACE.svc or NAME.NAMESPACE.svc.cluster.local.
#                    The rules of both
#                    commits and the expectations are sent there (POST
#                    /v1/backtests), and its report read back every
#                    SERVE_POLL seconds; the comment, and the job's exit, are
#                    as they are without it. LAPILLI_VERSION, LAPILLI_SHA256,
#                    LAPILLI and LAPILLI_RELEASES are passed over. Serve
#                    decides what judges the firings and where the history
#                    is: INCIDENTS, IMPACT, IMPACT_FOR and MATCH are refused,
#                    and of the arguments only --since (whole days, 1d to
#                    14d), --tz and --rules-select are taken
#   LOKI_PATHS       optional, with PATHS: where a Loki ruler's rule files
#                    are, directories or files in the repository as PATHS
#                    names them, none of PATHS: sent as a Loki ruler's, as
#                    --loki-rules reads them, their queries LogQL. Serve
#                    backtests them where its own rules hold a Loki
#                    ruler's, against its --loki
#   LAPILLI_ID_TOKEN the CI's ID token for the audience serve's trust file
#                    names (GitLab's id_tokens:, Bitbucket's
#                    BITBUCKET_STEP_OIDC_TOKEN from a step whose oidc:
#                    asks for serve's audience alone: one whose aud names
#                    Bitbucket's workspace audience alone isn't sent); or
#   LAPILLI_SERVE_TOKEN_FILE
#                    a file holding the plain token serve's trust file
#                    names, for a CI with no ID token; or
#   LAPILLI_SERVE_AUDIENCE
#                    the audience to ask the CI for an ID token for, where
#                    the host's script asks (github.sh, and gitea.sh in
#                    Forgejo Actions: ACTIONS_ID_TOKEN_REQUEST_URL), as the
#                    trust file names it: the port's URL, LAPILLI_SERVE's
#                    (another is refused, unless SERVE_AUDIENCE_ANY=1),
#                    never the CI's default nor a cloud's, which are
#                    refused whatever SERVE_AUDIENCE_ANY says
#   SERVE_AUDIENCE_ANY
#                    1: LAPILLI_SERVE_AUDIENCE may be other than
#                    LAPILLI_SERVE's URL, for a trust file whose audience
#                    is another, the job's log saying so
#                    (sts.amazonaws.com, api://AzureADTokenExchange,
#                    //iam.googleapis.com/..., https://github.com/...):
#                    other services take a token for those. In GitHub
#                    Actions, github.sh masks the token (::add-mask::) as
#                    soon as it has it
#   SERVE_WAIT       how long to wait for serve's report (default 30m), a
#                    second to a day, as IMPACT_FOR is read; past it, the
#                    backtest is cancelled (DELETE) and said in the comment
#   SERVE_POLL       how many seconds apart serve is asked (default 10, 0
#                    to 60)
#   SERVE_RETRY_MIN  for tests: the least serve's Retry-After is waited, in
#                    seconds (default 5, 0 to 5), or SERVE_POLL if longer;
#                    a second at least, whatever both say
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
# out before they are cut; of what lapilli said, nothing about why a rule
# couldn't be replayed, nor what a server or the network answered, nor how
# the run went (its progress lines), which the job's log alone has. What is
# copied to the job's log, lapilli's stderr as it comes, is each line after
# a "| ", and Azure's ##vso[ and GitHub's older ##[ made inert, so none of
# it is a CI's command. A
# commit
# holding a symbolic link where the rules are read, or anywhere with RENDER,
# isn't read: a link could lead to any file the job can read. The host's
# token is read from a file and given to curl on stdin, never on a command
# line; so is serve's, and the key its backtest is read by. What serve
# answers is read with jq alone, and what it says copied to the job's log
# as lapilli's stderr is, and to the comment only in the fenced block or as
# its report: its summary line as serve wrote it, or no report, and of the
# rest only lines lapilli writes, any other left out (report_of_serve).
# RENDER and RENDER_PREPARE run what the pull request holds, with the
# job's rights: they run without the variables CIs keep tokens in, and with a
# HOME of their own for each commit, the base first, but can read whatever
# the job can, the token's file included, and on Linux the environment the
# job's processes started with (/proc/PID/environ). Run this only for pull requests
# from people trusted with what the job can reach (the README says how,
# host by host).

set -eu
# inert: text as a CI's log can take it: no control character but the tab
# and the line break, and Azure Pipelines' ##vso[ and GitHub's older ##[
# (##[error], ##[add-matcher]), which each reads anywhere in a line, made
# inert.
inert() { tr -d '\000-\010\013-\037\177' | sed 's/##\([vV][sS][oO]\)\[/##\1(/g; s/##\[/##(/g'; }
say() { printf '%s: %s\n' "${recipe:-lib.sh}" "$(printf '%s' "$*" | tr '\n\t' '  ' | inert)" >&2; }
# logged FILE: FILE in the job's log, each line after a "| ": GitHub's and
# Gitea's ::commands are read only at a line's start.
logged() { inert <"$1" | sed 's/^/  | /' >&2; }
# live FILE: stdin into FILE, and each line in the job's log as it comes,
# as logged writes it: a long backtest says how it goes every 30 seconds,
# and a job that says nothing for minutes looks hung.
live() {
  : >"$1"
  while IFS= read -r line || [ -n "$line" ]; do
    printf '%s\n' "$line" >>"$1"
    printf '%s\n' "$line" | inert | sed 's/^/  | /' >&2
  done
}
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
# serve_url URL: whether lapilli serve's backtest port may be at URL, which
# the CI's token goes to: https, or plain http to this machine (127.0.0.1,
# [::1], localhost) or to a Service in a cluster, NAME.NAMESPACE.svc or
# NAME.NAMESPACE.svc.cluster.local, each a DNS label with a letter first;
# no user in its authority, no query. Wider than safe_url, which a host's
# API and a release go to: a runner in the cluster reaches the chart's
# Service, FULLNAME-backtest, in plain http. Narrower than serve's own
# signin.SafeURL: a name of one label is looked up under the resolver's
# search domains, which needn't be the cluster's, and NAME.NAMESPACE may be
# a name on the internet (a namespace may be a top-level domain's name).
# Plain http goes through no proxy (serve_curl).
serve_url() {
  case $1 in
    https://*) rest=${1#https://} ;;
    http://*) rest=${1#http://} ;;
    *) return 1 ;;
  esac
  auth=${rest%%/*}
  case $auth in
    *@* | '' | *[!]A-Za-z0-9.:_[-]*) return 1 ;;
  esac
  case $rest in *\?* | *\#*) return 1 ;; esac
  case $1 in https://*) return 0 ;; esac
  case $auth in
    '[::1]') name='[::1]' port= ;;
    '[::1]':*) name='[::1]' port=${auth#'[::1]:'} ;;
    \[*) return 1 ;;
    *:*) name=${auth%%:*} port=${auth#*:} ;;
    *) name=$auth port= ;;
  esac
  case $port in *[!0-9]* | ??????*) return 1 ;; esac
  name=$(printf '%s' "$name" | tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz')
  case $name in
    127.0.0.1 | localhost | '[::1]') return 0 ;;
  esac
  label='[a-z]([a-z0-9-]{0,61}[a-z0-9])?'
  printf '%s\n' "$name" | grep -Eqx "$label\\.$label\\.svc(\\.cluster\\.local)?"
}
# token_ok TOKEN: whether TOKEN can go in a header as it is, as lapilli
# serve takes a bearer token: visible characters, none a space, 8 KiB at
# most. Read through printf, a builtin, never on a command line.
token_ok() {
  [ -n "$1" ] && [ "${#1}" -le 8192 ] && [ -z "$(printf '%s' "$1" | tr -d '\041-\176')" ]
}

# id_token_audience reads an ID token on stdin, its payload decoded and
# not verified, and writes bitbucket-default when its aud names Bitbucket's
# workspace audiences alone (ari:cloud:bitbucket:...), ok when it names
# another, unknown when it can't be read: serve decides the rest.
id_token_audience() {
  jq -Rr 'def b64url: gsub("-"; "+") | gsub("_"; "/") | . + ("=" * ((4 - (length % 4)) % 4));
    (split(".") | if length == 3 then .[1] else "" end) as $p
    | (try ($p | b64url | @base64d | fromjson) catch null) as $c
    | if ($c | type) != "object" then "unknown"
      else ($c.aud | if type == "string" then [.] elif type == "array" then map(strings) else [] end) as $aud
        | if ($aud | length) > 0 and ($aud | all(ascii_downcase | startswith("ari:cloud:bitbucket:"))) then "bitbucket-default" else "ok" end
      end' 2>/dev/null || echo unknown
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
case ${FAIL_ON_EXPECTATIONS:-false} in
  true) held=1 ;;
  false | '') held= ;;
  *) say "FAIL_ON_EXPECTATIONS $FAIL_ON_EXPECTATIONS: want true or false"; exit 1 ;;
esac
case ${EXPECT_REQUIRED:-false} in
  true) required=1 ;;
  false | '') required= ;;
  *) say "EXPECT_REQUIRED $EXPECT_REQUIRED: want true or false"; exit 1 ;;
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
for p in ${LOKI_PATHS:-}; do
  in_repo "$p" || { say "LOKI_PATHS: $p isn't a path in the repository"; exit 1; }
  for q in ${PATHS:-}; do
    if [ "${p%/}" = "${q%/}" ]; then
      say "LOKI_PATHS: $p is in PATHS too: a Loki ruler's files are named in LOKI_PATHS alone"
      exit 1
    fi
  done
done
set +f
if [ -n "$(printf '%s' "${LOKI_PATHS:-}" | tr -d ' ')" ] && [ -n "${RENDER:-}" ]; then
  say "LOKI_PATHS goes with PATHS: RENDER writes one file of rules, read as --rules reads it"
  exit 1
fi
# Through lapilli serve, serve judges the firings: by the incidents and the
# impact windows it keeps, and its own settings, which a pull request
# doesn't choose. What only serve mode reads is refused without it.
serve=
if [ -n "${LAPILLI_SERVE:-}" ]; then
  for v in "INCIDENTS ${INCIDENTS:-}" "IMPACT ${IMPACT:-}" "IMPACT_FOR ${IMPACT_FOR:-}" "MATCH ${MATCH:-}"; do
    if [ -n "${v#* }" ]; then
      say "${v%% *}: lapilli serve judges the firings by the incidents and impact windows it keeps, by its own settings: leave it out with LAPILLI_SERVE"
      exit 1
    fi
  done
else
  for v in "SERVE_WAIT ${SERVE_WAIT:-}" "SERVE_POLL ${SERVE_POLL:-}" "SERVE_RETRY_MIN ${SERVE_RETRY_MIN:-}" "LAPILLI_SERVE_AUDIENCE ${LAPILLI_SERVE_AUDIENCE:-}" \
    "SERVE_AUDIENCE_ANY ${SERVE_AUDIENCE_ANY:-}" "LOKI_PATHS $(printf '%s' "${LOKI_PATHS:-}" | tr -d ' ')" \
    "LAPILLI_SERVE_TOKEN_FILE ${LAPILLI_SERVE_TOKEN_FILE:-}" "LAPILLI_ID_TOKEN ${LAPILLI_ID_TOKEN:+x}"; do
    if [ -n "${v#* }" ]; then
      say "${v%% *} is for a backtest through lapilli serve: give LAPILLI_SERVE, its backtest port, too"
      exit 1
    fi
  done
fi
# A file, not a directory: a path ending in / is refused, as git would
# take it for the one file a directory holds.
if [ -n "${INCIDENTS:-}" ] && { ! in_repo "$INCIDENTS" || [ "${INCIDENTS%/}" != "$INCIDENTS" ]; }; then
  say "INCIDENTS $INCIDENTS: want a file in the repository"
  exit 1
fi
if [ -n "${IMPACT:-}" ] && [ -z "$(printf '%s' "$IMPACT" | tr -d ' \t\n')" ]; then
  say "IMPACT: want a PromQL expression that returns something while users are hurt"
  exit 1
fi
# duration DUR: whether lapilli's --impact-for takes DUR, a second or more,
# as it reads one: whole days first, if any (1d, 1d12h), then numbers each
# with its unit, h, m or s, a fraction allowed (90s, 1.5h, 1h30m). Not a
# bare number, nor ms, nor a sign.
duration() { seconds "$1" >/dev/null; }
# seconds DUR: DUR, as duration takes it, in whole seconds.
seconds() {
  case $1 in '' | *[!0-9dhms.]*) return 1 ;; esac
  printf '%s\n' "$1" | awk '{
    s = $0; total = 0
    if (match(s, /^(0|[1-9][0-9]*)d/)) { total = substr(s, 1, RLENGTH - 1) * 86400; s = substr(s, RLENGTH + 1) }
    while (s != "") {
      if (!match(s, /^([0-9]+(\.[0-9]*)?|\.[0-9]+)[hms]/)) exit 1
      u = substr(s, RLENGTH, 1)
      total += substr(s, 1, RLENGTH - 1) * (u == "h" ? 3600 : u == "m" ? 60 : 1)
      s = substr(s, RLENGTH + 1)
    }
    if (total < 1) exit 1
    printf "%d\n", (total > 999999999 ? 999999999 : total)
  }'
}
if [ -n "${IMPACT_FOR:-}" ] && ! duration "$IMPACT_FOR"; then
  say "IMPACT_FOR $IMPACT_FOR: want a duration of a second or more, such as 10m, 1.5h or 1d"
  exit 1
fi
if [ -n "${IMPACT_FOR:-}" ] && [ -z "${IMPACT:-}" ]; then
  say "IMPACT_FOR is how long IMPACT must last: give IMPACT too"
  exit 1
fi
# MATCH's labels, a comma apart; spaces beside a comma passed over.
match=$(printf '%s' "${MATCH:-}" | sed 's/^ *//; s/ *$//; s/ *, */,/g')
case $match in
  '') ;;
  ,* | *, | *,,* | *[!A-Za-z0-9_.,]*) say "MATCH $MATCH: want label names, comma-separated, such as service or service,cluster"; exit 1 ;;
esac
if [ -n "${EXPECT:-}" ] && { ! in_repo "$EXPECT" || [ "${EXPECT%/}" != "$EXPECT" ]; }; then
  say "EXPECT $EXPECT: want a file in the repository"
  exit 1
fi
if [ -z "${EXPECT:-}" ] && { [ -n "$held" ] || [ -n "$required" ]; }; then
  say "FAIL_ON_EXPECTATIONS and EXPECT_REQUIRED hold the rules to EXPECT: give EXPECT, the expectations file"
  exit 1
fi
if [ -n "${LAPILLI_SERVE:-}" ]; then
  serve=${LAPILLI_SERVE%/}
  if ! serve_url "$serve"; then
    say "LAPILLI_SERVE $LAPILLI_SERVE: want lapilli serve's backtest port, an https URL, or plain http to this machine (127.0.0.1, [::1], localhost) or to a Service in the cluster, NAME.NAMESPACE.svc or NAME.NAMESPACE.svc.cluster.local"
    exit 1
  fi
  case $serve in https://*) serve_proto='=https' ;; *) serve_proto='=http' ;; esac
  # Nothing is installed or run here.
  passed=
  for v in "LAPILLI_VERSION ${LAPILLI_VERSION:-}" "LAPILLI_SHA256 ${LAPILLI_SHA256:-}" "LAPILLI ${LAPILLI:-}" "LAPILLI_RELEASES ${LAPILLI_RELEASES:-}"; do
    [ -z "${v#* }" ] || passed="$passed ${v%% *}"
  done
  [ -z "$passed" ] || say "lapilli serve backtests the rules, and nothing is installed or run here:$passed passed over"
  serve_wait_given=${SERVE_WAIT:-30m}
  if ! serve_wait=$(seconds "$serve_wait_given") || [ "$serve_wait" -gt 86400 ]; then
    say "SERVE_WAIT $serve_wait_given: want how long to wait for lapilli serve, a second to a day, such as 30m or 1h"
    exit 1
  fi
  serve_poll=${SERVE_POLL:-10}
  case $serve_poll in
    [0-9] | [1-5][0-9] | 60) ;;
    *) say "SERVE_POLL $serve_poll: want how many seconds apart to ask lapilli serve, 0 to 60"; exit 1 ;;
  esac
  # The least serve's Retry-After is waited: a Retry-After of 0, or none,
  # isn't a loop of requests. Lowered only for tests, and never below a
  # second, whatever SERVE_RETRY_MIN and SERVE_POLL say.
  retry_min=${SERVE_RETRY_MIN:-5}
  case $retry_min in
    [0-5]) ;;
    *) say "SERVE_RETRY_MIN $retry_min: want the least seconds lapilli serve's Retry-After is waited, 0 to 5 (for tests; 5 unless set)"; exit 1 ;;
  esac
  [ "$retry_min" -ge "$serve_poll" ] || retry_min=$serve_poll
  [ "$retry_min" -ge 1 ] || retry_min=1
  case $(date +%s 2>/dev/null) in
    '' | *[!0-9]*) say "date +%s doesn't say the time in seconds here: SERVE_WAIT can't be kept"; exit 1 ;;
  esac
  # The token: one of three, kept in a variable of the recipe's own,
  # never in the environment of what it runs.
  token_from=
  serve_token=
  if [ -n "${LAPILLI_ID_TOKEN:-}" ]; then
    serve_token=$LAPILLI_ID_TOKEN
    token_from=LAPILLI_ID_TOKEN
    if ! token_ok "$serve_token"; then
      say "LAPILLI_ID_TOKEN: want an ID token, visible characters and no space, 8 KiB at most"
      exit 1
    fi
    # A Bitbucket step's token that names its workspace's audience alone
    # (a step whose oidc: names none of serve's) is refused by serve, and
    # taken by whatever trusts the workspace's audience: it isn't sent.
    # Its payload is read, not verified, for its aud alone; on stdin.
    if [ "$(printf '%s' "$serve_token" | id_token_audience)" = bitbucket-default ]; then
      say "LAPILLI_ID_TOKEN names Bitbucket's workspace audience alone (ari:cloud:bitbucket:...), which lapilli serve refuses and other services that trust the workspace take: give the step oidc: audiences: with serve's audience, its port's URL"
      exit 1
    fi
  fi
  # Bitbucket's step token, which the job gives as LAPILLI_ID_TOKEN, is
  # in the step's environment too: nothing the recipe runs here needs it.
  unset LAPILLI_ID_TOKEN BITBUCKET_STEP_OIDC_TOKEN
  one="give lapilli serve one token: LAPILLI_ID_TOKEN, LAPILLI_SERVE_TOKEN_FILE or LAPILLI_SERVE_AUDIENCE"
  if [ -n "${LAPILLI_SERVE_TOKEN_FILE:-}" ]; then
    [ -z "$token_from" ] || { say "$one"; exit 1; }
    if [ ! -f "$LAPILLI_SERVE_TOKEN_FILE" ] || [ ! -r "$LAPILLI_SERVE_TOKEN_FILE" ]; then
      say "LAPILLI_SERVE_TOKEN_FILE $LAPILLI_SERVE_TOKEN_FILE can't be read"
      exit 1
    fi
    token_from=LAPILLI_SERVE_TOKEN_FILE
  fi
  if [ -n "${LAPILLI_SERVE_AUDIENCE:-}" ]; then
    [ -z "$token_from" ] || { say "$one"; exit 1; }
    if [ "${ci_tokens:-}" != actions ]; then
      say "LAPILLI_SERVE_AUDIENCE: ${recipe:-lib.sh} asks its CI for no ID token: give LAPILLI_ID_TOKEN, the one the CI gives the job for serve's audience (GitLab's id_tokens:, Bitbucket's BITBUCKET_STEP_OIDC_TOKEN), or LAPILLI_SERVE_TOKEN_FILE"
      exit 1
    fi
    if [ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] || [ -z "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]; then
      say "LAPILLI_SERVE_AUDIENCE: the job can't ask for an ID token (no ACTIONS_ID_TOKEN_REQUEST_URL): give it permissions: id-token: write (GitHub Actions), or enable-openid-connect: true (Forgejo Actions)"
      exit 1
    fi
    if ! safe_url "$ACTIONS_ID_TOKEN_REQUEST_URL"; then
      say "ACTIONS_ID_TOKEN_REQUEST_URL: want an https URL"
      exit 1
    fi
    if [ "${#LAPILLI_SERVE_AUDIENCE}" -gt 1024 ] || [ -n "$(printf '%s' "$LAPILLI_SERVE_AUDIENCE" | tr -d '\041-\176')" ]; then
      say "LAPILLI_SERVE_AUDIENCE: want the audience serve's trust file names, such as https://lapilli-ci.example.com"
      exit 1
    fi
    # A cloud's audience, or the CI's own default, is one other services
    # take: serve, and whoever is on the way to it, could take a token for
    # it there (AWS's STS, Azure's and Google Cloud's workload identity
    # federation, Sigstore, GitHub's default, the owner's URL).
    aud=$(printf '%s' "$LAPILLI_SERVE_AUDIENCE" | tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz')
    aud_host=${aud#https://}
    aud_host=${aud_host#http://}
    aud_host=${aud_host%%/*}
    aud_host=${aud_host%%:*}
    case $aud_host in amazonaws.com | *.amazonaws.com) aud=refused ;; esac
    case $aud in
      refused | api://azureadtokenexchange* | //iam.googleapis.com | //iam.googleapis.com/* | https://iam.googleapis.com | \
        https://iam.googleapis.com/* | https://sts.googleapis.com* | sigstore | https://github.com | https://github.com/* | https://github.com:*)
        say "LAPILLI_SERVE_AUDIENCE $LAPILLI_SERVE_AUDIENCE: a cloud's audience, or the CI's own default, which other services take a token for: give serve's own, the one its trust file names, its port's URL"
        exit 1
        ;;
    esac
    # The port's own URL alone, unless SERVE_AUDIENCE_ANY=1 says the trust
    # file names another: a list of what others take is never whole (a
    # registry's, an enterprise's, another cloud's region).
    case ${SERVE_AUDIENCE_ANY:-} in
      '' | 1) ;;
      *) say "SERVE_AUDIENCE_ANY ${SERVE_AUDIENCE_ANY}: want 1, to ask for an audience other than LAPILLI_SERVE's URL"; exit 1 ;;
    esac
    if [ "${LAPILLI_SERVE_AUDIENCE%/}" != "$serve" ]; then
      if [ "${SERVE_AUDIENCE_ANY:-}" != 1 ]; then
        say "LAPILLI_SERVE_AUDIENCE $LAPILLI_SERVE_AUDIENCE isn't LAPILLI_SERVE, $serve, where the token goes: give the port's URL, an audience no other service takes, or SERVE_AUDIENCE_ANY=1 for a trust file that names another"
        exit 1
      fi
      say "LAPILLI_SERVE_AUDIENCE $LAPILLI_SERVE_AUDIENCE isn't LAPILLI_SERVE, $serve, where the token goes (SERVE_AUDIENCE_ANY=1)"
    fi
    token_from=actions
  fi
  if [ -z "$token_from" ]; then
    say "LAPILLI_SERVE: give the token lapilli serve takes: LAPILLI_ID_TOKEN, the CI's ID token for serve's audience; LAPILLI_SERVE_TOKEN_FILE, a file holding a plain token; or, with github.sh or gitea.sh, LAPILLI_SERVE_AUDIENCE, to ask the CI for one"
    exit 1
  fi
elif [ -z "${LAPILLI:-}" ]; then
  case ${LAPILLI_VERSION:-} in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) say "LAPILLI_VERSION ${LAPILLI_VERSION:-}: want the Lapilli release to install, such as v0.33.0"; exit 1 ;;
  esac
  case $LAPILLI_VERSION in
    *[!a-z0-9.-]*) say "LAPILLI_VERSION $LAPILLI_VERSION: want a release's tag, such as v0.33.0"; exit 1 ;;
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
for tool in git curl jq tar; do
  command -v "$tool" >/dev/null 2>&1 || { say "$tool isn't installed: the recipe needs sh, git, curl, jq and tar"; exit 1; }
done
if [ -n "$serve" ] && ! command -v cmp >/dev/null 2>&1; then
  say "cmp isn't installed: through lapilli serve, the recipe needs it too (diffutils, or busybox's)"
  exit 1
fi
# The clone, all of it, whichever of its directories REPO is, and where
# this runs: a file an argument names in the clone is the pull request's,
# which is judged by it.
[ -d "$repo" ] || { say "REPO $repo isn't a directory"; exit 1; }
if top=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) && [ -n "$top" ]; then
  clone=$(cd "$top" && pwd -P)
else
  clone=$(cd "$repo" && pwd -P)
fi
here_now=$(pwd -P)
# in_clone PATH: whether PATH, as lapilli opens it from here, is in the
# clone.
in_clone() {
  case $1 in /*) f=$1 ;; *) f=$here_now/$1 ;; esac
  d=$(cd "$(dirname -- "$f")" 2>/dev/null && pwd -P) || return 1
  case $d/ in "$clone"/*) return 0 ;; esac
  return 1
}
# serve_since DUR: --since, through lapilli serve: whole days, 1 to 14,
# ending at what serve has settled, into serve_days.
serve_since() {
  case $1 in
    [1-9]d | 1[0-4]d) serve_days=${1%d} ;;
    *) say "--since $1: through lapilli serve, whole days, 1d to 14d, ending where serve has settled"; exit 1 ;;
  esac
}
serve_days='' serve_tz='' serve_select=''
# Each argument in turn, to the end and around: the same, in order, after.
# incidents_given: an argument gives incidents, or impact. Through lapilli
# serve, want is the flag the next argument is the value of, and taken
# says the argument is one serve is sent.
incidents_given=
want=
n=$#
while [ "$n" -gt 0 ]; do
  a=$1
  shift
  n=$((n - 1))
  if [ -n "$want" ]; then
    case $want in
      --since) serve_since "$a" ;;
      --tz) serve_tz=$a ;;
      --rules-select) serve_select=$a ;;
    esac
    want=
    set -- "$@" "$a"
    continue
  fi
  taken=
  if [ -n "$serve" ]; then
    case $a in
      --since | --tz | --rules-select) want=$a taken=1 ;;
      --since=*) serve_since "${a#*=}"; taken=1 ;;
      --tz=*) serve_tz=${a#*=} taken=1 ;;
      --rules-select=*) serve_select=${a#*=} taken=1 ;;
      --prometheus | --prometheus=* | --gcp-project | --gcp-project=* | --auth | --auth=* | --aws-region | --aws-region=* | \
        --bearer-token-file | --bearer-token-file=* | --step | --step=* | --late | --late=* | --scrape-interval | --scrape-interval=*)
        say "${a%%=*}: lapilli serve decides where the history is, and how it is asked: leave it out with LAPILLI_SERVE"
        exit 1
        ;;
      --lead | --lead=* | --match | --match=* | --refire | --refire=* | --sleep-hours | --sleep-hours=* | --work-hours | --work-hours=* | \
        --shifts | --shifts=* | --on-call | --on-call=* | --noisy-* | --impact | --impact=* | --impact-* | --incidents | --incidents=*)
        say "${a%%=*}: lapilli serve judges the firings, by its record and its own settings, which a pull request doesn't choose: leave it out with LAPILLI_SERVE"
        exit 1
        ;;
      --from | --from=* | --to | --to=*)
        say "${a%%=*}: lapilli serve replays only what has settled: give --since, whole days, 1d to 14d"
        exit 1
        ;;
      --loki | --loki=* | --loki-* | --grafana | --grafana=* | --grafana-* | --ruler | --ruler=* | --ruler-* | --alarms | --alarms=*)
        say "${a%%=*}: lapilli serve backtests the rules read from PATHS, LOKI_PATHS or RENDER against its own history, its Loki and its Grafana's data sources: leave it out with LAPILLI_SERVE"
        exit 1
        ;;
    esac
  fi
  case $a in
    --incidents | --incidents=* | --impact | --impact=*) incidents_given=1 ;;
  esac
  case $a in
    --impact | --impact=*)
      if [ -n "${IMPACT:-}" ]; then
        say "$a: IMPACT says already when users were hurt: give one of them"
        exit 1
      fi
      ;;
    --impact-for | --impact-for=*)
      if [ -n "${IMPACT_FOR:-}" ]; then
        say "$a: IMPACT_FOR says already how long impact must last: give one of them"
        exit 1
      fi
      ;;
  esac
  case $a in
    --rules | --rules=* | --compare | --compare=* | --rules-history | --rules-history=* | --changed-only | --markdown | \
      --json | --metrics | --check-replay* | --check-content*)
      say "$a: the recipe gives lapilli the rules and asks for the report itself; give it the rest, such as --prometheus URL and --since 14d"
      exit 1
      ;;
    --fail-unreplayed | --fail-unreplayed=*)
      say "$a: the recipe fails the job itself, with FAIL_ON_ERROR=true, when a rule that differs couldn't be replayed"
      exit 1
      ;;
    --expect | --expect=* | --expect-before | --expect-before=* | --fail-expectations* | --expect-required*)
      say "$a: the recipe reads the expectations from both commits itself: give EXPECT, and FAIL_ON_EXPECTATIONS or EXPECT_REQUIRED"
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
  if [ -n "$serve" ] && [ -z "$taken" ]; then
    say "${a%%=*}: through lapilli serve, the recipe sends it --since, --tz and --rules-select alone, and serve decides the rest: leave it out with LAPILLI_SERVE"
    exit 1
  fi
  set -- "$@" "$a"
done
if [ -n "$want" ]; then
  say "$want: want a value"
  exit 1
fi
if [ -n "$match" ] && [ -z "${INCIDENTS:-}${IMPACT:-}$incidents_given" ]; then
  say "MATCH is what a firing and an incident must agree on: give IMPACT or INCIDENTS too"
  exit 1
fi

tmp=$(mktemp -d)
# A backtest serve took and hasn't ended is cancelled when the job ends
# first (bt_live), so that serve doesn't run what no one waits for.
bt_live=
trap '[ -z "$bt_live" ] || serve_cancel || :; rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
out=$tmp/out
mkdir "$tmp/empty"
: >"$tmp/problem.log"

# capped MOST FILE: stdin into FILE, MOST bytes of it at most; it fails
# when there was more. Before curl 8.4.0 (Ubuntu 22.04 has 7.81),
# --max-filesize caps only an answer that says its length first: what curl
# writes is capped so, whatever its version. A command whose stdin it is
# writes $tmp/failed when it fails: { CMD || : >"$tmp/failed"; } | capped.
capped() { head -c "$(($1 + 1))" >"$2" && [ "$(wc -c <"$2")" -le "$1" ]; }

# host METHOD URL [FILE]: asks the host's API, the answer in $out, 16 MiB
# at most; writes the HTTP status, or "failed". curl reads no .curlrc (-q),
# follows no redirect, and speaks only HTTP and HTTPS.
host() {
  rm -f "$tmp/failed"
  if { ask_host "$@" || : >"$tmp/failed"; } | capped 16777216 "$out" && [ ! -e "$tmp/failed" ]; then
    code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$tmp/headers" | tail -n 1)
    echo "${code:-failed}"
  else
    echo failed
  fi
}

# ask_host METHOD URL [FILE]: the answer on stdout, its headers in
# $tmp/headers.
ask_host() {
  : >"$tmp/headers"
  if [ $# -ge 3 ]; then
    host_auth | curl -q -sS --max-time 30 --max-filesize 16777216 --proto '=http,https' -X "$1" -H @- -H 'Content-Type: application/json' \
      --data-binary "@$3" -D "$tmp/headers" -o - "$2"
  else
    host_auth | curl -q -sS --max-time 30 --max-filesize 16777216 --proto '=http,https' -X "$1" -H @- -D "$tmp/headers" -o - "$2"
  fi
}

# unmet: what the change does to the expectations that fails the job
# (FAIL_ON_EXPECTATIONS, EXPECT_REQUIRED), said once the comment is posted.
unmet=
judged=
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
  # Through lapilli serve, nothing is run here.
  [ -z "$serve" ] || return 0
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
    rm -f "$tmp/failed"
    if ! { curl -q -fsSL --max-time 300 --max-filesize "${f#* }" --proto '=http,https' --proto-redir '=https' -o - \
      "${from%/}/$LAPILLI_VERSION/${f% *}" 2>"$tmp/curl.log" || : >"$tmp/failed"; } | capped "${f#* }" "$tmp/release/${f% *}" ||
      [ -e "$tmp/failed" ]; then
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
    # As the checkout fetched it, when it did (a full clone fetches every
    # branch): a fetch needs the credentials a checkout may have dropped.
    if git -C "$repo" rev-parse --verify -q "refs/remotes/origin/$TARGET_BRANCH^{commit}" >/dev/null; then
      TARGET=refs/remotes/origin/$TARGET_BRANCH
    elif git -C "$repo" fetch --quiet --no-tags origin "refs/heads/$TARGET_BRANCH" 2>"$tmp/git.log"; then
      TARGET=FETCH_HEAD
    else
      trouble "the branch $TARGET_BRANCH isn't in the clone, and couldn't be fetched from origin: check out every branch, or let the job fetch (the recipes' README says how, CI by CI)" "$tmp/git.log"
      return 1
    fi
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
    # A path neither commit has is a mistake in PATHS (or LOKI_PATHS): its
    # rules, if it has any, would never be compared, and nothing would say
    # so.
    set -f
    for list in "PATHS $PATHS" "LOKI_PATHS ${LOKI_PATHS:-}"; do
      missing=
      for p in ${list#* }; do
        p=${p%/}
        if ! git -C "$repo" cat-file -e "$head:$p" 2>/dev/null && ! git -C "$repo" cat-file -e "$base:$p" 2>/dev/null; then
          missing="$missing $p"
        fi
      done
      if [ -n "$missing" ]; then
        say "${list%% *}:$missing: in neither commit: say where the rules are"
        exit 1
      fi
    done
    set +f
  fi
}

# folds_case: whether the file system the commits are written to takes two
# names that differ only in case as one.
folds_case() {
  : >"$tmp/case-probe" && [ -e "$tmp/CASE-PROBE" ]
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
    for p in $PATHS ${LOKI_PATHS:-}; do
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
  if [ -z "${RENDER:-}" ] && folds_case; then
    # Two files whose paths differ only in case, or a file and a directory
    # so, are one on a file system that folds case (a macOS or Windows
    # runner's): written one over the other, one's rules would never be
    # backtested, and the gates would pass. Refused where this machine's
    # file system folds case, the pair in the job's log alone: their names
    # are the pull request's. Elsewhere both are written, and read, as
    # Prometheus reads them.
    pair=$(git -C "$repo" ls-tree -r --name-only "$c" -- "$@" | awk '
      {
        f = $0; k = tolower(f)
        if ((k in file) && file[k] != f) { print file[k] " and " f; exit }
        if (k in dir) { print dir[k] " and " f; exit }
        file[k] = f
        n = split(f, part, "/"); p = ""
        for (i = 1; i < n; i++) {
          p = (i == 1 ? part[i] : p "/" part[i]); q = tolower(p)
          if (q in file) { print file[q] " and " p; exit }
          if (!(q in dir)) dir[q] = p
        }
      }')
    if [ -n "$pair" ]; then
      say "two paths that differ only in case, in $what: $pair"
      trouble "$what holds two paths that differ only in case, which a file system that folds case (macOS's, Windows') writes as one: not read (the job's log says which)"
      return 1
    fi
  fi
  if ! (cd "$tmp/empty" && GIT_INDEX_FILE=$tmp/index git --git-dir="$gitdir" --work-tree=. archive --worktree-attributes \
    --format=tar -o "$tmp/c.tar" "$c" -- "$@") 2>"$tmp/git.log" || ! tar -x -f "$tmp/c.tar" -C "$d" 2>>"$tmp/git.log"; then
    trouble "$what couldn't be read" "$tmp/git.log"
    return 1
  fi
  if [ -z "${RENDER:-}" ]; then
    # And whatever else this file system takes as one name (a letter
    # past ASCII in another case, or written in other code points):
    # fewer files written than the commit holds. Counted without reading
    # a name, a line each, as a name may hold a line's end: find prints a
    # dot a file; ls-tree quotes a name with one, on a line of its own.
    listed=$(git -C "$repo" ls-tree -r "$c" -- "$@" | awk '$2 == "blob" { n++ } END { print n + 0 }')
    written=$(find "$d" \( -type f -o -type l \) -exec sh -c 'for f do echo .; done' sh {} + | awk 'END { print NR }')
    if [ "$written" -lt "$listed" ]; then
      say "$what: $listed files, $written of them once written here"
      trouble "$what holds files whose names this machine's file system takes as one, as one that folds case does (macOS's, Windows'): not read (the job's log says how many)"
      return 1
    fi
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
# TARGET itself, into $tmp/incidents.yaml: a file, not a link, of 8 MiB at
# most. Not as the commit the rules are compared against has it, where the
# pull request branched from TARGET's branch, which its author picks: a
# pull request branched from before an incident was recorded would be
# judged without it.
incidents() {
  [ -n "${INCIDENTS:-}" ] || return 0
  mode=$(git -C "$repo" ls-tree "$target" -- "$INCIDENTS" | awk '{ print $1 }')
  case $mode in
    100644 | 100755) ;;
    '') trouble "INCIDENTS $INCIDENTS isn't in the commit the pull request goes into"; return 1 ;;
    *) trouble "INCIDENTS $INCIDENTS isn't a file in the commit the pull request goes into"; return 1 ;;
  esac
  if [ "$(git -C "$repo" cat-file -s "$target:$INCIDENTS")" -gt 8388608 ]; then
    trouble "INCIDENTS $INCIDENTS is more than 8 MiB"
    return 1
  fi
  git -C "$repo" cat-file blob "$target:$INCIDENTS" >"$tmp/incidents.yaml" ||
    { trouble "INCIDENTS $INCIDENTS couldn't be read from the commit the pull request goes into"; return 1; }
}

# expectations: EXPECT, as each commit has it, into
# $tmp/expect.head.yaml (the pull request's, for the rules it proposes) and
# $tmp/expect.base.yaml (the commit the rules are compared against's, for
# the rules in place): files, not links, of 1 MiB at most. Both sides as
# the pull request's branch has them, so that what it says changed is
# what it changed: a pull request is held to the expectations of its own
# commit, the target's where the job checks out a merge (merged results,
# or a branch rebased). The pull request may add the file, and the rules
# in place are held to its; or take it away, as if it held none: what it
# held is shown, taken away. In neither commit, it is said.
expectations() {
  [ -n "${EXPECT:-}" ] || return 0
  for side in head base; do
    if [ "$side" = head ]; then commit=$head what="the pull request's commit"; else commit=$base what="the commit it goes into"; fi
    mode=$(git -C "$repo" ls-tree "$commit" -- "$EXPECT" | awk '{ print $1 }')
    case $mode in
      100644 | 100755) ;;
      '') continue ;;
      *) trouble "EXPECT $EXPECT isn't a file in $what"; return 1 ;;
    esac
    if [ "$(git -C "$repo" cat-file -s "$commit:$EXPECT")" -gt 1048576 ]; then
      trouble "EXPECT $EXPECT is more than 1 MiB in $what"
      return 1
    fi
    git -C "$repo" cat-file blob "$commit:$EXPECT" >"$tmp/expect.$side.yaml" ||
      { trouble "EXPECT $EXPECT couldn't be read from $what"; return 1; }
  done
  if [ ! -f "$tmp/expect.head.yaml" ] && [ ! -f "$tmp/expect.base.yaml" ]; then
    trouble "EXPECT $EXPECT is in neither the pull request's commit nor the commit it goes into"
    return 1
  fi
  [ -f "$tmp/expect.head.yaml" ] || printf 'expectations: []\n' >"$tmp/expect.head.yaml"
}

# in_tree SIDE COMMAND: RENDER or RENDER_PREPARE, in a commit's files, with
# a HOME of that commit's own (what Helm caches there is fetched once a
# commit), without the variables CIs keep tokens in; REPO is the clone. It
# runs what the pull request holds, with the job's rights.
in_tree() {
  mkdir -p "$tmp/$1-home"
  (cd "$tmp/$1" && env -u GITHUB_TOKEN_FILE -u GITLAB_TOKEN_FILE -u BITBUCKET_TOKEN_FILE -u AZURE_DEVOPS_TOKEN_FILE \
    -u GITEA_TOKEN_FILE -u LAPILLI_INPUT_TOKEN -u LAPILLI_ID_TOKEN -u LAPILLI_SERVE_TOKEN_FILE -u GITHUB_TOKEN -u GH_TOKEN -u GITEA_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_TOKEN \
    -u ACTIONS_ID_TOKEN_REQUEST_URL -u ACTIONS_RUNTIME_TOKEN -u CI_JOB_TOKEN -u CI_JOB_JWT -u CI_JOB_JWT_V1 -u CI_JOB_JWT_V2 \
    -u CI_REGISTRY_PASSWORD -u CI_DEPLOY_PASSWORD -u CI_REPOSITORY_URL -u CI_DEPENDENCY_PROXY_PASSWORD -u CI_BUILD_TOKEN \
    -u SYSTEM_ACCESSTOKEN -u BITBUCKET_STEP_OIDC_TOKEN HOME="$tmp/$1-home" REPO="$clone" sh -c "$2")
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

# fenced WHAT LOG CHARS: WHAT, then LOG's last lines, CHARS characters of
# them at most, as a fenced block: what hidden wrote taken out (each a
# whole word), and URLs' user names and IP addresses, with no backtick,
# which could end the block, and no control or format character, line or
# paragraph separator, which could hide or reorder what is shown. Taken out
# first and cut after, so nothing is cut in two before it is found. Inside
# a fence, no host reads Markdown, HTML, a mention or a link.
fenced() {
  printf '```text\n'
  {
    [ ! -s "$2" ] || tail -n 40 "$2" | tail -c 1048576
  } | jq -Rrs --arg what "$1" --argjson most "$3" --rawfile hide "$tmp/hidden" '
    def esc: gsub("(?<c>[\\\\^$.|?*+()\\[\\]{}/-])"; "\\\(.c)");
    def clean:
      reduce ($hide | split("\n")[] | select(length > 0)) as $h (.;
        gsub("(?<![A-Za-z0-9._-])" + ($h | esc) + "(?![A-Za-z0-9_-]|\\.[A-Za-z0-9_-])"; "[hidden]"))
      | gsub("(?<s>[A-Za-z][A-Za-z0-9+.-]*://)[^/@\\s\"]*@"; "\(.s)")
      | gsub("\\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\\](:[0-9]+)?"; "[address]")
      | gsub("\\b([0-9]{1,3}\\.){3}[0-9]{1,3}(:[0-9]+)?\\b"; "[address]")
      | gsub("\t"; " ") | gsub("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]"; "") | gsub("`"; "'"'"'");
    ($what + "." | clean),
    if $most > 0 and length > 0 then split("\n") | map(clean) | join("\n") | .[-$most:] else empty end'
  printf '\n```\n'
}

# summary FIELD: the report's summary's FIELD, as its first line says.
summary() {
  head -n 1 "$tmp/report.md" | sed -n 's/^\[\/\/\]: # (lapilli-compare \({.*}\))$/\1/p' | jq -r ".$1 // empty" 2>/dev/null
}

# backtest ARG...: lapilli verdict on the rules of both commits, with the
# arguments given, or lapilli serve, with what it is sent of them; the
# report in $tmp/report.md, lapilli's exit status in $verdict (0 for a
# report serve gave); then what the report says of the expectations, and
# of the rules that couldn't be replayed.
verdict=
backtest() {
  if [ -n "$serve" ]; then
    : >"$tmp/lapilli.said"
    through_serve || :
  else
    run_lapilli "$@"
  fi
  judge
}

# run_lapilli ARG...: lapilli verdict, run here, as backtest says.
run_lapilli() {
  # MATCH's labels, the last first, so that each lands in its order.
  rest=$match
  while [ -n "$rest" ]; do
    set -- --match "${rest##*,}" "$@"
    case $rest in *,*) rest=${rest%,*} ;; *) rest= ;; esac
  done
  if [ -n "${IMPACT_FOR:-}" ]; then
    set -- --impact-for "$IMPACT_FOR" "$@"
  fi
  # One argument, as it is.
  if [ -n "${IMPACT:-}" ]; then
    set -- --impact "$IMPACT" "$@"
  fi
  if [ -n "${INCIDENTS:-}" ]; then
    set -- --incidents "$tmp/incidents.yaml" "$@"
  fi
  if [ -f "$tmp/expect.head.yaml" ]; then
    set -- --expect "$tmp/expect.head.yaml" "$@"
    if [ -f "$tmp/expect.base.yaml" ]; then
      set -- --expect-before "$tmp/expect.base.yaml" "$@"
    fi
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
  rm -f "$tmp/verdict.status"
  { "$bin" verdict "$@" --changed-only --markdown 2>&1 >"$tmp/report.md" || echo "$?" >"$tmp/verdict.status"; } | live "$tmp/lapilli.log"
  [ ! -s "$tmp/verdict.status" ] || verdict=$(cat "$tmp/verdict.status")
  # What of it the comment may show: not why a rule couldn't be replayed,
  # which --markdown keeps to the job's log ("query failed: " begins every
  # reason a query gives, the history unread or the query refused), nor
  # what a server or the network said, which says where the history is and
  # who reads it (an account, a gateway), nor how the run went, its
  # progress lines.
  grep -E -v "couldn't be replayed: |query failed: |HTTP [0-9]{3}: |dial (tcp|udp)|x509: |tls: |^lapilli verdict: after [0-9]" "$tmp/lapilli.log" >"$tmp/lapilli.said" || :
  case $verdict in
    0) ;;
    1) trouble "the backtest failed, or didn't finish (lapilli exited 1)" "$tmp/lapilli.said" ;;
    2)
      if grep -q -e 'unknown flag --expect' "$tmp/lapilli.log"; then
        trouble "the lapilli run has no --expect: give LAPILLI_VERSION (or LAPILLI) a release with it" "$tmp/lapilli.said"
      elif grep -q -e '--markdown needs a value' -e 'unknown flag --markdown' "$tmp/lapilli.log" && [ -n "${LAPILLI:-}" ]; then
        trouble "the lapilli LAPILLI names has no --markdown: give a later release" "$tmp/lapilli.said"
      elif grep -q -e '--markdown needs a value' -e 'unknown flag --markdown' "$tmp/lapilli.log"; then
        trouble "Lapilli $LAPILLI_VERSION has no --markdown: give LAPILLI_VERSION a later release" "$tmp/lapilli.said"
      else
        trouble "lapilli refused its arguments (it exited 2)" "$tmp/lapilli.said"
      fi
      ;;
    *) trouble "lapilli didn't run (exit status $verdict)" "$tmp/lapilli.said" ;;
  esac
}

# judge: what the report's summary line says decides, whoever wrote it,
# lapilli here or lapilli serve.
judge() {
  # Asked to fail when it does, a backtest of rules that couldn't be
  # replayed is one that failed, whatever why: the history out of reach,
  # or a query it refuses as written, which is the pull request's to mend.
  # The summary's unreplayed counts them, as the comment's "Could not
  # replay" lists them.
  # Held to the expectations, a change that breaks one fails the job, once
  # the comment says which: the report's own table, not a problem of the
  # run's.
  # judged: the report says what became of the expectations, or that
  # nothing changed. Held to them, a run that doesn't fails the job.
  if [ "$verdict" = 0 ] && [ -n "${EXPECT:-}" ]; then
    case $(summary nothing_changed)/$(summary expected) in
      true/* | */true) judged=1 ;;
    esac
  fi
  if [ -n "$judged" ] && [ "$(summary nothing_changed)" != true ] && { [ -n "$held" ] || [ -n "$required" ]; }; then
    broken=$(summary expectations_broken)
    unexpected=$(summary unexpected)
    unreplayed=$(summary unreplayed)
    case $broken$unexpected$unreplayed in
      '' | *[!0-9]*)
        judged=
        if [ -n "$serve" ]; then
          trouble "lapilli serve's report doesn't say what became of the expectations"
        else
          trouble "the report doesn't say what became of the expectations: give LAPILLI_VERSION a release with --expect" "$tmp/lapilli.said"
        fi
        ;;
      *)
        if [ -n "$held" ] && [ "$broken" -gt 0 ]; then
          unmet="the change breaks $broken expectations the rules in place meet (FAIL_ON_EXPECTATIONS=true)"
        elif [ -n "$required" ] && [ "$unexpected" -gt 0 ]; then
          unmet="the change adds $unexpected rules no expectation holds (EXPECT_REQUIRED=true)"
        elif [ "$unreplayed" -gt 0 ]; then
          # A rule that couldn't be replayed can't be held: not known isn't met.
          unmet="$unreplayed of the rules that differ couldn't be replayed, so their expectations can't be judged"
        fi
        ;;
    esac
  fi
  if [ "$verdict" = 0 ] && [ -n "$strict" ]; then
    n=$(summary unreplayed)
    case $n in
      '' | *[!0-9]*) trouble "the report has no summary line" "$tmp/lapilli.said" ;;
      0) ;;
      # Which, the report lists; why, the job's log says.
      *) trouble "$n of the rules that differ couldn't be replayed (FAIL_ON_ERROR=true)" ;;
    esac
  fi
}

# text FILE: whether FILE is UTF-8 text, which jq reads back as it is: a
# rule file is sent to lapilli serve as a JSON string.
text() { jq -Rsj . "$1" | cmp -s - "$1"; }

# side SIDE WHAT: a commit's rules as lapilli serve takes them, a JSON
# object a line, {"path", "content"}, in $tmp/SIDE.jsonl: each file
# --rules would read, named by its path under what PATHS names, as --rules
# reads a directory (a file it holds is the directory's, one below it a
# directory's below), a file PATHS names by its own name, and what RENDER
# wrote as rules.yaml. Hidden ones too, a name in their path beginning with
# a dot, as Prometheus's rule_files glob loads alerts/.page.yaml and
# alerts/..page.yaml and --rules reads one holding rules: serve passes over
# those holding none. A hidden one that isn't UTF-8 text, such as a
# .DS_Store, is no rule file, and isn't sent; but one in UTF-16 or UTF-32,
# its BOM first, Prometheus reads as rules, and is said as any file that
# isn't UTF-8 text is. A directory named .git is never sent; names
# beginning with .. are, as a commit's files hold no Kubernetes volume's
# own (the recipe refuses a commit holding a link). LOKI_PATHS's alike, each with
# "loki": true, a Loki ruler's. WHAT names the commit.
side() {
  s=$1 what=$2
  : >"$tmp/$s.jsonl"
  rm -f "$tmp/$s.unsent"
  set -f
  if [ -n "${RENDER:-}" ]; then
    set -- "$tmp/rules/$s"
  else
    set --
    for p in $PATHS; do
      set -- "$@" "$tmp/$s/${p%/}"
    done
  fi
  # LOKI_PATHS's after a mark, which no path under $tmp is.
  set -- "$@" -loki
  for p in ${LOKI_PATHS:-}; do
    set -- "$@" "$tmp/$s/${p%/}"
  done
  set +f
  loki=false
  for root; do
    if [ "$root" = -loki ]; then
      loki=true
      continue
    fi
    [ -e "$root" ] || continue
    # shellcheck disable=SC2016
    if ! find "$root" ! -path "$root" -type d -name .git -prune -o -type f -exec sh -c '
      root=$1 out=$2 loki=$3
      shift 3
      for f; do
        if [ -d "$root" ]; then rel=${f#"$root"/}; else rel=${f##*/}; fi
        if ! jq -Rsj . "$f" | cmp -s - "$f"; then
          # Hidden, and not text: no rule file, unless its BOM says UTF-16
          # or UTF-32, which Prometheus reads.
          case /$rel in
            */.*)
              case $(od -An -tx1 -N4 "$f" | tr -d " \n") in
                fffe* | feff* | 0000feff) ;;
                *) continue ;;
              esac
              ;;
          esac
          printf "%s" "$rel" >"$out.unsent"
          exit 1
        fi
        jq -nc --arg path "$rel" --rawfile content "$f" --argjson loki "$loki" \
          "{path: \$path, content: \$content} | if \$loki then .loki = true else . end" >>"$out.jsonl" || exit 1
      done' sh "$root" "$tmp/$s" "$loki" {} + 2>"$tmp/find.log"; then
      if [ -e "$tmp/$s.unsent" ]; then
        # Its path is the pull request's: in the job's log, not the comment.
        say "not UTF-8 text, in $what: $(cat "$tmp/$s.unsent")"
        trouble "a rule file in $what isn't UTF-8 text, which lapilli serve takes alone: not sent (the job's log says which)"
      else
        trouble "the rules of $what couldn't be read" "$tmp/find.log"
      fi
      return 1
    fi
  done
  dup=$(jq -rs 'group_by(.path) | map(select(length > 1) | .[0].path) | first // empty' "$tmp/$s.jsonl" 2>/dev/null) || dup=
  if [ -n "$dup" ]; then
    say "two of PATHS hold $dup, in $what"
    trouble "two of PATHS hold a file of the same name in $what, which lapilli serve reads as one directory's: name them apart (the job's log says which)"
    return 1
  fi
}

# request: the body of POST /v1/backtests, in $tmp/serve.json, built by jq
# alone: both sides' files, the expectations as expectations() wrote them,
# --rules-select, the period's days, the zone, the pull request's number
# and its commit. 4 MiB at most, as serve takes it: past it, said before
# anything is sent.
request() {
  side base "the commit it goes into" && side head "the pull request's commit" || return 1
  # The pull request's expectations, for the rules it proposes, and the
  # commit it goes into's, for those in place, when it has them.
  expect_head=/dev/null expect_base=/dev/null
  if [ -f "$tmp/expect.head.yaml" ]; then
    expect_head=$tmp/expect.head.yaml
    text "$expect_head" || { trouble "EXPECT $EXPECT isn't UTF-8 text in the pull request's commit, which lapilli serve takes alone"; return 1; }
  fi
  if [ -f "$tmp/expect.base.yaml" ]; then
    expect_base=$tmp/expect.base.yaml
    text "$expect_base" || { trouble "EXPECT $EXPECT isn't UTF-8 text in the commit it goes into, which lapilli serve takes alone"; return 1; }
  fi
  if ! jq -n --slurpfile before "$tmp/base.jsonl" --slurpfile after "$tmp/head.jsonl" \
    --rawfile expect "$expect_head" --rawfile expect_before "$expect_base" \
    --arg expect_given "${expect_head#/dev/null}" --arg before_given "${expect_base#/dev/null}" --arg select "$serve_select" \
    --arg tz "$serve_tz" --arg days "$serve_days" --argjson pr "$PR" --arg commit "$head" '
      {before: $before, after: $after}
      + (if $expect_given != "" then {expect: $expect} else {} end)
      + (if $before_given != "" then {expect_before: $expect_before} else {} end)
      + (if $select != "" then {rules_select: $select} else {} end)
      + (if $days != "" then {days: ($days | tonumber)} else {} end)
      + (if $tz != "" then {tz: $tz} else {} end)
      + {pull_request: $pr, commit: $commit}' >"$tmp/serve.json" 2>"$tmp/jq.log"; then
    trouble "the request to lapilli serve couldn't be written" "$tmp/jq.log"
    return 1
  fi
  size=$(wc -c <"$tmp/serve.json")
  if [ "$size" -gt 4194304 ]; then
    trouble "the rules and expectations are $(((size + 1048575) / 1048576)) MiB as lapilli serve's request, past the 4 MiB it takes: backtest fewer files"
    return 1
  fi
  say "sending lapilli serve $(jq -s length "$tmp/head.jsonl") files of the pull request's commit, and $(jq -s length "$tmp/base.jsonl") of the commit it goes into"
}

# serve_ask METHOD PATH BEARER [FILE]: asks lapilli serve's port, the
# bearer token (the CI's, or a backtest's key) given to curl on stdin; the
# answer in $tmp/serve.out, 2 MiB at most, its headers in
# $tmp/serve.headers, what curl said in $tmp/serve.curl. Writes the HTTP
# status, or "failed". curl reads no .curlrc (-q), follows no redirect, and
# speaks only the URL's own scheme.
serve_ask() {
  rm -f "$tmp/failed"
  : >"$tmp/serve.headers"
  if { serve_curl "$@" 2>"$tmp/serve.curl" || : >"$tmp/failed"; } | capped 2097152 "$tmp/serve.out" && [ ! -e "$tmp/failed" ]; then
    code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$tmp/serve.headers" | tail -n 1)
    echo "${code:-failed}"
  else
    echo failed
  fi
}

# Plain http goes through no proxy (--noproxy '*', whatever http_proxy
# says): the token would cross it in the clear, and leave the cluster.
serve_curl() {
  sc_method=$1 sc_path=$2 sc_bearer=$3
  if [ $# -ge 4 ]; then
    set -- --max-time 60 -H 'Content-Type: application/json' --data-binary "@$4"
  else
    set -- --max-time 30
  fi
  case $serve in http://*) set -- "$@" --noproxy '*' ;; esac
  printf 'Authorization: Bearer %s\n' "$sc_bearer" | curl -q -sS --connect-timeout 10 --max-filesize 2097152 --proto "$serve_proto" \
    -X "$sc_method" -H @- -H 'Accept: application/json' "$@" -D "$tmp/serve.headers" -o - "$serve$sc_path"
}

# serve_field FIELD: the answer's FIELD, a string, or nothing.
serve_field() { jq -r --arg f "$1" '.[$f] // empty | strings' "$tmp/serve.out" 2>/dev/null | head -n 1; }
# serve_number FIELD: the answer's FIELD, a whole number, or nothing.
serve_number() {
  jq -r --arg f "$1" '.[$f] // empty | numbers | floor | tostring' "$tmp/serve.out" 2>/dev/null | head -n 1 | sed -n '/^[0-9][0-9]*$/p'
}
# serve_said: what serve's answer says went wrong, its error, into
# $tmp/serve.said, and in the job's log, made inert: of an answer that
# isn't serve's JSON, a proxy's page say, the job's log alone has it.
serve_said() {
  jq -r 'if type == "object" and (.error | type) == "string" then .error else empty end' "$tmp/serve.out" >"$tmp/serve.said" 2>/dev/null ||
    : >"$tmp/serve.said"
  if [ -s "$tmp/serve.said" ]; then
    logged "$tmp/serve.said"
  elif [ -s "$tmp/serve.out" ]; then
    say "lapilli serve answered: $(shown "$tmp/serve.out")"
  fi
}
# retry_after DEFAULT: the seconds serve's Retry-After says, or DEFAULT
# without one; 300 at most, and at least SERVE_RETRY_MIN (5), or
# SERVE_POLL if longer, and a second whatever they say: a Retry-After of 0
# isn't a loop of requests.
retry_after() {
  tr -d '\r' <"$tmp/serve.headers" | sed -n 's/^[Rr][Ee][Tt][Rr][Yy]-[Aa][Ff][Tt][Ee][Rr]: *\([0-9][0-9]*\) *$/\1/p' | tail -n 1 |
    awk -v def="$1" -v least="$retry_min" '{ v = $1 + 0 } END { n = (NR ? v : def); if (n > 300) n = 300; if (n < least) n = least; print n }'
}
# pause SECONDS: sleeps, no further than the deadline.
pause() {
  left=$((deadline - $(date +%s)))
  [ "$left" -gt 0 ] || return 0
  sleep "$((${1:-0} < left ? ${1:-0} : left))"
}

# actions_token: an ID token for LAPILLI_SERVE_AUDIENCE, asked of GitHub's
# or Forgejo's Actions as core.getIDToken asks it, in serve_token: the
# request's token given to curl on stdin, the audience URL-encoded, the
# answer's value read with jq.
actions_token() {
  aud=$(jq -rn --arg a "$LAPILLI_SERVE_AUDIENCE" '$a | @uri')
  case $ACTIONS_ID_TOKEN_REQUEST_URL in
    *\?*) u="$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$aud" ;;
    *) u="$ACTIONS_ID_TOKEN_REQUEST_URL?audience=$aud" ;;
  esac
  case $u in https://*) proto='=https' ;; *) proto='=http' ;; esac
  rm -f "$tmp/failed"
  : >"$tmp/oidc.headers"
  if ! { printf 'Authorization: Bearer %s\nAccept: application/json\n' "$ACTIONS_ID_TOKEN_REQUEST_TOKEN" |
    curl -q -sS --max-time 30 --max-filesize 65536 --proto "$proto" -H @- -D "$tmp/oidc.headers" -o - "$u" 2>"$tmp/curl.log" ||
    : >"$tmp/failed"; } | capped 65536 "$tmp/oidc.json" || [ -e "$tmp/failed" ]; then
    logged "$tmp/curl.log"
    trouble "the CI gave the job no ID token for LAPILLI_SERVE_AUDIENCE (the job's log says why)"
    return 1
  fi
  code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$tmp/oidc.headers" | tail -n 1)
  serve_token=$(jq -r '.value // empty | strings' "$tmp/oidc.json" 2>/dev/null) || serve_token=
  # Masked in GitHub Actions' log, before anything could log it, as
  # core.getIDToken masks the token it gets: on stdout, % as %25, as the
  # runner reads a command's value. Only a token that could be sent: one
  # line of visible characters.
  if [ "${ci_masks:-}" = github ] && [ "${GITHUB_ACTIONS:-}" = true ] && token_ok "$serve_token"; then
    printf '::add-mask::%s\n' "$(printf '%s' "$serve_token" | sed 's/%/%25/g')"
  fi
  case $code in
    2??) ;;
    *)
      say "the CI didn't give an ID token (${code:-failed}): $(shown "$tmp/oidc.json")"
      serve_token=
      ;;
  esac
  if ! token_ok "$serve_token"; then
    serve_token=
    trouble "the CI gave the job no ID token for LAPILLI_SERVE_AUDIENCE (the job's log says why)"
    return 1
  fi
}

# through_serve: the rules of both commits, and the expectations, sent to
# lapilli serve's backtest port, which replays them with its own access to
# the history, judged by its record, in a queue of its own; then read back
# every SERVE_POLL seconds, saying where it is in the job's log, until it
# ends or SERVE_WAIT does. Its report in $tmp/report.md, where lapilli
# verdict would have written it, and $verdict 0; what went wrong is
# trouble, in serve's words where it has them.
through_serve() {
  request || return 1
  deadline=$(($(date +%s) + serve_wait))
  # The token is asked for, or read, for each request: an ID token is
  # taken within its hour, and a plain one may be rotated.
  while :; do
    case $token_from in
      actions) actions_token || return 1 ;;
      LAPILLI_SERVE_TOKEN_FILE)
        serve_token=$(tr -d '\r\n' <"$LAPILLI_SERVE_TOKEN_FILE" 2>/dev/null) || serve_token=
        if ! token_ok "$serve_token"; then
          trouble "LAPILLI_SERVE_TOKEN_FILE holds no token lapilli serve could take: visible characters, no space, 8 KiB at most"
          return 1
        fi
        ;;
    esac
    code=$(serve_ask POST /v1/backtests "$serve_token" "$tmp/serve.json")
    case $code in
      202) break ;;
      200)
        trouble "lapilli serve had this request already, from this token, and gave the key to read it by to the first: a job asks once"
        return 1
        ;;
      400)
        serve_said
        trouble "lapilli serve refused the backtest" "$tmp/serve.said"
        return 1
        ;;
      401)
        trouble "lapilli serve didn't take the job's token: the entry of its --backtest-trust for this repository says what it takes (its log says why)"
        return 1
        ;;
      409)
        serve_said
        trouble "the job's token started another backtest in lapilli serve: a job asks once, with a token of its own" "$tmp/serve.said"
        return 1
        ;;
      413)
        serve_said
        trouble "the request is past what lapilli serve takes" "$tmp/serve.said"
        return 1
        ;;
      429 | 503)
        serve_said
        wait_s=$(retry_after 60)
        if [ $(($(date +%s) + wait_s)) -lt "$deadline" ]; then
          say "lapilli serve can't take the backtest yet ($code): asking again in ${wait_s}s"
          pause "$wait_s"
          continue
        fi
        if [ "$code" = 429 ]; then
          trouble "lapilli serve had no room for the backtest within SERVE_WAIT ($serve_wait_given)" "$tmp/serve.said"
        else
          trouble "lapilli serve couldn't take the backtest within SERVE_WAIT ($serve_wait_given)" "$tmp/serve.said"
        fi
        return 1
        ;;
      failed)
        logged "$tmp/serve.curl"
        trouble "lapilli serve couldn't be reached (the job's log says why)"
        return 1
        ;;
      *)
        serve_said
        trouble "lapilli serve answered HTTP $code" "$tmp/serve.said"
        return 1
        ;;
    esac
  done
  serve_token=
  bt_id=$(serve_field id)
  bt_key=$(serve_field key)
  case $bt_id in '' | *[!0-9a-f]*) bt_id= ;; esac
  case $bt_key in '' | *[!A-Za-z0-9_-]*) bt_key= ;; esac
  if [ "${#bt_id}" -ne 32 ] || [ -z "$bt_key" ] || [ "${#bt_key}" -gt 256 ]; then
    trouble "lapilli serve took the backtest, but gave no id and key to read it by"
    return 1
  fi
  bt_live=1
  of=$(serve_number requests) p=$(serve_number position)
  say "lapilli serve took the backtest ($bt_id): ${of:-?} requests to the history, ${p:-?} before it in the queue"
  said_at=$(date +%s) said_state='' failures=0
  while :; do
    pause "$serve_poll"
    code=$(serve_ask GET "/v1/backtests/$bt_id" "$bt_key")
    case $code in
      200 | 202)
        failures=0
        state=$(serve_field state)
        case $code/$state in
          202/waiting)
            p=$(serve_number position)
            what="waiting in lapilli serve's queue, ${p:-?} before it"
            ;;
          202/running)
            asked=$(serve_number asked) of=$(serve_number requests)
            what="running in lapilli serve: ${asked:-?} of its ${of:-?} requests to the history asked"
            ;;
          200/done)
            bt_live=
            serve_report
            return
            ;;
          200/failed)
            bt_live=
            serve_said
            trouble "lapilli serve couldn't backtest the rules" "$tmp/serve.said"
            return 1
            ;;
          200/superseded)
            bt_live=
            trouble "a newer commit's backtest took this one's place in lapilli serve's queue"
            return 1
            ;;
          200/cancelled)
            bt_live=
            trouble "the backtest was cancelled in lapilli serve"
            return 1
            ;;
          *)
            say "lapilli serve said the backtest is $(printf '%s' "$state" | head -c 64) ($code)"
            serve_cancel
            trouble "lapilli serve said the backtest is in a state the recipe doesn't know: cancelled"
            return 1
            ;;
        esac
        # Where it is, when that changes, and every 30 seconds.
        now=$(date +%s)
        if [ "$state" != "$said_state" ] || [ $((now - said_at)) -ge 30 ]; then
          say "$what"
          said_at=$now said_state=$state
        fi
        ;;
      404)
        bt_live=
        trouble "lapilli serve has no such backtest: kept a day after it ends, or another serve's"
        return 1
        ;;
      429 | 503)
        pause "$(retry_after "$serve_poll")"
        ;;
      *)
        # A request lost on the way, or a replica restarting: asked again,
        # five times in a row at most.
        failures=$((failures + 1))
        if [ "$code" = failed ]; then logged "$tmp/serve.curl"; else serve_said; fi
        if [ "$failures" -ge 5 ]; then
          serve_cancel
          trouble "lapilli serve couldn't be read while it backtested (the job's log says why)"
          return 1
        fi
        ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      serve_cancel
      trouble "the backtest didn't come back within SERVE_WAIT ($serve_wait_given): cancelled in lapilli serve"
      return 1
    fi
  done
}

# serve_report: the report of a backtest done, as lapilli verdict would
# have written it, its summary line first.
serve_report() {
  if ! jq -e '.markdown | type == "string"' "$tmp/serve.out" >/dev/null 2>&1; then
    trouble "lapilli serve's answer holds no report"
    return 1
  fi
  if ! report_of_serve >"$tmp/report.json" 2>/dev/null || ! jq -e '.markdown | strings' "$tmp/report.json" >/dev/null 2>&1; then
    trouble "lapilli serve's report doesn't begin with the summary line lapilli writes: not shown"
    return 1
  fi
  jq -j '.markdown' "$tmp/report.json" >"$tmp/report.md"
  if jq -e '.long' "$tmp/report.json" >/dev/null 2>&1; then
    say "lapilli serve's report is longer than lapilli writes one (64000 bytes): read to its first 65536 characters, the rest left out"
  fi
  n=$(jq '.leaving' "$tmp/report.json" 2>/dev/null) || n=
  case $n in '' | *[!0-9]*) n=0 ;; esac
  if [ "$n" -gt 0 ]; then
    say "lapilli serve's report has $n lines of Markdown lapilli doesn't write (HTML, a link, a mention, a reference, a command): each left out of the comment, the first of them here"
    jq -r '.left[]' "$tmp/report.json" >"$tmp/left.log" 2>/dev/null || :
    logged "$tmp/left.log"
  fi
  note=$(serve_field note)
  [ -z "$note" ] || say "lapilli serve: $note"
  say "lapilli serve backtested the rules"
  verdict=0
}

# report_of_serve: the Markdown in serve's answer ($tmp/serve.out), as
# {markdown, leaving, left, long}: its first line, the summary line, as
# serve wrote it, or nothing when it isn't one (no report, then); and each
# line after it as serve wrote it when it is one lapilli writes, any other
# left out, a line saying so in place of each run of them, leaving counting
# them and left holding the first five, 300 characters of each. Past
# 65,536 characters, more than lapilli writes (64,000 bytes), it is read
# no further (long), what is past it left out too. Lapilli writes what
# came from the rules as code spans, each closed on its own line, and
# outside them only its own text: letters, digits, spaces and the
# punctuation of its tables, times and counts; a # only to start a
# heading; <details><summary>N ...</summary> and </details>, each opened
# and then closed, each with a blank line after it, as an HTML block runs
# to one. Not what a host reads as a
# mention (@), a reference (#1, !1, ~x, $1, &1, %x, GH-1), HTML or an
# entity (<, &), a link or an image ([, //, www., mailto:), an escape (\),
# a fence, or a quick action (a / first on a line, as GitLab runs one from
# a note with the job's token); nor, even in a code span, a character that
# doesn't show or moves the text around it. In a table, which every host
# splits into cells at each | before it reads a code span, a code span's |
# is \|, and no | has two backslashes or more before it, which hosts read
# differently (a line that starts with |, a delimiter row and the line
# above it, and each line after one until a blank line). A line is let
# through, or left out, whole: what each host makes of a line changed in
# part isn't known here. jq alone reads it.
report_of_serve() {
  # shellcheck disable=SC2016
  jq -c '
    # A line as {plain, code}, the text outside its code spans and in
    # them, each joined by spaces; or null when a run of backticks is left
    # open on it. A run of n opens a span, the next run of n closes it, as
    # CommonMark reads them. Split at each backtick, an empty part is one
    # more of a run.
    def spans:
      split("`") as $p
      | [foreach ($p[1:][], null) as $q ({run: 0, open: 0};
          if $q == null then
            (if .run == 0 then . elif .open == .run then .open = 0 else .open = -1 end)
            | .k = (if .open == 0 then "closed" else "open" end)
          else
            .run += 1
            | if $q == "" then .k = "" else
                (if .open == 0 then .open = .run elif .run == .open then .open = 0 else . end)
                | .k = (if .open == 0 then "plain" else "code" end) | .run = 0
              end
          end;
          [.k, $q])] as $t
      | if $t[-1][0] != "closed" then null
        else {plain: ([$p[0]] + [$t[] | select(.[0] == "plain") | .[1]] | join(" ")), code: ([$t[] | select(.[0] == "code") | .[1]] | join(" "))} end;
    def plain:
      test("^[A-Za-z0-9 |:;,.\u0027+%?()*/_\u2192\u2013\u2717\u25cb\u25cf-]*$")
      and (test("//|www\\.|mailto:|gh-[0-9]|\\*iteration:|%[a-z0-9_\"]"; "i") | not);
    # Nothing that does not show, or moves the text around it: control
    # and format characters, line and paragraph separators, spaces but the
    # space (every other of Zs), Hangul fillers, as lapilli shows them.
    def shows:
      test("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}\u00a0\u1680\u2000-\u200a\u202f\u205f\u3000\u115f\u1160\u3164\uffa0\u2800]") | not;
    # In a table, a | with no backslash before it ends a cell, one with
    # one is in it; one with two or more is read either way by one host
    # or another (comrak 1.0 takes \\| for the end of the cell), and
    # lapilli writes none: a backslash right before a | is \u{5c}.
    def ours($table):
      shows and (test("^ */") | not)
      and (($table | not) or (test("\\\\\\\\[|]") | not))
      and ((if test("^#{1,6} ") then sub("^#{1,6} "; "") else . end | spans) as $s
        | $s != null and ($s.plain | plain) and (($table | not) or ($s.code | test("(?<!\\\\)[|]") | not)));
    def kind($table):
      if test("^<details><summary>[0-9]+ [a-z ]+</summary>$") then "open"
      elif . == "</details>" then "close"
      elif ours($table) then "ours"
      else "left" end;
    (.markdown | strings) as $md
    | ($md | length > 65536) as $long
    | (if $long then $md[:65536] | split("\n") | .[:-1] else $md | split("\n") end) as $all
    | ($all[0] // "") as $first
    | if ($first | test("^\\[//\\]: # \\(lapilli-compare \\{[a-z0-9_\":,.+-]*\\}\\)$"))
        and ($first | ltrimstr("[//]: # (lapilli-compare ") | rtrimstr(")") | (try fromjson catch null) | type == "object")
      then
        $all[1:] as $rest
        | ($rest | length) as $n
        | [$rest[] | test("^ *[|]")] as $piped
        | [$rest[] | test("^ *[|]? *:?-+:? *([|] *:?-+:? *)*[|]? *$")] as $delim
        | [$rest[] | test("^ *$")] as $blank
        | (reduce range(0; $n) as $i ([];
            . + [$piped[$i] or $delim[$i] or ($delim[$i + 1] // false) or ($i > 0 and .[$i - 1] and ($blank[$i] | not))])) as $table
        | [range(0; $n) as $i | $rest[$i] | kind($table[$i])] as $tags
        # A <details> line, or </details>, starts an HTML block, which only
        # a blank line ends: a line after one before a blank line is in
        # it, its code spans read as nothing, its HTML as HTML. Lapilli
        # writes a blank line after each; a tag with none after it is left
        # out, and the lines after it up to a blank line.
        | (reduce range(0; $n) as $i ({html: false, out: []};
            if ($tags[$i] == "open" or $tags[$i] == "close") and $i + 1 < $n and ($blank[$i + 1] | not) then .html = true | .out += [true]
            elif .html and ($blank[$i] | not) then .out += [true]
            else .html = false | .out += [false] end) | .out) as $glued
        | [range(0; $n) as $i | if $glued[$i] then "left" else $tags[$i] end] as $kind
        | (reduce range(0; $n) as $i ({open: null, odd: [range(0; $n) | false]};
            if $kind[$i] == "open" then (if .open == null then .open = $i else .odd[$i] = true end)
            elif $kind[$i] == "close" then (if .open == null then .odd[$i] = true else .open = null end)
            else . end)
          | if .open == null then .odd else (.odd[.open] = true | .odd) end) as $odd
        | [range(0; $n) as $i | $kind[$i] == "left" or $odd[$i]] as $out
        | "(what lapilli serve wrote here isn\u0027t Markdown lapilli writes: left out, the job\u0027s log has it)" as $left
        | {markdown: ([$first] + [range(0; $n) as $i | if ($out[$i] | not) then $rest[$i] elif $i > 0 and $out[$i - 1] then empty else $left end]
              + (if $long and ($n == 0 or ($out[$n - 1] | not)) then [$left] else [] end)
            | join("\n") | if endswith("\n") then . else . + "\n" end), long: $long,
           left: [limit(5; range(0; $n) as $i | select($out[$i]) | $rest[$i][:300])], leaving: ([$out[] | select(.)] | length)}
      else {unusable: true} end' "$tmp/serve.out"
}

# serve_cancel: the backtest serve took, waiting or running, cancelled
# (DELETE): a job that stops waiting leaves serve nothing to do.
serve_cancel() {
  [ -n "$bt_live" ] || return 0
  bt_live=
  c=$(serve_ask DELETE "/v1/backtests/$bt_id" "$bt_key") || c=failed
  case $c in
    2??) say "cancelled the backtest in lapilli serve" ;;
    *) say "the backtest couldn't be cancelled in lapilli serve ($c)" ;;
  esac
}

# nothing_changed: whether the report says nothing that decides when a rule
# fires changed, on its first line, from a run that went well.
nothing_changed() {
  [ "$verdict" = 0 ] && [ -z "$problem" ] && [ -s "$tmp/report.md" ] && [ "$(summary nothing_changed)" = true ]
}

# compose: the comment, in $tmp/comment.md: the marker; the report, if
# lapilli wrote one; what went wrong, if anything did, in a fenced block.
# Under the 65,536 characters GitHub takes, the least of the hosts: a
# shorter log, or none, beside a long report; and a report too long even
# so (lapilli's is 64,000 bytes at most, serve's isn't taken on trust) cut
# to its first lines, within 60,000 bytes with the rest, saying so.
compose() {
  for logged_bytes in 4000 1000 0; do
    comment "$tmp/report.md" "$logged_bytes"
    [ "$(wc -c <"$tmp/comment.md")" -gt 65000 ] || return 0
  done
  comment /dev/null 0
  room=$((60000 - 200 - $(wc -c <"$tmp/comment.md")))
  # Whole lines, a <details> left open closed: each line is one lapilli
  # writes, or one in place of what serve wrote that it doesn't.
  LC_ALL=C awk -v room="$room" '
    { n += length($0) + 1; if (n > room) exit; print }
    /^<details>/ { open = 1 }
    $0 == "</details>" { open = 0 }
    END { if (open) printf "\n</details>\n" }' "$tmp/report.md" >"$tmp/report.cut"
  printf '\n**Cut short** by the recipe, to keep the comment under 60000 bytes: the rest of the report isn'"'"'t shown.\n' >>"$tmp/report.cut"
  comment "$tmp/report.cut" 0
}

# comment REPORT BYTES: the comment, in $tmp/comment.md, with REPORT and
# BYTES of the problem's log at most.
comment() {
  {
    printf '%s\n' "$marker"
    if [ -s "$1" ] && [ -s "$tmp/report.md" ] && [ "${verdict:-0}" -le 1 ]; then
      cat "$1"
    fi
    if [ -n "$problem" ]; then
      printf '\n### Lapilli backtest failed\n\n'
      if [ "$2" -gt 0 ]; then
        fenced "$problem" "$tmp/problem.log" "$2"
      else
        fenced "$problem" /dev/null 0
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
  # Where lapilli serve is, too: an address inside a cluster, say.
  [ -z "$serve" ] || hidden --prometheus "$serve" >>"$tmp/hidden"
  # The base first, then the pull request's: what the pull request runs
  # comes last.
  if commits && fetch_lapilli && files "$base" "$tmp/base" "the commit it goes into" &&
    files "$head" "$tmp/head" "the pull request's commit" && incidents && expectations; then
    if [ -z "${RENDER:-}" ] || { render base "the commit it goes into" && render head "the pull request's commit"; }; then
      backtest "$@"
    fi
  fi
  compose
  # Held to the expectations, a run that couldn't judge them (the file
  # unreadable, a rule that differs not replayed, no report) fails the
  # job, as does one that broke them whose comment couldn't be posted: a
  # gate that can't judge doesn't pass. What else goes wrong fails it only
  # with FAIL_ON_ERROR, as before.
  gate=$held$required
  if ! post; then
    say "the comment couldn't be posted"
    [ -z "$unmet" ] || say "$unmet"
    [ -z "$strict$unmet" ] || exit 1
  fi
  if [ -n "$problem" ] && [ -n "$strict" ]; then
    exit 1
  fi
  if [ -n "$gate" ] && [ -z "$judged" ]; then
    say "the expectations couldn't be judged (FAIL_ON_EXPECTATIONS or EXPECT_REQUIRED)"
    exit 1
  fi
  if [ -n "$unmet" ]; then
    say "$unmet"
    exit 1
  fi
  exit 0
}
