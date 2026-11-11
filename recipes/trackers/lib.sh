# shellcheck shell=sh
# What the tracker recipes do with Lapilli, alike for every tracker. A
# recipe sets name, defines these, then calls sync_tasks:
#
#   t_me                who the tracker's token is: an id of its account,
#                       which the others find in $me
#   t_find MARKER       the URL of an issue this job made whose text holds
#                       MARKER, or nothing: one $me wrote, labelled lapilli
#                       where the tracker has labels by name
#   t_create FILE       make an issue from the task in FILE (JSON); print its URL
#   t_ref URL           the issue's own reference (a number, a key) when URL
#                       is one of this tracker's issues; nothing when it isn't
#   t_state REF MARKER  print STATE UPDATED: STATE open, done or dropped, and
#                       when the issue last changed, in Unix seconds; nothing
#                       when the issue isn't one this job made for MARKER
#                       ($me wrote it, it holds MARKER, and it is in the
#                       project or team the recipe is for)
#   t_close REF STATUS  close the issue, as done or dropped
#
# Anyone may write lapilli-task-ID in an issue, and an editor in Lapilli
# may link a task to any issue: an issue someone else wrote is never taken
# for a task's, followed or closed.
#
# Lapilli's side, on the port --incidents-listen opens:
#
#   LAPILLI_URL         such as http://lapilli:8082 (or the first argument)
#   LAPILLI_TOKEN_FILE  a file holding the token in --tasks-token-file
#   CLOSE_ISSUES        true to close an issue whose task was done or dropped
#                       in Lapilli (default false)
#   MAX_ISSUES          issues made in one run, at most (default 50)
#   MAX_PAGES           of 200 tasks, at most (default 20)
#   SYNC_DAYS           a task done or dropped this long ago isn't checked
#                       against its issue any more (default 30)

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
# the marker its issue is found again by.
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
# needed.
me=
whoami() {
  [ -z "$me" ] || return 0
  if ! me=$(t_me) || ! printf '%s' "$me" | grep -Eq '^[A-Za-z0-9:_.-]{1,128}$'; then
    say "the tracker didn't say who its token is"
    exit 1
  fi
}

# sync_tasks: an issue for each open task without one, its link back; then
# each linked task checked against its issue:
#
#   - open or doing, its issue closed since the task last changed: the task
#     is done or dropped, as the issue was closed;
#   - done or dropped here because its issue was, the issue open again: the
#     task is open again;
#   - done or dropped by someone in Lapilli, the issue open and unchanged
#     since: with CLOSE_ISSUES=true, the issue is closed.
#
# Anything else is left as it is: a task reopened in Lapilli doesn't reopen
# its issue, and an issue someone is still at isn't closed under them.
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

  lapilli_tasks "linked=true&status=open,doing,done,dropped" "$work/linked"
  now=$(date +%s)
  while IFS= read -r task; do
    id=$(printf '%s' "$task" | jq -r '.id')
    number "a task's id" "$id"
    status=$(printf '%s' "$task" | jq -r '.status')
    updated=$(printf '%s' "$task" | jq -r "$epoch"' .updated | epoch')
    number "a task's time" "$updated"
    case $status in
      done | dropped) [ $((now - updated)) -le $((sync_days * 86400)) ] || continue ;;
    esac
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
    was=${state%% *}
    when=${state#* }
    number "an issue's time" "$when"
    case $status:$was in
      open:done | open:dropped | doing:done | doing:dropped)
        # Closed since the task last changed: the task follows.
        [ "$when" -ge "$updated" ] || continue
        if ! lapilli_patch "$id" "$(jq -cn --arg s "$was" '{status: $s}')"; then
          fail "task $id's status couldn't be written; skipped"
          continue
        fi
        moved=$((moved + 1))
        ;;
      done:open | dropped:open)
        if [ "$(printf '%s' "$task" | jq -r '.by // empty')" = tasks-token ]; then
          # Closed here because its issue was, and the issue is open again:
          # reopened, so the task is too.
          if ! lapilli_patch "$id" '{"status":"open"}'; then
            fail "task $id's status couldn't be written; skipped"
            continue
          fi
          moved=$((moved + 1))
        elif [ "$close" = true ] && [ "$when" -le "$updated" ]; then
          # Done or dropped in Lapilli, and the issue unchanged since. One
          # changed since is left open: someone is still at it.
          if ! t_close "$ref" "$status"; then
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
