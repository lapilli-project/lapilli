# Jira Service Management's alerts, an array of them as GET
# /jsm/ops/api/{cloudId}/v1/alerts gives them, to Lapilli's responses: each
# alert created in the period is a page, JSM- and its tinyId. ops.jq says
# how, and who acted is read.
include "ops";
main("JSM")
