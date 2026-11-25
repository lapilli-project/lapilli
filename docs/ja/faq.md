<!-- translated from docs/faq.md at blob b364a2ea1a264cb6c7fc159ec14053a9819bbd04 -->

# よくある質問

### 何が必要ですか？

ルールの評価対象となる履歴への読み取り権限、Prometheus 互換の API
（`--prometheus`：Prometheus、Thanos、Mimir、VictoriaMetrics、または `--auth` で使うクラウドのマネージド
Prometheus。CI の `e2e-compat` は、Thanos 0.42.5 と VictoriaMetrics 1.156.0 に対して verdict と
`--check-replay` を実行します）、ファイルまたはルールを保持する API から読み取るルール、そして何が問題だったのかの記録が必要です。履歴には `/api/v1/query_range` を問い合わせ、提供されている場合は、スクレイプ間隔を知るために
`/api/v1/status/config` と `/api/v1/targets` も問い合わせます。`--loaded-rules` は
`/api/v1/rules` も問い合わせ、評価間隔のために serve の `/api/v1/status/config` も問い合わせます。`--scrape-interval` を指定していても同じです。`--grafana` には Viewer 権限のサービスアカウントトークンが必要で、各クラウドには
[`lapilli verdict` を使う（英語）](../verdict.md)の表にある読み取り権限が必要です。

### インシデントの一覧は必要ですか？

`lapilli verdict` には、ファイルに書いたインシデント（`--incidents`）か `--impact` が必要です。両方を使うこともできます。`--impact` は、ユーザーが影響を受けている間だけ値を返す PromQL 式（たとえば 5xx の比率）で、その区間がインシデントになります。記録がまばらでも使えますし、インシデントが保管されている場所からスクリプトでコピーすることもできます（[ほかの場所に記録されたインシデント（英語）](../incidents.md)、[誰も記録しなかったインシデント（英語）](../impact.md)）。`lapilli serve` は、画面で記録したインシデントも受け付けます。何も記録されていない場合、インシデントを検知しなかった発火は、誰も記録していないだけのものと区別できないため、判定されません（[判定のしくみ（英語）](../how-it-judges.md)）。

### システムに何かを書き込みますか？

- `lapilli verdict` は履歴とファイルを読み取り、レポートを stdout に書き出します。レポートの送信先は
  `--push`（Pushgateway）と `--slack-webhook-file` だけです。
- `lapilli preflight` は何も書き込みません。送信するリクエストはすべて許可リストを通り、リストにないリクエストは送信前に拒否されます。台帳は読み取り専用で開きます（[デプロイ前に（英語）](../serve.md#before-it-is-deployed)）。
- `lapilli serve` は自分の台帳、つまり `--ledger` の SQLite ファイルか Postgres
  データベース（`--ledger-url-file`）に書き込みます。ルールの変更を提案し、`--changes` を指定したときだけ実行します。`--changes` にはサインインとロールが必要で、人がブラウザーで承認した後にだけ実行されます。実行は、リポジトリの認証情報がある場所で動くレシピが作成するプルリクエストか、書き込み用に渡した認証情報での Grafana、ルーラー、クラウドの API 経由の書き込みです（[ルールの変更（英語）](../changing-rules.md)）。
- `lapilli ledger copy` は、台帳を別の空の台帳に書き込みます。`--to-url-file` を指定すると、そのファイルが示す Postgres
  データベースに書き込み、コピー元の台帳を先に自身のスキーマに更新します（[コマンド（英語）](../commands.md#lapilli-ledger)）。
- [recipes](../../recipes) のスクリプトは別に動き、それぞれ専用のトークンを使います。タスクをトラッカーと同期させるスクリプトは、未完了のタスクごとにトラッカーに issue を作成します（[判定を続ける（英語）](../serve.md#tasks-kept-in-step-with-a-tracker)）。

### データをどこかに送りますか？

フラグとファイルで指定した先にだけ送ります。それらが示す API やフィードに、そしてそれらが必要とするものに、リクエストを送ります。クラウドの認証情報チェーン、OpenID Connect プロバイダーのエンドポイント、前段のサインインが署名に使う鍵（`--proxy-jwt google-iap` は Google の鍵を `www.gstatic.com` から、`aws-alb` はロードバランサーの鍵を
`public-keys.auth.elb.REGION.amazonaws.com` から取得します）、確認が必要な SNS サブスクリプションです。テレメトリーも更新確認もありません。人が読むために書き出す内容は、フラグで指定した送信先にだけ送られます。

- `lapilli verdict`：`--slack-webhook-file` の Slack Webhook にレポートを、`--push` にメトリクスを送ります。
- `lapilli serve`：`--slack-webhook-file`、`--team-digest`、`--notify-webhook-file` に週次ダイジェストを送ります。起きたこと（タスクの作成、インシデントの記録）は `--notify-webhook-file` に、`--notify-slack` を指定すると Slack にも送ります。人がスケジュールしたレポートは、それらと `--report-dest` に送ります。

これらにはルール名、ラベル、インシデントのタイトル、件数が含まれます。`lapilli
verdict --redact` は、すべての名前に別名を付け、送信する内容にも同じようにします（[レポートを外部に見せる（英語）](../redact.md)）。UI のページはスクリプトを実行せず、他の場所から何も読み込みません。各コマンドが何を読み取るか、`lapilli serve` が何をどのくらいの期間保管するか、何を変更できるかは、1 ページにまとめてあります。[読み取るもの、保持するもの、送るもの（英語）](../data.md)。

### 履歴はどのくらい必要で、どこまで遡れますか？

ルールが読み取るデータを履歴が保持している範囲まで遡れます。`lapilli verdict` は直近の `--since`（7d）、または `--from` から `--to` までを判定します。発火が記録されたかどうかは、その前の 2 週間にインシデントがあったかで決まり、`--impact` は期間の 2 週間前から読み取ります。`lapilli serve` は、初回の起動時に `--backfill`（28d、最大 10 年）をバックテストし、発火を `--keep`（400d）の間保管します。期間中に変わったルールは、`--rules FILE@TIME` または `--rules-history` で当時の状態のままバックテストします（[判定のしくみ（英語）](../how-it-judges.md)の "Rules that changed"）。クラウドによっては保持期間がもっと短くなります。Google Cloud は一部のメトリクスを 6 週間しか保持せず、6 週間より古い点は 10 分間隔でしか残しません（[`lapilli verdict` を使う（英語）](../verdict.md)）。

### Prometheus にどのくらい負荷をかけますか？

バックテストは、クエリを 1 日単位で問い合わせます。そのクエリを必要とするすべてのルールが 1 つのリクエストを共有するため、クエリと日ごとに 1 リクエストで、各 API に対して `--parallel`（8）個のルールを同時に処理します。API が押し返してきたとき（HTTP 429、5xx）は待ち、429 や 503 の後は同時リクエストを減らします。`--max-rate N` は 1 秒あたり N リクエストに抑えます。`--max-samples`（10,000,000）は、ルールのクエリを 1 回読み取るときに保持できる量の上限で、ピーク時にサンプルあたり 64 バイト程度です。上限を超えたルールはバックテストされません。`lapilli
serve` は、前回の実行以降の時間を 1 時間ごとにバックテストします。`--yield-on-429`、`--guard`、`--catch-up-hours` は、アラートが使うクォータに影響しないようにし、`lapilli preflight` はデプロイ前にリクエスト数、時間、メモリを見積もります（[`lapilli verdict` を使う（英語）](../verdict.md)、[design/quota.md（英語）](../design/quota.md)）。

### どのルール、データソース、クラウドに対応していますか？

ルールは、Prometheus のルールファイル、`PrometheusRule` リソース、Grafana のアラートプロビジョニングファイル、Terraform の `grafana_rule_group` の JSON、各クラウドのマネージドルールグループと PromQL で書かれたアラーム、それらを保持する ConfigMap、Loki ルーラーのルールファイル（`--loki-rules`）、そしてそれらが入ったディレクトリです。API が保持しているルールも読み取れます。`--loaded-rules`（Prometheus、Thanos、Mimir、VictoriaMetrics の `-vmalert.proxyURL` 経由の vmalert、Amazon Managed Service for
Prometheus）、`--grafana`、`--ruler`（Mimir、Cortex、Amazon のもの、Azure のもの）、Loki のルーラー（`--loki-loaded-rules`）、クラウド独自のアラーム（`--alarms`）です。Prometheus が発火させたもの（`--alerts`）と、他の場所に記録された発火（`--firings`）は、そのまま判定されます（[判定のしくみ（英語）](../how-it-judges.md)）。

クエリは、Prometheus 互換の API に対しては PromQL で、Loki に対しては LogQL でバックテストします（[ログに対するルール（英語）](../loki.md)）。SQL、Elasticsearch、InfluxDB、クラウド独自のログは対象外です（[まだできないこと（英語）](../limits.md)）。AWS、Azure、Google Cloud のマネージド Prometheus は `--auth` で読み取ります。Oracle Cloud にはないため、OKE では自分で運用している Prometheus を指定してください。4 つのクラウドの独自アラームは `--alarms` で読み取ります。PromQL で書かれたものはバックテストし、残りは一覧表示します。それらの履歴をコピーして取り込めば、その履歴から判定します（[クラウド独自のアラーム（英語）](../cloud-alarms.md)）。

### promtool、pint、vmalert の replay、Grafana のバックテストとは何が違いますか？

| ツール | できること | できないこと |
|---|---|---|
| `promtool test rules` | ユニットテストです。テストファイルに時系列を手で書き、決まった時刻に期待するアラートを書きます。 | 履歴もインシデントもありません。 |
| pint | ルールを lint します。CI でも動きます。稼働中の Prometheus につなぐと、たとえば時系列が存在するかを確認し、`alerts/count` チェックは、ある期間にルールが何回発火したかを見積もります。 | 発火を数えるだけで、その発火が何かを検知したかは分かりません。 |
| vmalert `-replay` | 過去の期間の履歴に対してルールを再評価し、その結果を時系列として書き戻します。 | 発火を再現するだけで、判定はしません。 |
| Grafana アラートルールのバックテスト | `alertingBacktesting` 機能トグルの背後にある実験的な機能で、既定ではオフです。Grafana 内で 1 つのルールを過去の期間に対して評価します。 | ルールの状態を再現するだけで、判定はしません。 |
| Lapilli | ルールを履歴に対してバックテストし（PromQL は履歴自身のエンジンが `query_range` で評価します）、発火ごとにインシデントと並べます。何かを検知したか、何も検知しなかったかです。 | 発火を判定するには、インシデントの記録か `--impact` 式が必要です。ないときは、発火を判定しません。 |

Lapilli は、発火ごとにインシデントを検知したかを示し、どの発火も検知しなかったインシデントは見逃しです。`--check-replay` は、バックテストを実際に発火したものと照らし合わせ、プルリクエストのバックテストは、ルールの変更をマージする前に同じことをします（[CI でのプルリクエストのバックテスト（英語）](../pr-backtests.md)）。

これらは一緒に使うものです。ルールを書くときは pint と promtool、ルールがしばらく発火した後と、それを変更するプルリクエストでは Lapilli を使います。

### Grafana 管理のアラートにも対応していますか？

はい。`--grafana URL` は、Grafana が保持しているルールを、UI で作成したものかどうかにかかわらず読み取ります。Viewer のトークンを `--grafana-token-file` で渡します。`--rules` は、そのプロビジョニングファイルを読み取ります。Grafana と同じ方法で評価します。reduce、threshold、math、classic condition、そしてクエリが何も返さないときの動作まで、Grafana 自身の状態のとおりです。クエリのエラーと、PromQL でも LogQL でもないデータソースに対するクエリは、バックテストできません（[判定のしくみ（英語）](../how-it-judges.md)、[まだできないこと（英語）](../limits.md)）。`--check-replay` は、バックテストを Grafana 自身の状態履歴と並べます（[バックテストは正しいか（英語）](../check-replay.md)）。

### Alertmanager のルーティング、サイレンス、抑制もバックテストしますか？

いいえ。ルールが発火させたとおりに発火を数えるだけで、Alertmanager のルーティング、グループ化、抑制、サイレンス、Grafana の通知ポリシーは、設定から読み取りません（[まだできないこと（英語）](../limits.md)）。`lapilli serve` は、Webhook レシーバー、または Alertmanager 0.33.0 以降ではそのイベントレコーダーから、実際に送られた通知を発火と並べて保持できます（[送信された通知そのまま（英語）](../notifications.md)）。サイレンスや抑制が発火を止めたときもそれと分かるため、なぜ誰も呼び出されなかったかが分かります。`lapilli serve` のメンテナンスウィンドウは独自のもので、その間に起きた、インシデントを検知しなかった発火を免除します。

### Kubernetes は必要ですか？

いいえ。`lapilli` は 1 つのバイナリで（`go install lapilli.dev/cmd/lapilli@latest`、リリース、またはイメージ
`ghcr.io/lapilli-project/lapilli`）、履歴に到達できる場所ならどこでも動きます。`lapilli serve` は `127.0.0.1:8080` で待ち受け、台帳をディレクトリ（`--ledger`、既定は `lapilli-ledger`）か Postgres に置きます。Helm チャートは、`lapilli verdict` を週次の
CronJob として、または `mode: serve` で `lapilli serve` を実行します（[charts/lapilli](../../charts/lapilli/README.md)）。

### 複数人で `lapilli serve` を使えますか？

はい。サインインの後ろで使えます。OpenID Connect プロバイダー（`--oidc-issuer`）、前段のサインインが署名した JWT（`--proxy-jwt`）、プロキシが設定するヘッダー（`--user-header`）のいずれかです。ロールで、誰が見られるか（`--viewers`）、変更できるか（`--editors`、`--admins`）、変更を承認できるか（`--approvers`）を決めます。admin は、ロールのページで、すべてのチームの、または 1 つのチームの編集ロールを与えられます。ロールもサインインもない場合は、このマシンだけで待ち受けているときにのみ全員が admin になり、それ以外では閲覧しかできません。Postgres なら、複数のレプリカとして動かせます（[判定を続ける（英語）](../serve.md)、[design/signin.md（英語）](../design/signin.md)、[複数のレプリカ（英語）](../serve.md#several-replicas)）。

### ライセンスは何ですか？

Apache 2.0 です（[LICENSE](../../LICENSE)）。UI のフォント Heebo は SIL Open Font License 1.1 です。

### どのくらい進んでいますか？

README の表現では、初期段階で、メンテナーは 1 人、さらに加わってもらう余地があります（[MAINTAINERS.md](../../MAINTAINERS.md)）。本番環境の Grafana 1 つで確かめたところ、71 個のアラートルールはすべてバックテストできます（[README](../../README.md)）。また、バックテストは、実際の Prometheus、Thanos、vmalert を使った VictoriaMetrics に対して、CI と、最新リリースでの週次の実行で確認しています（[バックテストは正しいか（英語）](../check-replay.md)）。[ROADMAP.md](../../ROADMAP.md) には、まだできないことが分野ごとにまとめてあり、[まだできないこと（英語）](../limits.md)には、今できることの限界が書かれています。

### どこで質問できますか？

質問は [Discussions](https://github.com/lapilli-project/lapilli/discussions) の Q&A に送ってください。同じ疑問を持った次の人が答えを見つけられます。アイデアは Ideas に、見せたいレポートは Show and tell に投稿し、`--redact` で安全にしてください。発火したときと違う結果になるルールや、その他の不具合は、[issue](https://github.com/lapilli-project/lapilli/issues/new/choose) として報告してください。種類ごとにフォームがあります。脆弱性は、[SECURITY.md](../../SECURITY.md) の指示どおり、非公開で報告してください。投稿に含めてはいけないものは [SUPPORT.md](../../SUPPORT.md) に、ビルド、テスト、変更の送り方は [CONTRIBUTING.md](../../CONTRIBUTING.md) にあります。有償サポートはありません。回答は、時間が許す範囲で行います。
