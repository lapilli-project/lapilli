# Design review — round 32 (`diff-live`: the measurement could not measure)

Constitution: **v0.7.1**. Target: `docs/design-diff-live.md` at **`372bf96`** — the measurement the
owner's round-31 fork chose. Four lenses in one wave: **identity** · **privacy and data handling** ·
**measurement validity** (a hostile statistician) · **the incumbent's user**.

Stakes: a new capability that reads a live cluster while holding a redacted bundle, and whose output
was to decide what the webhook accepts. Sized 4 × 3. Floors met: user data → privacy; a
product-defining change → identity first; a number that would be published → a lens whose only job
was whether it means anything.

**Independence: not achieved**, again. All four lenses are the author's own model family. Calibration,
not confirmation.

## Verdict: RETURN TO PREMISE — for the second round running

**The direction survives. `diff-live` does not.** Five findings at BLOCKER, and two of them are
arithmetic rather than argument: the number the tool exists to produce is **100% for every rule by
construction**, and the corpus it would compute over contains **zero rows for every rule the question
is about**. A measurement that cannot vary and cannot see its subject is not a measurement.

Round 31 died because a proposal chose its scope by which collector already existed. Round 32 died
because a proposal chose its corpus by which bundles already existed. That is the same mistake
wearing different clothes, and it is now the loop's finding about its own author rather than about
either document.

## 1. The two that are arithmetic

**A1 · Every cell is 100%, and the table's only content is the apiserver's event TTL.**
`events.json` is in every default-profile bundle. Kubernetes expires events at `--event-ttl`,
default **one hour**. The proposal measures at **T+24 h** and scores a bundle `api-unique` if *at
least one* of its files cannot be reproduced. Twenty-three hours after the last event expired, that
is true of every bundle of every rule. Computed on a realistic first-adopter corpus — 30 bundles
skewed 21/5/3/1 across four rules — the table reads 100%, 100%, 100%, 100%, and the
between-rule discriminating power is **0 percentage points**.

The incumbent lens reached the same result by walking it instead: at T+24 h a `KubePodCrashLooping`
pod still exists with the same uid (CrashLoopBackOff pods are not deleted), `revisionHistoryLimit`
defaults to 10 so the diffs survive, `logs --previous` returns output — and `events.json` is gone.
Four rules, one identical reason.

The comparison is not even like-for-like: the bundle collected events with
`involvedObject.name={pod}` and the proposal queries `involvedObject.uid`, so an event of a
same-named predecessor pod is unmatchable even inside the TTL.

**A2 · The corpus has no rows for the rules in question.** Lapilli seals a bundle only for an alert
carrying a `pod` label. Every rule round 31 K3 named as the hole — SLO burn rate, node conditions,
`KubeJobFailed` — carries none. So the table can rank the rules already accepted and is
*structurally silent* on "which rules should Lapilli start capturing", which is the only question the
fork asked. The measurement lens put the inheritance precisely: round 31's incumbent lens asked for
the live cluster **and Prometheus and the team's log store**; this proposal dropped two of the three
sources and kept the sealed-bundle universe — **it narrowed the inputs and kept the flaw**.

## 2. The three that are judgement

**A3 · `api-unique` is round 24's rejected fix in a new place.** The name was meant to carry the
caveat that an API-only measurement cannot see a team's log store. Round 24 refused exactly this
shape twice: *"`mode: "off"` was offered as an honest alternative. It is not"*, and of
`coverage_score: 1.0` with a warning string, *"three positive assertions with the caveat in an
advisory string"*. Worse than cosmetic: the bias is not noise but largest on the rules the cut is
about. A crashloop keeps the same pod uid, so its dead instance sits under
`/var/log/pods/<ns>_<pod>_<uid>/…` where a shipper already tailed it — `api-unique` scores it high
and a reader skims it as "only Lapilli had this". The row where a shipper genuinely misses the bytes
is the short-lived pod (a known Promtail/Alloy gap), which is `KubeJobFailed` — the row round 31 K3
said had already been buried once.

**A4 · The log rows test whether a command succeeded, not whether it returned the same bytes.** The
kubelet keeps one dead instance per container, so after a single restart `logs --previous` happily
returns instance N−1 while the bundle holds instance 1: scored reproducible, and it is not. In the
other direction a captured *current* instance is still fetchable as `--previous` after one restart:
scored unique, and it is not. Both were demonstrated. The fix costs nothing and no extra API call:
the bundle already records `container_id` in `logs/index.json`, and the live pod carries
`status.containerStatuses[].lastState.terminated.containerID`. Compare those.

**A5 · What the tool would see, and print.** The bundle is redacted; the cluster is not. A content
diff of `resources/*.json` prints exactly the field set redaction exists to remove — `env[].value`,
argv, probe headers — and because `kubectl.kubernetes.io/last-applied-configuration` is **dropped
outright** from a capture rather than redacted, a naive diff reports it as live-only and prints an
entire unredacted applied pod spec to a terminal, a shell history, a CI log. The log rows are worse
than neutral: the bundle's tail is capped at 2000 lines and 4 MiB, and the proposal's `kubectl logs`
carries no `--tail` or `--limit-bytes`, so the tool fetches text the bundle deliberately does not
contain — with none of the gates `mcp.allowLogs` exists to provide. And an agent handed this tool
plus a laptop's kubeconfig reads live log text with neither of the two controls `lapilli mcp`
applies: **Lapilli routing around its own control.**

## 3. Three things the round corrected about the author

**My own sentence, refuted with arithmetic.** §4 item 1 called the meaning of "reproducible" for a
surviving object *"the single choice that moves the headline number more than any other"*. Under the
denominator §4 item 4 defines — bundles with ≥1 unique file — the delta between the two answers is
**0 pp**. It moves the per-file fraction from 37.5% to 75% and the bundle count not at all. I
nominated the wrong quantity as the hard one.

**The premise of §4 item 5 was false.** I worried that an offline verifier would become a cluster
client. `mod demo;` in `main.rs` carries no `#[cfg]`: the CLI has driven a cluster through `kubectl`
in *every* build, including `--no-default-features`, since v0.1.0. The published claim is narrower —
no network code is *linked*, and the offline property is scoped to `lapilli verify`. The real
question underneath was never the binary; it is the credential and the privacy surface.

**Round 24's line held where I feared it, and was crossed where I did not look.** The Kubernetes API
is not "another store" in round 24's sense — `DESIGN.md` §2 defines Lapilli as a reader of it. What
this proposal actually reopened was round 24's *other* holding, about honest-sounding names on
positive assertions.

## 4. Dispositions

| # | Finding | Disposition |
|---|---|---|
| A1 | 100% for every rule; the table restates `--event-ttl` | **RETURN TO PREMISE** |
| A2 | Zero bundles for every candidate rule | **RETURN TO PREMISE** |
| A3 | `api-unique` is a caveat in a name, biased toward the wrong ranking | **RETURN TO PREMISE** |
| A4 | Log rows score command success, not instance identity | **APPLY to any successor** — compare `container_id` against `lastState.terminated.containerID`; it is already recorded on both sides |
| A5 | Unbounded log fetch; content diff prints the redacted set and the dropped `last-applied-configuration` | **APPLY**: existence-only, `--tail=1 --limit-bytes=1024`, no content diff without its own flag and its own `docs/egress.md` row |
| M1 | A `--output json` report beside a bundle is a copy no published list accounts for | **APPLY**: explicit `--output-file`, refused under a bundle root, and added to `data-handling.md`'s sidecar table and erasure list in the same change |
| M2 | The read set is whatever the operator's kubeconfig holds, not the narrow list the document names | **APPLY**: derive namespaces from `incident.target`, print the verbs and namespaces before the first call, ship a `contrib/` Role |
| M3 | No cell below n=6 supports a percentage; 3-vs-3 total separation is p=0.10; units are not independent (round 31 F5) | **APPLY**: counts with denominators, no rate below a stated n, denominator in incidents not bundles |
| M4 | "the only artifact an adopter can run in week one" is refuted by §4 item 4 two pages later | **APPLY**: the claim is dropped |

Nothing was REFUTED; nothing was OVERRIDDEN.

## 5. The fork

Three candidates, and the two strongest came from the lens told to be hostile.

1. **`lapilli rules-coverage <rules.yaml>`** — read the team's own Prometheus rule files on a laptop
   and print the share of their rules the webhook would accept, which it would drop, and why. No
   install, no RBAC, no cluster, five minutes, before any security review. It is round 29 F4's first
   metric, it produces the per-rule table the fork wanted, and it is on the right side of round 24's
   line because it reads a file the operator hands it.
2. **A `kubectl` plugin that substitutes at the moment of need** — `kubectl lapilli logs <pod>
   --previous` serves the sealed bundle when the API answers *previous terminated container not
   found*. It changes what someone types at 03:07 rather than what they read in a report, and it
   measures itself honestly for free: every fallback that fires is one real unreproducible-evidence
   event, counted at the moment a human actually needed the bytes, with n growing from use instead of
   from a sweep at a clock offset the author chose. Prior art for the shape exists (Argo Workflows'
   archived logs); nothing in krew does it for plain pods.
3. **A witness index** — the webhook records a small unsigned descriptor for *every* alert including
   the ones it drops (namespace, kind/name/uid, container ids, restart counts, event count), and a
   later pass re-asks the API at T+1 h … T+7 d. This is the only candidate that can produce a
   perishability curve for rules Lapilli does not yet capture, and it is the most machinery.

Recommendation: **1, then 2.** 1 is the week-one artifact and answers the fork's question without a
cluster client. 2 is the only proposal so far that an on-call engineer said would change their mind,
and it turns the measurement into a by-product of use.

**Owner's decision (2026-09-30): 1, then 2.** `rules-coverage` is the next design target. It is a
smaller object than the last two — it reads a file the operator hands it, touches no cluster, stores
nothing — but it is not unattended: it parses YAML from outside the project, it ships in a public
release, and its whole output is a claim about somebody else's alerting. Those are the axes its own
round should attack.

## 6. What this round taught the method

**Two proposals in a row died of the same disease: the corpus, or the scope, was chosen by what
already existed.** Round 31 covered the alert rules whose collector was already written. Round 32
measured the bundles that already existed. In both cases the thing being decided *was defined by the
thing that made deciding cheap*, and in both cases a lens found it in one paragraph. The rule this
earns: **before proposing a measurement, write down what it would look like if the answer were the
opposite of the one you expect.** For `diff-live` that sentence is "a table where some rules score
low" — and five minutes with the event TTL shows no such table can exist.

**A hostile lens with a browser beat four careful readings.** As in round 31, the finding that did
the most damage was five words from somebody else's documentation: `--event-ttl` defaults to one
hour.

**A metric that cannot see its largest confounder should not be published, whatever its column is
called.** Naming it `api-unique` felt like honesty while it was being written. It is the same move as
a `coverage_score: 1.0` with a warning string attached, which this project refused two years of
rounds ago.
