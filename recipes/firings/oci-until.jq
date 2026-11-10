# An hour past $until, for oci.sh's query: OCI writes a transition down a few
# minutes after it is triggered, and the period is of when they were
# triggered.
def epoch: . as $t
  | (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:?[0-9]{2})$")
     // error("not a time: \($t)"))
  | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
    - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[-2:] | tonumber) * 60) end);
($until | epoch) + 3600 | floor | todate
