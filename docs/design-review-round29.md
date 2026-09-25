# Design review — round 29 (the fork the owner asked for: keep Lapilli, or build something else?)

Constitution: **v0.7.1**. Target: not this repository but a decision — whether to continue
Lapilli or start a "Telemetry FinOps Controller" (a budget-driven, vendor-neutral reducer of
observability spend) that a separate conversation had made attractive. The decision memo and
the full log live in the owner's vault; this file keeps what the round found **about Lapilli**,
because it changes the roadmap and the design's landscape.

Six lenses in one wave: identity · the incumbent's user (for each candidate, rotated critics) ·
frequency & trigger · the CNCF TOC's own acceptance record · a devil's advocate run on a
non-Claude model (GPT-5). Independence: partial, one of six.

## 1. The decision

**Lapilli continues.** The alternative was refuted as an independent project — not because
it is hard, but because its gap closed in 2026: the OpenTelemetry Collector contrib now carries
a cardinality guardian (alpha), dynamic/adaptive tail sampling and a declarative telemetry
policy processor with provider extensions; Perses `metrics-usage` tracks which metrics
dashboards and rules actually use; OpenCost plugins cover observability billing. What is left
is three contributions, not a project. And under the pinned goal (Sandbox within twelve months,
solo), only Lapilli has an application event inside the window at all. Sunk cost was checked
for and is not the reason.

## 2. What the round found about Lapilli — and applied

**F1 · The Sandbox threshold was misread (TOC lens, CRITICAL).** The roadmap said what was left
was "five owner inputs and one outside install". The TOC's own postponement language in
2025–2026 is uniform: *one active maintainer, no adopters listed, limited community* — and no
single-maintainer, zero-adopter project was accepted in that period. Review sessions run about
every two months, first come first served; a postponement costs six to twelve months. So:
the code work is done and the community work has not started. Application target **2027-Q2**,
with no room for a failed first attempt; conditional odds honestly 20–30%. TAG Observability
was archived in 2025-12 — the reviewer is **TAG Operational Resilience**, the same TAG that
reviewed HolmesGPT, which is the project the TOC will compare Lapilli to (not Velero). Applied
to `ROADMAP.md` §1–§3.

**F2 · The record from round 23 contains a factual error.** "Audit logs give only the submitted
object" is wrong: at `RequestResponse` level the `responseObject` is the object as persisted,
after admission and defaulting, and GKE's default policy records create/update/delete at that
level. What audit logs do *not* give: ConfigMap bodies (Metadata level), kubelet status
writes (patch fragments), anything for a team without an audit pipeline, and a point-in-time
object without reassembling the last write. That is the residual, and it is narrower than
round 23 said. Applied as a correction note in `docs/design-review-round23.md` and a rewritten
differentiation line in `DESIGN.md` §3.

**F3 · A landscape omission: salesforce/sloop.** Sloop records resource state history and
lets you inspect resources that no longer exist — the open-source occupant of "the object as
it was", with a Helm chart and 1.6k stars, absent from `DESIGN.md` §3. Lapilli's claim cannot
be "the point-in-time object body"; it has to be **the object *and* its status, the previous
container's log and the rollout diff, sealed together at the alert into one portable file,
with no audit pipeline required**. Applied: Sloop and HolmesGPT rows in the §3 landscape.

**F4 · The adoption killer is trigger coverage, not silence.** The open item "Lapilli emits
nothing between incidents" is stale (41 series, 26 rules, a notification per capture) and a
non-issue for an operator tool (Velero and node-problem-detector are silent too). The real
problem: alerts without a `pod` label are counted and dropped, and the alerts an SRE cares
most about — SLO burn rate, `KubeDeploymentReplicasMismatch`, HPA maxed out, node conditions
— carry no pod label. Lapilli fires on exactly the class (crash loop, OOM) that git diff plus
a log store already answers. Applied: trigger coverage is the first v0.2 item, with two
metrics — the share of a team's rules the webhook accepts, and bundles read ÷ bundles sealed.

**F5 · The install trigger must not depend on a postmortem.** A probe of eighteen public
Kubernetes postmortems found none that names lost evidence; people close postmortems with a
guess rather than write down what they could not find. A tool whose install trigger is "the
retrospective where evidence was missing" has a trigger frequency near zero. Applied: the pitch
becomes part of the alert pipeline's standard (an Alertmanager receiver in the
kube-prometheus-stack values example), and the first outside install carries three counters
for ninety days: bundles sealed, bundles opened, postmortems that cite one.

**F6 · Contributors come from an extension surface, not from touch frequency.** Contributor
counts across comparable tools track how much a third party can plug in, not how often the
tool is used. Lapilli has none today. Applied *with a constraint the identity imposes*: the
surface opens on the Kubernetes-native side (collectors) and the consumer side (readers,
exporters, notify routes) — never as a fetcher from a vendor's store, which round 24 already
returned to premise.

**F7 · A pitch line that costs no code.** For teams that sample logs at ingestion: *"sample
cheaply — the previous container's tail, the events and the object bodies your sampler never
saw are what Lapilli reads from the API at the alert."* Kept as a GTM sentence. The other half
of that idea (an OpenTelemetry Collector processor Lapilli would contribute) was dropped: it is
the pattern round 27 withdrew.

**F8 · Small corrections.** `DESIGN.md` §11 still said the KMS real-cloud smoke was outstanding
(GCP was done on 2026-09-25, `test/fixtures/kms/`). The employer's cluster is not a conflict
of interest to the TOC but a candidate adopter, with the employer's consent.

## 3. Pause criterion

If by the end of **2027-Q1** there is neither a second maintainer nor a named adopter, Lapilli
pauses and the goal is asked again. Written down now so the decision is not made by fatigue.

## 4. What this round taught the method

The incumbent's-user lens must stamp its research date: every fact that refuted the
alternative is a commit from 2026-07 to 2026-09, and the same round three months earlier
would have answered differently. And when the goal is a listing, the listing body's own
decisions are a lens on the floor, not an optional one — without the TOC lens, "code complete
≈ close to listed" would have survived.
