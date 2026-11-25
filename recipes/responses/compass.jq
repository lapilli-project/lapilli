# Compass's alerts, an array of them as GET /v1/alerts gives them, to
# Lapilli's responses: each alert created in the period is a page, CP- and
# its tinyId (as the incidents' ids are). ops.jq says how, and who acted is
# read.
include "ops";
main("CP")
