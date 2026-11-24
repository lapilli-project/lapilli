<!-- translated from docs/getting-started.md at blob 556239756be4d6fc535a139051ce268cb93bf39e -->

# 시작하기

Lapilli를 설치하는 것부터 내 알림으로 첫 리포트를 읽는 것까지 다룹니다. 지금 쓰는
룰과, 그 룰의 이력을 가진 Prometheus 호환 API가 있으면 됩니다.
`lapilli verdict`는 이력과 파일을 읽기만 하고, 리포트는 지정한 곳 외에는 어디로도
보내지 않습니다.

## 샘플 데이터로 먼저 보기

```sh
lapilli demo
```

작은 쇼핑몰의 알림 룰 일곱 개를 지어낸 일주일치 이력에 백테스트하고, 그 쇼핑몰의
장애에 맞춰 정탐·오탐을 가립니다. `lapilli verdict`와 같은 코드를 씁니다. 이력은
그 자리에서 지어내므로 연결할 것이 없고, 읽거나 보내는 것도 없습니다. 리포트에는
이 페이지의 나머지가 내 알림에서 찾아 주는 것이 모두 들어 있습니다. 노이즈 룰과 그
룰을 조용하게 만들 더 긴 `for`, 장애를 잡은 룰, 아무것도 잡지 못한 장애와 거의
잡을 뻔한 룰, 사람이 자는 동안 울린 알림입니다. `--json`은 리포트 전체를 JSON으로
주고, `--tz`는 샘플의 일주일을 놓을 시간대를 정합니다. Go는 있고 아무것도 설치하지
않았다면 `go run lapilli.dev/cmd/lapilli@latest demo`로 같은 결과를 볼 수 있습니다
([docs/design/demo.md (영어)](../design/demo.md)).

```sh
lapilli demo --serve
```

같은 일주일을 `lapilli serve`의 화면으로 보여 줍니다. 정탐·오탐은 serve의 매시간
작업이 가립니다. 열어 볼 URL을 알려 주며, 이 머신에서만 열리고(`--listen`, 루프백
주소, 기본은 빈 포트) 읽기 전용입니다. 이력 DB는 임시 디렉터리에 두었다가 Ctrl-C로
끝내면 지웁니다. 시계는 샘플 일주일의 끝에 맞춰져 있고, 그곳이 화면이 보여 주는
마지막 온전한 일주일입니다.

## 설치

```sh
go install lapilli.dev/cmd/lapilli@latest
```

`go.mod`에 적힌 대로 Go 1.26.11 이상이 필요합니다. 아니면
[릴리스](https://github.com/lapilli-project/lapilli/releases)에서 바이너리(macOS와
Linux, amd64와 arm64)를 받거나, 엔트리포인트가 `lapilli`인
`ghcr.io/lapilli-project/lapilli` 이미지를 쓰면 됩니다.
받은 것을 오프라인으로도 확인하는 방법은
[릴리스 확인하기 (영어)](../../SECURITY.md#checking-a-release)에 있습니다.

## 장애를 한두 건 적어 두기

알림은 하나하나 무엇이 잘못됐는지에 맞춰 정탐·오탐을 가리므로, Lapilli에는 그
기록이 필요합니다. 가장 간단한 것은 YAML 파일입니다.

```yaml
incidents:
  - id: INC-100
    title: Login slow
    start: 2026-11-03T08:10:00Z
    end: 2026-11-03T08:50:00Z
    labels: {service: login}
  - id: INC-101
    title: Checkout errors
    start: 2026-11-11T10:02:00Z
    end: 2026-11-11T10:45:00Z
    labels: {service: checkout}
  - id: INC-102
    title: Search timeouts
    start: 2026-11-13T15:30:00Z
    labels: {service: [search, catalog]}
  - id: INC-103
    title: Orders stuck
    start: 2026-11-14T03:05:00Z
    end: 2026-11-14T03:30:00Z
    labels: {service: orders}
```

- `id`와 `start`는 필수입니다. `id`는 파일 안에서 한 번씩만 나와야 하고, `start`는
  오프셋이 붙은 RFC 3339 시각(`Z`, `+09:00`)입니다.
- `end`, `title`, `labels`는 선택입니다. `end`가 없으면 장애가 한 시간 이어진 것으로
  봅니다. `end`는 `start`보다 앞설 수 없습니다. `labels`는 어디가 잘못됐는지를
  나타내며, 라벨 하나에 값 하나 또는 값 목록을 씁니다. `--match service`를 주면
  `service` 라벨이 있는 알림은 그 값을 목록에 올린 장애나 `service` 라벨이 없는
  장애만 잡습니다. 라벨이 없는 알림이나 장애에는 이 조건을 적용하지 않습니다.

대상 기간 앞 2주의 장애도 적어 두세요. 장애를 잡지 못한 알림은 그 알림 앞 2주
안에 어느 한 시점에라도 장애가 있었거나, 그 시간에 걸쳐 `--impact`를 읽은
경우에만 정탐·오탐을 가립니다. 그렇지 않으면 세기만 하고 가리지 않습니다
([기록 없음 (영어)](../numbers.md#no-record)). INC-100이 그 때문에 있습니다.
아래에서 정탐·오탐을 가리는 주 앞이라 목록에는 나오지 않지만, 그 주의 모든
알림은 정탐·오탐이 가려집니다.

`incidents` 키가 없는 파일은, 비어 있는 파일도 포함해 거부합니다. 실패한 작업이
"아무 일도 없던 주"로 읽히지 않게 하려는 것입니다. 장애가 없다면
`incidents: []`라고 쓰세요. 다른 방법으로 장애를 가져와도 됩니다.

- [다른 도구에 있는 장애 기록 (영어)](../incidents.md): PagerDuty, incident.io,
  Jira, GitHub 이슈 등 여러 도구에서 이 파일을 만들어 주는 스크립트.
- [기록되지 않은 장애 (영어)](../impact.md): `--impact PROMQL`, 사용자 영향 신호의
  구간을 장애로 봅니다. 파일과 함께 쓸 수도, 혼자 쓸 수도 있습니다.

### 적어 둔 장애가 없다면? 여기서 시작

`lapilli impact suggest --prometheus http://prometheus:9090`은 서비스 앞단 스택
중 [기록되지 않은 장애 (영어)](../impact.md#which-of-them-your-history-has)에
나열된 것 가운데 내 Prometheus에 메트릭이 있는 것을 묻습니다. Istio, Linkerd,
Envoy, ingress-nginx, Traefik, OpenTelemetry의 HTTP 서버 메트릭이 대상입니다.
찾은 스택마다 시리즈가 몇 개인지, 지난 한 주 동안 `--impact`가 사용자 영향
구간을 몇 개 찾는지, 그리고 `--incidents` 대신 `lapilli verdict`에 줄
`--impact '...'`를 알려 줍니다. 읽기만 하고 아무것도 고르지 않습니다. 라벨과
임계치를 먼저 확인하세요.

## 지난주 정탐과 오탐 가리기

```sh
lapilli verdict --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

`--rules`의 룰을 `--prometheus`의 이력에 백테스트합니다. Prometheus 룰 파일,
`PrometheusRule` 리소스, Grafana 알림 프로비저닝 파일, 이것들을 담은 ConfigMap,
또는 이것들이 든 디렉터리를 줄 수 있습니다. 기간은 지난 7일(`--since 7d`)이거나,
RFC 3339로 준 `--from`부터 `--to`까지이며, 시각은 머신의 시간대나 `--tz`로
보여 줍니다. `--match service`는 한 서비스의 알림이 같은 시간에 났다는 이유만으로
다른 서비스의 장애를 잡은 것으로 치지 않게 합니다. 이 옵션이 없으면 시간만 맞는
알림이면 모두 장애를 잡은 것으로 칩니다.

`--rules alert-rules/` 대신 이렇게 줄 수도 있습니다.

- `--loaded-rules`: `--prometheus`의 API가 로드해 둔 룰.
- `--grafana https://grafana.example.com --grafana-token-file grafana-token`:
  Grafana가 가진 룰을 Viewer 서비스 계정 토큰으로 읽습니다. `--prometheus`도
  여전히 줘야 합니다.
- `--alerts`: Prometheus가 울린 알림을 `ALERTS` 시리즈에서 읽습니다. 백테스트는
  하지 않으므로 더 긴 `for`를 제안하지 않고 니어 미스도 찾지 않습니다.

클라우드의 관리형 Prometheus에는 `--auth aws`, `azure`, `google`이 머신이나 파드가
이미 가진 자격 증명을 보내고, `--bearer-token-file FILE`은 토큰을 보냅니다.
클라우드별 URL과 필요한 권한은 [`lapilli verdict` 사용법 (영어)](../verdict.md)에
있습니다.

## 리포트 읽기

지어낸 일주일을 `--from`, `--to`, `--tz UTC`로 주고, 위의 장애와 룰 세 개
(`CheckoutErrors`, `NodeLoadHigh`, `QueueBacklog`), 그리고 Prometheus 대신
응답하는 대역을 써서 위 명령을 돌리면 다음이 나옵니다. 세 부분은 뺐습니다.

```
Lapilli  2026-11-09 00:00 → 2026-11-16 00:00 UTC
14 firings, 3 incidents written down

RULE            SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
NodeLoadHigh    warning   13     1         12         8%         0
CheckoutErrors  critical  1      1         0          100%       0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE          FOR       FIRED   INCIDENTS  CAUGHT LATER BY
NodeLoadHigh  1m → 10m  13 → 1  1          up to 9m

INCIDENT                 START             LASTED  CAUGHT BY
INC-101 Checkout errors  2026-11-11 10:02  43m     CheckoutErrors +3m
INC-102 Search timeouts  2026-11-13 15:30  –       NodeLoadHigh -9m (noisy)
INC-103 Orders stuck     2026-11-14 03:05  25m     ✗ nothing fired

✗ 1 of 3 incidents had no alert.
! 1 of 3 incidents were caught only by noisy rules, which fire whatever happens
  (10 or more firings, under 20% of them catching anything).

Rules that nearly caught them: their condition held, but not for their "for", or fired too late; or their value came within a fifth of their threshold.
INCIDENT  RULE          HELD         CHANGE        WOULD FIRE  FIRED  ● NOTHING
INC-103   QueueBacklog  7m from -5m  for 10m → 6m  +1m         0 → 1  0 → 0
```

위에서부터 봅니다.

- **기간**과 그 안의 알림 수, 장애 수. 정탐·오탐을 가릴 기록이 없는 알림이 있으면
  그 아래 한 줄에 몇 건인지 나옵니다.
- **알림이 울린 룰마다**: 알림 수, 장애를 잡은 알림(○, 흰 돌), 아무것도 잡지
  못한 알림(●, 검은 돌), 정탐률(정탐·오탐이 가려진 알림 중 무언가를 잡은
  알림의 비율), 그리고 같은 알림이 해소된 뒤 `--refire`(1h) 안에 다시 울린 알림.
- **노이즈 룰**: 알림이 10건 이상이고 그중 무언가를 잡은 것이 20% 미만인 룰을 더
  긴 `for`로 백테스트합니다. 그 룰이 잡았던 장애를 모두 그대로 잡으면서 알림이
  절반 이하로 줄어드는 가장 짧은 `for`입니다.
- **장애**: 각 장애가 얼마나 이어졌는지(끝이 적혀 있지 않으면 `–`), 무엇이
  잡았는지, 시작 뒤 얼마 만에(`+3m`) 또는 시작 전 얼마에(`-9m`) 잡았는지.
  `(noisy)`는 노이즈 룰이 잡은 것이고, `✗ nothing fired`는 미탐입니다.
- **거의 잡을 뻔한 룰**: 조건은 맞았지만 `for`를 채우지 못했거나, 값이 임계치의
  5분의 1 안까지 다가온 룰입니다. 그 장애를 잡았을 변경과, 그러면 알림이 얼마나
  자주 울렸을지를 함께 보여 줍니다. `for`가 6m였다면 `QueueBacklog`는 INC-103이
  시작하고 1분 뒤에 울렸을 것이고, 그 주에 한 번이었을 것입니다.

위에서 뺀 것은 이렇습니다. 아무것도 잡지 못한 알림을 `--tz` 기준으로 울린 시각에
따라 나눈 것(사람이 자는 동안, 근무 시간 외, 근무 중), 룰이 더하는 것(각 룰이
처음으로 또는 혼자 잡은 장애와 중복 룰), 그리고 한 번도 울리지 않았거나 백테스트할
수 없었던 룰과 그 이유입니다.
리포트가 무엇에 근거하는지는 [정탐·오탐을 가르는 기준 (영어)](../how-it-judges.md)에,
각 숫자의 정의와 알림 전부가 든 리포트 전체인 `--json`에서 다시 세는 방법은
[숫자의 의미 (영어)](../numbers.md)에 있습니다.

## 다음 단계

룰을 바꾸기 전에 `--compare`가 제안한 룰을 지금 쓰는 룰과 나란히 놓고, 같은 기간에
같은 장애에 맞춰 백테스트합니다([병합 전에 비교하기 (영어)](../compare.md)).

```sh
lapilli verdict --rules proposed/ --compare alert-rules.yaml --prometheus http://prometheus:9090 \
  --incidents incidents.yaml
```

Helm 차트는 매주 월요일 09:00에 `lapilli verdict`를 돌려 직전 한 주를 백테스트하고,
리포트는 Slack으로, 메트릭은 Pushgateway로 보냅니다
([주간 리포트 (영어)](../every-week.md)).

`lapilli serve`는 매시간 정탐·오탐을 가려 웹 UI가 있는 이력 DB에 계속 쌓습니다
([매시간 정탐·오탐 가리기 (영어)](../serve.md)). `lapilli preflight`는 같은 플래그를
받아 앞으로 만나게 될 것을 알려 줍니다. 룰, 이력, 그리고 백테스트가 쓸 요청량과
메모리입니다. 아무것도 쓰거나 백테스트하지 않고, 문제가 없으면 0으로 끝납니다
([배포하기 전에 (영어)](../serve.md#before-it-is-deployed)).

```sh
lapilli preflight --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

같은 플래그로 `lapilli serve`를 띄우면 처음 시작할 때 지난 28일을 백테스트하고
(`--backfill`), 이력 DB는 `lapilli-ledger`에 두며, `127.0.0.1:8080`에서 받습니다.
거기에 닿는 사람은 모두 admin입니다. Kubernetes에서는
[차트](../../charts/lapilli/README.md)에 이만큼만 주면 됩니다.

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

`rules.configMaps`에는 릴리스의 네임스페이스에 있는 룰 ConfigMap 이름을 줍니다.
`incidents`는 파일과 같은 목록을 받고, UI에서 사람이 직접 적어도 됩니다. 역할을
정하지 않으면 파드에 닿는 사람은 누구나 보기만 할 수 있습니다
([사람을 구분하는 방법 (영어)](../../charts/lapilli/README.md#who-people-are)).
