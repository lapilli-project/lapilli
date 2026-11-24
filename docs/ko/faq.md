<!-- translated from docs/faq.md at blob 4619a0297ab9a69d25cb7084cc4dfa5360931ea3 -->

# 자주 묻는 질문

### 무엇이 필요한가요?

룰을 평가하는 대상인 이력을 읽을 권한, Prometheus 호환 API(`--prometheus`:
Prometheus, Thanos, Mimir, VictoriaMetrics, 또는 `--auth`로 쓰는 클라우드의 관리형
Prometheus. CI의 `e2e-compat`는 Thanos 0.42.5와 VictoriaMetrics 1.156.0에 대해
verdict와 `--check-replay`를 돌립니다), 파일이나 룰을 가진 API에서 읽은 룰, 그리고
무엇이 잘못됐는지의 기록이 필요합니다. 이력에는 `/api/v1/query_range`를 묻고, 제공되는
곳에서는 스크레이프 간격을 알기 위해 `/api/v1/status/config`와
`/api/v1/targets`도 묻습니다. `--loaded-rules`는 `/api/v1/rules`도 묻습니다.
`--grafana`에는 Viewer 권한의 서비스 계정 토큰이 필요하고, 각 클라우드에는
[`lapilli verdict` 사용법 (영어)](../verdict.md)의 표에 있는 읽기 권한이 필요합니다.

### 장애 목록이 있어야 하나요?

`lapilli verdict`에는 파일에 담긴 장애(`--incidents`)나 `--impact`가 필요하고, 둘 다
써도 됩니다. `--impact`는 사용자가 피해를 보는 동안 값을 돌려주는 PromQL
표현식(예: 5xx 비율)으로, 그 구간이 장애가 됩니다. 기록이 성기어도 됩니다. 장애가
기록된 곳에서 스크립트로 복사해 올 수도 있습니다
([다른 도구에 있는 장애 기록 (영어)](../incidents.md),
[기록되지 않은 장애 (영어)](../impact.md)). `lapilli serve`는 화면에서 기록한 장애도
받습니다. 아무것도 기록되지 않은 때에는, 장애를 잡지 못한 알림이 정말 아무 일도 없던
것인지 장애를 아무도 적지 않았을 뿐인지 구별할 수 없으므로 정탐·오탐을 가리지 않습니다
([정탐·오탐을 가르는 기준 (영어)](../how-it-judges.md)).

### 내 시스템에 무언가를 쓰나요?

- `lapilli verdict`는 이력과 파일을 읽고 리포트를 stdout에 씁니다. 리포트는
  `--push`(Pushgateway)와 `--slack-webhook-file` 말고는 어디로도 보내지 않습니다.
- `lapilli preflight`는 아무것도 쓰지 않습니다. 보내는 모든 요청이 허용 목록을
  거치고 목록에 없는 요청은 보내기 전에 거부하며, 이력 DB는 읽기 전용으로 엽니다
  ([`lapilli serve`를 배포하기 전에 (영어)](../serve.md#before-it-is-deployed)).
- `lapilli serve`는 자기 이력 DB, 즉 `--ledger`의 SQLite 파일이나 Postgres
  데이터베이스(`--ledger-url-file`)에 씁니다. 룰 변경을 제안하고, `--changes`를 줘야만
  실행합니다. `--changes`에는 로그인과 역할이 필요하고, 사람이 브라우저에서 승인한
  뒤에야 실행됩니다. 실행은 저장소 자격 증명이 있는 곳에서 도는 레시피가 여는 PR이거나,
  쓰기용으로 준 자격 증명으로 Grafana, ruler, 클라우드의 API를 통해 씁니다
  ([룰 변경 (영어)](../changing-rules.md)).
- `lapilli ledger copy`는 이력 DB를 다른 빈 이력 DB로 옮겨 씁니다. `--to-url-file`을
  주면 그 파일이 가리키는 Postgres 데이터베이스에 쓰고, 복사해 오는 이력 DB를 먼저
  자기 스키마로 올립니다
  ([명령 (영어)](../commands.md#lapilli-ledger)).
- [recipes](../../recipes)의 스크립트는 따로 돌며 자기 토큰을 씁니다. 작업을 트래커와
  맞춰 두는 스크립트는 열려 있는 작업마다 트래커에 이슈를 만듭니다
  ([매시간 정탐·오탐 가리기 (영어)](../serve.md#tasks-kept-in-step-with-a-tracker)).

### 데이터를 어딘가로 보내나요?

플래그와 파일에 적은 곳으로만 보냅니다. 그 플래그와 파일이 가리키는 API와 피드에
요청하고, 그 요청에 딸린 곳에도 요청합니다. 클라우드의 자격 증명 체인, OpenID Connect
제공자의 엔드포인트, 앞단 로그인이 서명하는 키(`--proxy-jwt google-iap`은 Google의
키를 `www.gstatic.com`에서, `aws-alb`는 로드 밸런서의 키를
`public-keys.auth.elb.REGION.amazonaws.com`에서 가져옵니다), 확인해야 하는 SNS 구독이
그렇습니다. 텔레메트리도 업데이트 확인도 없습니다. 사람이 읽도록 쓴 내용은 플래그가
정한 곳으로만 갑니다.

- `lapilli verdict`: `--slack-webhook-file`의 Slack 웹훅으로 리포트를, `--push`로
  메트릭을 보냅니다.
- `lapilli serve`: `--slack-webhook-file`, `--team-digest`, `--notify-webhook-file`로
  주간 리포트를 보내고, 일어난 일(작업이 만들어짐, 장애가 기록됨)은
  `--notify-webhook-file`로, `--notify-slack`을 주면 Slack으로도 보내며, 사람이
  예약한 리포트는 그곳들과 `--report-dest`로 보냅니다.

여기에는 룰 이름, 라벨, 장애 제목, 건수가 들어 있습니다. `lapilli verdict
--redact`는 모든 이름에 별칭을 붙이고, 보내는 내용에도 그렇게 합니다
([외부에 보여 줄 리포트 (영어)](../redact.md)). 화면의 페이지는 스크립트를 실행하지
않고 다른 곳에서 아무것도 불러오지 않습니다.

### 이력이 얼마나 있어야 하고, 얼마나 거슬러 갈 수 있나요?

이력이 룰이 읽는 데이터를 보관하는 만큼 갑니다. `lapilli verdict`는 최근
`--since`(7d), 또는 `--from`부터 `--to`까지의 정탐·오탐을 가립니다. 알림의 기록
여부는 알림 전 2주 안에 장애가 있었는지로 정하고, `--impact`는 기간 시작 2주 전부터
읽습니다. `lapilli serve`는 처음 시작할 때 `--backfill`(28d, 최대 10년)만큼
백테스트하고, 알림은 `--keep`(400d) 동안 보관합니다. 기간 중에 바뀐 룰은
`--rules FILE@TIME`이나 `--rules-history`로 그때의 모습대로 백테스트합니다
([정탐·오탐을 가르는 기준 (영어)](../how-it-judges.md)의 "Rules that changed").
클라우드에 따라 보관 기간이 더 짧습니다. Google Cloud는 일부 메트릭을 6주만 보관하고,
6주보다 오래된 점은 10분 간격으로만 남깁니다
([`lapilli verdict` 사용법 (영어)](../verdict.md)).

### Prometheus에 얼마나 요청하나요?

백테스트는 쿼리를 하루 단위로 묻습니다. 그 쿼리가 필요한 모든 룰이 한 요청을 같이
쓰므로 쿼리와 날짜마다 요청은 하나이고, API마다 `--parallel`(8)개 룰을 동시에
돌립니다. API가 밀어내면(HTTP 429, 5xx) 기다리고, 429나 503 뒤에는 동시 요청을
줄이며, `--max-rate N`은 초당 N건으로 묶어 둡니다. `--max-samples`(10,000,000)는 룰
쿼리 한 번을 읽을 때 담을 수 있는 양의 상한으로, 정점에서 샘플당 64바이트쯤입니다.
넘으면 그 룰은 백테스트하지 않습니다. `lapilli serve`는 마지막으로 돈 뒤의 시간을 매
시간 백테스트합니다. `--yield-on-429`, `--guard`, `--catch-up-hours`는 알림이 쓸
할당량을 건드리지 않게 하고, `lapilli preflight`는 배포 전에 요청 수, 시간, 메모리를
추정합니다([`lapilli verdict` 사용법 (영어)](../verdict.md),
[design/quota.md (영어)](../design/quota.md)).

### 어떤 룰, 데이터 소스, 클라우드를 지원하나요?

룰은 Prometheus 룰 파일, `PrometheusRule` 리소스, Grafana 알림 프로비저닝 파일,
각 클라우드의 관리형 룰 그룹과 PromQL로 쓴 알람, 이들을 담은 ConfigMap, Loki ruler
룰 파일(`--loki-rules`), 그리고 이것들이 든 디렉터리입니다. API가 가진 룰도 읽습니다.
`--loaded-rules`(Prometheus, Thanos, Mimir, VictoriaMetrics의 `-vmalert.proxyURL`을
통한 vmalert, Amazon Managed Service for Prometheus), `--grafana`, `--ruler`(Mimir,
Cortex, Amazon의 것, Azure의 것), Loki의 ruler(`--loki-loaded-rules`), 클라우드
자체 알람(`--alarms`)이 그렇습니다. Prometheus가 울린 알림(`--alerts`)과 다른 곳에
기록된 알림(`--firings`)은 있는 그대로 정탐·오탐을 가립니다
([정탐·오탐을 가르는 기준 (영어)](../how-it-judges.md)).

쿼리는 Prometheus 호환 API에 대해 PromQL로, Loki에 대해 LogQL로 백테스트합니다
([로그 룰: Loki (영어)](../loki.md)). SQL, Elasticsearch, InfluxDB, 클라우드 자체
로그는 안 됩니다([아직 안 되는 것 (영어)](../limits.md)). AWS, Azure, Google Cloud의
관리형 Prometheus는 `--auth`로 읽습니다. Oracle Cloud에는 없으므로 OKE에서는
직접 운영하는 Prometheus를 주세요. 네 클라우드의 자체 알람은 `--alarms`로 읽습니다.
PromQL로 쓴 것은 백테스트하고, 나머지는 목록으로 보여 주며, 그 이력을 복사해 오면
그것으로 정탐·오탐을 가립니다
([클라우드 자체 알람 (영어)](../cloud-alarms.md)).

### Grafana 관리형 알림도 되나요?

됩니다. `--grafana URL`은 Grafana가 가진 룰을, UI에서 만들었든 아니든 읽습니다.
`--grafana-token-file`에 Viewer 토큰을 줍니다. `--rules`는 그 프로비저닝 파일을
읽습니다. Grafana가 평가하는 방식 그대로, 즉 reduce, threshold, math, classic
condition과 쿼리가 아무것도 돌려주지 않을 때의 동작까지 Grafana 자체의 상태대로
평가합니다. 쿼리 오류와, PromQL도 LogQL도 아닌 데이터 소스에 대한 쿼리는
백테스트할 수 없습니다([정탐·오탐을 가르는 기준 (영어)](../how-it-judges.md),
[아직 안 되는 것 (영어)](../limits.md)). `--check-replay`는 백테스트를 Grafana 자체
상태 이력과 나란히 놓습니다
([백테스트가 맞는지 확인하기 (영어)](../check-replay.md)).

### Alertmanager의 라우팅, 사일런스, 인히비션도 백테스트하나요?

아니요. 룰이 울리는 대로 알림을 셀 뿐이고, Alertmanager의 라우팅, 그룹핑, 인히비션,
사일런스와 Grafana의 알림 정책은 설정에서 읽지 않습니다
([아직 안 되는 것 (영어)](../limits.md)). `lapilli serve`는 웹훅 리시버나,
Alertmanager 0.33.0부터는 이벤트 레코더에서 실제로 보낸 알림 전송 기록을 알림 옆에 보관할 수
있고([실제로 보낸 알림 (영어)](../notifications.md)), 사일런스나 인히비션이 알림을
억제한 때도 알려 줍니다. 그래서 알림이 울렸는데도 아무도 호출받지 않은 이유를 알 수
있습니다. `lapilli serve`의 메인터넌스 윈도우는 자체 기능으로, 그 안에서 장애를
잡지 못한 알림을 면제합니다.

### Kubernetes가 필요한가요?

아닙니다. `lapilli`는 바이너리 하나이고(`go install lapilli.dev/cmd/lapilli@latest`,
릴리스, 또는 이미지 `ghcr.io/lapilli-project/lapilli`), 이력에 닿는 곳이면 어디서든
돕니다. `lapilli serve`는 `127.0.0.1:8080`에서 듣고 이력 DB를 디렉터리(`--ledger`,
기본값은 `lapilli-ledger`)나 Postgres에 둡니다. Helm 차트는 `lapilli verdict`를 주
1회 CronJob으로, 또는 `mode: serve`로 `lapilli serve`를 실행합니다
([charts/lapilli](../../charts/lapilli/README.md)).

### 여러 사람이 `lapilli serve`를 쓸 수 있나요?

로그인 뒤에서 됩니다. OpenID Connect 제공자(`--oidc-issuer`), 앞단 로그인이 서명한
JWT(`--proxy-jwt`), 프록시가 넣는 헤더(`--user-header`) 중 하나입니다. 역할은 누가
볼 수 있는지(`--viewers`), 바꿀 수 있는지(`--editors`, `--admins`), 변경을 승인할 수
있는지(`--approvers`)를 정합니다. admin은 역할 페이지에서 모든 팀의 또는 한 팀의 편집
역할을 줄 수 있습니다. 역할도 로그인도 없으면, 이 머신에서만 듣는 경우에만 모두가
admin이고, 그 밖에서는 보기만 할 수 있습니다. Postgres를 쓰면 여러 복제본으로 돕니다
([매시간 정탐·오탐 가리기 (영어)](../serve.md),
[design/signin.md (영어)](../design/signin.md),
[여러 복제본 (영어)](../serve.md#several-replicas)).

### 라이선스는 무엇인가요?

Apache 2.0입니다([LICENSE](../../LICENSE)). 화면의 글꼴 Heebo는 SIL Open Font
License 1.1을 따릅니다.

### 얼마나 진행됐나요?

README의 표현으로는 초기 단계이고, 메인테이너는 한 명이며 더 필요합니다
([MAINTAINERS.md](../../MAINTAINERS.md)). 운영 중인 Grafana 한 곳에서 확인했고, 그
알림 룰 71개가 모두 백테스트됩니다([README](../../README.md)).
[ROADMAP.md](../../ROADMAP.md)에는 아직 안 되는 것이 영역별로 있고,
[아직 안 되는 것 (영어)](../limits.md)에는 지금 되는 것의 한계가 있습니다.

### 어디에 물어보나요?

질문은 [Discussions](https://github.com/lapilli-project/lapilli/discussions)의 Q&A로
보내 주세요. 같은 문제를 만난 다음 사람이 답을 찾을 수 있습니다. 아이디어는 Ideas,
보여 주고 싶은 리포트는 Show and tell에 올리되 `--redact`로 안전하게 만드세요.
울린 것과 다르게 백테스트되는 룰이나 그 밖의 잘못된 점은
[이슈](https://github.com/lapilli-project/lapilli/issues/new/choose)로 올립니다.
유형별 양식이 있습니다. 취약점은 [SECURITY.md](../../SECURITY.md)에 적힌 대로
비공개로 신고합니다. 글에 무엇을 넣지 말아야 하는지는
[SUPPORT.md](../../SUPPORT.md)에, 빌드, 테스트, 변경 보내기는
[CONTRIBUTING.md](../../CONTRIBUTING.md)에 있습니다. 유료 지원은 없습니다. 답변은
시간이 되는 대로 드립니다.
