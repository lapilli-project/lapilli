#!/usr/bin/env bash
# The alert rules in docs/metrics.md, executed.
#
# They are documentation — the chart ships no PrometheusRule, deliberately (docs/egress.md says
# why) — but documentation that has never been run is a guess. Round 14 found that the two
# API-server rules as first written did not fire in the cases they were written for: a rule that
# selects on a series cannot fire once that series has DISAPPEARED, and `time() - <a pod's clock>`
# pages on skew alone. Nothing in the repo would have caught either.
#
# So: extract the YAML block, check it parses and every expression is valid PromQL, then run
# promtool's own unit tests over the timelines that matter. Needs docker (prom/prometheus).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOC="$ROOT/docs/metrics.md"
IMAGE=${PROM_IMAGE:-prom/prometheus:v3.5.0}
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

# The single fenced yaml block under "## Alerts worth having".
awk '/^## Alerts worth having/ {found=1} found && /^```yaml$/ {inblock=1; next}
     inblock && /^```$/ {exit} inblock {print}' "$DOC" > "$OUT/rules-body.yaml"
[ -s "$OUT/rules-body.yaml" ] || { echo "FAIL: no yaml block found under '## Alerts worth having'"; exit 1; }

RULES=$(grep -c '^- alert:' "$OUT/rules-body.yaml")
[ "$RULES" -ge 20 ] || { echo "FAIL: only $RULES alerts extracted; the awk range is wrong"; exit 1; }

{ echo "groups:"; echo "  - name: kairn"; echo "    rules:";
  sed 's/^/      /' "$OUT/rules-body.yaml"; } > "$OUT/rules.yaml"

# The one expression that is a placeholder for the reader, not a rule: it names the key id the
# auditors hold. Substitute something syntactically valid so the file can be checked at all.
sed -i.bak 's/<the key_id auditors hold>/deadbeef/' "$OUT/rules.yaml" && rm -f "$OUT/rules.yaml.bak"

cat > "$OUT/tests.yaml" <<'YAML'
rule_files: [rules.yaml]
evaluation_interval: 30s
tests:
  # A rule cannot fire on a series that is gone. This is why KairnNotReporting exists and why it
  # is the only one of the three written with absent().
  - interval: 30s
    name: the controller stops being scraped entirely
    input_series:
      - series: 'kairn_apiserver_polls_total{result="ok"}'
        values: '1+1x10 _x80'
      - series: 'kairn_apiserver_poll_ok'
        values: '1x10 _x80'
    alert_rule_test:
      - eval_time: 8m
        alertname: KairnApiServerUnusable
        exp_alerts: []
      - eval_time: 8m
        alertname: KairnNotReporting
        exp_alerts: []
      - eval_time: 20m
        alertname: KairnNotReporting
        exp_alerts:
          - exp_labels: { severity: critical }
            exp_annotations:
              summary: "No Kairn controller is reporting: the cluster has no evidence recorder"

  # A controller that was blind from the moment it started never emits a last-success timestamp,
  # so the escalation must not depend on one.
  - interval: 30s
    name: blind from the first poll
    input_series:
      - series: 'kairn_apiserver_polls_total{result="ok"}'
        values: '0x80'
      - series: 'kairn_apiserver_polls_total{result="unreachable"}'
        values: '1+1x79'
      - series: 'kairn_apiserver_poll_ok'
        values: '0x80'
    alert_rule_test:
      - eval_time: 2m
        alertname: KairnApiServerUnusable
        exp_alerts: []
      - eval_time: 10m
        alertname: KairnApiServerUnusable
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: "Kairn cannot use the API server: no capture will run (the pod stays Ready on purpose)"
              description: "Break it down with kairn_apiserver_polls_total by result: forbidden or unauthorized is RBAC or the ServiceAccount token, not-found is a missing CRD, unreachable is the network (docs/egress.md), api-error is the server itself."
      - eval_time: 14m
        alertname: KairnApiServerBlindTooLong
        exp_alerts: []
      - eval_time: 25m
        alertname: KairnApiServerBlindTooLong
        exp_alerts:
          - exp_labels: { severity: critical }
            exp_annotations:
              summary: "Kairn has not reached the API server for 15m; captures have been stopping that long"

  # A healthy controller whose clock is far behind Prometheus's must not page. The first version
  # of KairnApiServerBlindTooLong did, immediately, on skew alone.
  - interval: 30s
    name: a healthy controller with a skewed clock
    input_series:
      - series: 'kairn_apiserver_poll_ok'
        values: '1x80'
      - series: 'kairn_apiserver_polls_total{result="ok"}'
        values: '1+1x79'
      - series: 'kairn_apiserver_last_success_timestamp_seconds'
        values: '-100000x80'
    alert_rule_test:
      - eval_time: 30m
        alertname: KairnApiServerBlindTooLong
        exp_alerts: []
      - eval_time: 30m
        alertname: KairnApiServerUnusable
        exp_alerts: []

  # During an outage the state gauges hold their last value on purpose. Without the `unless`
  # guard that makes "captures are stuck" page too, which is false: the picture is stale.
  - interval: 30s
    name: an outage does not also page about stuck captures
    input_series:
      - series: 'kairn_apiserver_poll_ok'
        values: '0x80'
      - series: 'kairn_apiserver_polls_total{result="ok"}'
        values: '0x80'
      - series: 'kairn_captures{phase="capturing"}'
        values: '1x80'
    alert_rule_test:
      - eval_time: 25m
        alertname: KairnCapturesStuck
        exp_alerts: []
      - eval_time: 25m
        alertname: KairnApiServerUnusable
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: "Kairn cannot use the API server: no capture will run (the pod stays Ready on purpose)"
              description: "Break it down with kairn_apiserver_polls_total by result: forbidden or unauthorized is RBAC or the ServiceAccount token, not-found is a missing CRD, unreachable is the network (docs/egress.md), api-error is the server itself."

  # The volume alert has to fire on a filling disk and stay quiet on a healthy one. This is the
  # ratio expression, which is the part most likely to be written wrong.
  - interval: 1m
    name: the bundle volume fills up
    input_series:
      - series: 'kairn_bundle_fs_bytes{state="used"}'
        values: '500000000+10000000x120'
      - series: 'kairn_bundle_fs_bytes{state="free"}'
        values: '500000000-10000000x120'
      - series: 'kairn_retention_sweeps_total{result="ok"}'
        values: '1+1x120'
    alert_rule_test:
      - eval_time: 10m
        alertname: KairnBundleVolumeFilling
        exp_alerts: []
      - eval_time: 70m
        alertname: KairnBundleVolumeFilling
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: "Kairn's bundle volume is over 85% full; captures fail when it is full"
              description: "Enable retention (docs/design-retention.md) or raise persistence.size. Abandoned staging directories are reclaimed regardless of the byte and age bounds."

  # A sweep that never completes must fire even though the series exists — the absent() half covers
  # the controller that never got one away at all.
  - interval: 1m
    name: retention stops sweeping
    input_series:
      - series: 'kairn_retention_sweeps_total{result="ok"}'
        values: '7x200'
    alert_rule_test:
      - eval_time: 30m
        alertname: KairnRetentionNotSweeping
        exp_alerts: []
      - eval_time: 190m
        alertname: KairnRetentionNotSweeping
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: "Kairn's retention sweep has not completed in 2h"

  # …but a genuinely stuck capture on a healthy controller still does.
  - interval: 30s
    name: a stuck capture on a healthy controller still pages
    input_series:
      - series: 'kairn_apiserver_poll_ok'
        values: '1x80'
      - series: 'kairn_apiserver_polls_total{result="ok"}'
        values: '1+1x79'
      - series: 'kairn_captures{phase="capturing"}'
        values: '1x80'
    alert_rule_test:
      - eval_time: 25m
        alertname: KairnCapturesStuck
        exp_alerts:
          - exp_labels: { phase: capturing }
YAML

echo "==> $RULES alert rules extracted from docs/metrics.md"
docker run --rm -v "$OUT:/w" -w /w --entrypoint promtool "$IMAGE" check rules rules.yaml
docker run --rm -v "$OUT:/w" -w /w --entrypoint promtool "$IMAGE" test rules tests.yaml
echo "alert rules OK"
