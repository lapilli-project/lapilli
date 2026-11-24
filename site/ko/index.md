---
layout: default
home: true
lang: ko
permalink: /ko/
title: 그 알림, 사람을 호출할 만한가요?
description: "Lapilli는 울린 알림 하나하나를 실제로 일어난 장애와 맞춰 봅니다. 장애를 잡았으면 정탐, 아무 일도 없었으면 오탐. lapilli verdict로 한 번, lapilli serve로 매시간."
---

<p class="tags"><span class="tag">Apache-2.0 · 오픈소스</span>{% if site.version %}
<a href="{{ site.repo }}/releases/tag/{{ site.version }}">Lapilli {{ site.version }} 릴리스: 바뀐 점 보기</a>{% endif %}</p>

# 그 알림, 사람을 호출할 만한가요?

<p class="lede">Lapilli는 울린 알림 하나하나를 실제로 일어난 장애와 맞춰 봅니다.
장애를 잡은 알림은 정탐(흰 돌), 아무 일도 없었던 알림은 오탐(검은 돌)입니다.
룰마다 정탐과 오탐을 세어 보면 그 룰이 제 몫을 하는지 알 수 있고, 어떤 알림도
잡지 못한 장애는 미탐 장애로 드러납니다.</p>

<div class="cta">
  <a class="primary" href="{{ '/ko/docs/getting-started/' | relative_url }}">시작하기 <span aria-hidden="true">→</span></a>
  <a href="{{ '/ko/docs/' | relative_url }}">문서 읽기</a>
  <a href="#help-build-it">함께 만들기</a>
</div>

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```

<p class="hint"><code>lapilli demo</code>는 작은 쇼핑몰의 알림 일주일치를 지어낸
데이터로 정탐·오탐을 가려 봅니다. 연결할 것이 없습니다. <a href="#install">바이너리, 컨테이너
이미지, Helm 차트</a>로도 설치할 수 있습니다.</p>

<div class="shot">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-dashboard-dark.png' | relative_url }}">
  <img alt="lapilli serve 개요 화면. 왼쪽 사이드바에 메뉴가 있고, 지난주 정탐률, 알림 수, 미탐 장애, MTTD(없음), 노이즈 룰, 자는 동안 울린 오탐 알림, 재발생 알림, 배포 중 알림이 카드로 나옵니다. 카드마다 주별 추이 선과 전주 대비가 있고, 그 아래 '먼저 볼 것'에 조용히 할 룰과 살펴볼 룰이 작업으로 만드는 버튼과 함께 있습니다" src="{{ '/serve-dashboard.png' | relative_url }}" width="1240" height="980">
</picture>
</div>

<ul class="facts">
  <li><b>읽는 곳</b> Prometheus와 그 API를 쓰는 저장소, Grafana, Loki, 클라우드 자체 알람</li>
  <li><b>도는 방식</b> <code>lapilli verdict</code>로 한 번, <code>lapilli serve</code>로 매시간</li>
  <li><b>저장</b> SQLite 파일 하나, 또는 여러 복제본이 함께 쓰는 Postgres</li>
  <li><b>변경</b> 기본은 읽기만 하고, 사람이 승인한 것만 바꿉니다</li>
</ul>

이미 있는 것으로 동작합니다. 알림 룰, Prometheus에 쌓인 이력, 그리고 갖고 있는
장애 기록이면 됩니다. 기록이 빈약해도 괜찮습니다. 기본은 읽기만 하고, 사람이
승인한 것만 바꿉니다.

아직 초기 단계이고, 메인테이너는 한 명이며 더 필요합니다. 운영 중인 Grafana 한
곳에서 확인했습니다. 알림 룰 71개가 모두 백테스트되었고, 당시 룰 그대로 4주를
백테스트하니 가장 시끄러운 룰의 알림이 177건으로, 그 팀이 손으로 센 알림 수와
같았습니다.

## 알림이 울렸습니다. 그다음은요? {#the-alert-fired-then-what}

알림은 무언가 기준선을 넘었다는 것만 알려 줍니다. 사람을 호출한 것이 옳았는지는 알려 주지 않습니다. 그래서 세 가지 질문이 남습니다.

<div class="grid">
  <div><h3>정말 문제가 있었나요?</h3><p>새벽 세 시에 호출을 받았는데 고칠 것이 없었습니다. 한 번뿐이었을까요, 이 룰이 늘 그런 걸까요? Lapilli는 룰마다 장애를 잡은 알림(정탐)과 아무 일도 없었던 알림(오탐)을 셉니다.</p></div>
  <div><h3>그 장애를 잡은 알림이 있었나요?</h3><p>알림보다 고객이 먼저 장애를 찾았습니다. Lapilli는 어떤 룰도 잡지 못한 미탐 장애와, 거의 잡을 뻔한 룰을 보여 줍니다. 조건은 맞았지만 <code>for</code>를 채우지 못했거나 임계치 바로 아래에서 멈춘 룰입니다.</p></div>
  <div><h3>이 변경은 무엇을 바꿀까요?</h3><p><code>for</code>를 늘리고, 임계치을 바꾸고, 룰을 없앱니다. 짐작으로 병합하고, 결과는 다음 장애 때에야 압니다. Lapilli는 병합하기 전에 지난 몇 주에 대해 그 변경을 백테스트합니다.</p></div>
</div>

## 한 번: `lapilli verdict` {#once-lapilli-verdict}

일주일, 한 달, 또는 병합하려는 변경을 백테스트하면 다음을 받습니다. 정탐과 오탐,
괜히 사람을 깨운 알림, `for`를 늘리면 조용해질 노이즈 룰, 아무 알림도 잡지 못한
장애, 그리고 그 장애를 거의 잡을 뻔한 룰(니어 미스)입니다.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx        critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE                FOR       FIRED     INCIDENTS  CAUGHT LATER BY
ExportJob5xx        5m → 15m  118 → 31  0          –
CheckoutLatencyP99  5m → 10m  14 → 6    1          up to 4m

Out of hours, in Asia/Seoul (at work mon-fri 08:00-18:00, asleep 22:00-08:00):
of the firings that caught nothing, 57 came while people slept, and 37 more off hours.
RULE                SEVERITY  ● ASLEEP  ● OFF HOURS  ● AT WORK
ExportJob5xx        critical  52        31           35
NodeNotReady        critical  3         0            1
ReadReplicaLag      warning   2         1            5
CheckoutLatencyP99  warning   0         4            8
DBPrimaryCPU        critical  0         1            0

What each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:
RULE                INCIDENTS  FIRST  ALONE
CheckoutLatencyP99  1          0      0
DBPrimaryCPU        1          1      1
ReadReplicaLag      1          1      1
A noisy rule's catch doesn't count beside one by a rule that isn't noisy.

38 rules never fired: CertificateExpiring, DiskWillFillIn4h, EtcdNoLeader, KafkaUnderReplicated, NodeClockSkew, PodCrashLooping, PVCAlmostFull, RedisDown, TargetDown, WatchdogMissing and 28 more, all in --json.

INCIDENT                         START             LASTED  CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  38m     DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  1h12m   ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  –       ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  24m     ✗ nothing fired

✗ 2 of 4 incidents had no alert.
! --impact-for 5m counts impact that lasted 5m as an incident, but 9 of the 43 rules judged fire only once
  their condition held longer: their "for" and one evaluation, with no "for" an evaluation (6m for 6
  of them). Impact shorter than that they can't be sure to catch. --impact-for 6m counts only what they can.

Rules that nearly caught them: their condition held, but not for their "for", or fired too late; or their value came within a fifth of their threshold.
INCIDENT  RULE             HELD          CHANGE         WOULD FIRE  FIRED  ● NOTHING
INC-43    BatchQueueDepth  20m from -5m  for 30m → 19m  +14m        0 → 3  0 → 2
impact-1  CheckoutLatency  reached 0.86  > 0.9 → 0.85   +6m         0 → 2  0 → 1
```

`--compare`는 지금 룰과 제안한 룰을 같은 기간에 나란히 백테스트해, 룰마다 정탐·오탐과
장애마다 잡은 룰을 변경 전 → 후로 보여 줍니다. 장애 기록이 없으면 룰마다 얼마나 자주
울리는지를 보여 줍니다. `--expect`는 바뀌는 룰을 기대치에 묶습니다. 일주일에 몇 번까지
울려도 되는지, 자는 시간에는 몇 번까지인지, 계속 잡아야 하는 장애는 무엇인지입니다.
룰을 바꾸는 PR에서 CI로 돌리세요. GitHub, GitLab, Bitbucket, Azure DevOps, Gitea용
스크립트와 GitHub Actions용 액션이 리포트를 Markdown 댓글로 PR에 달고, 그 댓글 하나를
계속 갱신합니다. 리포트는 JSON과 Prometheus 메트릭으로도 나오고, Slack과 Pushgateway로도
보냅니다.

`--check-replay`는 백테스트 결과를 실제로 울린 기록(Prometheus, Grafana 자체 알림 상태
이력, 클라우드 알람 이력)과 룰별로 나란히 놓습니다. 결과를 믿기 전에 내 룰에서 백테스트가
맞는지 먼저 확인할 수 있습니다. `--check-content`는 알림이 울릴 때 무슨 내용을 전하는지
점검하고, `--redact`는 밖에 보여 줄 리포트를 위해 모든 이름을 별칭으로 바꿉니다.

## 매시간: `lapilli serve` {#every-hour-lapilli-serve}

`lapilli serve`는 이력 DB를 유지합니다. 모든 알림, 장애, 룰의 모든 버전을 담고,
매시간 그 시점의 룰로 백테스트합니다. 정탐과 오탐은 웹 UI에 나오고, 팀은 거기서
조치합니다. 배포하기 전에는 `lapilli preflight`가 같은 플래그를 받아 무엇을 만나게
될지 알려 줍니다. 백테스트할 수 있는 룰, 이력과 그 이력에 접속하는 인증, 이력 DB,
그리고 실행에 드는 요청 수와 메모리입니다. 읽기만 하고 아무것도 쓰지 않습니다.

<div class="tour">
<figure>
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-rules-dark.png' | relative_url }}">
  <img alt="lapilli serve의 룰 목록. 최근 4주의 노이즈 룰만 걸러 보았고, 룰마다 배지, 라벨, 팀, 숫자가 나옵니다" src="{{ '/serve-rules.png' | relative_url }}" class="end" width="1240" height="1600" loading="lazy">
</picture>
<figcaption><h3>비용이 큰 룰부터 찾기</h3><p>노이즈 룰만 걸러 본 룰 목록입니다. 룰마다 정탐·오탐 수, 조용하게 만들 <code>for</code> 제안, 사람들이 자는 동안 울린 알림이 함께 나옵니다.</p><p class="go"><a href="{{ '/docs/serve/#rules-and-proposals' | relative_url }}">룰 목록 문서 (영어)</a></p></figcaption>
</figure>
<figure>
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-rule-dark.png' | relative_url }}">
  <img alt="lapilli serve의 노이즈 룰 페이지. 배지와 작업 버튼, 직전 4주와 나란히 놓인 숫자 다섯 개, 최근 84일을 칸으로 나타낸 띠가 있습니다" src="{{ '/serve-rule.png' | relative_url }}" width="1240" height="980" loading="lazy">
</picture>
<figcaption><h3>룰 하나를 주 단위로 읽기</h3><p>룰 페이지입니다. 정탐률과 알림 수를 직전 기간과 나란히 보고, 최근 84일을 하루 한 칸으로, 룰의 버전별 변화와 먼저 할 일을 봅니다. 변경은 백테스트와 함께 제안되고 사람이 승인합니다.</p><p class="go"><a href="{{ '/docs/changing-rules/' | relative_url }}">룰 변경 문서 (영어)</a></p></figcaption>
</figure>
</div>

<div class="grid">
  <div><h3>대시보드</h3><p>지난주 정탐률, 알림 수, 미탐 장애, 노이즈 룰, 괜히 사람을 깨운 알림을 전주와 나란히 봅니다. 12주 주별 추이, 룰별·주별 노이즈 분포, 먼저 볼 것까지 한 화면에 있습니다.</p></div>
  <div><h3>모든 변경을 백테스트</h3><p>룰의 새 버전이 들어오면 그 전 일주일에 대해 백테스트해서, 그 변경이 무엇을 했을지 보여 줍니다.</p></div>
  <div><h3>장애 기록</h3><p>장애를 적거나 오탐을 실제 장애였다고 표시하면, Prometheus에 다시 묻지 않고 이력 전체의 정탐·오탐을 그 자리에서 다시 가립니다.</p></div>
  <div><h3>메인터넌스 윈도우</h3><p>배포, 점검, 야간 배치처럼 노이즈가 예상되는 시간입니다. 윈도우 안에서 울리고 아무것도 잡지 못한 알림은 면제하며, 한 번만 또는 매일·매주 반복할 수 있습니다.</p></div>
  <div><h3>이벤트 구간</h3><p>세일, 출시, 캠페인 같은 기간을 파일, UI, PromQL 식으로 지정합니다. 면제는 없고, 모든 수치를 구간 안과 밖으로 나눠 보며, 구간마다 정탐률을 직전 4주와 비교합니다.</p></div>
  <div><h3>목표</h3><p>팀이 알림에 거는 기준입니다. 정탐률, 미탐 장애, critical 알림, 재발생 알림의 한도를 주별로 또는 세일마다 정합니다. 목표를 놓치면 알려 주고, 버튼 하나로 작업이 됩니다.</p></div>
  <div><h3>사용자 영향 신호</h3><p>에러 비율이나 지연 시간처럼 사용자가 피해를 보고 있다는 신호를 지정하면, 그 신호가 성립한 구간도 장애가 되고, 주마다 알림의 정탐·오탐을 그 알림에 연결된 신호로 가립니다.</p></div>
  <div><h3>팀, 작업, 댓글</h3><p>팀이 룰과 장애를 소유합니다. 제안은 버튼 하나로 담당자, 기한, 트래커 링크가 달린 작업이 됩니다.</p></div>
  <div><h3>장애는 원래 있는 곳에서</h3><p>스크립트가 PagerDuty, incident.io, Jira, GitHub 이슈에서 장애를 복사해, 토큰으로 장애만 받는 전용 포트로 보냅니다. 같은 내용을 다시 보내면 아무 일도 일어나지 않습니다.</p></div>
  <div><h3>클라우드 알람 보관</h3><p>스크립트가 CloudWatch, Azure Monitor, Cloud Monitoring, OCI 알람 이력을 읽어 전용 토큰으로 복사해 넣으므로, 이력 DB가 클라우드보다 오래 보관합니다. 자체 룰이 없어도 lapilli serve는 그것만으로 돌아갑니다. 클라우드 API로도 읽으면 알람과 그 이력이 한 룰이 됩니다.</p></div>
  <div><h3>울린 알림만이 아니라 보낸 통지까지</h3><p>Alertmanager, Grafana, SNS, Azure Monitor, Cloud Monitoring이 실제로 보낸 통지를 전용 포트로 받습니다. 룰마다 통지 수, 반복 통지, 자는 동안 보낸 통지를 알림 수와 나란히 봅니다.</p></div>
  <div><h3>통지와 주간 리포트</h3><p>일어난 일은 JSON 웹훅과 Slack으로 갑니다. 변경과 함께 기록하는 outbox를 거칩니다. 매주 주간 리포트도 같은 길로 가고, 팀마다 따로 받을 수 있습니다.</p></div>
  <div><h3>Git으로 선언</h3><p>팀, 메인터넌스 윈도우와 이벤트 구간, 목표, 리포트, 역할을 파일 하나에 적으면, 재시작 없이 파일이 바뀔 때마다 그대로 맞춥니다. 선언한 것은 UI에서 읽기 전용입니다.</p></div>
  <div><h3>역할</h3><p>뷰어, 에디터, 관리자. 로그인은 Lapilli가 OpenID Connect 공급자로 직접 하거나, 앞단의 인증(클라우드 IAP, 로드밸런서 인증, oauth2-proxy)에 맡깁니다.</p></div>
  <div><h3>변경 효과</h3><p>룰 변경마다 예측과 실제 결과를 나란히 보고, 장애가 여전히 그 룰에 걸리는지, 실제로 쓰이는지 봅니다. 이력 DB에서만 계산하고 어디에도 보내지 않습니다.</p></div>
  <div><h3>승인을 거친 변경</h3><p>더 긴 for, 더 앞당긴 임계치, 중복 룰 정리, 사람이 쓴 런북 링크. 각각 백테스트와 함께 제안되고, 사람이 그 페이지에서 승인하면 다시 확인한 뒤 반영합니다. 저장소 자격 증명을 가진 스크립트가 GitHub, GitLab, Bitbucket, Gitea, Azure DevOps에 PR로 열거나, 허용된 경우 Grafana API, Mimir·Cortex·Amazon·Azure ruler API, 클라우드 자체 알람 API(CloudWatch, Azure Monitor, Cloud Monitoring, OCI)로 반영합니다. 켜기 전에는 꺼져 있고, 비상 정지 스위치와 한도가 있으며, 울리고 있는 알림은 끄지 않습니다.</p></div>
  <div><h3>AI 에이전트 연동 (MCP)</h3><p>에이전트는 화면에 나오는 내용을 묻고, 자기가 제안한 룰 변경을 반영하기 전에 백테스트하며, 허용되면 사람이 승인할 변경 제안으로 남깁니다. 승인하거나 반영하는 일은 하지 않습니다.</p></div>
  <div><h3>영어, 스페인어, 일본어, 한국어</h3><p>언어마다 번역 파일이 하나 있고, 번역이 빠진 문구가 있으면 테스트가 실패합니다.</p></div>
</div>

이력 DB는 SQLite 파일 하나, 또는 여러 복제본이 함께 쓰고 그중 하나가 리더가 되는
Postgres 데이터베이스입니다. 테스트는 12주 동안 알림 20만 건에서 매시간 실행을 2초
안에, 화면 하나를 룰마다 쿼리 몇 개로 묶어 둡니다. 알림마다 쿼리를 하나씩 보내는 일은 없습니다. `/metrics`는 정탐·오탐
수치를 Prometheus에 내주고, Helm 차트에는 그것을 보여 주는 Grafana 대시보드가 있습니다.
UI에는 JavaScript가 없습니다.

## 정탐·오탐을 가르는 기준 {#how-it-judges}

- **알림**은 알림 룰을 이력에 대해 백테스트해서 얻습니다. 룰은 Prometheus 룰 파일,
  `PrometheusRule`, Grafana 알림 프로비저닝, 클라우드의 관리형 룰 그룹, PromQL로 쓴
  클라우드 알람(CloudWatch PromQL 알람, Google Cloud 알림 정책), 또는 이들을 담은
  ConfigMap이면 되고, 각 버전은 유효했던 기간에 적용합니다. LogQL로 쓴 Loki ruler 룰도
  됩니다. Prometheus나 Grafana가 로드한 룰을 그대로 쓸 수도 있습니다. 알림은 `ALERTS`
  시계열에서, 또는 클라우드 알람 이력처럼 파일이나 작업이 복사해 넣은 기록에서 올 수도
  있습니다. Grafana 룰은 Grafana와 같은 방식으로 평가합니다.
- **장애**는 YAML 파일, UI와 API, 스크립트로 가져온 PagerDuty·incident.io·Jira·GitHub
  이슈, 그리고 사용자 영향 신호가 성립한 구간(`--impact`)에서 옵니다. 장애 기록이
  빈약해도 동작합니다.
- **알림이 장애를 잡았다**는 것은 알림이 장애 직전이나 장애 중에 시작했고, 지정한
  라벨이 일치한다는 뜻입니다. 장애 전부터 울리고 있던 알림은 아무것도 잡지 않은 것으로
  봅니다. 원래 켜져 있었으니까요.
- **기록이 없으면 가리지 않습니다.** 아무것도 잡지 못한 알림은 그 주변에 적힌 것이
  있을 때만 오탐으로 가립니다. 그 알림 전 2주 안의 장애, 또는 그 시간 동안 읽은 사용자
  영향 신호입니다. 그렇지 않으면 세기만 하고 가리지 않으며, 리포트에 그렇게 적습니다.

나머지, 노이즈 룰과 무엇을 바꿀지, 니어 미스는
[How it judges (영어)]({{ '/docs/how-it-judges/' | relative_url }})에 있습니다.

## 알림이 있는 곳에서 그대로 {#works-where-your-alerts-are}

Prometheus 호환 API면 됩니다. Prometheus, Thanos, Mimir, VictoriaMetrics, AWS·Azure·Google
Cloud의 관리형 Prometheus, CloudWatch의 PromQL API를 머신이나 파드가 이미 가진 자격
증명으로 읽습니다. 로그 룰은 Loki에 대해 백테스트합니다
([Rules on logs (영어)]({{ '/docs/loki/' | relative_url }})).
Oracle Cloud에는 관리형 Prometheus가 없으니, OKE에서는 직접 운영하는 것을 쓰세요.
클라우드 고유 형식의 알람(CloudWatch, Azure Monitor, Cloud Monitoring, OCI Monitoring)은
그 이력으로 정탐·오탐을 가리고, 클라우드마다 작은 스크립트가 있으며, `lapilli serve`는 클라우드가
보관하는 기간보다 오래 그 이력을 보관합니다. 연동은 범용입니다. 특정 트래커가 아니라
웹훅입니다.

## 지금 있는 것에서 시작하세요 {#start-from-what-you-have}

먼저 옮기거나 다시 쓸 것은 없습니다. 아래는 모두 저장소의 <a href="{{ site.repo }}/tree/main/recipes">recipes</a>에 있는 스크립트이거나 플래그 몇 개입니다.

<div class="grid">
  <div><h3>저장소에 있는 룰</h3><p>알림 룰을 바꾸는 PR에 백테스트 결과가 댓글로 달리고, 병합을 막을 수 있는 상태 체크가 붙습니다. GitHub, GitLab, Bitbucket, Azure DevOps, Gitea용이 있습니다.</p><p class="go"><a href="{{ '/docs/pr-backtests/' | relative_url }}">Pull request backtests (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/backtest"><code>recipes/backtest</code></a></p></div>
  <div><h3>도구에 있는 장애 기록</h3><p>PagerDuty, incident.io, FireHydrant, Rootly, Grafana IRM, Compass, Jira, GitHub 이슈에서 스크립트가 장애를 복사해 옵니다. 도구가 장애에 연결해 둔 알림도 함께 가져옵니다.</p><p class="go"><a href="{{ '/docs/incidents/' | relative_url }}">Incidents kept elsewhere (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/incidents"><code>recipes/incidents</code></a></p></div>
  <div><h3>클라우드 알람 이력</h3><p>CloudWatch, Azure Monitor, Cloud Monitoring, OCI 알람과 Datadog 모니터, New Relic 알림 조건을 실제로 울린 이력으로 정탐·오탐을 가립니다.</p><p class="go"><a href="{{ '/docs/cloud-alarms/' | relative_url }}">A cloud's own alarms (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/firings"><code>recipes/firings</code></a></p></div>
  <div><h3>그때의 룰</h3><p>git, Argo CD, Flux에서 룰의 이전 버전을 가져와, 주마다 그 시점의 룰로 백테스트합니다.</p><p class="go"><a href="{{ '/docs/how-it-judges/#rules-that-changed' | relative_url }}">Rules that changed (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/versions"><code>recipes/versions</code></a></p></div>
  <div><h3>PR로 나가는 변경</h3><p>승인된 변경은 룰이 보관된 저장소에 PR로 열립니다.</p><p class="go"><a href="{{ '/docs/changing-rules/#carrying-out-a-change' | relative_url }}">Changing rules (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/changes"><code>recipes/changes</code></a></p></div>
  <div><h3>트래커의 작업</h3><p>Jira, Linear, GitHub, GitLab, Azure Boards, Shortcut, YouTrack의 이슈가 Lapilli의 작업과 상태를 맞춥니다.</p><p class="go"><a href="{{ '/docs/serve/#tasks-kept-in-step-with-a-tracker' | relative_url }}">Tasks kept in step with a tracker (영어)</a> · <a href="{{ site.repo }}/tree/main/recipes/trackers"><code>recipes/trackers</code></a></p></div>
</div>

## 믿고 쓸 수 있게 {#built-to-be-trusted}

- **사람이 승인한 것만 바꿉니다.** 기본은 읽기만 합니다. 켜면 제안한 룰 변경을
  사람이 UI에서 승인한 뒤에만 반영합니다. 룰이 있는 저장소에 PR로, 또는 Grafana·ruler·
  클라우드 API로 반영하고, 되돌릴 수 있게 원래 것을 보관합니다. 알림을 묵음 처리하지
  않고, 울리고 있는 알림을 끄지 않으며, AI 에이전트는 제안만 할 수 있습니다.
- **비밀 값은 제자리에.** 토큰과 웹훅 URL은 파일에서 읽고 화면에 보이지 않으며,
  지정한 곳 말고는 아무 데도 보내지 않습니다.
- **점검한 릴리스.** 릴리스는 게시 전에 govulncheck로 점검하고, 최신 릴리스는 매일
  다시 점검합니다. 릴리스는 게시한 뒤 바꿀 수 없고(v0.14.0부터), 빌드 출처 증명이
  붙습니다. 바이너리, 차트, 이미지마다 `gh attestation verify`가 되고, v0.28.0부터
  릴리스마다 둔 Sigstore 번들로 바이너리와 차트는 오프라인에서도 검증됩니다
  ([Checking a release (영어)](https://github.com/lapilli-project/lapilli/blob/main/SECURITY.md#checking-a-release)).
- **열린 곳에서.** 취약점 신고는 GitHub 비공개 보안 권고로 받고, 코드는 CodeQL과
  OpenSSF Scorecard로 스캔합니다.
- **온콜 모두를 위해.** `lapilli serve`의 화면은 스크린 리더, 키보드만, 200% 확대,
  휴대폰에서 WCAG 2.2 AA를 목표로 하고, CI가 모든 언어에서 구조와 대비를 지킵니다.
  준수한다는 주장이 아니라 목표입니다
  ([Accessibility (영어)](https://github.com/lapilli-project/lapilli/blob/main/ACCESSIBILITY.md)).

## 함께 만들기 {#help-build-it}

Lapilli는 메인테이너가 한 명이고, 여러 조직에서 함께할 사람을 더 찾고 있습니다. 열린
질문들이 좋은 질문입니다. 백테스트는 실제로 울린 것을 얼마나 충실히 재현할 수 있을까?
빈약한 장애 기록으로 공정하게 가리려면 무엇이 필요할까? 지금 메인테이너에게 없는 것이
필요한 영역도 있습니다. 관리형 Prometheus가 있는 AWS나 Azure 계정, 실제로 쓰는 Loki
알림, 장애 관리 도구입니다.

- [로드맵](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md):
  아직 하지 않는 것을 영역별로, 코드 어디에 있는지와 함께
- [첫 기여 이슈](https://github.com/lapilli-project/lapilli/labels/good%20first%20issue):
  각자 어디를 고치고 언제 끝나는지 적힌 작은 변경. 계정이나 도구가 필요한 것은
  [help wanted](https://github.com/lapilli-project/lapilli/labels/help%20wanted)
- [메인테이너](https://github.com/lapilli-project/lapilli/blob/main/MAINTAINERS.md):
  한 영역에 기여하다 그 영역의 메인테이너가 되는 길
- [거버넌스](https://github.com/lapilli-project/lapilli/blob/main/GOVERNANCE.md):
  원칙과 결정 방식
- [기여 안내](https://github.com/lapilli-project/lapilli/blob/main/CONTRIBUTING.md):
  빌드, 테스트, 변경 보내기
- [Discussions](https://github.com/lapilli-project/lapilli/discussions):
  질문, 아이디어, 그리고 Lapilli가 내 알림에서 찾은 것

가장 쓸모 있는 제보는 이것입니다. 실제로 울린 것과 다르게 백테스트되는 룰.

## 설치 {#install}

<div class="ways">
<div markdown="1">
### Go

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```
</div>
<div markdown="1">
### 바이너리

Linux와 macOS, amd64와 arm64용으로
[릴리스]({{ site.repo }}/releases{% if site.version %}/tag/{{ site.version }}{% endif %})에서:

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
{% if site.version %}curl -fsSL {{ site.repo }}/releases/download/{{ site.version }}/lapilli-{{ site.version }}-$os-$arch.tar.gz | tar -xz
./lapilli-{{ site.version }}-$os-$arch/lapilli demo{% else %}gh release download -R lapilli-project/lapilli -p "lapilli-*-$os-$arch.tar.gz"{% endif %}
```
</div>
<div markdown="1">
### 컨테이너 이미지

```sh
docker run --rm ghcr.io/lapilli-project/lapilli{% if site.version %}:{{ site.version }}{% endif %} demo
```
</div>
<div markdown="1">
### Helm 차트

주 1회 CronJob으로, 또는 `mode=serve`로 매시간:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
```
</div>
</div>

릴리스는 게시 전에 점검하고, 게시한 뒤에는 바꿀 수 없습니다. 받은 파일은
[Checking a release (영어)]({{ site.repo }}/blob/main/SECURITY.md#checking-a-release)로
검증할 수 있습니다.
[시작하기]({{ '/ko/docs/getting-started/' | relative_url }})는 여기서부터
내 알림에 대한 첫 리포트까지 안내합니다.

## 이름 {#the-name}

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> 흰 조약돌과 검은 조약돌로 판결하는 것이 옛 관습이었다. 검은 돌은 피고를 유죄로,
> 흰 돌은 무죄로 했다.
>
> — 오비디우스, *변신 이야기* XV.41–42

*Lapilli*는 작은 돌이라는 뜻입니다. 알림 하나하나가 한 표이고, 일주일에 한 번 항아리를
비워 돌을 셉니다.
