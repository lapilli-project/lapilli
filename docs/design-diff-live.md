# Design — `lapilli diff-live`: which of a bundle's evidence the cluster can no longer produce

Status: **RETURNED TO PREMISE (round 32).** Kept as written, because a refuted proposal is worth
more on the record than off it — but **nothing below is the plan**. Read
`docs/design-review-round32.md` first.

Two of the five findings that killed it are arithmetic rather than argument. The number this tool
exists to produce is **100% for every rule by construction**: `events.json` is in every default
bundle, Kubernetes expires events at a one-hour default TTL, and the measurement runs at T+24 h, so
"at least one file that cannot be reproduced" is true everywhere and the between-rule discriminating
power is zero. And the corpus it computes over — bundles Lapilli sealed — has **no rows at all** for
the rules the question is about, because those alerts carry no `pod` label and are dropped before a
bundle exists. A measurement that cannot vary and cannot see its subject is not a measurement.

The rest: `api-unique` is round 24's rejected shape (a positive published number with its caveat in
its name) and its bias points the ranking the wrong way; the log rows test whether a command
succeeded rather than whether it returned the same container instance; and a content diff would
print the field set redaction exists to remove.

The replacement is a fork, with the owner — `rules-coverage` read from the operator's own rule files,
or a `kubectl` plugin that serves the bundle when the API says the previous container is gone.

Originally: the measurement the owner's round-31 fork decision chose.

## 1. The question, and why a number rather than an argument

Round 29 F4 says alerts without a `pod` label are the ones an SRE is paged for, and Lapilli refuses
them. Round 31 killed the obvious fix and, more usefully, killed the way it was chosen: the cut was
made by which collector already existed, and the rule where evidence is genuinely destroyed
(`KubeJobFailed`) was buried among three where `kubectl describe` answers in twenty seconds.

The same round's hostile lens gave the replacement: stop arguing about which rules are worth
capturing and **measure which bundles hold something the cluster can no longer produce**. Run it at
T+24 h against every sealed bundle, group by alert rule, and let the table pick the cut.

It is also the only artifact proposed so far that an adopter can run on their own cluster in week
one. Round 29 F5 found that no public postmortem names lost evidence, so "install it for the next
bad night" has a trigger frequency near zero. A per-rule table produced from *their* incidents is a
different kind of argument.

## 2. What it compares against — and the line this must not cross

**Round 24 returned backfill to premise** because fetching evidence from other stores would make
Lapilli a client of them. A tool that re-runs queries against Prometheus and a team's log store sits
near that line, and the proposal is to stay on this side of it:

> **v1 compares against the live Kubernetes API and nothing else.**

The reason is not squeamishness. What Lapilli claims to save is what *the API* stops being able to
answer: the previous container's log tail after kubelet GC, the pod object after deletion, events
after the one-hour default TTL, the ReplicaSet revision after `revisionHistoryLimit` prunes it. A
team's Prometheus retention and their log shipper are properties of their stack, not of Lapilli's
claim, and measuring them requires becoming their client.

**This has a cost the output must carry, not the documentation.** Round 31's incumbent lens said the
bundle's value is near zero for a team with a working log store — and an API-only measurement cannot
see that store, so it will score a log tail as unreproducible when the team can `grep` it in
Loki. A number that overstates its own subject is exactly what this project refuses elsewhere. So
the column is named **`api-unique`**, never `unique`, and the tool prints what it did not judge
beside what it did.

## 3. Shape

```
lapilli diff-live <bundle.ieb|dir>… [--context <ctx>] [--output text|json]
```

Reads a bundle, asks the cluster what still exists, and reports per file. It reaches the cluster
**through `kubectl`**, the way `lapilli demo` already does, for the reason `demo.rs` gives: the
`lapilli` crate stays free of kube-rs, and `--no-default-features` stays a verifier with no network
code at all. A measurement tool is not a reason to link a Kubernetes client into the offline
verifier.

| Bundle file | Asked of the cluster | `api-unique` when |
|---|---|---|
| `resources/pod.json` | `get pod -o json` | the pod is gone, **or** exists with a different `metadata.uid` |
| `logs/<c>-previous.log` | `logs <pod> -c <c> --previous` | the command fails or returns nothing |
| `logs/<c>-current.log` | `logs <pod> -c <c>` | the pod is gone, or the container has restarted since |
| `events.json` | `get events --field-selector involvedObject.uid=…` | no event in the bundle's window survives |
| `diffs/**` | the ReplicaSets/ControllerRevisions named in `diffs/index.json` | any named revision is no longer in history |
| `resources/{replicaset,deployment,…}.json` | `get <kind> -o json` | gone, or a different `uid` |
| `metrics/**` | — | **not judged**, and reported as such |

Per bundle it prints the verdict, and across bundles a table grouped by `incident.trigger.rule`.

## 4. What must not be decided in passing

Round 31's lesson was not that the previous proposal was careless but that it *settled its own
hardest trade in a later paragraph*. These stay open until the loop or a measurement closes them.

1. **What "reproducible" means for an object that still exists.** A pod with the same `uid` is the
   same object, but not in the same state — the bundle holds it as it was at the alert. Is that
   file reproducible or not? This single choice moves the headline number more than any other, and
   both answers are defensible.
2. **What the tool sees, and what it prints.** The bundle is redacted; the live cluster is not.
   Comparing means the process reads unredacted objects and log lines. Does it diff contents at all,
   or only existence? If it diffs, what reaches the terminal, and what would `--output json` put on
   disk next to a bundle whose whole point is that its contents were redacted before they were
   written?
3. **What it needs to be allowed to do.** `get pods`, `pods/log`, `events`, `replicasets` across the
   namespaces of the bundles being measured. That is the read set an operator audits before granting,
   and a measurement tool asking for cluster-wide log read is its own privacy surface
   (`docs/data-handling.md` is the page that would have to say so).
4. **Whether the number means anything at n=3.** "The share of bundles per rule holding at least one
   `api-unique` file" needs bundles per rule. A first adopter has a handful, and most rules have
   zero. A table that reports `100%` from one bundle is worse than no table.
5. **Whether this belongs in `lapilli` at all.** The CLI is an *offline verifier* and that is a
   selling point — `--no-default-features` builds one with no network code. `diff-live` makes it,
   optionally, a cluster client. Shelling out to `kubectl` keeps the dependency out of the binary but
   not the capability out of the tool. A separate `lapilli-diff-live` binary, or a script in
   `contrib/`, are both real answers.

## 5. What this is not

Not a fetcher: it never writes anything from the cluster into a bundle, and never repairs one. Not a
verifier: `lapilli verify` answers whether a bundle is intact and this answers whether its contents
could be obtained again, which are different questions with different failure modes. Not a
continuous check: it runs when someone asks, at T+24 h or later, and nothing in the controller
depends on it.
