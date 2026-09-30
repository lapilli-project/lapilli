# Design — `lapilli rules-coverage`: how much of your alerting Lapilli can actually record

Status: **RETURNED TO PREMISE (round 33).** Kept on the record; **nothing below is the plan**. Read
`docs/design-review-round33.md` first.

Measured, not estimated: on the real `kube-prometheus-stack` rule set this method answers **4 of 155**
rules, and **0 of the 41 that page**. 56% land in `unknown`, almost all of it exporter metrics a
kube-state-metrics table can never cover. Run on Lapilli's own alert rules it answers essentially
none of them. And §2's sentence "the set that matters" defined the important rules as the rules the
method can classify — the third round in a row of choosing the question to fit the available answer.

The way out came from the same lenses: Prometheus stores an `ALERTS` series carrying each firing
alert's **actual** label set, so one saved query collapses `unknown` to zero, supplies the fire counts
that fix the denominator, and moots the compiled-in label table. Reading two JSON files the operator
exports is the same side of round 24's line as reading a rule file.

The round's more valuable half was not about this tool: it found one live product defect, now fixed
(an ordinary alert name like `Disk > 90%` killed the rest of its payload), and one unproven claim
about relabelled `pod` labels that needs a cluster test before anyone calls it a defect.

Originally: the owner's round-32 fork chose it over `diff-live`, which could not measure.

## 1. What it is for

An operator hands it their Prometheus rule files. It prints, per rule, whether an alert from that
rule would become a capture — and when not, which refusal it would hit. No cluster, no RBAC, no
install, no security review. Five minutes on a laptop, from files they already have in git.

Round 29 F5 found that no public postmortem names lost evidence, so *"install it for the next bad
night"* has a trigger frequency near zero. A number computed from **their own alerting**, before
anything is installed, is a different kind of argument — and if it says Lapilli would record 14% of
what pages them, that is a true thing they should know before the security review, not after.

## 2. The problem this has to solve honestly, and it is the whole problem

**A Prometheus rule file does not say what labels its alerts will carry.** The labels of a firing
alert are the labels of the series the expression returns, plus the rule's own `labels:` block. The
file gives the second and not the first.

```yaml
- alert: KubePodCrashLooping          # carries pod, namespace, container
  expr: max_over_time(kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"}[5m]) >= 1
  labels: { severity: warning }       # ← the only labels the file states
```

The `pod` label that decides everything comes from `kube_pod_container_status_waiting_reason`, which
the file never mentions carrying it. Meanwhile:

```yaml
- alert: SLOBurnRateFast
  expr: sum by (service) (rate(http_errors[5m])) / sum by (service) (rate(http_total[5m])) > 0.05
```

Here `by (service)` provably drops everything else, so no `pod` — determinable from the file alone.

So the three outcomes are **accept**, **drop (with the reason)**, and **unknown**, and the third is
not a failure of effort. What decides which:

| Case | Determinable from the file? |
|---|---|
| `by (…)` / `without (…)` aggregation whose result cannot contain `pod` | **yes** — provably dropped |
| the rule's own `labels:` supply `pod` | **yes** — provably present |
| a bare selector on a metric whose label set is known and stable | **yes, with a table** — `kube-state-metrics` and cAdvisor label sets are documented and change with their versions |
| a selector on the team's own metric, or over their recording rules | **no** |

A built-in table of well-known metric label sets resolves most of the upstream
kube-prometheus-stack rules, which is the set that matters for the question round 29 F4 asked. A
team's own SLO rules over their own recording rules land in `unknown`, and the output says so rather
than guessing.

## 3. What the tool reproduces

It must reproduce the controller's actual refusals, not a sketch of them. From `webhook.rs` and
`reconcile.rs`:

| Refusal | Rule property? |
|---|---|
| `no-pod` — the `pod` label absent or empty | **yes**, subject to §2 |
| `target-not-watched` — the `namespace` label outside `watchNamespaces` | partly: whether a `namespace` label exists is a rule property; which namespaces are watched is install config, so it needs `--watch-namespaces` to be answered |
| a missing `namespace` falls back to the controller's own namespace, so the capture targets `lapilli-system` | **yes**, and it is worth its own line: this is an accept that produces a useless capture |
| `bad-firing-ts`, `payload-cap`, `cluster-mismatch` | **no** — properties of a payload or an install, not a rule |

## 4. Shape

```
lapilli rules-coverage <rules.yaml>… [--watch-namespaces a,b] [--output text|json]
```

Reads files. Writes a table. Touches nothing else — no network, no cluster, no kubeconfig, and
nothing on disk but what `--output` is pointed at.

## 5. What must not be decided in passing

Round 31 chose its scope by what already existed; round 32 chose its corpus the same way. The
discipline this round owes is to name the thing that decides whether the output is worth printing —
and it is item 1, not item 4.

1. **What to do about `unknown`.** If a third of a team's rules land there, is the headline
   `accepted / total`, `accepted / (total − unknown)`, or three numbers? The first understates, the
   second flatters, and the third is what an honest tool prints and nobody reads. This decides
   whether the output persuades or misleads, and it is the same trap `api-unique` fell into one round
   ago: a caveat carried in a name rather than in the number.
2. **Where the metric label table comes from, and what happens when it is wrong.** It would be
   compiled into the binary, so it ages with the release while `kube-state-metrics` moves. A stale
   entry produces a confident wrong verdict, which is worse than `unknown`. Does it carry a version,
   refuse to answer for metrics it does not recognise, or get generated from something?
3. **Whether the tool may be pointed at anything live at all.** The mock output in the fork question
   — mine — included *"dropped rules by how often they fired, last 30d"*. That number cannot come
   from a file; it requires querying Prometheus or Alertmanager, which is the client relationship
   round 24 refused. It has to come out, or it has to come from something the operator hands over
   (an exported payload, a stated count), and saying which is a decision.
4. **Whether a rule file is untrusted input.** It comes from outside the project, and a YAML parser
   is where that becomes a question: aliases, deeply nested documents, enormous strings. The CLI
   currently parses only its own bundles.
5. **What the output claims.** "Lapilli would record 14% of your rules" is a claim about somebody
   else's alerting made by a tool with an interest in the answer. What is the wording that is true
   when the number is bad for Lapilli?

## 6. What this is not

Not a client of anything: it reads files an operator gives it. Not a cluster tool: it never sees one.
Not a recommendation engine — it does not say which rules they should add a `pod` label to, because
the rules are upstream defaults and rewriting them to suit a recorder is what round 31 rejected as
option C.
