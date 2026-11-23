#!/bin/sh
# Sends what Alertmanager's event recorder wrote to a file, or to Kafka, on
# to lapilli serve's events path, as its webhook output would have posted
# it (docs/design/inputs.md, "Every delivery, from Alertmanager's event
# recorder"). It resumes where it stopped, from an offset file.
#
#   alertmanager.sh file PATH   the recorder's file output (file_outputs'
#                               path): one event a line, appended. Its
#                               offset is a byte offset into the file;
#                               a file rotated (renamed, as logrotate does,
#                               to PATH.1 or the like) is read to its end
#                               first, and one truncated in place is read
#                               again from its start
#   alertmanager.sh kafka       the recorder's Kafka output, consumed with
#                               kcat: KAFKA_BROKERS, KAFKA_TOPIC. With
#                               KAFKA_GROUP, as a member of that consumer
#                               group, the offsets Kafka keeps for it;
#                               without, the offsets kept in OFFSET_FILE
#
#   LAPILLI_URL         where serve's notifications port is, such as
#                       http://lapilli-notify:8083: http or https, with no
#                       user before @
#   LAPILLI_TOKEN_FILE  a file holding --notify-token-file's token, on one line
#   OFFSET_FILE         where it keeps where it stopped (default PATH.offset
#                       for a file; required for Kafka without KAFKA_GROUP)
#   BATCH               events a request, 1 to 1000 (default 100): serve
#                       takes 1,000 and 1 MB at most
#   SKIP                receivers to pass over, comma-separated, as serve's
#                       ?skip= (Lapilli's own webhook receiver, kept beside)
#   FOLLOW              true to keep reading, every POLL seconds (default
#                       false: read to the end, send, and stop)
#   POLL                seconds between reads when following (default 5)
#   RETRIES             how often a request serve answers 429 or 5xx is
#                       tried again, as Retry-After says (default 8)
#   KAFKA_START         where a partition with no offset kept begins:
#                       beginning, end, or the default, an hour ago, which is
#                       as far back as serve takes an event's own time
#   KCAT_CONFIG         a file of librdkafka's settings, as kcat -F reads
#                       one (security.protocol, sasl.*)
#
# What it needs: sh, jq, curl; kcat for Kafka. It only reads the file, or
# the topic. An event is sent at least once: one sent again is the same
# delivery to serve, kept once. serve takes an event's own time when it is
# within the hour before it arrives, and the time it arrives otherwise, so
# run it often: every minute, or FOLLOW=true beside Alertmanager.
#
# The token goes where LAPILLI_URL points. Over plain http it is read by
# whatever is on the way: keep that to the cluster serve runs in.
set -eu
say() { printf 'alertmanager.sh: %s\n' "$*" >&2; }
mode=${1:-}
case $mode in
  file) path=${2:?the file the recorder writes, as its file_outputs path} ;;
  kafka) ;;
  *)
    say "say where the events are: file PATH, or kafka"
    exit 2
    ;;
esac
: "${LAPILLI_URL:?where the notifications port of serve is, such as http://lapilli-notify:8083}"
: "${LAPILLI_TOKEN_FILE:?a file holding the token of --notify-token-file}"
BATCH=${BATCH:-100}
FOLLOW=${FOLLOW:-false}
POLL=${POLL:-5}
RETRIES=${RETRIES:-8}
case $RETRIES in *[!0-9]* | '') say "RETRIES is a number"; exit 2 ;; esac
case $BATCH in *[!0-9]* | '' | 0*) BATCH=x ;; esac
if [ "$BATCH" = x ] || [ "$BATCH" -gt 1000 ]; then
  say "BATCH is a number of events, 1 to 1000"
  exit 2
fi
case $POLL in *[!0-9]* | '' | 0) say "POLL is a number of seconds"; exit 2 ;; esac
case $FOLLOW in true | false) ;; *) say "FOLLOW is true or false"; exit 2 ;; esac
# A request's body is 1 MB at most: what is sent is held under it, the
# brackets and commas of a batch counted.
maxbody=1000000
# How much of the file is read at a time.
maxread=16777216

# The token goes where the URL points, serve's port: https, or http as a
# Service in the cluster is reached, and curl speaks no other scheme
# (proto). The address isn't said: a user and token before @ would be in
# the log.
proto=
case $LAPILLI_URL in
  *@*)
    say "LAPILLI_URL holds a user or token before @, refused"
    exit 2
    ;;
  [Hh][Tt][Tt][Pp][Ss]://?*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://?*) proto='=http' ;;
esac
if [ -z "$proto" ]; then
  say "LAPILLI_URL is not http or https: want where serve's notifications port is, such as http://lapilli-notify:8083"
  exit 2
fi
case $LAPILLI_URL in *\?* | *\#*) say "LAPILLI_URL has a query or an anchor: want the port's address alone"; exit 2 ;; esac
target="${LAPILLI_URL%/}/v1/alertmanager/events"
# ?skip=, each a receiver's name as serve takes one; 20 at most.
if [ -n "${SKIP:-}" ]; then
  q=$(printf '%s' "$SKIP" | jq -Rr 'split(",") | map(select(length > 0)) | if length > 20 or any(test("^[A-Za-z0-9_.:-]{1,200}$") | not) then error("bad") else map("skip=" + @uri) | join("&") end' 2>/dev/null) || {
    say "SKIP is receivers' names, comma-separated, 20 at most"
    exit 2
  }
  [ -z "$q" ] || target="$target?$q"
fi

# secret NAME FILE: what FILE holds, one line, as put.sh reads its token:
# no control character, spaces after it dropped. Neither the token nor
# what else the file holds is said.
secret() {
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
      exit 2
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret LAPILLI_TOKEN_FILE "$LAPILLI_TOKEN_FILE")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM HUP

# post FILE: the events in FILE, one a line, as one JSON array. 0 when
# serve took them, or refused them as events it can't read (they are
# passed over, and said: sent again, they would be refused again); 1
# otherwise, and the offset isn't moved. A line that isn't a JSON object
# isn't an event: it is left out, and counted.
post() {
  jq -cRs '[split("\n")[] | select(length > 0) | (fromjson? // null) | select(type == "object")]' "$1" >"$work/body" 2>/dev/null || {
    say "the events couldn't be read"
    return 1
  }
  lines=$(grep -c . "$1" || true)
  events=$(jq length "$work/body")
  [ "$events" -eq "$lines" ] || say "$((lines - events)) lines that aren't an event left out (the recorder's Kafka output writes JSON with format: json)"
  [ "$events" -gt 0 ] || return 0
  # curl tries again on 429 and 503, after what Retry-After says.
  code=$(printf 'Authorization: Bearer %s\n' "$token" |
    curl -q -sS --proto "$proto" --max-time 60 --max-filesize 1048576 --retry "$RETRIES" --retry-max-time 300 -X POST -H @- \
      -H 'Content-Type: application/json' --data-binary "@$work/body" -o "$work/answer" -w '%{http_code}' "$target") || code=failed
  case $code in
    2??)
      # Numbers alone: nothing the server says acts on a terminal.
      say "$events events sent, $(jq -r '(.kept // 0) | tonumber' "$work/answer" 2>/dev/null || echo 0) deliveries kept"
      return 0
      ;;
    400)
      say "$events events serve can't read, passed over: $(jq -Rsc '.[0:300]' "$work/answer")"
      return 0
      ;;
    *)
      say "the events weren't sent ($code): $(jq -Rsc '.[0:300]' "$work/answer" 2>/dev/null || echo '""')"
      return 1
      ;;
  esac
}

# batches FILE: FILE's lines, BATCH at most and under maxbody bytes in all,
# into $work/batch.N, and, in $work/ends, each batch's name and how many
# bytes of FILE it ends at. A line longer than a request isn't sent: it is
# counted in ends' last field.
batches() {
  rm -f "$work"/batch.* "$work/ends"
  LC_ALL=C awk -v max="$BATCH" -v bytes="$maxbody" -v dir="$work" '
    function flush() {
      if (n > 0 || long > 0) {
        if (n > 0) close(f)
        print (n > 0 ? f : "-") " " off " " long > (dir "/ends")
      }
      b++; f = dir "/batch." b; n = 0; size = 2; long = 0
    }
    BEGIN { b = 0; f = dir "/batch.0"; size = 2 }
    {
      len = length($0) + 1
      if (len + 1 > bytes) { off += len; long++; next }
      if (n == max || size + len > bytes) flush()
      print > f
      n++; size += len; off += len
    }
    END { flush() }
  ' "$1"
  touch "$work/ends"
}

# inode PATH: the file's inode number. ls -i is how POSIX gives it; the
# name isn't read back from what it writes.
# shellcheck disable=SC2012
inode() { ls -di "$1" | awk '{print $1}'; }

# head_sum PATH N: a sum of the file's first N bytes, which a file
# truncated and written again doesn't keep.
head_sum() { head -c "$2" "$1" | cksum | awk '{print $1 "-" $2}'; }

# send_from PATH FROM: the complete lines of PATH from byte FROM on, sent,
# a batch at a time; after each, kept (file_kept) as the offset. 0 when
# all were sent; 1 when a batch wasn't.
send_from() {
  tail -c +"$(($2 + 1))" "$1" | head -c "$maxread" >"$work/chunk"
  # Only whole lines: one the recorder is still writing waits.
  nl=$(tr -dc '\n' <"$work/chunk" | wc -c | tr -d ' ')
  [ "$nl" -gt 0 ] || return 0
  head -n "$nl" "$work/chunk" >"$work/lines"
  batches "$work/lines"
  while read -r b end long; do
    [ "$long" -eq 0 ] || say "$long events over a request's 1 MB, passed over"
    if [ "$b" != - ] && ! post "$b"; then
      return 1
    fi
    file_kept "$1" "$(($2 + end))"
  done <"$work/ends"
  # More than one read holds: read on.
  if [ "$(wc -c <"$work/chunk" | tr -d ' ')" -ge "$maxread" ]; then
    send_from "$1" "$(($2 + $(wc -c <"$work/lines" | tr -d ' ')))"
  fi
}

# file_kept PATH OFFSET: where it stopped, kept: the file's inode, the
# offset, and the sum of the first bytes up to 64 of it. Written aside and
# moved, so a stop midway leaves the last one whole.
file_kept() {
  n=$2
  [ "$n" -le 64 ] || n=64
  printf '%s %s %s\n' "$(inode "$1")" "$2" "$(head_sum "$1" "$n")" >"$OFFSET_FILE.tmp"
  mv "$OFFSET_FILE.tmp" "$OFFSET_FILE"
}

# file_once: what was written since the last time, sent.
file_once() {
  if [ ! -f "$path" ]; then
    say "no file yet: $(basename "$path")"
    return 0
  fi
  ino=
  off=0
  sum=
  if [ -s "$OFFSET_FILE" ]; then
    read -r ino off sum <"$OFFSET_FILE" || true
    case $off in *[!0-9]* | '') say "$(basename "$OFFSET_FILE") isn't an offset this wrote: read from the start"; ino='' off=0 ;; esac
  fi
  now=$(inode "$path")
  if [ -n "$ino" ] && [ "$ino" != "$now" ]; then
    # Rotated: the file it was reading, renamed beside it, is read to its
    # end first; then the new one from its start.
    for old in "$path".*; do
      [ -f "$old" ] && [ "$old" != "$OFFSET_FILE" ] && [ "$(inode "$old")" = "$ino" ] || continue
      send_old "$old" "$off" || return 1
      break
    done
    off=0
    sum=
    file_kept "$path" 0
  fi
  size=$(wc -c <"$path" | tr -d ' ')
  n=$off
  [ "$n" -le 64 ] || n=64
  if [ "$size" -lt "$off" ] || { [ -n "$sum" ] && [ "$(head_sum "$path" "$n")" != "$sum" ]; }; then
    # Truncated in place, or written again from its start.
    say "$(basename "$path") was truncated: read from its start"
    off=0
  fi
  send_from "$path" "$off"
}

# send_old PATH FROM: a rotated file's last lines, sent; where it stopped
# isn't kept, as the file is read to its end once.
send_old() {
  tail -c +"$(($2 + 1))" "$1" | head -c "$maxread" >"$work/chunk"
  nl=$(tr -dc '\n' <"$work/chunk" | wc -c | tr -d ' ')
  [ "$nl" -gt 0 ] || return 0
  head -n "$nl" "$work/chunk" >"$work/lines"
  batches "$work/lines"
  while read -r b _ long; do
    [ "$long" -eq 0 ] || say "$long events over a request's 1 MB, passed over"
    if [ "$b" != - ] && ! post "$b"; then
      return 1
    fi
  done <"$work/ends"
}

# run_kcat MODE ARGS: kcat in MODE (-C, -L), with the brokers and the
# settings file.
run_kcat() {
  m=$1
  shift
  if [ -n "${KCAT_CONFIG:-}" ]; then
    kcat "$m" -F "$KCAT_CONFIG" -b "$KAFKA_BROKERS" "$@"
  else
    kcat "$m" -b "$KAFKA_BROKERS" "$@"
  fi
}

# kafka_once: the topic's events since the offsets kept, sent.
kafka_once() {
  # KAFKA_START as kcat's -o: an hour ago, by default, in milliseconds.
  case ${KAFKA_START:-} in
    beginning | end) start=$KAFKA_START ;;
    '') start="s@$((($(date +%s) - 3600) * 1000))" ;;
    *) say "KAFKA_START is beginning or end, or unset for an hour ago"; exit 2 ;;
  esac
  if [ -n "${KAFKA_GROUP:-}" ]; then
    # A member of the group: Kafka keeps its offsets, as kcat commits them
    # once it has read the messages. One read and not yet sent when this
    # stops isn't read again. A group with no offsets yet begins at the
    # end, or where KAFKA_START says.
    reset=latest
    [ "${KAFKA_START:-}" != beginning ] || reset=earliest
    run_kcat -C -G "$KAFKA_GROUP" -X auto.offset.reset="$reset" -e -q -c 10000 -f '%s\n' "$KAFKA_TOPIC" >"$work/kafka" </dev/null || {
      say "kcat failed to read $KAFKA_TOPIC"
      return 1
    }
    batches "$work/kafka"
    while read -r b _ long; do
      [ "$long" -eq 0 ] || say "$long events over a request's 1 MB, passed over"
      if [ "$b" != - ] && ! post "$b"; then
        return 1
      fi
    done <"$work/ends"
    return 0
  fi
  # Offsets kept here, a line a partition: PARTITION NEXT.
  touch "$OFFSET_FILE"
  run_kcat -L -t "$KAFKA_TOPIC" -J >"$work/meta" </dev/null || {
    say "kcat failed to list $KAFKA_TOPIC's partitions"
    return 1
  }
  jq -r --arg t "$KAFKA_TOPIC" '.topics[] | select(.topic == $t) | .partitions[] | .partition | tonumber' "$work/meta" | head -n 1000 >"$work/partitions"
  [ -s "$work/partitions" ] || {
    say "$KAFKA_TOPIC has no partitions"
    return 1
  }
  while read -r p; do
    next=$(awk -v p="$p" '$1 == p && $2 ~ /^[0-9]+$/ {n = $2} END {print n}' "$OFFSET_FILE")
    from=${next:-$start}
    run_kcat -C -t "$KAFKA_TOPIC" -p "$p" -o "$from" -e -q -c 10000 -f '%o %s\n' >"$work/kafka" </dev/null || {
      say "kcat failed to read $KAFKA_TOPIC's partition $p"
      return 1
    }
    # Each line an offset and its message: the messages, and where each
    # line ends, for the offset to keep after each batch.
    LC_ALL=C awk -v offsets="$work/offsets" '$1 ~ /^[0-9]+$/ { o = $1; sub(/^[0-9]+ /, ""); print; print o > offsets }' "$work/kafka" >"$work/lines"
    [ -s "$work/lines" ] || continue
    batches "$work/lines"
    while read -r b end long; do
      [ "$long" -eq 0 ] || say "$long events over a request's 1 MB, passed over"
      if [ "$b" != - ] && ! post "$b"; then
        return 1
      fi
      # The message the batch ends at: the line whose end it is.
      upto=$(LC_ALL=C awk -v end="$end" '{ off += length($0) + 1; if (off == end) { print NR; exit } }' "$work/lines")
      last=$(sed -n "${upto}p" "$work/offsets")
      kafka_kept "$p" "$((last + 1))"
    done <"$work/ends"
  done <"$work/partitions"
}

# kafka_kept PARTITION NEXT: the offset to read the partition from next,
# kept beside the others, written aside and moved.
kafka_kept() {
  awk -v p="$1" '$1 != p' "$OFFSET_FILE" >"$OFFSET_FILE.tmp"
  printf '%s %s\n' "$1" "$2" >>"$OFFSET_FILE.tmp"
  mv "$OFFSET_FILE.tmp" "$OFFSET_FILE"
}

case $mode in
  file)
    OFFSET_FILE=${OFFSET_FILE:-$path.offset}
    ;;
  kafka)
    : "${KAFKA_BROKERS:?the brokers, host:port, comma-separated}"
    : "${KAFKA_TOPIC:?the topic the kafka_outputs of the recorder write to}"
    command -v kcat >/dev/null || {
      say "kcat isn't installed"
      exit 2
    }
    if [ -z "${KAFKA_GROUP:-}" ]; then
      : "${OFFSET_FILE:?where the offsets are kept, without KAFKA_GROUP}"
    fi
    case $KAFKA_BROKERS in *[!A-Za-z0-9.:,_[\]-]* | -*) say "KAFKA_BROKERS is host:port, comma-separated"; exit 2 ;; esac
    case $KAFKA_TOPIC in *[!A-Za-z0-9._-]* | '' | -*) say "KAFKA_TOPIC is a topic's name"; exit 2 ;; esac
    case ${KAFKA_GROUP:-x} in *[!A-Za-z0-9._-]* | -*) say "KAFKA_GROUP is a group's name"; exit 2 ;; esac
    ;;
esac

while :; do
  if [ "$mode" = file ]; then
    file_once || status=1
  else
    kafka_once || status=1
  fi
  if [ "$FOLLOW" != true ]; then
    exit "${status:-0}"
  fi
  status=
  sleep "$POLL"
done
