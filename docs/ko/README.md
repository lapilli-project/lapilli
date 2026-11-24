<!-- translated from docs/README.md at blob d1bd96ee70d5126de152f6e3ce7c89f14ea25ddc -->

# Lapilli 문서

Lapilli가 알림 룰의 정탐·오탐을 어떻게 가리는지, 그리고 어떻게 돌리는지를
설명합니다. 기간을 정해 `lapilli verdict`로 한 번 돌리거나, `lapilli serve`로
매시간 돌립니다. 같은 문서가 [lapilli.dev/docs](https://lapilli.dev/docs/)와
[위키](https://github.com/lapilli-project/lapilli/wiki)에도 있습니다. 위키는
docs/를 변경할 때마다 다시 써서 만든 사본입니다. Lapilli가 무엇이고 어떻게
설치하는지는 [README](../../README.md)에 있습니다.

아직 일부 페이지만 한국어로 옮겼습니다(시작하기, 용어, 자주 묻는 질문). 나머지는
영어 페이지로 연결됩니다. 한국어로 옮긴 뒤 영어 페이지가 바뀌었으면 해당 한국어
페이지에 그렇게 적어 둡니다.

## 시작

- [시작하기](getting-started.md): `lapilli demo`로 샘플 데이터에서 첫 리포트를
  보고, 설치한 뒤 장애를 한두 건 적어 두고, 내 알림으로 첫 리포트를 읽습니다.
  그다음 `lapilli serve`로 계속 정탐·오탐을 가립니다
- [용어](glossary.md): 돌, 알림, 장애, 이력 DB 등 이 문서가 쓰는 말
- [자주 묻는 질문](faq.md): 무엇이 필요한지, 무엇을 기록하는지, 무엇을
  어디로 보내는지

## 기간을 정해 가리기

- [정탐·오탐을 가르는 기준](../how-it-judges.md): 알림과 장애가 어디서 오는지,
  알림이 언제 장애를 잡은 것인지, 노이즈 룰과 바꿀 것, 룰이 더하는 것, 알림이
  언제 울렸는지, 니어 미스, 바뀐 룰
- [`lapilli verdict` 사용법](../verdict.md): 명령, 만들어 주는 리포트, 읽는 데이터
  소스와 속도, 실패하는 경우
- [숫자의 의미](../numbers.md): 각 숫자가 무엇인지, 그리고 Lapilli 없이 리포트에서
  다시 세는 방법
- [클라우드 자체 알람](../cloud-alarms.md): CloudWatch, Azure Monitor, Google
  Cloud, OCI 알람이 울린 것과, Datadog 모니터와 New Relic 알림 조건의 정탐·오탐
  가리기
- [로그 룰: Loki](../loki.md): Loki를 대상으로 LogQL 룰 백테스트
- [백테스트가 맞는지 확인하기](../check-replay.md): `--check-replay`, 실제로 울린
  알림과 나란히 놓은 백테스트
- [알림 내용 점검](../check-content.md): `--check-content`, 룰이 울릴 때 무엇을
  말하는지와 어떻게 쓰여 있는지

## 변경하기 전에

- [병합 전에 비교하기](../compare.md): `--compare`, 제안한 룰을 지금 쓰는 룰과
  나란히 놓기, 그리고 룰에서 기대하는 동작(`--expect`)
- [CI의 PR 백테스트](../pr-backtests.md): 알림 룰을 바꾸는 PR마다 댓글로 다는
  백테스트와, 병합을 막을 수 있는 커밋 상태

## 장애 기록

- [다른 도구에 있는 장애 기록](../incidents.md): 장애 기록을 보관된 곳에서
  복사해 오기, 그리고 그 도구가 장애마다 연결해 둔 알림
- [기록되지 않은 장애](../impact.md): `--impact`, 사용자 영향 신호에서 찾은 장애.
  `lapilli impact suggest`는 내 이력에 어떤 신호가 있는지 알려 줍니다

## 리포트

- [주간 리포트](../every-week.md): Helm 차트의 주간 리포트를 Slack과 Pushgateway로
  보내기
- [외부에 보여 줄 리포트](../redact.md): `--redact`, 모든 이름을 별칭으로 바꾼
  리포트

## 매시간: `lapilli serve`

- [매시간 정탐·오탐 가리기](../serve.md): 이력 DB와 대시보드, 팀, 작업, 목표.
  배포하기 전에 점검하기, Postgres의 이력 DB, 여러 복제본, 다른 곳에 기록된
  알림, 트래커와 맞춰 두는 작업
- [실제로 보낸 알림](../notifications.md): 알림과 함께 보관하는 전송 기록, 그리고
  알림이 울렸는데 아무도 호출하지 않은 이유
- [룰 변경](../changing-rules.md): 변경 제안, 승인, 룰을 보관하는 곳에서 이루어지는
  변경
- [MCP로 묻기](../mcp.md): `/mcp`의 MCP

## 참고 자료

- [명령](../commands.md): `lapilli verdict`, `serve`, `preflight`, `ledger`,
  `impact`, `demo`의 모든 옵션, `-h`가 출력하는 그대로
- [HTTP API](../api.md): `lapilli serve`가 응답하는 모든 엔드포인트와 각각에 필요한
  역할
- [메트릭](../metrics.md): `lapilli serve`가 `/metrics`로 내보내는 모든 메트릭
- [업그레이드](../upgrading.md): 어느 버전에서 이력 DB가 바뀌었는지, 그리고 되돌리기

## 그 밖에

- [문제 해결](../troubleshooting.md): 오류나 이상한 숫자가 무슨 뜻인지, 어떻게
  해야 하는지
- [아직 안 되는 것](../limits.md): 백테스트가 아직 재현하지 못하는 쿼리, 상태,
  타이밍, 그리고 일부만 읽는 것
- Helm 차트: [charts/lapilli](../../charts/lapilli/README.md)
- 장애, 알림, 작업, 이전 버전의 룰을 복사해 오고, 승인된 변경으로 PR을 열고, CI에서
  백테스트를 돌리는 스크립트: [recipes](../../recipes)
- 각 부분을 어떻게 설계했는지: [design](../design)
