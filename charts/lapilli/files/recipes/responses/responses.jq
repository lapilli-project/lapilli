# What the scripts here share to write a page's response as Lapilli takes it
# (docs/design/responses.md, "What comes in"): {id, alerts, sent}, and
# acknowledged when a person acknowledged the page, and resolved with
# resolved_by (alert or person) once it is resolved. Read with include
# "responses"; and jq -L DIR. The alerts are written with alerts.jq, in
# ../incidents, which the incidents scripts share.
#
# No person is named: only whether one acted. A name or an id of a person
# is never copied into what a script writes.

# epoch: an RFC 3339 time as seconds since 1970, a fraction of a second
# kept, or null for anything else, a date that isn't one (2026-13-45)
# among them.
def epoch:
  if type != "string" then null
  else
    (capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z") // null) as $m
    | if $m == null then null
      else (($m.d + "Z") | try fromdateiso8601 catch null) as $b
        | if $b == null then null
          else $b + (("0" + ($m.f // "")) | tonumber)
            - (if $m.z == "Z" then 0
               else (if $m.z[0:1] == "-" then -1 else 1 end) * (($m.z[1:3] | tonumber) * 3600 + ($m.z[4:6] | tonumber) * 60) end)
          end
      end
  end;

# time: a time as it was written when it reads as one, else null.
def time: if epoch == null then null else . end;

# earliest, latest: of a list of times, the first or last in time as it was
# written, or null when there is none.
def earliest: [.[] | select(epoch != null)] | if length == 0 then null else min_by(epoch) end;
def latest: [.[] | select(epoch != null)] | if length == 0 then null else max_by(epoch) end;

# id: a page's id as Lapilli takes one: 256 bytes at most, of letters,
# digits and - _ . :
def id_ok: type == "string" and test("\\A[A-Za-z0-9_.:-]{1,256}\\z");

# response($id; $alerts; $sent; $ack; $at; $by): the page as Lapilli takes
# it, or null when it has no id it takes, no alert or no time sent.
# $ack is when a person acknowledged it, or null; $at is when it resolved
# and $by is alert or person, or null: both are written or neither.
def response($id; $alerts; $sent; $ack; $at; $by):
  if ($id | id_ok | not) or ($alerts | length) == 0 or ($sent | epoch) == null then null
  else {id: $id, alerts: $alerts, sent: $sent}
    + (if ($ack | epoch) != null then {acknowledged: $ack} else {} end)
    + (if ($at | epoch) != null and ($by | IN("alert", "person")) then {resolved: $at, resolved_by: $by} else {} end)
  end;

# in_period($from; $to): whether a time is in the period, from on and
# before to, both in seconds.
def in_period($from; $to): epoch as $e | $e != null and $e >= $from and $e < $to;

# written: the responses once each by id, in the order they were sent.
def written: map(select(. != null)) | unique_by(.id) | sort_by(.sent | epoch);
