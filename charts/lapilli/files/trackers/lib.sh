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
#   t_state REF MARKER  print STATE UPDATED: STATE open, done or dropped, and
#                       when the issue last changed, in Unix seconds; nothing
#                       when the issue isn't one this job made for MARKER
#                       ($me wrote it, marked for MARKER as t_find says, and
#                       it is in the project or team the recipe is for);
#                       gone when the tracker says it isn't there (deleted)
#                       and gives the project or team itself
#   t_close REF STATUS  close the issue, as done or dropped
#
# Anyone may write lapilli-task-ID in an issue, and an editor in Lapilli
# may link a task to any issue: an issue someone else wrote is never taken
# for a task's, followed or closed. Nor is the job's issue for one task
# taken for another's because the first task's text names the second's
# marker: the text is the task's (an editor's, or an agent's proposal's),
# and only the line the recipe writes after it counts.
#
# Lapilli's side, on the port --incidents-listen opens:
#
#   LAPILLI_URL         such as http://lapilli:8082 (or the first argument)
#   LAPILLI_TOKEN_FILE  a file holding the token in --tasks-token-file
#   CLOSE_ISSUES        true to close an issue whose task was done or dropped
#                       in Lapilli (default false)
#   MAX_ISSUES          issues made in one run, at most (default 50)
#   MAX_PAGES           of 200 tasks, at most (default 20)
#   SYNC_DAYS           a task done or dropped, and not changed for this
#                       long, isn't checked against its issue any more
#                       (default 30, at most 3650); Lapilli leaves it out
#                       of what it gives, so they aren't paged through

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

lapilli=${1:-${LAPILLI_URL:-}}
case $lapilli in
  http://* | https://*) ;;
  *)
    say "LAPILLI_URL is where --incidents-listen is, such as http://lapilli:8082"
    exit 1
    ;;
esac
lapilli=${lapilli%/}
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --tasks-token-file}"
lapilli_token=$(cat "$LAPILLI_TOKEN_FILE")
close=${CLOSE_ISSUES:-false}
max_issues=${MAX_ISSUES:-50}
max_pages=${MAX_PAGES:-20}
sync_days=${SYNC_DAYS:-30}
number MAX_ISSUES "$max_issues"
number MAX_PAGES "$max_pages"
number SYNC_DAYS "$sync_days"
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
# the task's text, so nothing in that text comes after it.
issue_text() {
  jq -r '[.body // empty, "",
    (if .rule then "Rule: \(.rule)" else empty end),
    (if .incident then "Incident: \(.incident)" else empty end),
    (if .team then "Team: \(.team)" else empty end),
    (if .assignee then "Assignee: \(.assignee)" else empty end),
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
  if ! code=$(curl -sS --max-time 30 --retry 3 -o "$work/answer" -w '%{http_code}' "$@"); then
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
    if ! printf 'Authorization: Bearer %s\n' "$lapilli_token" |
      curl -fsS --max-time 30 --retry 3 -H @- -o "$work/page" "$lapilli/api/v1/tasks?$1&after=$after"; then
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
  printf 'Authorization: Bearer %s\n' "$lapilli_token" |
    curl -fsS --max-time 30 --retry 3 -X PATCH -H @- -H 'Content-Type: application/json' \
      --data-binary "$2" -o /dev/null "$lapilli/api/v1/tasks/$1"
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
    when=${state#* }
    number "an issue's time" "$when"
    case $status:$was in
      open:done | open:dropped | doing:done | doing:dropped)
        # Closed since the task's status was set: the task follows.
        [ "$when" -ge "$set_at" ] || continue
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
  say "made $made issues, linked $linked tasks, moved $moved tasks, closed $closed issues, left $alone alone"
  if [ "$failed" -gt 0 ]; then
    say "$failed failed (above)"
    exit 1
  fi
}
