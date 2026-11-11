# A page of a Compass alert's log, as GET /v1/alerts/{id}/logs gives it, to
# when the alert was closed: the latest entry that says so, as seconds since
# 1970, or nothing. Compass doesn't document what a close says; Opsgenie,
# whose alerts Compass's are, writes "Alert closed by ..." and "Alert closed
# via ...". So an entry of type close, or whose text starts "Alert closed",
# is a close. Change it here if yours say otherwise. $since is the latest
# close found on the pages before, or "".
def epoch:
  capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end);
def closes:
  ((.logType // "") | ascii_downcase) == "close"
  or ((.log // "") | test("\\A\\s*alert (is )?closed\\b"; "i"));
[(.values // [])[] | select(closes) | .logTime | strings | epoch]
+ (if $since == "" then [] else [$since | tonumber] end)
| max // empty
