---
layout: default
title: Which of your alerts are worth their page?
description: "lapilli verdict judges every firing against what actually went wrong: a white stone when it caught an incident, a black one when nothing was wrong."
---

# Which of your alerts are worth their page?

`lapilli verdict` judges every firing against what actually went wrong: a white
stone when the alert caught an incident, a black stone when nothing was wrong.
Count a rule's stones over a week and you know whether it earns its place; an
incident with no stone is one your alerts missed.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul
148 firings, 3 incidents written down, 1 more found by --impact

RULE                FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
OrderCancelTask5xx  118    0         118        0%         41
CheckoutLatencyP99  14     2         12         14%        3
ReadReplicaLag      9      1         8          11%        0
NodeNotReady        4      0         4          0%         0
DBPrimaryCPU        3      2         1          67%        0

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

## How it judges

- **Firings** come from replaying your alert rules against history
  (`--rules`: Prometheus rule files, `PrometheusRule`, Grafana alerting
  provisioning, or ConfigMaps holding them, each version while it was in
  effect), from the `ALERTS` series
  Prometheus keeps (`--alerts`), or from a file (`--firings`). Grafana rules
  are evaluated the way Grafana does.
- **Incidents** come from a YAML file, and from windows where a signal of user
  impact holds (`--impact`), so a thin incident log still works.
- **A firing catches an incident** when it starts shortly before or during it
  and agrees on the labels you name. One already firing before caught nothing:
  it was on anyway.
- **Before a change is merged**, `--compare` replays the rules in place
  beside those proposed, over the same period, and shows each rule's stones
  and each incident's catches before → after.

Any Prometheus-compatible API works: Prometheus, Thanos, Mimir,
VictoriaMetrics, or the managed Prometheus of AWS, Azure or Google Cloud, with
the credentials the machine or pod already has. Oracle Cloud has no managed
Prometheus; on OKE, use the one you run there. Lapilli only reads.

## The name

> *mos erat antiquus niveis atrisque lapillis,*
> *his damnare reos, illis absolvere culpa*
>
> It was the old custom to judge with white and black pebbles: the black to
> condemn the accused, the white to acquit them.
>
> — Ovid, *Metamorphoses* XV.41–42

*Lapilli* are small stones. Each firing is a vote; once a week the urn is
emptied and the stones are counted.

[Source on GitHub](https://github.com/lapilli-project/lapilli)
