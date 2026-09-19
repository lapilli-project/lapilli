# Design Review — Round 4 (v0.2 change-diff; loop engineering, 4 inner rounds)

Constitution: **Loop Engineering Constitution v0.4.0** (amended to v0.5.0 by this loop).
Artifact: [`design-change-diff.md`](design-change-diff.md). Outcome: **time-boxed, not dry**.
The last two fixes (R4) were folded without a further attacking round, so they carry
required kind E2E canaries (⚑ in the design).

## Round 0 — scope and gates

- **Target:** the v0.2 "real spec change-diff" design, reified first as a draft document
  (snapshot v0), since no written design existed.
- **Category:** architecture decision + security-sensitive (a stateful subsystem was on the
  table, RBAC growth, and user workload data in a shareable file).
- **Break-even:** cost ≈ 9 critic runs + arbiter time. Downside of shipping wrong: weeks
  building a stateful recorder; controller memory blowups; credentials leaking into portable
  bundles; confidently wrong "what changed" answers during incidents. Downside ≫ cost.
- **Sizing:** Medium-High. Privacy and security lenses mandatory (user workload data).
- **Roles:** Claude drafted and arbitrated (author = arbiter), so REFUTED/OUT-OF-SCOPE
  dispositions at BLOCKER/MAJOR were re-checked by a rotated critic.
- **Independence: not achieved (calibration only).** All critics were Claude. Partly
  compensated by **measuring** the load-bearing Kubernetes claims on a live kind v1.37
  cluster instead of trusting any critic.

| Round | Snapshot | Critics | Result |
|---|---|---|---|
| R1 diverge | v0 `5388c1950a5f` | K8s mechanics · security/privacy · scope/product · format/consistency | 2 BLOCKER, 12 MAJOR, several MINOR |
| R2 converge | v1 `7e07185231ac` | rotated: mechanics · security · scope/consistency | 4 NEW-BLOCKER (2 share a root cause), several WEAK |
| R3 confirm | v2 `513e355cb3f8` | one strong critic on the v1→v2 diff | 5 MAJOR, 3 WEAK |
| R4 confirm | v3 `c48f8f97bd4b` | one strong critic on the v2→v3 diff | 2 MAJOR → folded into v4, not re-attacked |

## R1 — what died, what changed

**Cut: the always-on ConfigMap recorder ("Layer 2").** All four lenses attacked it
independently. Each problem below survived a steelman attempt:
- With hash-only storage it could not show `lazy → eager`.
- A kube-rs reflector holds every ConfigMap in plaintext, which the 32 MiB budget didn't cover.
- Unsalted SHA-256 of short values is a lookup table.
- Eviction from the ring lets anyone who can write a ConfigMap erase evidence.
- It needs cluster-wide `list/watch configmaps`, reaching `kube-system/aws-auth`.

Kept as a deferred section listing the requirements any future recorder must meet. APPLY.

**Added: Layer 1.5.** Content changes that arrive as a new ConfigMap name (kustomize
hash suffix, immutable ConfigMaps) are diffed with two GETs, with no watch and no state.
APPLY (mechanics + scope, independently).

**Timing/pairing** (mechanics + format + scope, independently): "current vs previous RS by
creationTimestamp" fails on rollback (the reused RS keeps its old creation time), on a pod
belonging to the old RS, on paused Deployments, and when several rollouts land in one window.
A remediation rollback after the alert could even be reported as the cause. APPLY: anchor on
the pod's own revision; emit every pair in range with `after_firing`; pending edits
separately.

**Silent diff failure** (format, BLOCKER): no way to record a failed diff, and the v0.1
collector already swallows owner-chain GET errors with `if let Ok`. APPLY: per-entry
`status`, an `expected` list, and a PARTIAL rule.

**Redaction** (security, BLOCKER): env literals already go raw into `resources/`. DESIGN
described a redactor that did not exist. A diff adds the *previous* value, which is often
the leaked credential that was just rotated. This produced the loop's **one FORK**: redact
everything by default (safe, but kills the diff's point) vs pattern-based (useful,
best-effort). **Owner decided: pattern + entropy, best-effort, with a strict mode.**

**Actor forgeability** (security, MAJOR): partly APPLY (label `fieldManager` as
client-asserted). The detection half was **REFUTED** by the arbiter ("RS `generation` can't
tell scale from template edit; §5 disclaims fidelity"), and was then **overturned in R2 by
the independent re-check**: managedFields fieldsets *do* tell them apart, at no extra cost.
Now a spoofable-hint `warnings` entry.

**Doc overclaims found** (scope, MINOR): DESIGN §4 claimed "ConfigMap content hashes" and a
deterministic redactor that don't exist. APPLY.

## R2 — the fixes that broke, and the measurement that saved one

- **Change time from RS managedFields** (NEW-BLOCKER): a managedFields `time` is per
  manager, not per field. The controller's single entry moves on every scale, including
  HPA, so a days-old change could read as seconds old.
- **Actor from the RS write** (NEW-BLOCKER): that write is the deployment controller, so the
  actor would always be `kube-controller-manager`.
- **Entropy rule over all of `resources/`** (NEW-BLOCKER) would redact UIDs, containerIDs and
  digests. It had the same root cause as a missing field-scope table, so both went into one
  normative table.
- **Format** called itself RFC 6902 while pointing into "after", so it couldn't express
  `remove`. The merge-key `display` became the normative identity.

**Measured on kind before folding**, because the critic said it hadn't checked a live cluster:
- the managedFields drift was confirmed;
- the reused RS keeps its creationTimestamp;
- **the critic's own fix was wrong**: it proposed the *earliest* "Scaled up … from 0" event,
  but identical events coalesce, so the rollback survives only as the *latest* timestamp;
- after a rollback, the Deployment's only template owner can be the original
  `kubectl-create` entry, so the actor must be matched by time or be `null`.

This became Constitution v0.5.0's "measure before folding".

## R3 — confirm (5 MAJOR, all APPLY)

- **Per-character entropy can't exceed log2 of the alphabet size.** Hex tops out at 4.0, so
  the 4.5 threshold could never catch hex keys. Per-charset thresholds replace it.
- **Event time was applied to revision numbers kept in `revision-history`.** It now applies
  only to an RS's current revision; the others are `unknown`.
- **`expected` owners of unsupported kinds** (Job, Argo Rollout) would make every such
  bundle PARTIAL. A new `unsupported_kind` status fixes that.
- **The client-go event recorder aggregates and spam-filters events.** Matching now accepts
  the "combined" prefix and uses series fields, and sanity checks were added.
- **Structural redaction gaps:** env values and scripts weren't tokenized (`-D…password=`,
  `export X=`), JDBC query-string passwords were missed, JWTs were missed, and useful URLs
  and paths were over-redacted.

**Arbiter rule logged:** redaction is best-effort by owner choice. A single missing pattern
is MINOR; only structural gaps are MAJOR. This became Constitution v0.5.0's "bound
open-ended categories".

## R4 — confirm (2 MAJOR, folded; the loop's budget ended here)

1. **Scale-from-zero** (KEDA, nightly jobs) emits "from 0" and passed every sanity check,
   so it read as a fresh change. Fix: a never-reused RS uses its exact `creationTimestamp`.
   A reused RS's event is accepted only together with a predecessor "scaled down … to 0"
   event. ⚑ canary.
2. **ConfigMap `binaryData`** had no redaction scope, so a keystore could land raw in
   `diffs/`. Fix: always redacted. ⚑ canary.

Several MINORs were folded too: "live pod = no deletionTimestamp", classification by the
top controller, a value check on URL path segments, a documented `null` rate for the actor,
and a correction to the Helm driver wording.

## Verdict

**Time-boxed, not dry.** The trend was 2 BLOCKER → 4 NEW-BLOCKER → 5 MAJOR → 2 MAJOR, and
the later findings were more local and less structural. Neither R4 fix changes the design's
shape. Because they were not re-attacked, both are **required kind E2E canaries** in the
implementation, and the timing heuristics in general are to be proven by kind tests rather
than more critic rounds.

Carried into implementation as requirements:
- canaries: scale 0→N yields no in-range entry; a `binaryData` keystore never appears; a
  planted credential in every candidate path never appears; negative vectors (image refs,
  service URLs, JVM flags, paths, UIDs) stay visible;
- E2E scenarios: rollout, rollback, rollout+rollback in one window, paused Deployment,
  Recreate strategy, HPA-style scale during the window.
