# Design Review — Round 12 (notification: the implementation)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact: the implemented feature, not
a document — `crates/kairn-controller/src/notify.rs`, `enqueue_notification` and
`pack_into_place` in `reconcile.rs`, `crates/kairn-net/src/lib.rs`, the `notify` surface of the
chart, and `test/e2e/notify.sh`, reviewed against
[`design-notify.md`](design-notify.md) v1 (which round 11 produced).

**Snapshot `369a0bb2fa7ec757`** — a frozen copy of those 13 files; all three critics attacked
the same bytes.

## Round 0

- **Category:** security-sensitive **and** product-defining. It is a new egress path out of a
  cluster from a tool whose whole purpose is custody of evidence, and it is the feature meant
  to make the product warm enough to keep installed.
- **Break-even:** cost ≈ 3 critic runs plus the arbitration reading. Downside of being wrong:
  workload content or a webhook credential in a chat channel, a channel muted the first night,
  or a feature that cannot be installed from its own documentation. Downside ≫ cost.
- **Sizing:** medium-high → 3 lenses, up to 2 rounds. R1 lenses: **security/privacy**
  (mandatory — this sends data off-cluster), **correctness/concurrency** (a new tokio task, a
  filesystem claim, a coalescing window and a new call site in the reconcile loop), and the
  **on-call SRE / installing admin** (round 11's two BLOCKERs were both in this lens).
- **Independence: not achieved (calibration only).** All three critics are Claude. Each was
  told to run the repo's code and that a clean lens is a legitimate result; two of the three
  previous rounds found real defects this way, which is calibration, not independence. An
  external (human or non-Claude) review of the verifier and this egress path remains a release
  gate — [`independent-review.md`](independent-review.md).
- **Roles:** I wrote this code, so I am the author *and* the arbitrator. Per the constitution's
  modal case, every BLOCKER/MAJOR I refute or rule out of scope goes to a rotated critic for
  independent re-attack, and the converge round attacks **my fix diff** as a named slice.

## Arbitrator's own finding, logged before the wave returned

Not a critic's, and not applied to the frozen snapshot: **the retrieval command names the wrong
Deployment.** `retrieval()` prints `kubectl -n <ns> exec deploy/kairn`, but the chart names the
Deployment `include "kairn.fullname"`, which is `<release>-kairn` unless the release is itself
called `kairn` (`helm template myrel charts/kairn` → `name: myrel-kairn`). So the one
copy-pasteable line in the message fails for any other release name — which is exactly the
class of defect round 11's P2 was about. Held for the Apply phase with the critics' findings.

Two more, from adopting `kairn-net` in the CLI and the KMS client (reported by the
implementer, judged here). Both are cases of a server-side rule applied where the threat model
is different, and both are held for the same Apply phase:

- **`kairn verify https://…` now refuses a presigned URL that resolves into a private range**
  (`Reach::Internet`), so verifying against a self-hosted MinIO or an internal S3 gateway stops
  working. That rule exists to stop a *server* following a URL a lower-privileged user chose.
  Here the principal **is** the user: they typed the URL, they hold the credentials, and the
  response goes to their own terminal. Refusing private addresses protects nobody and breaks a
  real setup. → `Reach::Cluster` for the CLI: loopback and private allowed, link-local
  (metadata), multicast, broadcast and unspecified still refused, which costs nothing.
- **A 1-hour hard deadline appeared on a single bundle fetch**, invented because
  `kairn_net::client` can only express an overall timeout. Bundles are capped at 1 GiB, so 1 h
  is ~290 KB/s — a rate a real link can miss. The 60 s per-chunk stall guard is the check that
  actually matters, and it is already there. → make the overall timeout `Option<Duration>` in
  `kairn-net` so a streaming caller can say "no overall deadline, I bound the stream myself".

Accepted from the same report, no change needed: making `kairn-net` an **optional** dependency
behind the `remote` feature. `--no-default-features` promises a CLI with no network code, and
the controller image ships that build precisely so `kubectl exec` cannot turn it into a bucket
reader; a mandatory dependency would link reqwest, hyper and rustls back into it.

## Findings

### Security / privacy lens

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| S1 | BLOCKER | `Endpoint::display()` includes the **path**, and for a Slack webhook the path *is* the credential. So the live webhook URL is written to the INFO log on **every successful send**, at startup, and — via `kairn-net`'s error strings — into `status.notification.reason` on any DNS blip, where anyone with `get incidentcaptures` can read it. Demonstrated by printing all three sinks. | "The path is in a Secret, and logs are pod-local." Fails: logs go to an aggregator with far wider access than the Secret, and a CR's status is readable cluster-wide. It also contradicts this design's own text ("errors never print the URL", "fixed reason codes"). `display()`'s doc even reasons only about the query, which inverts the fact that a Slack path needs no query to be usable. | **APPLY.** `display()` elides the path as well as the query — `kairn-net` cannot know whether a caller's path is a secret, so eliding always is the only safe default. Transport text stops reaching `status` at all: `reason` becomes a fixed code. |
| S2 | MAJOR | Only the group **leader** gets a `.notified` claim, but `enqueue_notification` gates on *each capture's own* claim file — so the other N−1 captures of a grouped incident re-announce it on any later reconcile. A controller restart re-lists every object, so every historical grouped incident re-pages the channel. | "Does reconcile really reach the enqueue for a terminal capture?" It does, and the critic traced it: `Phase::Exported` → status exports → `drive_exports` → all settled → `enqueue_notification`. Verified by running a dispatcher twice over one bundle root: 4 of 5 members replayable. | **APPLY.** Claim **every** member before sending. That also makes the reconcile fast-path correct, which is what made the hole reachable. |
| S3 | MAJOR | `escape()` is applied twice to the namespace and owner (once inside the `format!`, again on the assembled header), so a real `&` renders as `&amp;amp;`; and the cap appends `…` *after* taking `max` characters, so the header is 151 at the boundary. A 63-character namespace (the legal maximum) plus a long `Deployment/<name>` is enough — no adversary needed — and Slack answers 400, which `post()` treats as final while the claim is already spent. The incident is then **never** announced. | "Is 150 really the limit, and is it reachable?" Yes on both: Slack caps a `header` `plain_text` at 150, and the critic produced 144/151/255-character headers from ordinary and adversarial names. The 16 KiB payload cap the design promises turned out not to be implemented at all. | **APPLY.** Escape exactly once, make the cap inclusive of the ellipsis and measured on the escaped output, and add the missing pre-send size checks (header ≤ 150, section ≤ 3000, payload ≤ 16 KiB with graceful degradation). |
| S4 | MAJOR | `pack_into_place` serializes the **full** `Summary` — `last_line` included — to `<incident>.summary.json`, so the one field the feature promises never to carry is written verbatim to a sidecar that is outside the hash tree, outside the signature, never read by anything, and never deleted. Prune the `.ieb` for retention and the plaintext log line outlives the evidence it came from. | Tried to find a consumer that needs it: `grep` shows none outside `summary.rs`; `enqueue_notification` reads `last_words`, `change`, `termination`, `memory` and never `last_line`. So it is a zero-benefit copy, and there is nothing to weigh against removing it. | **APPLY.** Write `.without_log_line()`. The critic's related point — that `facts_only()`/`without_log_line()` are **deny-lists**, so a content field added to `Summary` later would be sent by default — is taken as a pin test over the serialized field names, which fails closed at review time. |
| S5 | MINOR | The host lives in the ConfigMap while the credential lives in the Secret, and `update configmaps` is granted more freely than `update secrets`; also `RouteSpec` has no `deny_unknown_fields` and `Routes::load` does `secrets_dir.join(&name)` with an unvalidated route name. | The RBAC-gradient half does not survive: **`export.destinations` has exactly this shape** (`destinations.json`, same ConfigMap, settled in round 6), so anyone who can edit ConfigMaps in the controller's own namespace can already redirect where evidence goes. That is a property of the chart's configuration model, not something notification introduced, and the proposed `KAIRN_NOTIFY_HOSTS` pin would add a second place to keep the host in sync. The two concrete sub-findings do survive on their own. | **Split. APPLY** the name pattern check and `#[serde(deny_unknown_fields)]` (a filesystem read aimed by an unvalidated string is worth closing whatever the threat model). **VALID-OUT-OF-SCOPE** for the gradient: recorded as a documented residual, with its parity to `destinations.json` stated, not fixed here. |

### On-call SRE / installing admin lens

Every finding here came with a rendered message or a `helm template` transcript. This critic
worked from a clean overlay of the snapshot, not the live tree, and said so.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| O1 | BLOCKER | Independently found the `deploy/kairn` defect I had logged above, and went further: the message also prints bare `kairn` where every other caller in the repo uses `/usr/local/bin/kairn`, and for a 5-pod group the command retrieves **one** of the five bundles while two ids dangle unusable in the context line and two more hide behind `+2 more`. | None available — I had already confirmed the core of it with `helm template myrel`. The repo already knows the answer: `demo.rs` finds the controller by the `app.kubernetes.io/name=kairn` label, and `service.yaml` carries a comment saying the name varies with the release. | **APPLY.** Deployment name from the chart via `KAIRN_DEPLOYMENT`, absolute binary path, and a loop over every member id instead of the leader's alone. |
| O2 | MAJOR | **The strongest finding of the round.** A group is forgotten the instant it flushes, so every re-fire of the same alert on the same workload is a brand-new group and a brand-new message. Driving the real `admit()` loop: one crashlooping pod, one rule, one workload, Alertmanager's default 5-minute `group_interval` → **36 byte-identical messages in three hours**. The rate cap structurally cannot see it: 1 message per ≥5 min never reaches 10 per 5 min. | Tried "surely the claim stops it": no — the claim is per **incident id**, and a crashloop mints a new capture (and a new pod name) each time. Tried "surely that is what `maxPerWindow` is for": no, and that is the point — the cap is per route per window, so a drip is invisible to it. This is round 11's P1 ("the channel gets muted that night") in a different costume, and it mutes the channel on a Tuesday afternoon rather than at 3am. | **APPLY.** A per-`GroupKey` cooldown: inside it, count instead of posting; when it expires, post once carrying `×N since <time>`. Post immediately regardless of cooldown when the **verdict changes** — a new termination reason or a new rollout is the only repeat worth a message. |
| O3 | MAJOR | Under a node drain (80 captures, 9 namespaces, 6 deployments) grouping works — 20 groups — but `maxPerWindow: 10` posts 10 and **drops 10**, and the suppression debt is only ever carried by a *later* successful send, so when the storm is the last thing that happens nobody is ever told. The design doc asserts the opposite twice. Narrower bug in the same code: the debt is taken with `mem::take` *before* the POST is known to have succeeded, so a failed send eats it. | The mechanism half does not survive any defence — the doc claims "the channel never silently loses messages" and "can never outpace the channel", and both are false as built. | **Split. APPLY** the mechanism: post the suppression notice **immediately** on the first suppression in a window (with a `kubectl get incidentcapture` pointer) rather than deferring it, and take the debt only after a POST succeeds; correct both doc claims. **Not taken:** dropping the default from 10 to 5. Fewer allowed sends means *more* suppression, and with O2's cooldown in place the repeat traffic that motivated the number is gone. The immediate notice is the fix; the number is not. |
| O4 | MAJOR | Three install-time failures. **(a)** My chart's locality regex runs against `host[:port]` while the controller's runs against the host alone, so a cluster-local receiver on a non-80 port is refused by a message that contradicts the controller's own passing unit test. **(b)** A typo'd `pathSecret` renders a non-optional Secret volume, and with `strategy: Recreate` and one replica the old pod is deleted first — so a typo in a **Slack** secret name leaves the cluster with **zero** evidence recorders. **(c)** A route that loads but cannot deliver is invisible: no printer column, `status.notification` only on the group leader, no route-health gauge, and NOTES.txt mentions notification zero times even with a route configured, while the webhook token gets a create-secret command. | (a) and (b) are plainly my bugs. For (c) I tried "the startup log is enough": it is not — the webhook token, the directly comparable surface, gets a NOTES block *and* the export path got its own printer column, so this surface is the outlier, and `KairnNotificationsFailing` cannot fire until a real incident has already been missed. | **APPLY all three.** (a) strip the port before matching. (b) `optional: true` plus a startup `error!` — notification is a convenience, capture is the product, and it must never be able to take capture down. (c) a `kairn_notify_routes{state}` gauge with an alert, a `NOTIFY` printer column, and a NOTES.txt block with the `kubectl create secret generic … --from-literal=path=…` command. |
| O5 | MINOR | The largest, boldest line is the tool talking about itself (`📋 evidence captured ·`, identical in every message); the rollout line the design itself calls "the single most actionable fact" is rendered **fifth**; `PARTIAL`, the reason a number in the message might be wrong, is **last**; `(client-asserted)` is 17 characters of hedging inside the most important clause; and the retrieval commands render as ordinary prose with a `\` continuation and no copy affordance. | The observations are all checkable against the rendered output and the design's own priority claim, so there is nothing to refute. | **APPLY,** with one part scrutinized: putting the command block in `mrkdwn` triple backticks means it is no longer `plain_text`. That is safe **only** because every value in it — controller namespace, bundle dir, incident id, cluster id — is admin-set or pattern-constrained, and I am making that enforceable with a test that the block matches a strict charset. `COMPATIBILITY.md`'s promise is narrowed to the accurate one: strings that came from an alert or a workload are escaped and rendered `plain_text`. |

**The product question, asked and answered:** this critic judged notification *not* bolted on — the recorder's real defect was being cold, and a grouped one-screen summary with a working retrieval line addresses that rather than substituting for it; the postmortem draft the design names as a non-goal is downstream of this, so this order is right. But it called O1 and O2 fatal to that thesis as built: "as it stands the message either points at a Deployment that doesn't exist, or arrives 36 times in an afternoon."

### Correctness / concurrency lens

Six probes, all of which **fail** on the frozen snapshot; the probe file is kept at
`scratchpad/r12-correctness-probes.rs`.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| C1 | BLOCKER | Independently reached S2 — the leader-only claim — and produced the failing test: two POSTs for one incident, `left: Sent, right: AlreadyNotified`. Adds the mechanism: a kube watcher **relist** (watch expiry, every few minutes) is enough, and the duplicates render with *shrinking* pod counts ("5 pods", then "4 pods", …), each claim-distinct and each counted `sent`. | None — two independent lenses reached it and one has a failing test. | **Merged with S2**, one fix. Also takes C1's refinement: write the claim for every member **even when the leader's claim comes back `Ok(false)`**, or the stragglers keep re-entering. |
| C2 | MAJOR | `Summary::from_dir` sets `termination: Some(..)` whenever a container status exists — the `map` is on the container status, not on the `lastState.terminated` block. So a **running** pod is announced as `terminated, restart 0` while the same message says `no terminated instance` three fields later, and `is_empty()` can never be true for any pod that has a status. The "nothing to say" gate that O2's mute risk depends on is therefore **dead code**. | Tried "does any real alert hit this?": yes — `KubeContainerWaiting` and any memory or CPU alert fires on a live pod. There is no defence. | **APPLY.** Build `termination` only when the terminated block is actually present. This is a bug in `kairn-bundle`, so it also affects any other consumer of `Summary`, not just notification. |
| C3 | MAJOR | The `select!` timer branch `await`s `send()` **and** `report()` inline, so while one group's POST works through its retry budget `rx.recv()` is never polled: no other group flushes, no other route sends, and `enqueue` starts dropping once the 256-deep channel fills. Measured against a receiver that accepts and never answers: **21.0 s** for one group, serial per ready group. A dead route therefore delays healthy routes past the 120 s hard cap the code advertises — during exactly the storm coalescing exists for. | Tried "is it permanent loss?": no, dropped notifications recover on a later relist, which is why the critic rated it MAJOR rather than BLOCKER. That lowers severity; it does not excuse it. | **APPLY.** Flush off the coalescing task: `tokio::spawn` the send/report per ready group so the loop returns to `select!` immediately. The claim file already makes concurrent sends safe — which is what makes this fix cheap. |
| C4 | MINOR | `allow_send` hands out the suppression debt with `mem::take` **before** the POST is known to have worked, so a failed send destroys the count. Failing test: `left: Some(0), right: Some(1)`. | Same defect O3 spotted from the product side. | **Merged with O3.** Fix by *restoring* the debt in the `Err` arm rather than taking it later — the renderer needs the count before the POST. `spent` is deliberately **not** refunded: the attempt really did consume a slot. |
| C5 | MINOR | Reached S3's double-escaping independently and sharpened it: `escape(text, max)` truncates the **input** while the output can be 5× longer, so the 2800-character cap cannot hold a section under Slack's 3000 — measured **3269** characters — and the `…` is appended by input count, so a cut can land mid-entity. | None; merged. | **Merged with S3.** Takes C5's concrete shape: escape once at the leaf, never at the block level, and cap the *rendered* string by output length. |

**Below the bar for a slot, both with failing tests, both taken anyway because they are cheap
and both make the message state something false:** `verdict` counts *distinct reasons present*,
so one `OOMKilled` plus four pods that reported nothing renders "5 pods, same reason" — it will
now say `1 of 5 reported OOMKilled`; and this lens independently reached the `deploy/kairn`
defect.

**Clean axes, with the probes named** (these are coverage, not proof of absence): workload content on the **wire** path — a staged bundle with eight distinct canaries rendered through all four route shapes carried the log line, the termination message and the API-key canary in **none** of them, and content mode added exactly the changed field plus before/after; every Slack text object was `plain_text` in every mode and no ping or labelled link could be constructed (O5 deliberately makes the fenced command block `mrkdwn`, gated by `safe_commands`; the injection half of this probe still holds and is now a test); `kairn-net` parsing survived decimal-integer hosts, octal-dotted metadata addresses, `//host`, a scheme inside `host`, doubled ports, empty and trailing-dot hosts and CR/LF in the path; neither new filename can escape `bundle_root` or collide with another incident's files, across the whole suffix space; and no route name, host or path reaches a metrics label.

## Apply

Every disposition above is in the tree, with a test pinning it. Root-cause merges: S2+C1 (one
claim fix), S3+C5 (one escaping fix), O3+C4 (one rate-cap fix). Nine new tests in `notify.rs`,
two in `kairn-bundle`, plus the chart's render checks.

| Finding | What changed |
|---|---|
| S1 | `kairn_net::Endpoint::display()` returns scheme and authority only. `Route::post` returns a `PostError { code, detail }`; only the code reaches `status`, the detail is logged. |
| S2 + C1 | The dispatcher claims every member of a group once it settles, on the failed and suppressed paths too. The reconcile fast-path carries a comment saying it is correct **only** because of that. |
| S3 + C5 | `escape` caps the rendered output and is applied once, at the leaf; `fit` cuts a finished block without splitting an entity; `SLACK_HEADER`/`SLACK_SECTION`/`SLACK_CONTEXT`/`MAX_BODY` are enforced, with the body shedding detail rather than being rejected. |
| S4 | `pack_into_place` writes `.without_log_line()`, and a pin test over `Summary`'s serialized fields fails closed when a field is added without being classified. |
| S5 (split) | `name_ok` and `#[serde(deny_unknown_fields)]`. The RBAC gradient is **not** fixed, for the reason logged above. |
| O1 | `Site { namespace, deployment }` from `KAIRN_DEPLOYMENT`, which the chart sets to `include "kairn.fullname"`; absolute binary path; a `for id in …` loop over every member. |
| O2 | A per-`GroupKey` cooldown (30 min) keyed on `verdict_key` — the termination reason plus the rollout — with `×N more since 14:05` on the next message and a new `repeat` metric result. A changed reason or a new rollout goes out immediately. |
| O3 + C4 | The first group a window turns away posts `cap_notice` immediately, pointing at `kubectl get incidentcapture`; further ones are counted. The debt is restored when a send fails. The two false claims in the design doc are corrected. |
| O4 | The chart strips the port before its locality check; the route's Secret volume is `optional: true` so a typo'd Slack secret cannot leave the cluster with no recorder; a NOTES block with the create-secret command and the log line to look for; `kairn_notify_routes{state}` with its own alert; a `NOTIFY` printer column. |
| O5 | Header is the workload and the finding; `PARTIAL` leads; the rollout is second; `~` replaces `(client-asserted)` with one footnote; the commands are a fenced `mrkdwn` block gated by `safe_commands`, which falls back to `plain_text` rather than trusting itself. |
| C2 | Fixed in `kairn-bundle`: `termination` only when `lastState.terminated` is an object, `restarts` promoted to `Summary`. This was a bug for every consumer of `Summary`, not just notification. |
| C3 | Ready groups are `tokio::spawn`ed, so the coalescing loop returns to `select!` immediately. |
| sub-bar | `all N pods: <reason>` / `M of N pods: <reason>` instead of counting distinct reasons present. |
| arbitrator's own | The three held findings: the Deployment name (same as O1), `Reach::Cluster` for the CLI, and the invented 1-hour fetch deadline replaced by `Option<Duration>` in `kairn-net`. |

## R2 (converge) — attacking the fix diff

Snapshot **`22fc6693707d3cbd`** (the 2 736-line fix diff plus the post-fix files). Rotated
critics, per the rule that I am both author and arbitrator here: no agent that produced a
finding in R1 judged its own fix.

### E2E lens (writing the kind scenarios against the fixed code)

| # | Sev | Finding | Disposition |
|---|---|---|---|
| E1 | MAJOR | **A counted repeat was never claimed** and got no `status.notification`: the cooldown branch `continue`d before both. So it re-enqueued on every reconcile of that object — silently while the cooldown held — and once the group's verdict moved on, the stale capture no longer matched the cooldown entry and would be **announced as if it were news**. Found while writing the E2E, which had to delete the repeat capture to stop the next step going flaky — that workaround is the evidence. | **APPLY.** A counted repeat is settled, so it is claimed like any other outcome, and reported with `state: repeat`, `reason: in-cooldown`. This was a defect in R1's own O2 fix, which is exactly what a converge round is for. |
| E2 | MINOR | The crafted `pod` label cannot be tested end to end: the CRD pattern refuses the crafted `alertname` outright, and a capture for a nonexistent pod yields an empty summary, which is never announced — so no real message can carry it. Kubernetes will not accept a pod name containing `<` either. | **VALID-OUT-OF-SCOPE**, logged: the escaping is reachable only from a unit test that sets the group key directly, which exists. What the E2E proves instead is the invariant — no raw `<!channel>`, `<!here>` or `<url\|label>` in any request body, over the whole request log. |

The rewritten `test/e2e/notify.sh` (824 lines) asserts the header *structurally*, derives the
Deployment name at runtime rather than hardcoding it, requires **exactly one** `mrkdwn` block
opening and closing with a fence, requires **exactly five** `.notified` files for a five-member
group, reads every `<incident>.summary.json` off the node and checks it carries no log line, and
adds a repeat scenario (same verdict → no POST for 150 s; new rollout → a message carrying
`×N more since HH:MM UTC`). Deliberately still uncovered, and listed in the script: the rate cap
and its notice, `includeDemo`, `format: json`, and the exported-bundle retrieval line.

### Claim / concurrency / cooldown lens

This critic copied the tree, verified the production lines byte-identical to the snapshot, and
ran **the real `spawn()` dispatcher loop** against a loopback receiver with a settable status
code. Six failing tests; two passing controls. That is the strongest evidence any lens has
produced in this project.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| X1 | BLOCKER | **The cooldown was armed before the send's outcome was known.** `sent.insert` ran in the loop, before the spawned POST, so a group whose send failed *or that the rate cap turned away* left the map saying "announced at T" — and every re-fire for the next 30 minutes was counted as a repeat and never posted, **even after the endpoint recovered**. Failing test: receiver answers 503, then flips to 200, and a brand-new capture of the same workload produces **zero further requests, ever**. Worse without any network fault: a 200-group storm caps ~190 and mutes every one of those incidents for half an hour, with the contentless cap notice the only thing the channel sees. | None. This is the exact failure this feature was already bitten by once — "the claim is spent, so the incident was never announced at all" — reintroduced through timing rather than through a length limit. My R1 fix caused it. | **APPLY.** `Sent::delivered`, `sent` behind an `Arc<Mutex<…>>` so the spawned send can report back, and the cooldown test requires `delivered`. On any non-`Sent` outcome the carried repeats go back — the same asymmetry `restore_suppressed` already handles for the rate cap, which is exactly the parallel the critic drew. |
| X2 | MAJOR | **`verdict_key` was reason-exact, so an alternating reason defeated the cooldown completely.** A container that dies `OOMKilled` on one restart and `Error` on the next — the ordinary shape of a process that sometimes hits its limit mid-allocation and sometimes exits non-zero first — produced a different key every firing. Failing test: 4 posts inside one cooldown; at a crash every 2 minutes that is ~30 messages an hour. | I tried keying on the *set* of reasons across the group instead, and it does not work: each firing's group contains only that firing's members, so the set is still a single reason and still flips. The only thing that holds is removing the reason from the key entirely. | **APPLY**, and the mechanism is now different from what the critic proposed: the key is `rollout_key` — **only a new rollout is news**. A changed reason is folded into the count, and the reasons seen while quiet ride out on the next message as `also seen: Error, OOMKilled`, so damping does not hide the flap. |
| X3 | MAJOR | `verdict_key` read `leader()` — whichever pod the kubelet sealed first — while `verdict()` itself already prints `"N of M pods: reason"`, i.e. the code knows members disagree. Swapping two members of an identical group produced two different keys. | None; the inconsistency is in the same file. | **APPLY.** `rollout_key` is taken over the whole member set as a sorted, deduplicated string. |
| X4 | MAJOR | Two holes in R1's claim fix. **(a)** Every PVC written by the old build carries incidents where only the leader is claimed — the upgraded controller's first relist lets the survivors form a group, elect a new leader and **announce a closed incident again**. **(b)** A controller killed between the leader's claim and the members loop leaves them never claimed, and `main.rs` waits on SIGINT while the kubelet sends SIGTERM. | Both test-backed; (a) is reachable by every existing installation, which is the kind of "it only happens once" that still happens to everyone exactly once. | **APPLY.** Every member is claimed **unconditionally and before the POST**, including on `already-notified`, so the reconcile gate can see the claims while the send is still in flight. |
| X5 | MINOR | `sent.retain(… \|\| e.repeats > 0)` kept any entry with unreported repeats **forever** — defeating the bound the comment two lines above claimed — and `retain` sits in the tick arm, which does not run at all when `open` is empty. A retained stale entry also makes a genuinely new episode render `×N more since 14:05` with a count from the previous one. | The clause was mine and it was wrong: a lost count is a lost count, not lost evidence, and the map must be bounded. | **APPLY.** The clause is gone; with X1's fix the repeats are already given back on failure and ride out on success. |

**Attacks that failed, which is what makes the verdicts worth anything:** two groups sharing a
member via differently-resolved owners is unreachable, because `enqueue_notification` derives the
owner from the capture's own immutable `<id>.summary.json` and returns before enqueueing if that
read fails; two concurrent sends for one key is unreachable, because `admit` keeps at most one
open group per key and dedups by `incident_id`; two replicas racing is unreachable, because the
chart pins one replica with `Recreate`; `report()` cannot land out of order, because a capture
can lead at most one firing (the leader's claim is taken synchronously before any `await`); and a
200-group storm has real backpressure, because only `maxPerWindow` of the spawned tasks ever POST.

**Verdicts as given:** Fix A (claim every member) **WEAK** → now closed by X4. Fix B (send off the
coalescing task) **NEW-BLOCKER** → the mechanism was right but it split the loop's state from the
send, which is X1; closed by sharing `sent`. Fix C (the cooldown) **NEW-BLOCKER** → three findings
in it; rebuilt around `rollout_key` and `delivered`.

One thing this lens raised and I am **not** fixing here: `main.rs` waits on SIGINT while
Kubernetes sends SIGTERM, so a rollout can leave a capture claimed and never announced. The
critic itself judged this pre-existing rather than introduced by the fix — the inline send had the
identical exposure — so it is **VALID-OUT-OF-SCOPE**, logged, and belongs with the claim-before-POST
design rather than with this round.

### Escaping / rendering / what may be printed lens

This lens measured rather than argued: every finding carries a number it produced.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| Y1 | MAJOR | **`escape` flattened only `Cc`.** `char::is_control` misses U+2028/2029, the bidi overrides and isolates, the zero-width marks and the BOM — and the CRD's boundary pattern on `rule` refuses exactly `<`, `>` and `&`, i.e. precisely what `escape` already handled. So an alert author can put a line separator into the context element and write their own `kairn verify: OK (signed)` underneath Kairn's text: the round-11 spoof, reachable again without using a single blocked character. | Tried "line-break rendering is client-dependent": true for U+2028, but bidi reordering is not, and the fix is one match arm either way. | **APPLY.** `invisible()` covers the separators, overrides, isolates, zero-width marks and the BOM, and they render as a space. |
| Y2 | MAJOR | **The fenced command block was `fit` as a whole.** Measured: at 96 members the block hits the cap and the trailing `done` plus the closing fence are cut; from 105 on, ids vanish; at 200 pods only 103 survive while the context line shows 3 + `+197 more`, so 97 bundles are unretrievable from the message and the copy button yields an unterminated `for … do`. This falsifies my own as-built note 5 ("loops over **every** capture") at exactly the scale grouping exists for. | None — the numbers are the argument. | **APPLY.** The id list is budgeted inside `retrieval` (`MAX_IDS` = 40) with a `# and N more in this incident: kubectl … get incidentcapture` line; the text is fitted with room reserved for the fence, the fence is applied after, and `safe_commands` runs on **the text that is actually sent**. |
| Y3 | MINOR | `escape`'s budget compared a **rendered** count against an **input** count, so once an entity had inflated the output past the input length every remaining character was pushed unchecked — worst case ≈1.8×`max`; `escape("&xy", 6)` returned 7 characters. Blocks did not overflow only because `fit` re-capped each one. | None: this falsifies the doc comment I wrote on that function and the claim in the design doc. | **APPLY.** One budget check per character, on rendered length, with room reserved for the ellipsis only while input remains. `escape(_, 0)` is now empty rather than `…`. |
| Y4 | MINOR | `fit`'s entity repair searched the **entire** prefix, so a bare `&` with no `;` after it discarded everything: `fit("&" + "B"×5000, 2900)` returned `"…"`. | Latent today (the only caller passing unescaped text is the short fallback), but a one-character input collapsing a whole block is not a thing to leave in. | **APPLY.** The lookback is bounded to one entity's worth of characters, indexed by char boundary — the critic explicitly warned that slicing the last six *bytes* panics on multi-byte text, which it would have. |
| Y5 | MINOR | `safe_commands` was so narrow that legitimate object-store prefixes silently lost the copy button (`s3://evidence/team+platform`, `prod~1`, `a,b`, `prod#1`), and when the fallback *was* taken the text went out **unescaped**. | Only an admin can set `bundle_dir`, so this is defence in depth rather than a live injection — which is why it is MINOR, not why it should stay. | **APPLY.** The allow-list gains the inert path characters, and the fallback escapes. What is still excluded is what matters: no backtick, `<`, `|`, `*` or `@`. |

**Verdicts as given:** Fix A (escape/fit/limits) **WEAK** → Y1, Y3, Y4 closed. Fix B (the `mrkdwn`
block) **WEAK** → Y2, Y5 closed; the gate itself held under attack (backtick, `<`, `|`, `*`, `~`,
`@` are all outside the allow-list, so the fence cannot be closed and no link or mention syntax
can be formed), and the "pattern-constrained" claim was verified end to end against
`refuse_capture` → `path_safe`, `main.rs`'s cluster-id check and the chart's pattern. Fix C
(never printing the endpoint path) **HOLDS**, with the attacks named: seven secret-bearing paths
through `Route::new` all produce path-free errors; `display()` on a route with path *and* query
returns the authority only; reqwest errors go through `without_url()`; no metrics label, Event or
status field carries the endpoint; and `Endpoint` has no `Display` impl, so `%endpoint` cannot
compile into a leak. It also measured the limits as real: worst section 835 characters against a
2900 cap, worst body 5115 bytes of 16384, so the degrade branch is genuinely unreachable.

Two residuals it handed over, both now closed: `kairn_net::elide` kept the **path** and dropped
only the query, so a URL that failed to *parse* would print it — unreachable from notification,
but a trap for the next caller; and `kairn-kms` formatted reqwest errors without `without_url()`.

## Verdict

**Round 12 is time-boxed, not dry.** Two rounds, five lenses, 19 findings: 4 BLOCKERs, 11 MAJORs,
4 MINORs. Every one is applied with a test, and the whole workspace plus `release-check` is green.
But the honest reading of R2 is uncomfortable and worth stating plainly:

**Five of R2's seven findings were defects in fixes made during R1.** The cooldown I added to stop
36 identical messages muted incidents that had never been announced, and its key was defeated by
an ordinary alternating termination reason — producing *more* messages than the bug it replaced.
The escaping I tightened still let an alert author forge a line using characters that are
invisible rather than markup. The command block I made copy-pasteable silently dropped half the
incident ids at the scale grouping exists for. A round that finds that much in the previous
round's work has not exhausted its search, and I am not going to call it dry because the tests
now pass.

What makes the R2 verdicts worth something is the attacks that **failed**, each named with why:
two groups sharing a member is unreachable (the owner comes from the capture's own immutable
sidecar); two concurrent sends for one key is unreachable (`admit` keeps one open group per key
and dedups by incident id); `report()` cannot land out of order (a capture leads at most one
firing, and the leader's claim precedes any `await`); the storm has real backpressure (only
`maxPerWindow` of the spawned tasks ever POST); the `mrkdwn` gate could not be got through; and
the path-hiding fix HOLDS against seven probes. Those are the parts I would defend.

**Before the tagged release** (all of these are gates the owner holds, not things I can close):

1. **R3 on the render and cooldown slices**, with rotated critics. R2 left two WEAK verdicts that
   became HOLDS only by my own fixes going unexamined — which is exactly the situation the
   constitution says to re-attack rather than accept.
2. **Independence is still not achieved.** Every critic across rounds 11 and 12 was Claude. That
   catches correlated blind spots not at all, and five-of-seven regressions in one round is the
   shape of a system grading its own homework competently but not independently. An external
   (human, or non-Claude) review of the verifier and this egress path remains the release gate it
   already was.
3. **The rate cap and its notice, `includeDemo`, `format: json` and the exported retrieval line
   are not covered by the E2E** — listed in the script rather than left implied.
4. **`main.rs` waits on SIGINT while Kubernetes sends SIGTERM**, so a rollout can leave a capture
   claimed and never announced. Judged VALID-OUT-OF-SCOPE (the pre-fix code had the identical
   exposure) and carried forward as a dependency, not a clean bill.
5. The **ConfigMap/Secret RBAC gradient** stands as a documented residual, at parity with
   `export.destinations`, which round 6 settled the same way.

**What I would ship on:** the feature does what the design says, the invariants that matter
(no log line, no credential in a log or a status field, one message per incident, capture is never
delayed or failed by notification) are each held by a test that fails when the invariant breaks,
and the kind E2E exercises them on a live cluster. What I would not do is call the loop finished.
