# Design Review — Round 15 (the permission self-check)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
`crates/lapilli-controller/src/perms.rs` (new) and everything that reads it — the new series in
`telemetry.rs`, `LAPILLI_WATCH_NAMESPACES` in `main.rs` and `charts/lapilli/templates/deployment.yaml`,
the rows and alerts in `docs/metrics.md`, and the assertions in `test/e2e/run.sh`.

**Snapshot `4eb8d23cf9d990fc`** (`git hash-object` of the reviewed diff plus the new file). Two
lenses, one round. Both rotated: neither critic judged its own round-14 subject.

## Round 0

- **Category:** security-sensitive. The check makes claims about RBAC, `/metrics` has no
  authentication, and a CNCF Sandbox reviewer reads both.
- **Break-even:** cost ≈ 2 critic runs. Downside is two-sided. A check that misses a permission the
  code needs destroys the reason the feature exists; a check for a permission the code does *not*
  need raises a false alarm, and a false alarm is worse than silence here, because the only value
  this series has is the trust that green means the install is sound. Downside ≫ cost.
- **Sizing:** 2 lenses × 1 round. A security/RBAC lens told to verify the check list against the
  real call sites and against the chart, and a correctness lens told to mutation-test systematically.
- **Independence: not achieved (calibration only).** Both critics were Claude. One stood up its own
  kind cluster and broke each of the chart's three bindings in turn; the other ran 32 hand-written
  mutations against the suite. That is calibration, not independence; external review stays a
  release gate.

## Findings

### Security / RBAC lens

Verdict as given: **BLOCKER.**

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| S1 | BLOCKER | **`watch` is checked nowhere.** The controller is a ListWatch watcher, and `watch` is a distinct RBAC verb from `list`, so the single most load-bearing permission in the chart was the one permission this feature did not prove. | Tried "the poller's `list` would fail alongside it": the cluster transcript refutes that. With only `watch` removed from `role/lapilli`, `list incidentcaptures` answers allowed, all nine checks pass, and `lapilli_apiserver_poll_ok` reads 1 — while no alert is ever noticed. | **APPLY.** `watch` and `list` on IncidentCapture, and a test that one missing verb out of several fails the check. |
| S2 | MAJOR | The one-verb-per-resource canaries picked the wrong verb three times and skipped three resources. `collector.rs:173` does `pods.get(&target.pod)` with a hard `?` *before* any log is read, so an install with `list pods` + `get pods/log` but no `get pods` produces zero logs with every gauge green. Symmetrically `get controllerrevisions` is a verb the controller never issues — `diffs.rs:417` only lists. `get deployments`/`statefulsets`/`daemonsets` and `patch events` (kube-runtime's Recorder patches a repeat) were unchecked. | Tried "the chart grants get/list/watch together, so one implies the others": the chart does, but this check exists for when the chart's RBAC and reality diverge. And asking for a verb the code never issues is simply wrong in the other direction. | **APPLY.** `Check` now carries `verbs: &'static [&'static str]` — every verb the code issues, each citing its call site — and a test pins the whole `(group, verb, resource)` set. |
| S3 | MAJOR | The namespace a capture reads comes from the alert's `namespace` label, not from `watchNamespaces`. On a namespaced install an alert naming another namespace produces an unreadable capture with every check green; and a typo'd entry in `LAPILLI_WATCH_NAMESPACES` becomes a permanent critical page, because the API server answers a plain `allowed: false` for a namespace that does not exist — or is not even a legal name (`list pods@Team_A` → denied). | The false-alarm half has no defence. The out-of-scope half is pre-existing behaviour that this change does not make worse, and it is not silent: the capture comes out PARTIAL with collector failures counted. | **Split.** The malformed-entry half: **APPLY** — entries are validated as DNS-1123 labels and a bad one is dropped with an error rather than reported as a permission the cluster could never grant. The out-of-scope-namespace half: **VALID-OUT-OF-SCOPE**, documented in `docs/metrics.md` under "What it does not cover". |
| S4 | MAJOR | `/metrics` is unauthenticated by design, and a gauge per check turns it into a live capability inventory of the ServiceAccount — "the flight recorder cannot read pod logs right now" tells an attacker exactly when their actions will not be recorded — plus configuration disclosure: the mere *presence* of a conditional check reveals `signing.mode=static`, that export credentials sit in a Secret, or that the controller reads tenant ConfigMaps. | **Half succeeded.** The endpoint already lets a reader infer "the recorder is degraded" from `lapilli_notify_routes{state="error"}` and the export gauges, so degradation is a difference of degree. What survives is that the *capability map* and the *config disclosure* are different in kind: an unprivileged in-cluster attacker cannot enumerate another ServiceAccount's permissions without impersonation or `get roles`. | **APPLY.** Replaced by counts — `lapilli_permissions_denied`, `lapilli_permissions_unknown`, and `lapilli_permission_checks_total{result}`. The detail goes to the log. The cost is stated in the docs rather than hidden: metrics say how many, the log says which. |
| S5 | MINOR | `status.reason` was thrown away — the field where the API server names the missing object. Deleting a ClusterRole under a live binding returns `reason: rbac: … clusterrole "lapilli-collector" not found` with **no** `evaluationError`, and the operator got only "no capture can find the pod it was fired about". | None. | **APPLY,** root-cause-merged with S4: `reason` is now logged, and the log is where the detail lives. |

### Correctness lens

Verdict as given: **BLOCKER**, with **ten surviving mutations** out of 32 tried.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| C1 | BLOCKER | Same as S1/S2, reached independently by comparing `checks()` against the call sites, with two throwaway tests that fail on the current code. | None. | **APPLY** (merged with S1/S2). |
| C2 | MAJOR | **`group` is invisible to every test.** Round 14's lesson was applied to `name` and not to `group`, so the recorded-request fingerprint left it out: `group → None`, `group → "apps"`, and `create-captures`' group → `"definitely-not-lapilli.dev"` all left the whole suite green. On a real cluster the last one is a permanent critical page on the most important check with nothing actually wrong. | None. This is the project's recurring blind spot recurring inside the fix for the previous instance of it. | **APPLY.** The fingerprint is now `<group>/<verb> <resource>[/<sub>]@<namespace>#<name>` — nothing about a request is invisible — and the whole set is asserted. |
| C3 | MAJOR | `check_once` had no time budget, unlike `poll_once` in the same crate, which had been given one for exactly this reason one commit earlier. A blackholed authorizer stalls the pass for the client's 295 s read timeout. | None; a probe modelled on the existing blackhole test fails. | **APPLY,** and the fix went further than asked. A per-question bound alone leaves a pass running one timeout per question — about twenty of them — so there is now `ASK_TIMEOUT` (5 s) *and* `PASS_BUDGET` (30 s), with everything the budget did not reach reported as `unknown`. |
| C4 | MAJOR | A firing `lapilli_permission_ok == 0` **resolves** when the authorizer flaps, because `Err → None →` the series vanishes, and this repository documents in the same file that a rule on a vanished series cannot fire. So a denied permission plus a five-minute wobble reads as "RBAC fixed". | None; the trap is quoted from this project's own round-14 note. | **APPLY,** dissolved by the S4 redesign: counts are always emitted once a pass has run, so there is no series to vanish, and `unknown` has an alert of its own. |
| C5 | MINOR | `needs_from_cluster` had no tests at all, and the two `break`s were load-bearing but unpinned: six more mutations survived, including removing the `break` in the `Err` arm — which lets a later denial overwrite "could not ask" with "denied", collapsing the one distinction the module is built around. | None. | **APPLY.** Tests for the parsing (empty segments, a duplicate credentials Secret, a whitespace-only name, an unreadable profile), both `break`s pinned, and `credentials_secret` is now trimmed as well as checked for emptiness. |

**Attacks that failed, which is what makes the rest worth anything.** RBAC **aggregation** was probed
and is no blind spot (an aggregated ClusterRole answers correctly). `resourceNames` is honoured, so
the named-Secret design is right — verified both ways (`lapilli-signing-key` allowed, `other-secret`
denied, unnamed denied). `SelfSubjectAccessReview` needs no RBAC of its own, confirmed by a
ServiceAccount with **no RoleBinding at all** still getting an answer — which is precisely the case
the module exists for. SSAR cost was computed rather than guessed: ~305 sequential requests per pass
at 50 watched namespaces, ≈0.5 req/s, real audit-log volume but no APF or latency problem. The
startup ordering is sound: the webhook and `/healthz` listeners are bound before
`needs_from_cluster` is awaited, so a slow API server cannot fail the probes. Sixteen other
mutations — gauge inversion, `set_permissions` as a no-op, emitting `unknown` as `0`, empty
`checks()`, `missing()` counting `None`, scope swaps, dropping `verb`/`resource`/`subresource`,
`evaluationError`-as-denial — were all already killed. A 40-run flake hunt on the shared global
`metrics()` found 0 flakes.

## Two things the loop found that no critic asked for

**A vacuous test, caught by mutating it.** Making the boundedness test fast with
`#[tokio::test(start_paused = true)]` made it pass with `ASK_TIMEOUT` removed — under paused time the
295 s read timeout elapses instantly too. The bound was unverified. The test is now split: one
question's bound is checked in **real time** (about five seconds), the pass budget under paused time.
Confirmed: removing the per-question bound makes the real-time test fail after **296.42 s**, which is
the read timeout to two decimal places.

**The log the docs point at was not greppable.** Having decided to keep the permission detail in the
log rather than on an unauthenticated endpoint, the E2E then could not find it: the controller logs
with ANSI colour, so every field name is wrapped in escape codes and `kubectl logs | grep check=`
matches nothing. A pod log is never a terminal. `with_ansi(false)`.

## A process failure of my own

One of my edit scripts replaced a test body without asserting that the pattern matched, so the
replacement **silently did nothing** and only a compile error revealed it. The same class of mistake
as the `|| true` in round 14's fix: a step that cannot fail loudly will fail quietly. Every edit
script in this round now asserts its anchor.

## Verdict

**Applied, and not declared dry.** Both lenses returned a BLOCKER against code I had written,
mutation-tested myself, and believed correct — I had killed six mutations and ten more survived. The
central design assumption, one canary verb per resource, was wrong in both directions at once, and
the metric's shape had to be replaced rather than adjusted.

**Still open, logged as dependencies rather than fixed here:**

- The out-of-scope namespace (S3): an alert naming a namespace outside `watchNamespaces` still
  produces an empty capture. Documented, not prevented. Rejecting it at the webhook would break an
  install whose admin granted a ClusterRole by hand, so the fix needs a decision about which of the
  two is authoritative.
- No live coverage of the `unknown` path. The E2E produces denials (403) but nothing in the gate
  makes the API server stop answering; that path is covered by unit tests only.
- The `reason` enrichment is best-effort: a plain "no matching policy" denial carries an **empty**
  reason, as measured. Only the more interesting cases — a missing Role or ClusterRole — name the
  object.
