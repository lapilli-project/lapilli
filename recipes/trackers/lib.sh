# shellcheck shell=sh
# What the tracker recipes do with Lapilli, alike for every tracker. A
# recipe sets name, defines these, then calls sync_tasks:
#
#   t_me                who the tracker's token is: an id of its account,
#                       which the others find in $me
#   t_find MARKER       the URL of an issue this job made for MARKER, or
#                       nothing: one $me wrote, labelled lapilli where the
#                       tracker has labels by name, its text kept($m) (the
#                       line issue_text ends it with), or Jira's one label
#                       MARKER
#   t_create FILE       make an issue from the task in FILE (JSON); print its URL
#   t_ref URL           the issue's own reference (a number, a key) when URL
#                       is one of this tracker's issues; nothing when it isn't
#   t_state REF MARKER  print STATE UPDATED [CHANGED]: STATE open, done or
#                       dropped; when the issue last changed, and when its
#                       state did (UPDATED where the tracker doesn't say), in
#                       Unix seconds: a comment changes the one, not the
#                       other; nothing
#                       when the issue isn't one this job made for MARKER
#                       ($me wrote it, marked for MARKER as t_find says, and
#                       it is in the project or team the recipe is for);
#                       gone when the tracker says it isn't there (deleted)
#                       and gives the project or team itself
#   t_close REF STATUS  close the issue, as done or dropped
#
# And, to keep comments in step (COMMENTS=true), where the recipe has them:
#
#   t_comments REF      the issue's comments, the earliest first, a JSON
#                       object a line: {"id", "author", "mine", "at",
#                       "body"}: id the tracker's (letters, digits and
#                       ._:-), mine whether $me wrote it, at an RFC 3339
#                       time; people's only, not the tracker's own notes,
#                       nor those only some may read; it touches
#                       $work/capped when it stopped before the last
#   t_comment REF FILE  post FILE's text as a comment on the issue; print
#                       the tracker's id for it
#
# Anyone may write lapilli-task-ID in an issue, and an editor in Lapilli
# may link a task to any issue: an issue someone else wrote is never taken
# for a task's, followed or closed. Nor is the job's issue for one task
# taken for another's because the first task's text names the second's
# marker: the text is the task's (an editor's, or an agent's proposal's),
# and only the line the recipe writes after it counts.
#
# Each token goes where its address points, and curl speaks that
# address's scheme alone (--proto). A recipe says where its tracker's API
# is (tracker_api): https, or this machine over http, for tests. Lapilli's
# is http too, as a Service in the cluster is reached: over plain http its
# token is read by whatever is on the way, so keep that to the cluster
# serve runs in. An address with a user or token before @ is refused, and
# neither an address nor a token is said. A token file holds the token
# alone, on one line (secret, secret_line). An answer is 16 MiB at most.
#
# Lapilli's side, on the port --incidents-listen opens:
#
#   LAPILLI_URL         such as http://lapilli:8082 (or the first argument):
#                       http or https, with no user before @
#   LAPILLI_TOKEN_FILE  a file holding the token in --tasks-token-file, on
#                       one line
#   CLOSE_ISSUES        true to close an issue whose task was done or dropped
#                       in Lapilli (default false)
#   MAX_ISSUES          issues made in one run, at most (default 50)
#   MAX_PAGES           of 200 tasks, at most (default 20)
#   SYNC_DAYS           a task done or dropped, and not changed for this
#                       long, isn't checked against its issue any more
#                       (default 30, at most 3650); Lapilli leaves it out
#                       of what it gives, so they aren't paged through
#   COMMENTS            true to keep comments in step, both ways (default
#                       false): each comment people wrote on a task in
#                       Lapilli is posted to its issue once, quoted, with
#                       who wrote it and the line it is found again by;
#                       each comment on the issue that isn't this job's is
#                       copied to the task once. An edit or a deletion
#                       after that isn't followed, either way
#   MAX_COMMENTS        comments sent each way for a task in a run, at
#                       most (default 200)
#   COMMENT_NAMES       true to say who wrote a comment posted from Lapilli,
#                       as the sign-in proxy names them (often an email);
#                       by default it says "Written in Lapilli:" alone

# shellcheck disable=SC2154 # name is set by the recipe sourcing this
say() { printf '%s: %s\n' "$name" "$*" >&2; }

# number NAME VALUE: refuse a VALUE that isn't a whole number.
number() {
  case $2 in
    '' | *[!0-9]*)
      say "$1 is a number, not $(printf '%s' "$2" | jq -Rc .)"
      exit 1
      ;;
  esac
}

# secret NAME FILE: what FILE holds, one line of the characters a token is
# written in (a Bearer token's, RFC 6750), as it goes in a header, where a
# second line, or a carriage return, would be a header of its own. Spaces,
# tabs and a carriage return after it, as an editor may leave them, are
# dropped. Neither the token nor what else the file holds is said.
secret() {
  s=$(cat "$2")
  while :; do
    case $s in
      *[[:space:]]) s=${s%?} ;;
      *) break ;;
    esac
  done
  case $s in
    '' | *[!A-Za-z0-9._~+/=-]*)
      say "$1 doesn't hold a token: one line of letters, digits and - . _ ~ + / ="
      exit 1
      ;;
  esac
  printf '%s' "$s"
}

# secret_line NAME FILE: as secret, of any characters but a control
# character, a tab among them, a space allowed inside: Lapilli's own
# token, which serve takes of any characters, a tracker's that goes
# base64'd, and one a tracker says nothing of.
secret_line() {
  s=$(cat "$2")
  while :; do
    case $s in
      *[[:space:]]) s=${s%?} ;;
      *) break ;;
    esac
  done
  case $s in
    '' | *[[:cntrl:]]*)
      say "$1 doesn't hold a token: one line, with no tab or other control character"
      exit 1
      ;;
  esac
  printf '%s' "$s"
}

# tracker_api NAME URL: the tracker's API is at URL, which the variable
# NAME gave: https, or this machine over http, for tests (127.0.0.1, [::1]
# or localhost, with a port or not, and no name that only starts with
# one), the scheme in any case, as curl takes it. fetch gives curl that
# scheme alone (tracker_proto). Each recipe calls it before it asks.
tracker_proto=
tracker_api() {
  tracker_proto=
  case $2 in
    *@*)
      say "$1 holds a user or token before @, refused"
      exit 1
      ;;
    [Hh][Tt][Tt][Pp][Ss]://*) tracker_proto='=https' ;;
    [Hh][Tt][Tt][Pp]://*)
      case ${2#*://} in
        127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) tracker_proto='=http' ;;
        127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) tracker_proto='=http' ;;
      esac
      ;;
  esac
  if [ -z "$tracker_proto" ]; then
    say "$1 is not https"
    exit 1
  fi
}

lapilli=${1:-${LAPILLI_URL:-}}
# Lapilli's token goes where LAPILLI_URL points, serve's port: https, or
# http as a Service in the cluster is reached, and curl speaks no other
# scheme (lapilli_proto).
lapilli_proto=
case $lapilli in
  *@*)
    say "LAPILLI_URL holds a user or token before @, refused"
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://?*) lapilli_proto='=https' ;;
  [Hh][Tt][Tt][Pp]://?*) lapilli_proto='=http' ;;
esac
if [ -z "$lapilli_proto" ]; then
  say "LAPILLI_URL is where --incidents-listen is, such as http://lapilli:8082"
  exit 1
fi
lapilli=${lapilli%/}
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --tasks-token-file}"
lapilli_token=$(secret_line LAPILLI_TOKEN_FILE "$LAPILLI_TOKEN_FILE")
close=${CLOSE_ISSUES:-false}
max_issues=${MAX_ISSUES:-50}
max_pages=${MAX_PAGES:-20}
sync_days=${SYNC_DAYS:-30}
number MAX_ISSUES "$max_issues"
number MAX_PAGES "$max_pages"
number SYNC_DAYS "$sync_days"
comments=${COMMENTS:-false}
comment_names=${COMMENT_NAMES:-false}
max_comments=${MAX_COMMENTS:-200}
number MAX_COMMENTS "$max_comments"
case $comments in
  true | false) ;;
  *)
    say "COMMENTS is true or false"
    exit 1
    ;;
esac
if [ "${#sync_days}" -gt 4 ] || [ "$sync_days" -gt 3650 ]; then
  say "SYNC_DAYS is 3650 at most"
  exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM HUP

# jq's epoch: a time as trackers write it (with or without fractions, Z or
# an offset) in Unix seconds.
epoch='def epoch: sub("\\.[0-9]+"; "") |
  if test("Z$") then fromdateiso8601
  else capture("^(?<t>.*T[0-9:]{8})(?<s>[+-])(?<h>[0-9]{2}):?(?<m>[0-9]{2})$") |
    ((.t + "Z") | fromdateiso8601) - ((if .s == "+" then 1 else -1 end) * ((.h | tonumber) * 3600 + (.m | tonumber) * 60))
  end;'

# issue_text: the task on stdin as an issue's text, in Markdown, ending with
# the marker its issue is found again by: a line of its own, always after
# the task's text, so nothing in that text comes after it. The rule's, the
# incident's, the team's and the assignee's names are each a code span: a
# name chosen by whoever writes a rule's source, or an incident's, mentions
# no one and shows no link or image in the issue.
issue_text() {
  jq -r 'def code: gsub("`"; "'"'"'") | gsub("[\r\n]"; " ") | "`" + . + "`";
    [.body // empty, "",
    (if .rule then "Rule: \(.rule | code)" else empty end),
    (if .incident then "Incident: \(.incident | code)" else empty end),
    (if .team then "Team: \(.team | code)" else empty end),
    (if .assignee then "Assignee: \(.assignee | code)" else empty end),
    (if .due then "Due: \(.due[0:10])" else empty end),
    (if .url then "In Lapilli: \(.url)" else empty end),
    "", "Kept in step with Lapilli: lapilli-task-\(.id)"] | join("\n")'
}

# jq's kept($m): whether an issue's text (.) is marked for $m as issue_text
# marks it: its last line starting "Kept in step with Lapilli:" is the one
# for $m, exactly. One the task's own text holds comes before the recipe's,
# so it is never the last; lines people add below it in the tracker are
# left out.
# shellcheck disable=SC2016,SC2034 # $m is jq's; the recipes use kept
kept='def kept($m): [. // "" | split("\n")[] | rtrimstr("\r") | select(startswith("Kept in step with Lapilli:"))]
  | last == "Kept in step with Lapilli: " + $m;'

# fetch CURL-ARGUMENTS: the tracker's answer, taken whole before it is
# read, for a 2xx; anything else is said and fails, its HTTP status kept
# for gone.
fetch() {
  : >"$work/answer"
  if ! code=$(curl -q -sS --proto "${tracker_proto:?tracker_api says where the tracker is}" --max-time 30 --max-filesize 16777216 --retry 3 \
    -o "$work/answer" -w '%{http_code}' "$@"); then
    code=000
  fi
  printf '%s\n' "$code" >"$work/status"
  case $code in
    2??) cat "$work/answer" ;;
    *)
      say "the tracker answered $code: $(head -c 300 "$work/answer" | jq -Rsc .)"
      return 1
      ;;
  esac
}

# gone: whether the tracker's last answer said what was asked for isn't
# there: 404, or GitHub's 410 for an issue deleted.
gone() {
  case $(cat "$work/status" 2>/dev/null) in
    404 | 410) return 0 ;;
  esac
  return 1
}

# lapilli_curl CURL-ARGUMENTS: curl to Lapilli: its token on stdin, not the
# command line, where ps would show it; LAPILLI_URL's scheme alone; and an
# answer of 16 MiB at most.
lapilli_curl() {
  printf 'Authorization: Bearer %s\n' "$lapilli_token" |
    curl -q --proto "$lapilli_proto" --max-time 30 --max-filesize 16777216 --retry 3 -H @- "$@"
}

# lapilli_tasks QUERY FILE: every task the query finds, a JSON object a line.
lapilli_tasks() {
  after=0
  pages=0
  : >"$2"
  while :; do
    pages=$((pages + 1))
    if [ "$pages" -gt "$max_pages" ]; then
      say "more than $max_pages pages of tasks; stopping (raise MAX_PAGES)"
      exit 1
    fi
    if ! lapilli_curl -fsS -o "$work/page" "$lapilli/api/v1/tasks?$1&after=$after"; then
      say "Lapilli didn't give the tasks"
      exit 1
    fi
    if ! jq -e '.tasks | type == "array"' "$work/page" >/dev/null 2>&1; then
      say "Lapilli's answer isn't a page of tasks"
      exit 1
    fi
    jq -c '.tasks[]' "$work/page" >>"$2"
    next=$(jq -r '.next // empty' "$work/page")
    [ -n "$next" ] || return 0
    number "the next page" "$next"
    if [ "$next" -le "$after" ]; then
      say "Lapilli gave the same page again; stopping"
      exit 1
    fi
    after=$next
  done
}

# lapilli_patch ID JSON: set a task's link or status.
lapilli_patch() {
  lapilli_curl -fsS -X PATCH -H 'Content-Type: application/json' \
    --data-binary "$2" -o /dev/null "$lapilli/api/v1/tasks/$1"
}

# lapilli_comments ID FILE: a task's comments, as Lapilli has them.
lapilli_comments() {
  if ! lapilli_curl -fsS -o "$2" "$lapilli/api/v1/tasks/$1/comments"; then
    return 1
  fi
  jq -e '.comments | type == "array"' "$2" >/dev/null 2>&1
}

# lapilli_copy ID JSON: a comment from the issue, copied to the task. It
# prints Lapilli's HTTP status: 2xx taken, 4xx refused, anything else
# failed; what Lapilli said is in $work/copied: whether it was kept (not
# when the task keeps as many as it does), and as sent.
lapilli_copy() {
  : >"$work/copied"
  lapilli_curl -sS -X POST -H 'Content-Type: application/json' \
    --data-binary "$2" -o "$work/copied" -w '%{http_code}' "$lapilli/api/v1/tasks/$1/comments" || printf '000'
}

# lapilli_mark ID COMMENT REF: note that a comment people wrote was posted to
# the task's issue as REF, so it is never posted again.
lapilli_mark() {
  lapilli_curl -fsS -X PATCH -H 'Content-Type: application/json' \
    --data-binary "$(jq -cn --arg r "$3" '{posted: $r}')" -o /dev/null "$lapilli/api/v1/tasks/$1/comments/$2"
}

# comment_text: a comment people wrote in Lapilli, on stdin, as the issue's
# comment's text: the comment quoted line by line (so nothing in it starts
# a line of its own: no quick action, no heading), and the line it is found
# again by, always last; who wrote it only with COMMENT_NAMES=true, since
# the sign-in proxy's name for them is often their email, and the issue
# may be public.
comment_text() {
  jq -r --arg names "$comment_names" '[(if $names == "true" then "\(.by // "someone" | gsub("[\r\n]"; " ")) wrote in Lapilli:" else "Written in Lapilli:" end), "",
    (.body | split("\n") | map("> " + rtrimstr("\r")) | join("\n")),
    "", "Kept in step with Lapilli: lapilli-comment-\(.id)"] | join("\n")'
}

# copy_text: a comment from the tracker, on stdin, as Lapilli takes it: one
# with no text (an image, a table) says so, and one longer than Lapilli
# keeps is cut, and says where the rest is.
# shellcheck disable=SC2016 # jq's
copy_text='def copy_text: if (gsub("[\\s]"; "") == "") then "(no text here: see the issue)"
  elif length > 10000 then .[0:9900] + "\n\n(cut here: the rest is in the issue)" else . end;'


# sync_comments ID REF: the task's comments and its issue's, each way, once.
sync_comments() {
  if ! lapilli_comments "$1" "$work/lc"; then
    fail "Lapilli didn't give task $1's comments; skipped"
    return 0
  fi
  rm -f "$work/capped"
  if ! t_comments "$2" >"$work/tc"; then
    fail "the tracker didn't give the comments of task $1's issue; skipped"
    return 0
  fi
  # What the tracker said is checked before any of it goes on.
  if ! jq -e -s 'all(.[]; (.id | type == "string" and test("\\A[A-Za-z0-9._:-]{1,100}\\z"))
      and (.mine | type == "boolean") and (.at | type == "string") and (.body | type == "string")
      and (.author | type == "string"))' "$work/tc" >/dev/null; then
    say "the tracker's comments on task $1's issue aren't as they should be; stopping"
    exit 1
  fi
  # Lapilli to the tracker: people's comments Lapilli doesn't know were
  # posted (it keeps the tracker's id for each, ref). One this job posted
  # but couldn't note is found among its own by the line it ends with,
  # and noted now. What Lapilli knows is never inferred from a read of the
  # tracker's comments, which can be pushed past what is read: with more
  # comments on the issue than are read, nothing is posted to it.
  if [ -e "$work/capped" ]; then
    say "task $1's issue has more comments than are read: nothing is posted to it this run"
  else
    # shellcheck disable=SC2016 # $kept and $m are jq's
    jq -c --slurpfile tc "$work/tc" "$kept"'[$tc[] | select(.mine)] as $ours
      | .comments[] | select((.origin // "") == "" and (.ref // "") == "")
      | ("lapilli-comment-\(.id)") as $m
      | . + {found: ([$ours[] | select(.body | kept($m)) | .id][0] // "")}' "$work/lc" | head -n "$max_comments" >"$work/out"
    while IFS= read -r c; do
      cid=$(printf '%s' "$c" | jq -r '.id')
      number "a comment's id" "$cid"
      cref=$(printf '%s' "$c" | jq -r '.found')
      if [ -z "$cref" ]; then
        printf '%s' "$c" | comment_text >"$work/ctext"
        if ! cref=$(t_comment "$2" "$work/ctext"); then
          fail "the tracker didn't take a comment on task $1's issue; skipped"
          return 0
        fi
        posted=$((posted + 1))
      fi
      if ! printf '%s' "$cref" | grep -Eq '^[A-Za-z0-9._:-]{1,100}$'; then
        say "the tracker didn't say the id of the comment posted on task $1's issue: it is found again by its last line"
        continue
      fi
      if ! lapilli_mark "$1" "$cid" "$cref"; then
        fail "Lapilli didn't note comment $cid as posted; skipped"
        return 0
      fi
    done <"$work/out"
  fi
  # The tracker to Lapilli: comments not this job's whose ids no comment
  # on the task has (copied in, or posted from it), MAX_COMMENTS of them,
  # counted after those are left out. One with no text, or too long, is
  # made to fit; Lapilli keeps a text or a name that doesn't show as what
  # does, under the comment's id, so it isn't sent again ahead of the
  # rest. One Lapilli still refuses is said and passed over, and doesn't
  # hold up the others or fail the run. Once the task keeps as many as
  # it does, Lapilli keeps no more: nothing more is sent for it.
  # shellcheck disable=SC2016 # $copy_text is jq's
  jq -c --slurpfile lc "$work/lc" "$copy_text"'[$lc[0].comments[] | .ref // "" | select(. != "")] as $have
    | select(.mine | not) | select(.id as $i | $have | index($i) | not)
    | {text: (.body | copy_text), author: .author, ref: .id, at: .at}' "$work/tc" | head -n "$max_comments" >"$work/in"
  while IFS= read -r c; do
    code=$(lapilli_copy "$1" "$c")
    case $code in
      2??)
        if ! jq -e '.kept == true' "$work/copied" >/dev/null 2>&1; then
          say "task $1 keeps as many comments copied from its issue as a task does: no more are copied to it"
          break
        fi
        copied=$((copied + 1))
        if ! jq -e '.as_sent == true' "$work/copied" >/dev/null 2>&1; then
          altered=$((altered + 1))
        fi
        ;;
      4??)
        say "Lapilli refused comment $(printf '%s' "$c" | jq -r '.ref') on task $1's issue ($code); passed over"
        refused=$((refused + 1))
        ;;
      *)
        fail "a comment on task $1's issue couldn't be copied ($code); skipped"
        return 0
        ;;
    esac
  done <"$work/in"
}

# fail WHAT: say what failed for one task, and count it.
fail() {
  say "$*"
  failed=$((failed + 1))
}

# whoami: who the tracker's token is, in $me, asked the first time it is
# needed: an id, or a login (a GitHub App's is NAME[bot]), on one line.
me=
whoami() {
  [ -z "$me" ] || return 0
  if ! me=$(t_me) || ! jq -en --arg me "$me" '$me | test("\\A[A-Za-z0-9:_.-]{1,128}(\\[bot\\])?\\z")' >/dev/null; then
    say "the tracker didn't say who its token is"
    exit 1
  fi
}

# sync_tasks: an issue for each open task without one, its link back; then
# each linked task checked against its issue, by who set the task's status
# and when (Lapilli's status_by and status_at: a later edit of anything
# else, the job's link among them, doesn't count):
#
#   - open or doing, its issue closed since the task's status was set: the
#     task is done or dropped, as the issue was closed;
#   - done or dropped by this job, because its issue was, the issue open
#     again: the task is open again;
#   - done or dropped by someone in Lapilli, the issue open and unchanged
#     since: with CLOSE_ISSUES=true, the issue is closed. Who set it not
#     known (the changes saying so were pruned) is someone, not this job.
#
# Anything else is left as it is: a task reopened in Lapilli doesn't reopen
# its issue, and an issue someone is still at isn't closed under them. An
# issue deleted in the tracker is said, and its task left as it is: there
# is nothing to follow or close.
#
# What fails for one task (an issue the tracker won't make, or can't give,
# or close; a write Lapilli refuses) is said and skipped, and the rest are
# done; the run then fails. The tracker unreachable, or Lapilli's answer
# not to be trusted, stops it there.
sync_tasks() {
  made=0
  linked=0
  moved=0
  closed=0
  alone=0
  failed=0
  posted=0
  copied=0
  altered=0
  refused=0
  lapilli_tasks "linked=false&status=open,doing" "$work/new"
  while IFS= read -r task; do
    id=$(printf '%s' "$task" | jq -r '.id')
    number "a task's id" "$id"
    whoami
    # Found again by its marker, among the issues this job made: one made
    # in a run that couldn't put its link back isn't made twice.
    if ! url=$(t_find "lapilli-task-$id"); then
      say "the tracker couldn't be searched for task $id's issue"
      exit 1
    fi
    if [ -z "$url" ]; then
      if [ "$made" -ge "$max_issues" ]; then
        say "made $max_issues issues (MAX_ISSUES); the rest wait for the next run"
        break
      fi
      printf '%s' "$task" >"$work/task"
      if ! url=$(t_create "$work/task") || [ -z "$url" ]; then
        fail "the tracker didn't make an issue for task $id; skipped"
        continue
      fi
      made=$((made + 1))
    fi
    if [ -z "$(t_ref "$url")" ]; then
      say "the tracker gave an issue link that isn't one of its own: $(printf '%s' "$url" | jq -Rc .)"
      exit 1
    fi
    if ! lapilli_patch "$id" "$(jq -cn --arg l "$url" '{link: $l}')"; then
      fail "task $id's link couldn't be written; skipped"
      continue
    fi
    linked=$((linked + 1))
  done <"$work/new"

  # Those done or dropped only while lately changed: Lapilli leaves out
  # the rest, so however many were closed before, they aren't paged through.
  lapilli_tasks "linked=true&status=open,doing" "$work/open"
  since=$(jq -nr --arg d "$sync_days" 'now - ($d | tonumber) * 86400 | floor | todate')
  lapilli_tasks "linked=true&status=done,dropped&updated_after=$since" "$work/closed"
  cat "$work/open" "$work/closed" >"$work/linked"
  while IFS= read -r task; do
    id=$(printf '%s' "$task" | jq -r '.id')
    number "a task's id" "$id"
    status=$(printf '%s' "$task" | jq -r '.status')
    # Who set its status, and when; when the changes saying so were
    # pruned, no one known, at its last change.
    set_by=$(printf '%s' "$task" | jq -r '.status_by // empty')
    set_at=$(printf '%s' "$task" | jq -r "$epoch"' (.status_at // .updated) | epoch')
    number "a task's time" "$set_at"
    ref=$(t_ref "$(printf '%s' "$task" | jq -r '.link')")
    # Another tracker's issue, or a link written by hand: left alone.
    [ -n "$ref" ] || continue
    whoami
    if ! state=$(t_state "$ref" "lapilli-task-$id"); then
      fail "the tracker didn't give task $id's issue; skipped"
      continue
    fi
    if [ -z "$state" ]; then
      # Someone else's issue, or this job's for another task: linked by
      # hand, it is neither followed nor closed.
      say "task $id's issue isn't one this job made for it; left alone"
      alone=$((alone + 1))
      continue
    fi
    if [ "$state" = gone ]; then
      # Deleted in the tracker: said each run, and not a failure. A person
      # clearing the task's link in Lapilli has a new issue made.
      say "task $id's issue is gone from the tracker; left alone"
      alone=$((alone + 1))
      continue
    fi
    was=${state%% *}
    rest=${state#* }
    when=${rest%% *}
    changed=${rest#* }
    number "an issue's time" "$when"
    number "an issue's time" "$changed"
    if [ "$comments" = true ]; then
      sync_comments "$id" "$ref"
    fi
    case $status:$was in
      open:done | open:dropped | doing:done | doing:dropped)
        # Closed since the task's status was set: the task follows. By when
        # its state changed, not when it last changed: a comment on a
        # closed issue, this job's own among them, doesn't close it again.
        [ "$changed" -ge "$set_at" ] || continue
        if ! lapilli_patch "$id" "$(jq -cn --arg s "$was" '{status: $s}')"; then
          fail "task $id's status couldn't be written; skipped"
          continue
        fi
        moved=$((moved + 1))
        ;;
      done:open | dropped:open)
        if [ "$set_by" = tasks-token ]; then
          # Closed here because its issue was, and the issue is open again:
          # reopened, so the task is too.
          if ! lapilli_patch "$id" '{"status":"open"}'; then
            fail "task $id's status couldn't be written; skipped"
            continue
          fi
          moved=$((moved + 1))
        elif [ "$close" = true ] && [ "$when" -le "$set_at" ]; then
          # Done or dropped in Lapilli, and the issue unchanged since. One
          # changed since is left open: someone is still at it.
          # In a subshell, as the others are: what it sets is its own.
          if ! (t_close "$ref" "$status"); then
            fail "the tracker didn't close task $id's issue; skipped"
            continue
          fi
          closed=$((closed + 1))
        fi
        ;;
    esac
  done <"$work/linked"
  if [ "$comments" = true ]; then
    say "posted $posted comments to issues, copied $copied comments to tasks ($altered with a text or a name that doesn't show kept as what does), passed over $refused Lapilli refused"
  fi
  say "made $made issues, linked $linked tasks, moved $moved tasks, closed $closed issues, left $alone alone"
  if [ "$failed" -gt 0 ]; then
    say "$failed failed (above)"
    exit 1
  fi
}
