<!-- translated from docs/getting-started.md at blob bdc3b1ddc9c9f475ab5f547062ef287c85375ed0 -->

# はじめに

Lapilli のインストールから、自分のアラートで最初のレポートを出すまでを説明します。必要なのは、今使っているルールと、その履歴を持つ Prometheus 互換の API です。`lapilli verdict` は履歴とファイルを読み取るだけで、レポートは指定した送り先以外には送りません。

## まずサンプルデータで見る

```sh
lapilli demo
```

は、小さなショップの架空のアラートルール 7 つを、架空の 1 週間分の履歴でバックテストし、そのショップのインシデントと照らし合わせて判定します。`lapilli verdict` と同じコードを使います。履歴はその場で作られるので、接続するものはなく、何も読み取らず、何も送りません。レポートには、このページの残りで自分のアラートから見つけるものがひととおり含まれています。ノイズルールとそれを静かにする長めの `for`、インシデントを検知したルール、何も検知しなかったインシデントとあと少しで検知できたルール、人が眠っている間に発火したアラートです。`--json` はレポート全体を JSON で出力し、`--tz` はサンプルの 1 週間を置くタイムゾーンを指定します。Go はあるものの Lapilli をインストールしていない場合は、`go run lapilli.dev/cmd/lapilli@latest demo` で同じ結果を得られます（[docs/design/demo.md（英語）](../design/demo.md)）。`lapilli demo --write sample/` は、サンプルのルールとインシデントを `rules.yaml` と `incidents.yaml` としてそのディレクトリに書き出します。2 つの形式を見て、自分用にコピーできます。

```sh
lapilli demo --serve
```

は、同じ 1 週間を `lapilli serve` の画面で見せます。判定するのは serve 自身の毎時のジョブです。開く URL が表示されます。開けるのはこのマシンからだけで（`--listen`。既定ではループバックアドレスの空いているポート）、読み取り専用です。台帳は一時ディレクトリに置かれ、Ctrl-C で終了すると削除されます。時計はサンプルの 1 週間の終わりに合わせてあり、その週が画面に出る最後の丸 1 週間です。チームが 3 つあるので、「概要」でチームを選んで「チーム」ページを見られます。承認待ちの変更提案も 1 つあります。レポートが提案する長めの `for` を、バックテスト付きで出したものです。読み取り専用なので承認はできず、読むだけです。

## インストール

```sh
go install lapilli.dev/cmd/lapilli@latest
```

Go 1.26.11 以降が必要です（`go.mod` のとおり）。または、[リリース](https://github.com/lapilli-project/lapilli/releases)（macOS と Linux、amd64 と arm64）からバイナリを入手するか、イメージ `ghcr.io/lapilli-project/lapilli` を使います。このイメージのエントリポイントは `lapilli` です。入手したものをオフラインでも確認する方法は、[リリースの検証（英語）](../../SECURITY.md#checking-a-release)にあります。

## インシデントを 1 つか 2 つ記録する

発火は、実際に何が問題だったかと照らし合わせて判定されるので、Lapilli にはその記録が必要です。最も簡単なのは YAML ファイルです。

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

- `id` と `start` は必須です。`id` はファイル内で重複しないもの、`start` はオフセット付きの RFC 3339 の時刻（`Z`、`+09:00`）です。
- `end`、`title`、`labels` は省略できます。`end` がなければ、インシデントは 1 時間続いたものとして扱います。`end` を `start` より前にすることはできません。`labels` は問題が起きた場所を表し、ラベルごとに値 1 つか値のリストを書きます。`--match service` を付けると、`service` ラベルを持つ発火が検知するのは、その値を挙げているインシデントか、`service` ラベルを持たないインシデントだけです。ラベルのない発火やインシデントは、その条件で絞られません。

対象期間の前の 2 週間のインシデントも記録してください。何も検知しなかった発火を判定するのは、その発火の前の 2 週間のどこかでインシデントがあったとき、または発火の時間帯に `--impact` を読み取ったときだけです。そうでなければ、判定せずに数えるだけです（[記録なし（英語）](../numbers.md#no-record)）。INC-100 はそのためにあります。下で判定する週より前なので表には出ませんが、その週のすべての発火が判定されます。

`incidents` キーのないファイルは、空のファイルも含めて拒否されます。ジョブが失敗したことが、問題のなかった 1 週間に見えてしまわないようにするためです。インシデントがないときは `incidents: []` と書いてください。または、別の方法でインシデントを用意します。

- [ほかの場所に記録されたインシデント（英語）](../incidents.md)：PagerDuty、incident.io、Jira、GitHub の Issue などのツールから、このファイルを書き出すスクリプト。
- [誰も記録しなかったインシデント（英語）](../impact.md)：`--impact PROMQL`。ユーザー影響を示すシグナルの区間をインシデントとして扱います。ファイルと併用することも、単独で使うこともできます。

### 記録したインシデントがない場合はここから

`lapilli impact suggest --prometheus http://prometheus:9090` は、[誰も記録しなかったインシデント（英語）](../impact.md#which-of-them-your-history-has)に挙げられているサービス前段のスタックのうち、どれのメトリクスをその Prometheus が持っているかを調べます。対象は Istio、Linkerd、Envoy、ingress-nginx、Traefik、OpenTelemetry の HTTP サーバーメトリクスです。見つかったものごとに、系列の数、直近 1 週間にその `--impact` が見つけるユーザー影響の区間の数、そして `--incidents` の代わりに `lapilli verdict` に渡す `--impact '...'` を表示します。読み取るだけで、何も選びません。ラベルとしきい値を先に確認してください。

## 先週を判定する

```sh
lapilli verdict --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

`--rules` は、`--prometheus` の履歴に対してルールをバックテストします。渡せるのは、Prometheus のルールファイル、`PrometheusRule` リソース、Grafana のアラートのプロビジョニングファイル、`grafana_rule_group` の Terraform の JSON（[docs/verdict.md（英語）](../verdict.md#rules-kept-in-terraform)）、それらを収めた ConfigMap、またはそれらのディレクトリです。期間は直近 7 日（`--since 7d`）、または RFC 3339 で指定する `--from` から `--to` までで、時刻はマシンのタイムゾーンか `--tz` で表示します。`--match service` は、同じ時間帯に起きたというだけで、あるサービスのアラートが別のサービスのインシデントを検知したことにならないようにします。付けなければ、時間が合う発火はどれもインシデントを検知したことになります。

`--rules alert-rules/` の代わりに、次も使えます。

- `--loaded-rules`：`--prometheus` の API が読み込んでいるルール。
- `--grafana https://grafana.example.com --grafana-token-file grafana-token`：Grafana が持っているルール。閲覧者（Viewer）のサービスアカウントトークンで読み取り、`--prometheus` も引き続き指定します。
- `--alerts`：Prometheus が発火したものを `ALERTS` 系列から取得します。バックテストはしないので、長めの `for` の提案も、ニアミスの探索もありません。

クラウドのマネージド Prometheus には、`--auth aws`、`azure`、`google` で、マシンや Pod がすでに持っている認証情報を送ります。トークンは `--prometheus-token-file FILE` で渡します。クラウドごとの URL と必要な権限は、[`lapilli verdict` を使う（英語）](../verdict.md)にあります。

## レポートを読む

架空の 1 週間を `--from`、`--to`、`--tz UTC` で指定し、上のインシデントと、3 つのルール（`CheckoutErrors`、`NodeLoadHigh`、`QueueBacklog`）、そして Prometheus の代わりに応答する代役を使うと、上のコマンドは次を出力します。3 つの部分は省略しています。

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

上から順に説明します。

- **期間**と、その期間の発火とインシデントの数です。一部の発火に判定の根拠となる記録がなかった場合は、その下の行に件数が出ます。
- **発火したルールごと**の行：発火数、インシデントを検知した発火（○、白い石）、何も検知しなかった発火（●、黒い石）、適合率（判定した発火のうち、何かを検知した割合。`PRECISION`）、そして同じアラートが解消してから `--refire`（1h）以内に起きた発火です。
- **ノイズルール**：発火が 10 回以上で、何かを検知した割合が 20% 未満のルールです。長めの `for` でバックテストし直します。それぞれが検知した全インシデントを保てる最短の `for` で、発火は多くても半分になります。
- **インシデント**：それぞれが続いた時間（終了を記録していなければ `–`）、検知したルール、そして開始から何分後（`+3m`）または何分前（`-9m`）だったかです。`(noisy)` はノイズルールによる検知、`✗ nothing fired` は見逃しを示します。
- **あと少しで検知できたルール**：条件は成立したものの `for` に届かなかったルール、または値がしきい値まであと 20% 以内に迫ったルールです。インシデントを検知できたはずの変更と、そうしていた場合の発火の頻度が分かります。`for` を 6m にしていれば、`QueueBacklog` は INC-103 の 1 分後に、その週で 1 回発火していたはずです。

各表の列が表すものは次のとおりです。列の名前はレポートの英語のままなので、`code` で示します。

| 表 | 列 | 意味 |
|---|---|---|
| 発火したルール | `RULE` | ルールの名前 |
| | `SEVERITY` | ルールの `severity` ラベル。同じ名前でバージョンやルールが異なる場合は、複数を `/` でつなぎます。どのルールにも付いていなければ、この列は表示されません |
| | `FIRED` | 期間中の発火数 |
| | `○ CAUGHT` | インシデントを検知した発火の数 |
| | `● NOTHING` | 何も検知しなかった発火の数。インシデントが終わった後に起きたものと、判定する記録がないものも含みます |
| | `PRECISION` | 判定した発火のうち、何かを検知したものの割合（適合率）。判定したものがなければ `–` |
| | `REFIRED` | 同じアラートが最後に解消してから `--refire`（既定は 1h）以内に起きた発火の数（再発火） |
| 長めの `for` | `FOR` | ルールの現在の `for` と、検知した全インシデントを保てる、より長い最短の `for` |
| | `FIRED` | 現在の `for` での発火数と、長めの `for` での発火数 |
| | `INCIDENTS` | 検知したインシデントの数と、長めの `for` でも検知できる数 |
| | `CAUGHT LATER BY` | 検知が遅れる時間の最大値。インシデントがなければ `–` |
| 各ルールが加えるもの | `INCIDENTS` | 検知したインシデントの数 |
| | `FIRST` | ほかのどのルールよりも先に検知したインシデントの数 |
| | `ALONE` | ほかのどのルールも検知しなかったインシデントの数 |
| インシデント | `INCIDENT` | インシデントの id とタイトル |
| | `START` | 開始時刻（`--tz`） |
| | `LASTED` | 開始から終了までの時間。終了を記録していなければ `–` |
| | `CAUGHT BY` | 検知したルール。それぞれに、最初の発火が開始の何分後（`+3m`）または何分前（`-9m`）だったかが付きます。`(noisy)` はノイズルールによる検知、`(linked)` はインシデントが挙げているアラートによる検知です |

`CAUGHT BY` の `✗ nothing fired` は、そのインシデントに対して発火したルールがまったくなかったことを表します。`✗ nothing in time; after its end` は、間に合って発火したルールはなかったものの、インシデントの最中に条件が成立していたルールが、終了後に発火したことを表します。それぞれに終了の何分後かが付きます。それらの発火も、やはり黒い石です。`? not known` は、バックテストしたルールのどれも発火しなかったものの、バックテストできなかったルールがあり、それが発火していたかもしれないことを表します。表の下の行がそれを別に数えており（`? N of M incidents ...`）、`--json` では各インシデントが `"not_known": true` になります。

上では省略したものがあります。発火が何も検知しなかった場合の、`--tz` での時間帯別の内訳（睡眠時間、業務時間外、業務時間）、各ルールが加えるもの（それぞれが検知したインシデント、最初に検知したもの、単独で検知したもの、代替可能なルール）、そして一度も発火しなかったルールやバックテストできなかったルールとその理由です。レポートが何に基づくかは[判定のしくみ（英語）](../how-it-judges.md)に、各数値の定義と、`--json`（発火をすべて含むレポート全体）から数え直す方法は[数値の意味（英語）](../numbers.md)にあります。

## 次のステップ

ルールを変更する前に、`--compare` は、提案するルールを今のルールと並べて、同じ期間、同じインシデントに対してバックテストします（[変更をマージする前に（英語）](../compare.md)）。

```sh
lapilli verdict --rules proposed/ --compare alert-rules.yaml --prometheus http://prometheus:9090 \
  --incidents incidents.yaml
```

Helm チャートは、毎週月曜 09:00 に、その前の 1 週間を対象に `lapilli verdict` を実行し、レポートを Slack に、メトリクスを Pushgateway に送ります（[毎週（英語）](../every-week.md)）。

`lapilli serve` は、毎時の判定を、Web UI のある台帳に記録し続けます（[判定を続ける（英語）](../serve.md)）。`lapilli preflight` は同じフラグを受け取り、何に直面するかを示します。ルール、履歴、そして実行にかかるリクエスト数とメモリです。何も書き込まず、バックテストもせず、問題がなければ終了コード 0 で終わります（[デプロイ前に（英語）](../serve.md#before-it-is-deployed)）。

```sh
lapilli preflight --rules alert-rules/ --prometheus http://prometheus:9090 \
  --incidents incidents.yaml --match service
```

同じフラグを付けた `lapilli serve` は、初回の起動時に直近 28 日をバックテストし（`--backfill`）、台帳を `lapilli-ledger` に保存し、`127.0.0.1:8080` で待ち受けます。そこに到達できる人は全員が管理者です。Kubernetes では、[チャート](../../charts/lapilli/README.md)に渡すのはこれだけで足ります。

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

`rules.configMaps` には、リリースと同じ名前空間にあるルールの ConfigMap を指定します。`incidents` にはファイルと同じリストを渡せます。または、UI でインシデントを記録します。ロールを設定しなければ、Pod に到達できる人は全員、見ることしかできません（[ユーザーの区別（英語）](../../charts/lapilli/README.md#who-people-are)）。
