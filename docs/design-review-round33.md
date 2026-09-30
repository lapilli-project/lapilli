# Design review — round 33 (`rules-coverage`: the tool answers four rules in a hundred and fifty-five)

Constitution: **v0.7.1**. Target: `docs/design-rules-coverage.md` at **`dc61125`**. Three lenses:
**can it answer at all** · **untrusted input and the claims it makes** · **the incumbent's user**.

Sized 3 × 3. This is a smaller object than the last two — it reads a file, touches no cluster,
stores nothing — but it parses YAML from outside the project, ships in a public release, and its
whole output is a claim about somebody else's alerting.

**Independence: not achieved.** Same limitation, third round running.

## Verdict: RETURN TO PREMISE — the third in a row, and the round found two live product defects

The tool as proposed answers **4 of 155** real kube-prometheus-stack alert rules, and **0 of the 41
that page**. That is measured, not estimated. But the more valuable half of this round is not about
the tool: pointing a lens at *what a real alert actually carries* turned up two things about the
shipped product, one of which is fixed in this commit.

## 1. The measurement, computed rather than argued

A lens rendered `kube-prometheus-stack 91.8.2` (155 alerting rules), wrote a PromQL label-set
analyzer implementing §2's method, and built the label table from all 40 upstream
`kube-state-metrics` metric docs (340 metrics, 60 carrying `pod`) plus cAdvisor's label map.

| Corpus | accept | drop | **unknown** |
|---|---|---|---|
| all 155 rules | 4 | 63 | **87 (56%)** |
| the 41 `severity: critical` rules | 0 | 17 | **24** |

The unknown mass is exporter metrics a KSM+cAdvisor table can never cover — node-exporter 27,
prometheus 23, etcd 12, kubelet 7, alertmanager 4 — and `up` alone appears in 15 alerts where no
metric-name-keyed entry can be correct, because whether `up` carries a pod depends on which job's
targets are pods: `TargetDown`, `KubeletDown`, `KubeAPIDown` and `PrometheusDown` need four
different answers from one entry.

Dogfooded, it is worse: run the tool on **Lapilli's own** alert rules in `docs/metrics.md` (26 at the time; the lens said 28) and it
prints roughly 0 accept, a handful of provable drops, and ~22 unknown. The project's alerting is
unanswerable to the project's tool.

**And the author did it again.** §2 says the table "resolves most of the upstream rules, *which is
the set that matters*". The set that matters was defined as the set the method can resolve. Round 31
chose its scope by which collector existed; round 32 chose its corpus by which bundles existed;
round 33 chose which rules matter by which rules it could classify. The rule was written into round
32 §6 and broken in the next document.

## 2. Two things about the shipped product

**P1 · An ordinary alert name kills the rest of the payload. Fixed here.** The CRD constrains
`trigger.rule` to `^[^<>&|\x00-\x1F\x7F]{1,200}$` — deliberately, because that string is rendered
into a chat message and a permanent Markdown document. The webhook had no matching pre-check. So
`Disk > 90%`, an ordinary name for an alert, produced a 422 at admission, surfaced to Alertmanager
as a 500, and — in the words of a comment the project had already written for the *neighbouring*
field — *"because `handle` returns on the first create error, the rest of this payload's alerts
never attempted"*. One badly-named rule in a fifty-alert storm loses the other forty-nine.

The identical failure was fixed once, for `firingTs`, and `refuse_firing_ts` exists because of it.
The lesson did not travel one field across. `crd::rule_ok` and `webhook::refuse_rule` now refuse it
where it costs one alert, counted as `lapilli_alerts_dropped_total{reason="bad-rule-name"}`;
mutation-proven, and the regression test uses the names an SRE actually writes.

**P2 · Alerts may be accepted with the *exporter's* pod as the target — not yet proven on a
cluster.** `generateServiceMonitorConfig` relabels `pod`, `namespace` and `container` onto every
ServiceMonitor target, unconditionally — read in the source, no condition around it. The in-corpus
proof that these reach the rules: `alertmanager.rules.yaml` filters on `container="alertmanager"`, a
label Alertmanager does not expose about itself, so it can only come from that block — which also
sets `pod`. The kube-state-metrics ServiceMonitor applies no `metricRelabelings` by default.

If that carries through, `KubeJobFailed` fires with `pod=<kube-state-metrics pod>`, Lapilli's webhook
sees a non-empty `pod`, **accepts**, and records the exporter instead of the failed Job. With
`honorLabels: true` the namespace would be the Job's and the pod the exporter's — a combination that
does not exist — which is the "success report for nothing" the `no-pod` refusal was built to prevent,
arriving through a door nobody checked. It would also mean round 29 F4 and round 31 were half wrong:
the problem is not only *dropped*, it is *accepted with the wrong target*.

**This is written as unproven on purpose.** Three source reads make it likely and none of them is a
cluster. Twice this week an airtight-looking inference was wrong, so the disposition is a test, not a
fix: an E2E step that POSTs a `KubeJobFailed`-shaped payload with a relabelled `pod` and asserts what
the controller does with it.

## 3. The rest, by lens

**Untrusted input.** The project's own rule — flatten and bound every string somebody else chose, at
the leaf, once — is nowhere in the proposal, whose output medium is a terminal.

The lens also claimed **OSC 8 (hyperlink) and OSC 52 (clipboard write) pass through `md()`
untouched**, so an alert name committed to a monitoring repo could write the reader's clipboard.
*Checked at both surfaces and refuted:* an OSC sequence opens with ESC and closes with BEL or
ESC-backslash, and `invisible` — in `notify.rs` and in `postmortem.rs` alike — begins with
`char::is_control`, which covers all three. The payload is flattened to spaces and cannot reach a
terminal. That check is now a test on each surface rather than a note in this log, because the claim
is plausible enough to be raised again and a reader of round 33 should find the answer in the code.

The YAML half stands. Measured against the locked `serde_yaml 0.9.34+deprecated`: billion-laughs and
deep nesting are refused by the parser, but **alias fan-out is not** — 701 KB of input became 102 MB
of values in 11.2 s, ×146 — and a 50 MB scalar is accepted. §5 item 4 named the two risks the parser
already handles and omitted the one it does not.

**A gate that did not exist.** `deny.toml` asserted in two places that *"`cargo audit` is the
advisory gate (scripts/release-check.sh)"*. Every `cargo audit*` string in that script is
`cargo auditable build` — one word apart, and a different tool: one scans for vulnerabilities, the
other embeds a dependency list. There was no advisory scan anywhere in the repository. Run by hand:
343 crates, **0 advisories**, which is the point — nobody knew that. A `cargo-audit` job runs on every
pull request now and `deny.toml` says what is true.

**The output could be mistaken for a cluster report.** It prints the controller's own refusal codes,
takes an install-config flag, and the title says what Lapilli "can actually record". A JSON file
containing `{"rule": "...", "refusal": "no-pod"}` pasted into a security review reads as observed
behaviour. Every verdict needs a `predicted-` prefix and the JSON a mandatory provenance block
including `cluster_contacted: false`.

**The incumbent.** The denominator is rules; people are woken by fires. Of 200 rules, ~190 are
dormant, so "14%" cannot say whether the four that page are inside it. And what reaches the webhook
at all is decided by the Alertmanager route tree — which Lapilli's *own* alert rule already says
(*"check the Alertmanager route's matchers"*) — so a competently routed install is near 100% by
construction and a low headline describes a route nobody would deploy.

## 4. Dispositions

| # | Finding | Disposition |
|---|---|---|
| 1 | 4/155, 0/41 critical; unknown is the majority | **RETURN TO PREMISE** |
| 2 | "the set that matters" defined as the set it can resolve | **RETURN TO PREMISE** |
| 3 | Denominator is rules, not fires | **RETURN TO PREMISE** |
| P1 | An ordinary alert name kills the payload | **FIXED in this commit**, mutation-proven |
| P2 | Alerts accepted with the exporter's pod | **TEST FIRST** — an E2E step before any fix |
| 4 | OSC 8 / OSC 52 pass through the Markdown escape | **REFUTED on checking** — `invisible` begins with `char::is_control` on both surfaces, so ESC and BEL are flattened. Kept as a test in `notify.rs` and `postmortem.rs`, not as a note |
| 5 | serde_yaml alias fan-out, ×146; 50 MB scalars | **APPLY** to any successor that parses YAML: caps stated the way `verify.rs` states them, plus a fuzz target |
| 6 | `deny.toml` claimed an advisory gate that did not exist | **FIXED in this commit** |
| 7 | Output indistinguishable from a cluster report | **APPLY**: `predicted-` verdicts, provenance block, `cluster_contacted: false` |
| 8 | Alertmanager route tree decides the real denominator | **APPLY**: `alertmanager.yml` as a second input, also a file in git |
| 9 | Recording rules are resolvable within the given files | **APPLY**, calibrated: worth 1 of 155 upstream, but flips sloth burn-rate from unknown to provable drop |
| 10 | KSM label tables are stable enough (1 of 54 used metrics changed v2.13→v2.20) | **clean** — the table should still carry its version range and refuse outside it |

## 5. The way out, which the lenses also supplied

`ALERTS` is a synthetic series Prometheus stores for every firing alert, carrying **its actual label
set**. One query the operator runs and saves:

```
count by (alertname, namespace) (count_over_time(ALERTS{alertstate="firing"}[10d]))
```

Lapilli reads two JSON files and never opens a socket — the same side of round 24's line as reading a
rule file. It collapses `unknown` to zero because a fired alert *states* its labels, it supplies the
fire counts that fix the denominator, and it moots the compiled-in metric table and its version rot
entirely. Two caveats belong in the output: default retention is 10 d, and a rule that never fired
stays unknown — which is itself the answer to whether it matters.

## 6. What this round taught the method

**Three proposals, one disease, and the third was written after the rule against it.** Scope by
available collector, corpus by available bundle, relevance by available classifier. The rule needs
teeth: *the next proposal must state, in its first section, what it cannot see* — before it says what
it can.

**Point a lens at the input, not the design.** Every finding that mattered here came from reading
what a real alert actually carries: the relabel block, the `container="alertmanager"` filter, the
`ALERTS` series, the 155 rules. The design was fine as designs go; the world it assumed was not the
world.

**A comment can hide a missing gate for as long as it exists.** `deny.toml` said `cargo audit` ran
in a script where only `cargo auditable build` appeared. Nobody greps a claim they have read.
