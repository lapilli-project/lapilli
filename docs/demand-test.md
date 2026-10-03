# The demand test

Round 35 refuted *"zero adopters proves there is no demand"* on the grounds that **nobody had been
asked**, and the owner chose path B: fix the false claims, then test demand. This file is the second
half, written **before any answer exists**, because a decision rule invented after the answers come
in is not a rule.

`ROADMAP.md` item 8 describes what an install measures. This describes the conversation that has to
happen first — you do not ask someone to run a controller for a week before establishing that the
problem is theirs.

## 1. What is actually being tested

Not "is Lapilli useful". After round 35 the claim has been cut down to one sentence, and it is this
sentence that gets tested:

> **Kubernetes events expire after an hour, and the object as it was is overwritten by the next
> rollout. An incident review that happens days later cannot get either of them back.**

Everything else the project used to lead with is out of scope here because it is already refuted:

| not being tested | why |
|---|---|
| logs are gone | Loki's default retention is infinite; 82% of production clusters run a log store (CNCF 2025) |
| metrics are gone | Prometheus keeps 15 days by default |
| the capture is special | Robusta ships the same enrichers, enricher for enricher |
| portability across a trust boundary | round 35 §4 — real, recurring, paid, and **owned** by Replicated Troubleshoot |

Round 35 also produced a **null result that this test must be able to confirm**: what stalls an
incident review may be culture, incentives and time rather than evidence availability. If that is
what comes back, the test has succeeded and the answer is no.

### The sentence above is necessary and **not sufficient**, which this file got wrong at first

The residue is the *collection's* value, and round 35 established that the collection is **table
stakes**: Robusta fires on the same webhook and ships an enricher for each item in a bundle. So a
"yes, I lost the events" answer, on its own, does not support Lapilli — **it supports Robusta**, and
the honest thing to tell that person is to install Robusta.

What only Lapilli claims is the **seal**: the window as a file you own, that verifies offline, with
no credentials to anything. A demand test that does not reach that claim cannot distinguish "this
problem is real" from "this problem is real and already solved by a tool with 3,100 stars."

So the hypothesis has two parts and **both** must survive:

| part | what it establishes | if only this one survives |
|---|---|---|
| **A — the residue** | events and the object are genuinely lost | the need is real; **Robusta is the answer**, not Lapilli |
| **B — the seal** | the evidence had to leave the cluster's credential boundary, or had to be shown unaltered | the need is Lapilli's |

Part B is the one round 35 §4 looked for in the market and did not find — but it looked at vendors
and regulations, never at an operator. That is why it is still worth asking, and why a test that
skips it would have produced a false green.

## 2. The decision rule, fixed before asking

Ask **at least five** people who operate Kubernetes and are not the author.

| outcome | condition | what follows |
|---|---|---|
| **Alive** | **≥2 of 5** name expired events or the lost object **at layer 1, unprompted**, **and ≥1 of them** answers layer 2b with evidence that had to reach someone who could not reach the cluster, or that somebody questioned the integrity of | continue; proceed to ROADMAP item 8 and get one install |
| **Robusta, not Lapilli** | the layer-1 residue is confirmed, but **nobody** has an answer to layer 2b — the evidence never left the credential boundary and nobody ever questioned it | the need is real and **this project is not the answer to it**. Say so, recommend Robusta, and treat the seal as unbought: that is round 29's pause criterion on the differentiator rather than on the problem |
| **Ambiguous** | 0 at layer 1, but **≥3** say yes at layer 2 **and can name a specific past incident** | one more round of five, cold channel only (§4) — warm-channel agreement is not evidence |
| **Real need, no trigger** | layer 1 and layer 2b both land, but **nobody can name the event that would make them go looking** for a tool — no audit, no dispute, no repeated unexplained incident | the need is real and **unbuyable**, which is not the same as refuted. Round 36 added this row: every analogue with the install-before-the-incident property (flight recorders, Replicated's support bundles) was adopted by **mandate or default inclusion**, never by latent need, so the next question is which of those two is available — not what to build |
| **Dead** | 0 at layer 1, and the layer-2 yeses cannot name a specific incident | round 29's pause criterion applies **now**, not 2027-Q1 |

The second row is the outcome this file existed to make visible, and the first version of it could
not: a test that asked only about lost evidence would have returned "alive" for a population whose
correct answer is a different tool.

### What seven searches changed about the odds, and what they did not

Between the first version of this file and now, seven strategy searches ran and **all seven closed**,
each against a rule fixed before its result existed: three survival paths (round 35 §4), the
"lighter than the incumbent" analogy, four structural constraints in the incumbent (round 36 §2),
the warning-tier positioning (round 36), and ISMS-P 2.11.5. The last of those is the one that bears
hardest on this file, because it tested **layer 2b on paper**: the Korean certification control for
incident response has **no record-integrity requirement at all**, and its accepted evidence is a
hand-written report. The one ISMS-P control that does require tamper-evidence names **WORM media**
as the expected mechanism.

So the prior on this test is now worse than when it was written, and that is recorded rather than
softened. Two things keep it worth running anyway:

- **Everything closed so far closed on public evidence**, and the one channel that was never used is
  a person answering a question. 0 of 357 public postmortems naming lost evidence is a fact about
  what people **publish**, not about what happens — a distinction round 36's frequency lens stated
  itself.
- **The author's judgement has been wrong seven times in a row**, six optimistic and one pessimistic.
  A seven-for-seven record is a reason to distrust the eighth prediction in *either* direction, which
  is exactly what a test with a pre-committed rule is for.

Two rules that make the above mean something:

- **A layer-2 "yes" with no specific incident behind it is politeness, not data.** The question is
  designed to be easy to agree with. Agreement is only evidence when a date, a workload or an
  outcome comes with it.
- **Layer 3 is recorded and then discounted.** Stated willingness to install is the weakest
  evidence in the stack and it is not in the decision rule at all.

## 3. The script

**Do not mention Lapilli, or this repository, until layer 3 is finished.** The product is the
hypothesis; naming it contaminates the measurement. There is no version of this where leading with
the tool produces usable information.

### Layer 1 — unprompted recall (the only strong evidence)

> "최근에 포스트모템이나 장애 회고 쓴 것 중에, **보고 싶었는데 못 본 게** 있었나요?"
>
> *("Thinking of the last incident review you wrote — was there anything you wanted to look at and
> couldn't?")*

Then stop talking. Do not offer examples. Do not say "like events, or the pod spec". The entire
value of this layer is that the answer was not supplied by the asker.

Record the answer verbatim, and record **in which order** things were named — the first thing
someone says is what actually cost them.

### Layer 2 — prompted recognition (weaker, still useful)

Only after layer 1 is exhausted.

> "쿠버네티스 이벤트는 기본적으로 1시간 뒤에 사라지고, 오브젝트는 다음 롤아웃이 덮어씁니다.
> 이 둘 때문에 막힌 적이 **있나요, 없나요?** 없으면 없다고 해주셔도 됩니다."
>
> *("Kubernetes events are gone after an hour by default, and the object is overwritten by the next
> rollout. Has either of those ever blocked you — yes or no? 'No' is a fine answer.")*

The explicit permission to say no is load-bearing. Without it this question has a yes-bias and
returns nothing.

If yes: **"언제, 어떤 워크로드였나요?"** — if a specific incident cannot be named, record it as a no.

### Layer 3 — the counterfactual, with an exit (recorded, not counted)

> "그때 알림이 뜬 순간의 상태가 파일 하나로 자동 봉인돼 있었다면 그 장애가 더 빨리 끝났을까요?
> **아니면 결국 로그만 봐도 됐을 상황이었나요?**"
>
> *("If the state at the moment the alert fired had been sealed automatically into one file, would
> that incident have closed faster — or was it a situation where the logs alone were enough
> anyway?")*

The second clause is not padding. It is the only thing making "no" socially available, and without
it the answer is worthless.

### Layer 2b — the seal, which is the only thing that is Lapilli's

Asked of anyone who said yes at layer 1 or 2. Two questions, and **neither mentions a file**:

> "그 증거를 결국 **누구에게** 보여줬나요? 그 사람이 클러스터에 직접 접근할 수 있었나요?"
>
> *("Who did you end up having to show that evidence to? Could they reach the cluster themselves?")*

> "증거가 **고쳐지지 않았다는 걸** 증명해야 했던 적이 있나요? 아니면 아무도 그걸 묻지 않았나요?"
>
> *("Have you ever had to show that the evidence hadn't been altered — or did nobody ask?")*

The second question's own answer is the project's verdict. The seal exists to answer a question
somebody asks. If across five operators **nobody has ever been asked** whether their incident
evidence was tampered with, then the differentiator has no occasion to be used, and that is a
finding about the product rather than about the asking.

Record it even when the answer is "클러스터 접근 되는 사람한테만 보여줬다" / "아무도 안 물었다" —
especially then.

### The question that must also be asked

> "장애 회고가 느려지는 이유가 **증거가 없어서**인가요, 아니면 **시간이나 우선순위** 때문인가요?"
>
> *("Is what slows an incident review down the missing evidence, or the time and the priority?")*

This is round 35's null result, offered as a real option. If most people pick the second half, that
is the finding.

## 4. Channels, and the bias in each

| channel | signal quality | the bias to correct for |
|---|---|---|
| **Cold** — CNCF Slack (`#kubernetes-users`, `#observability`), r/kubernetes, a Kubernetes meetup you did not organize | **highest** | low response rate; no obligation to be kind, which is exactly why the answers are usable |
| **Warm** — colleagues, a community you organize | fast, high response | **people who know you will say yes.** Layer 2 agreement from a warm channel does not satisfy the Ambiguous→Alive path in §2 |
| **The repository** (GitHub Discussions, an issue) | ~zero | a 14-day-old project with no stars has no passing traffic; absence of replies here measures nothing |

The warm channel is still worth using **first**, for one reason only: it finds out whether the
question itself is comprehensible before it is spent on strangers.

## 5. Recording

Append to this file, one block per person — role, cluster scale, channel, and the layer-1 answer
**verbatim and first**, before any interpretation. Interpretation goes in a separate paragraph so
that a later reader can disagree with it without having to trust it.

Nothing has been recorded yet. The count is **0 of 5**, and until it is 5 this file is a plan and
not a result.

## 6. The message to actually send

A script nobody can send is not a test. These are ready to use. Neither mentions Lapilli, because
the product is the hypothesis and naming it contaminates the answer (§3).

**Cold channel** — a Kubernetes community, a meetup, a forum. Post or DM:

> 쿠버네티스 운영하시는 분들께 한 가지만 여쭙고 싶습니다. 제품 홍보가 아니고, 제가 세운 가정이
> 틀렸는지 확인하려는 것입니다.
>
> **최근에 장애 회고나 포스트모템 쓰면서, 보고 싶었는데 못 본 게 있었나요?**
>
> "없었다", "회고를 안 쓴다", "볼 시간이 없었다" 도 저한테는 똑같이 유용한 답입니다.

Then stop. Do not supply examples, do not mention events or object state, and do not follow up with
"like events, or the pod spec" — the entire value of layer 1 is that the answer was not suggested.

**Warm channel** — colleagues, a community you organise. Same question, with one sentence added
because people who know you will try to be helpful:

> 제가 만든 걸 정당화하려는 게 아니라 **반증하려는** 것이라서, 아니라고 해주시는 게 더 도움이 됩니다.

Round 36 is the reason that sentence is there: the author has been wrong seven times running, and a
warm channel's instinct is to confirm.

### When someone says yes at layer 1 but has no answer to layer 2b

That is the **"Robusta, not Lapilli"** row, and it is the most likely outcome. Say so, and be
specific rather than vague — this is measured, not a guess:

> 그 경우라면 Robusta 를 보시는 게 맞습니다 (MIT). 같은 Alertmanager 웹훅에서 트리거되고,
> `KubeDeploymentReplicasMismatch` 같은 워크로드 알림에는 **기본 활성화된 전용 플레이북**이 있어서
> 오너 체인을 타고 실패한 파드의 이벤트까지 가져옵니다. 그리고 Slack 으로 보내므로 **쿠버네티스
> 이벤트의 1시간 TTL 이 의미를 잃습니다** — enricher 가 이벤트를 복사해 나간 순간부터요.

Evidence for each clause, so the recommendation is honest and checkable: `helm/robusta/values.yaml:449`
(the `DeploymentReplicasMismatch` builtin), `playbooks/robusta_playbooks/event_enrichments.py:320`
(`list_pods_using_selector(ns, dep.spec.selector, "status.phase!=Running")`), and
`docs/design-review-round36.md` §4 for the Slack-retention finding.

### What a "yes" to layer 2b has to look like before it counts

Round 36 tested layer 2b against a written standard and it failed: ISMS-P 2.11.5 asks for a response
procedure and a report, with **nothing** about the record's integrity. So a vague "compliance needs
it" does not clear layer 2b. What clears it is one of:

- a person outside the organisation who had to be given the evidence and **could not be given
  cluster access** — and who asked how they could tell it had not been edited;
- a dispute, a claim, or an audit finding where the authenticity of an incident record was
  **contested**, not merely filed.

If the answer is "we put it in a Jira ticket and nobody asked", that is a **no** at layer 2b, and the
two HN practitioner voices round 36 found say exactly that about their own practice.
