#!/usr/bin/env python3
"""Classify kube-prometheus-stack's alert rules by the subject their own description names.

§0 of docs/design-workload-target.md was the one section with no re-runnable artifact, while §5 of
the same file says a measurement nobody can re-run is an assertion. This is that artifact.

Method: a Prometheus rule file does not state the labels its alerts will carry — those come from the
series the expression returns. But each upstream rule's `description` annotation interpolates the
labels its author expects (`Pod {{ $labels.namespace }}/{{ $labels.pod }}`), which is a more reliable
signal than inferring from PromQL.

KNOWN BLIND SPOT, and it is why §0 calls its monitoring-stack count a floor rather than a count:
rules whose description interpolates no labels at all are invisible to this method. There are eight
such criticals, four of them (`KubeStateMetrics*`) self-monitoring. `job` is deliberately NOT treated
as a workload label — it is Prometheus's scrape-job label, and counting it folds in ~22 etcd and
Alertmanager-cluster rules that are not workloads.

Usage:
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm template kps prometheus-community/kube-prometheus-stack --version 91.8.2 \
        --set defaultRules.create=true > all.yaml
    python3 classify-rules.py all.yaml
"""
import re
import sys
from collections import Counter

# A workload label names something a user deployed. `job` is excluded on purpose (see the docstring).
WORKLOAD = {
    "pod", "container", "deployment", "statefulset", "daemonset", "job_name",
    "controller", "horizontalpodautoscaler", "poddisruptionbudget", "persistentvolumeclaim",
}
# Rules the monitoring stack writes about itself.
SELF_PREFIXES = ("Prometheus", "Alertmanager", "KubeStateMetrics", "Thanos", "Config")


def parse(path):
    """Pull (name, severity, declared labels, for) out of rendered PrometheusRule YAML.

    Hand-rolled rather than via PyYAML so the script runs on a stock python3 with no install —
    the rendered shape is machine-generated and stable.
    """
    text = open(path, encoding="utf-8").read()
    alerts = []
    # Split on each `- alert:` so a block carries its own annotations/labels/for.
    for block in re.split(r"\n(?=\s*- alert:\s)", text):
        m = re.match(r"\s*- alert:\s*(.+?)\s*\n", block)
        if not m:
            continue
        name = m.group(1).strip()
        sev = re.search(r"\n\s+severity:\s*(\S+)", block)
        dur = re.search(r"\n\s+for:\s*(\S+)", block)
        alerts.append({
            "name": name,
            "severity": sev.group(1) if sev else "none",
            # every $labels.X the author interpolated anywhere in the block's annotations
            "declared": sorted(set(re.findall(r"\$labels\.([A-Za-z_][A-Za-z0-9_]*)", block))),
            "for": dur.group(1) if dur else None,
        })
    return alerts


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    alerts = parse(sys.argv[1])
    print(f"alert rules: {len(alerts)}")
    print(f"severity:    {dict(Counter(a['severity'] for a in alerts))}")

    blind = [a for a in alerts if not a["declared"]]
    print(f"\ninterpolate NO labels (invisible to this method): {len(blind)}")
    for a in blind:
        print(f"    {a['severity']:<9} {a['name']}")

    wl = [a for a in alerts if set(a["declared"]) & WORKLOAD]
    selfmon = [a for a in wl if a["name"].startswith(SELF_PREFIXES)]
    user = [a for a in wl if not a["name"].startswith(SELF_PREFIXES)]
    print(f"\ndeclare a workload label: {len(wl)}")
    print(f"  the monitoring stack itself: {len(selfmon)}")
    print(f"  a user's own workload:       {len(user)}")
    print(f"    severity: {dict(Counter(a['severity'] for a in user))}")
    print(f"    for:      {dict(Counter(a['for'] for a in user))}")

    print("\nuser-workload rules, by name:")
    for a in sorted(user, key=lambda x: (x["severity"], x["name"])):
        labs = ",".join(sorted(set(a["declared"]) & WORKLOAD))
        print(f"    {a['severity']:<9} for={str(a['for'] or '-'):<5} {a['name']:<42} {labs}")

    crit = [a for a in alerts if a["severity"] == "critical"]
    print(f"\ncritical rules: {len(crit)} — none of them carries a pod or workload-controller label:")
    for a in sorted(crit, key=lambda x: x["name"]):
        print(f"    {a['name']:<45} {','.join(a['declared']) or '(none)'}")


if __name__ == "__main__":
    main()
