# Argo CD's Applications, and Flux's Kustomizations and HelmReleases, as
# kubectl get -o json or Argo CD's API gives them (one, or a List of them),
# to the revisions they deployed and when: "REVISION TIME" a line, the
# revision as git names a commit, the time in UTC to the second. What
# doesn't have that shape is passed over; git.sh checks the rest again.
#
# - An Argo CD Application: each entry of status.history, its revision
#   (revisions[$source] for one of several sources) and deployedAt.
# - A Flux Kustomization: each snapshot of status.history (Flux 2.7 on)
#   that wasn't a failed reconcile, its metadata.revision and
#   firstReconciled; before 2.7, its lastAppliedRevision, when its Ready
#   condition last turned true: the last reconcile's time, not the
#   revision's first.
# - A Flux HelmRelease: each snapshot of status.history whose release was
#   deployed (deployed, or superseded by a later one; not failed, pending,
#   uninstalled or unknown), whose chart version ends +SHA (a chart built
#   from a git source), or +SHA.N (a HelmChart from valuesFiles), and its
#   lastDeployed. A chart from an OCIRepository ends with its digest's
#   first hex, which names no commit: git.sh passes it over as a revision
#   the clone doesn't have.
def epoch:
  capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end);
# A commit's hex, out of how each tool writes a revision: Argo CD's hex,
# Flux's main@sha1:HEX, sha1:HEX, main/HEX, or a chart version's +HEX.
def hex: strings | ascii_downcase | capture("(?:\\A|[@:/+])(?<h>[0-9a-f]{7,64})(?:\\.[0-9]+)?\\z").h;
def at: strings | try (epoch | floor | todate) catch empty;
def deploy(rev; t): (rev | hex) as $r | (t | at) as $t | "\($r) \($t)";
(if .kind == "List" then .items[] else . end)
| if .kind == "Application" then
    .status.history[]? | deploy(if (.revisions | type) == "array" then .revisions[$source] else .revision end; .deployedAt)
  elif .kind == "Kustomization" then
    if ((.status.history // []) | length) > 0 then .status.history[]
      | select(.lastReconciledStatus != "ReconciliationFailed") | deploy(.metadata.revision; .firstReconciled)
    else deploy(.status.lastAppliedRevision; [.status.conditions[]? | select(.type == "Ready" and .status == "True") | .lastTransitionTime][0])
    end
  elif .kind == "HelmRelease" then
    .status.history[]? | select(.status == "deployed" or .status == "superseded")
    | select((.chartVersion // "") | test("\\+[0-9a-f]{7,64}(\\.[0-9]+)?\\z")) | deploy(.chartVersion; .lastDeployed)
  else empty end
