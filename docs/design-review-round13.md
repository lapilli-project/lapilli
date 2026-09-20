# Design Review — Round 13 (graceful shutdown, and the egress policy that was withdrawn)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifacts: the SIGTERM/shutdown change in
`crates/kairn-controller/src/{main.rs,notify.rs}`, and a chart-generated egress NetworkPolicy.

**Snapshot `f13e56658b8c5f21`** — the working tree plus `since-commit.patch`, the change under
review. Two lenses, one round, both rotated (no critic from round 12 judged its own subject).

## Round 0

- **Category:** security-sensitive and behaviour-defining. Shutdown runs on **every rollout**, not
  rarely, and a notification group is claimed before it is posted — so losing one on the way out
  marks captures notified that were never announced. The egress policy is a security control that
  an auditor reads.
- **Break-even:** cost ≈ 2 critic runs. Downside: a shutdown path that silently drops messages on
  every upgrade, or a NetworkPolicy that reads as an allowlist and enforces nothing. Downside ≫
  cost.
- **Sizing:** 2 lenses × 1 round. Correctness/concurrency for the shutdown, and a combined
  platform-engineer/security lens for the policy.
- **Independence: not achieved (calibration only).** Both critics were Claude. Both **ran the
  artifact** rather than reading it — one against the real `spawn()` with failing tests, one by
  installing the chart into a live kind cluster and reading back what the API server stored. That
  is calibration, not independence, and the external review remains a release gate.

## Findings

### Platform / security lens — the egress policy

Verdict as given: **should not ship in this form.**

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| G1 | BLOCKER | The admin's peers were spliced in at **rule** level while every documented example was written as a **peer**, so the API server pruned the unknown fields and stored `{}` — *allow all egress, everywhere*. `helm lint`, `helm install` and a values review all pass. Read back from a live cluster: `[… {"ports":[{"port":443}]}, {}, …]`, and `except: [169.254.0.0/16]` gone entirely. | None. A manifest that reads as a tight allowlist and enforces nothing is worse than shipping no manifest: it manufactures a green audit artifact. | **APPLY — withdraw the template.** |
| G2 | MAJOR | The flagship API-server example (`ipBlock: 10.96.0.1/32`, port 443) cannot work: `ipBlock` matches the **post-DNAT** destination, which on the test cluster was `172.21.0.2:6443` — different address *and* port. | None; verified against `endpoints/kubernetes`. | **APPLY** into the doc, with the `kubectl get endpoints` command and the DNAT rule stated. |
| G3 | MAJOR | `0.0.0.0/0` with a link-local hole is security theatre **and** it breaks credential modes Kairn advertises: EKS Pod Identity (`169.254.170.23`) and GKE Workload Identity (`169.254.169.254`) both live inside the recommended `except`. Also, the citation was wrong — `DESIGN.md` §11 is the roadmap; the promise is §7. | The `except` looked like defence in depth against SSRF. It is not: `kairn-net` already refuses link-local for every endpoint it parses, and `export.rs` does not use `kairn-net` at all — it uses `object_store`'s credential chain, which needs that range. | **APPLY.** Example deleted, the trap documented as a warning, the citation corrected to §7. |
| G4 | MINOR | The DNS rule had `namespaceSelector` but no `podSelector` (opening 53 to all of kube-system); `Chart.yaml` declares no `kubeVersion` for the 1.21+ label it relies on; NodeLocal DNSCache (`169.254.20.10`) matches no selector *and* sits inside the recommended `except`; missing peers: STS for IRSA, the metadata endpoints, and IPv6 (`0.0.0.0/0` does not imply `::/0`). | All checkable; all correct. | **APPLY** into the doc's peer table and checklist. |
| G5 | MINOR | `values.schema.json` accepted `[{}, {"totallyNotAField":"lol"}]`, and `scripts/helm-renders.sh` ended with `grep -qv "port: 53"` — vacuous, because `-q` exits 0 on the first line that does *not* match and almost none do. The same vacuous pattern was already there for `prometheus.io/scrape`, pre-dating this change. | None; I wrote both. | **APPLY.** Both greps fixed, and every render is now strict-decoded — see the note below about where that check had to live. |

**A third process failure, in the fix for G5.** I first added the strict-decoding check to
`scripts/helm-renders.sh` and described it as working offline. It does not: `--validate=strict`
needs the API server's openapi, so `kubectl apply --dry-run=client` still contacts a cluster — and
it contacted *whatever context happened to be current*, which is the same defect as process failure
2 below, introduced by the fix for the finding about silently-pruned fields. It passed when I ran it
because the current context happened to be reachable. The check now runs inside the kind E2E as a
**server-side** dry run, which is where a cluster exists and is pinned, and which is also the only
way to see a field the API server would prune — the thing G1 was about.

**What shipped instead:** `docs/egress.md` — the peer table derived from the code, the reason there
is no template (**NetworkPolicy v1 cannot match a DNS name**, and most of Kairn's peers are cloud
endpoints whose addresses change), the three ways to enforce it properly, and a worked Cilium
example. The critic's own framing is recorded there: cluster-wide egress policy is usually the
platform team's object, not an application chart's.

### Correctness lens — the shutdown change

Verdict as given: **NEW-BLOCKER.** Three independent holes, each with a failing test against the
real `notify::spawn()`.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| F1 | BLOCKER | `shutdown.notified()` is constructed **inside** the `select!`, so it is dropped whenever another branch wins — and `notify_waiters()` stores no permit, so that wakeup is gone for good. The flush then never runs, `drain` burns its whole budget, and every open group is abandoned. Measured against the real loop: **12 of 40 shutdowns flushed nothing** with a backlog. | Tried "surely the next iteration re-registers": it does, but a fresh `Notified` snapshots the current generation and can only fire on the *next* `notify_waiters()`, and there is none. | **APPLY.** `notify_one()`, which stores a permit. The feature did not work on a meaningful fraction of the rollouts it was written for. |
| F2 | MAJOR | `drain`'s 10 s is shorter than `post`'s own budget (3 attempts × 5 s + 2 s + 4 s ≈ 21 s), so the flush claims the group and then dies mid-retry — the claim spent, nobody told. The binding constraint was read as the 30 s grace period; it is the retry budget. Also `lookup_host` is unbounded, and dropping the runtime **waits** for blocking tasks. | None; the arithmetic is the argument. | **APPLY.** `ATTEMPTS_DRAINING = 1` for the flush (best-effort is right there — the evidence is already sealed), and `RESOLVE_TIMEOUT` in `kairn-net`. |
| F3 | MAJOR | The flush takes `open` but never drains `rx`: captures `enqueue` had accepted but the loop had not yet grouped are thrown away, and on restart the process-start gate claims and skips them. | None; test-backed. | **APPLY.** `while let Ok(p) = rx.try_recv() { admit(…) }` before the take. |
| F4 | MAJOR | **A regression this change introduced.** Handling SIGTERM at all meant the controller stream was dropped at t=0, cancelling every in-flight reconcile — where previously SIGTERM was unhandled, so a capture mid-collection ran until SIGKILL. `drain` protected the notification side only. | The critic downgraded it to MINOR for not having verified what a cancelled reconcile loses. I am **escalating** it: capture is the product, and a rollout must not cut one short. | **APPLY.** `Controller::graceful_shutdown_on` plus a 12 s `RECONCILE_GRACE` before the 10 s notification drain — both inside the default 30 s grace period. |
| F5 | MINOR | A line-continuation artifact put a run of spaces inside the drain-timeout warning — the one message an operator sees when F1 fires. Same pattern in `main.rs`'s plain-HTTP warning. | None. | **APPLY**, both. |

**Attacks that failed, which is what makes the rest worth anything:** `drain`'s reply handshake is
*not* racy (`Notified` registers at construction, and the critic's attempt to break it confirmed
the guarantee instead); the `tasks.join_next(), if !tasks.is_empty()` guard is load-bearing and
correct (without it the arm busy-spins and starves the timer); a panicking send surfaces as
`Some(Err(JoinError))` and both loops cope; there is no reap-arm starvation, because tokio's
polling order is randomised — which is also why F1 fires rather than being deterministic; `tick` is
cancel-safe (absolute deadline, rebuilt each iteration); and the `dispatch` extraction is faithful
— same order, same lock scopes, no `await` inside a `sent.lock()` in either version, and dropping
the unused `first_at` binding changes nothing.

## Two process failures of my own, recorded because they nearly shipped

1. **I reported F1 and F3 as applied when they were not.** The script that would have written them
   aborted on an earlier assertion, so nothing landed — and `grep notify_one` matched the *test* I
   had just added, so the check I used to confirm the fix confirmed the wrong file. Caught by
   re-reading `drain` itself. The lesson is the same one this project keeps relearning: the
   `detail: content` bug survived because the fixtures encoded the same wrong assumption as the
   code, and this survived because the test exercised the library primitive instead of the caller.
   **Generation and verification that share a blind spot agree with each other.** There is now a
   test that calls `drain` and fails if it goes back to `notify_waiters`; I checked that it fails.
2. **The E2E acted on whatever kubectl context happened to be current.** One run's commands went to
   a GKE cluster when the context changed underneath it mid-run. They were refused there for want
   of permission — luck, not a safeguard, since the suite does `kubectl apply`, `helm upgrade`,
   `kubectl delete pod` and `kubectl scale`. `run.sh` now creates its cluster into a kubeconfig of
   its own, exports `KUBECONFIG` to every sub-script, and refuses to continue unless the current
   context is the cluster it just made.

## Verdict

**Applied, and not declared dry.** Both lenses returned blocking findings against code I had just
written and believed correct, and F4 was a regression the change itself introduced. The kind E2E
then confirmed the fixed shutdown path on a live cluster (`the coalescing group was flushed on
SIGTERM, not abandoned`), which the pre-fix build did not.

What R14 should attack, if it runs: the `JoinSet` lifetime (a send aborted when the set drops after
`drain` returns), the interaction between `RECONCILE_GRACE` and a KMS-`Sealing` capture, and
whether `ATTEMPTS_DRAINING = 1` leaves the rate-cap notice unsendable at shutdown.

**Was still open, and owned by the repository rather than by this round:** there was **no metric
for API server reachability**. `/healthz` is deliberately decoupled from the API, so a controller
that cannot reach it stays `1/1 Running` while every capture stops — the one failure that halts the
product was the one nothing reported. `docs/egress.md` said to verify an egress policy with
`kairn demo` rather than pod status because of it.

**Built since**, and then taken apart by round 14 — see
[`design-review-round14.md`](design-review-round14.md). `kairn_apiserver_poll_ok`,
`kairn_apiserver_polls_total{result}` and `kairn_apiserver_last_success_timestamp_seconds`, read
from the state poller's own `list`: the work the controller already needs to do, so there is no
synthetic probe to disagree with reality. The probes were deliberately left alone, because
restarting the pod fixes neither a network policy nor an RBAC change and it cuts short a capture in
flight — the mistake F4 caught in the other direction.

The first version of it did not work. A poll that **hung** rather than failing never reported
anything, so the gauge sat at `1` for kube's 295 s read timeout — and a hang is precisely what a
dropped egress packet looks like from inside the pod, which is the case this document is about.
