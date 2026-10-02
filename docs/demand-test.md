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
| **Dead** | 0 at layer 1, and the layer-2 yeses cannot name a specific incident | round 29's pause criterion applies **now**, not 2027-Q1 |

The second row is the outcome this file existed to make visible, and the first version of it could
not: a test that asked only about lost evidence would have returned "alive" for a population whose
correct answer is a different tool.

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
