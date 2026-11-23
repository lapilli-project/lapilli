---
layout: default
home: true
lang: ja
permalink: /ja/
title: そのアラート、人を呼ぶ価値はありますか？
description: "Lapilli は、すべての発火を実際に起きた問題と照らし合わせて判定します。インシデントを検知していれば白い石、何も問題がなければ黒い石です。lapilli verdict で一度だけ、または lapilli serve で毎時実行できます。"
---

<p class="tags"><span class="tag">Apache-2.0 · オープンソース</span>{% if site.version %}
<a href="{{ site.repo }}/releases/tag/{{ site.version }}">Lapilli {{ site.version }} をリリースしました：新しくなった点</a>{% endif %}</p>

# そのアラート、人を呼ぶ価値はありますか？

<p class="lede">Lapilli は、すべての発火を実際に起きた問題と照らし合わせて判定します。アラートがインシデントを検知していれば白い石、何も問題がなければ黒い石です。ルールの石を数えれば、そのルールが残しておく価値のあるものかが分かります。石が一つもないインシデントは、アラートが見逃したものです。</p>

<div class="cta">
  <a class="primary" href="{{ '/docs/getting-started/' | relative_url }}">はじめる <span aria-hidden="true">→</span></a>
  <a href="{{ '/docs/' | relative_url }}">ドキュメントを読む</a>
  <a href="#help-build-it">開発に参加する</a>
</div>

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```

<p class="hint"><code>lapilli demo</code> は、小さなショップの架空の
1 週間分のアラートを、何も接続せずに判定します。<a href="#install">バイナリ、コンテナイメージ、Helm チャート</a>からも始められます。</p>

<div class="shot">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="{{ '/serve-dashboard-dark.png' | relative_url }}">
  <img alt="lapilli serve の概要画面。サイドバーにページが並んでいます。先週の適合率、発火、見逃したインシデント、検知までの時間（なし）、ノイズルール、睡眠時間中に何も検知しなかった発火の通知、再発火、デプロイ中の発火があり、それぞれに週ごとの推移を示す線が付き、ほとんどは前週と並べて表示されています。その下は「まず確認すること」で、ノイズを減らすべきルールや確認すべきルールが、それぞれタスクにするボタン付きで並んでいます" src="{{ '/serve-dashboard.png' | relative_url }}" width="1240" height="980">
</picture>
</div>

<ul class="facts">
  <li><b>読み取り</b> Prometheus とその API に対応したもの、Grafana、Loki、クラウド独自のアラーム</li>
  <li><b>実行</b> <code>lapilli verdict</code> で一度だけ、<code>lapilli serve</code> で毎時</li>
  <li><b>保存</b> SQLite ファイル 1 つ、または複数のレプリカで共有する Postgres データベース</li>
  <li><b>変更</b> 人が承認したものだけ。既定では読み取りのみ</li>
</ul>

使うのは、すでに手元にあるものだけです。ルール、Prometheus
の履歴、そしてどんな形であれ残しているインシデントの記録です。記録はまばらでも構いません。既定では読み取るだけで、変更するのは人が承認したものだけです。

まだ初期段階で、メンテナーは 1 人、さらに加わってもらう余地があります。本番環境の Grafana
1 つで確かめたところ、71 個のアラートルールはすべてバックテストでき、当時のルールで
4 週間をバックテストすると、最もノイズの多いルールの発火は 177
回でした。そのチームが手作業で数えた通知の数と同じです。

## 一度だけ：`lapilli verdict` {#once-lapilli-verdict}

1 週間、1 か月、あるいはこれからマージする変更をバックテストすると、石の数、睡眠中に無駄に人を起こした発火、ノイズルールとそれを静かにする長めの
`for`、どのアラートも検知しなかったインシデント、そしてあと少しで検知できたルールが分かります。

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

38 rules never fired.

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

`--compare` は、現在のルールと提案されたルールを同じ期間でバックテストし、ルールごとの石とインシデントごとの検知を変更前 →
変更後で示します。インシデントがなければ、各ルールがどれだけ発火するかを示します。`--expect`
は、変更されるルールに期待される水準を守らせます。1 週間に何回まで発火してよいか、睡眠時間中には何回までか、そして検知し続けなければならないインシデントです。ルールを変更するプルリクエストで、CI
として実行してください。GitHub、GitLab、Bitbucket、Azure DevOps、Gitea 向けのスクリプトと GitHub Actions
向けのアクションが、レポートを Markdown でプルリクエストにコメントし、その 1 件のコメントを最新に保ちます。レポートは JSON
や Prometheus メトリクスとしても出力でき、Slack や Pushgateway にも送れます。

`--check-replay` は、Prometheus、Grafana
自身のアラート状態履歴、またはクラウドのアラームが記録した実際の発火と、バックテストの結果をルールごとに並べます。判定を信じる前に、自分のルールでバックテストを確かめられます。`--check-content`
は各アラートが発火したときに伝える内容を確認し、`--redact` は外部に見せるレポートのために、すべての名前を別名に置き換えます。

## 毎時：`lapilli serve` {#every-hour-lapilli-serve}

`lapilli serve` は台帳を持ちます。すべての発火、インシデント、ルールのバージョンを記録し、毎時、その時点のルールでバックテストします。石は Web UI
に表示され、チームはそこで対応します。デプロイする前に `lapilli preflight`
に同じフラグを渡すと、何に直面するかを示します。バックテストできるルール、履歴とそこへのサインイン、台帳、そして実行にかかるリクエスト数とメモリです。読み取るだけで、何も書き込みません。

<div class="grid">
  <div><h3>ダッシュボード</h3><p>先週の適合率、発火、見逃したインシデント、ノイズルール、睡眠中に無駄に人を起こした発火を前週と並べて表示します。12 週間の週別推移、ルール別・週別のノイズの所在、まず確認すべきことも示します。</p></div>
  <div><h3>すべての変更をバックテスト</h3><p>ルールの新しいバージョンは、その直前の 1 週間でバックテストされます。その変更が何をもたらしたはずかが分かります。</p></div>
  <div><h3>インシデントの記録</h3><p>インシデントを記録するか、黒い石が実際の問題だったと指定すると、Prometheus に問い合わせることなく、台帳全体がすぐに判定し直されます。</p></div>
  <div><h3>メンテナンスウィンドウ</h3><p>デプロイ、メンテナンス、夜間バッチなど。ウィンドウ内で何も検知しなかった発火は免除されます。1 回だけ、または毎日・毎週の繰り返しで設定できます。</p></div>
  <div><h3>イベント期間</h3><p>セール、ローンチ、キャンペーンなど。ファイル、UI、PromQL 式で名前を付けます。何も免除せず、すべての数を期間の内と外に分けて読み、各期間中の適合率を直前 4 週間と並べて示します。</p></div>
  <div><h3>目標</h3><p>チームがアラートに求める基準です。適合率、見逃したインシデント、critical の発火、再発火の上限や下限を、週ごとまたはセールごとに設定します。未達の目標は通知され、ボタン一つでタスクになります。</p></div>
  <div><h3>ユーザー影響の指標</h3><p>エラー率やレイテンシなど、ユーザーが影響を受けていることを示す指標に名前を付けると、その条件が成り立つ区間もインシデントになります。各週の発火は、それに紐付いた指標で判定されます。</p></div>
  <div><h3>チーム・タスク・コメント</h3><p>ルールとインシデントはチームが所有します。おすすめの対応はボタン一つでタスクになり、担当者、期限、トラッカーへのリンクを付けられます。</p></div>
  <div><h3>インシデントを記録元から</h3><p>スクリプトが PagerDuty、incident.io、Jira、GitHub の Issue からインシデントをコピーし、トークン付きでインシデントだけを受け付ける専用のポートに送ります。同じ内容をもう一度送っても何も起きません。</p></div>
  <div><h3>クラウドのアラームを保存</h3><p>スクリプトが CloudWatch、Azure Monitor、Cloud Monitoring、OCI のアラーム履歴を読み取り、専用のトークンでコピーするので、台帳にはクラウドよりも長く記録が残ります。独自のルールがなくても、lapilli serve はそれだけで動きます。アラーム自体もクラウドの API から読み取るため、アラームとその履歴は 1 つのルールとして扱われます。</p></div>
  <div><h3>発火だけでなく通知も</h3><p>Alertmanager、Grafana、SNS、Azure Monitor、Cloud Monitoring が実際に送った通知を、専用のポートで受け取ります。ルールごとの通知、繰り返し、睡眠時間中の通知を、発火と並べて示します。</p></div>
  <div><h3>通知とダイジェスト</h3><p>起きたことは、変更と同時に書き込まれる送信キューを通じて、JSON で Webhook に、また Slack に送られます。週次ダイジェストも同様で、チームごとに専用のダイジェストを持てます。</p></div>
  <div><h3>Git で宣言</h3><p>チーム、メンテナンスウィンドウとイベント期間、目標、レポート、権限を 1 つのファイルで宣言します。ファイルが変われば、再起動なしでそのとおりに保たれます。宣言されたものは UI では読み取り専用です。</p></div>
  <div><h3>権限</h3><p>閲覧者、編集者、管理者。Lapilli 自身が任意の OpenID Connect プロバイダーでサインインさせるか、前段に置いたサインイン（クラウドの identity-aware proxy、ロードバランサーのもの、oauth2-proxy）を使います。</p></div>
  <div><h3>変更の効果</h3><p>ルールの変更ごとに、その予測と実際の結果を並べます。インシデントの記録がまだ届いているか、Lapilli が使われているかも示します。台帳から計算し、どこにも送りません。</p></div>
  <div><h3>承認された変更</h3><p>長めの for、より近いしきい値、代替可能なルールの廃止、人が書いたランブックのリンク。それぞれがバックテスト付きで提案され、そのページで人が承認し、もう一度確認されてから反映されます。リポジトリの認証情報を持つスクリプトが GitHub、GitLab、Bitbucket、Gitea、Azure DevOps にプルリクエストとして開くか、変更が許可されている場所では Grafana の API、Mimir・Cortex・Amazon・Azure の ruler の API、またはクラウド独自のアラーム（CloudWatch、Azure Monitor、Cloud Monitoring、OCI）の API を通じて適用します。有効にするまではオフです。停止スイッチと上限があり、発火中のものは決してオフにしません。</p></div>
  <div><h3>MCP 経由のエージェント</h3><p>エージェントはページに表示される内容を問い合わせ、自分が提案するルール変更を誰かが行う前にバックテストし、許可されていれば、人が承認するための変更提案として保存できます。提案を承認したり適用したりすることはありません。</p></div>
  <div><h3>英語・スペイン語・日本語・韓国語</h3><p>言語ごとに翻訳ファイルが 1 つあり、翻訳のないテキストがあるとテストが失敗します。</p></div>
</div>

台帳は SQLite ファイル 1 つか、複数のレプリカが共有し、そのうち 1 つがリーダーを務める Postgres
データベースです。テストは、12 週間・20 万件の発火に対する毎時の実行を 2 秒以内に、各ページを
ルールごとに数件のクエリ（発火ごとに 1 件ではなく）に抑えています。`/metrics` は石を Prometheus に渡し、Helm
チャートにはそれを表示する Grafana ダッシュボードが含まれます。UI はスクリプトを一切実行しません。

## 判定のしくみ {#how-it-judges}

- **発火**は、アラートルールを履歴に対してバックテストして得ます。ルールとして使えるのは、Prometheus
  のルールファイル、`PrometheusRule`、Grafana アラートのプロビジョニング、クラウドのマネージドルールグループ、PromQL
  で書かれたクラウドのアラーム（CloudWatch PromQL アラーム、Google Cloud のアラートポリシー）、またはそれらを収めた ConfigMap
  で、それぞれのバージョンを有効だった期間に使います。LogQL で書かれた Loki ruler のルールも使えます。Prometheus や Grafana
  が読み込んでいるルールも使えます。発火は `ALERTS` 系列や、クラウドのアラーム履歴のようにファイルやジョブで取り込んだものからも得られます。Grafana
  のルールは Grafana と同じ方法で評価します。
- **インシデント**は、YAML ファイル、UI と API、スクリプト経由の PagerDuty・incident.io・Jira・GitHub の
  Issue、そしてユーザー影響の指標が成り立つ区間（`--impact`）から得ます。インシデントの記録がまばらでも機能します。
- **発火がインシデントを検知した**とみなすのは、インシデントの少し前か最中に発火が始まり、指定したラベルが一致したときです。それより前から発火していたものは何も検知していません。どのみち鳴っていたからです。
- **記録がなければ判定しません**。何も検知しなかった発火を判定するのは、その前後に何かが記録されているときだけです。つまり、発火前の
  2 週間以内のインシデントか、その時間帯に読み取った影響指標です。それ以外の場合は判定せずに件数だけを数え、レポートにもそう示します。

ノイズルール、何を変えるべきか、ニアミスなど、ほかの点は[判定のしくみ]({{ '/docs/how-it-judges/' | relative_url }})にあります。

## アラートのある場所で動きます {#works-where-your-alerts-are}

Prometheus 互換の API ならどれでも使えます。Prometheus、Thanos、Mimir、VictoriaMetrics、AWS・Azure・Google Cloud
のマネージド Prometheus、または CloudWatch の PromQL API に、マシンや Pod がすでに持っている認証情報で接続します。ログに対するルールは Loki
に対してバックテストします（[ログに対するルール]({{ '/docs/loki/' | relative_url }})）。Oracle Cloud にはマネージド Prometheus
がないため、OKE ではそこで自分で運用している Prometheus を使ってください。クラウド独自の形式のアラーム（CloudWatch、Azure Monitor、Cloud Monitoring、OCI
Monitoring）は、クラウドごとの小さなスクリプトを使って履歴から判定します。`lapilli serve`
は、その履歴をクラウドの保存期間を超えて保持します。連携は汎用的で、特定のトラッカーではなく Webhook を使います。

## 信頼して使えるように {#built-to-be-trusted}

- **人が承認したものだけを変更します**。初期状態では読み取るだけです。有効にすると、自ら提案したルール変更を実施します。いずれも UI
  で人が承認した後に、ルールが置かれている場所へのプルリクエストとして、あるいは Grafana・ruler・クラウドの API
  を通じて、置き換える内容を元に戻せるよう保存したうえで適用します。アラートをサイレンスすることも、発火中のアラートをオフにすることもありません。エージェントにできるのは提案だけです。
- **シークレットは外に出しません**。トークンや Webhook の URL はファイルから読み込み、画面に表示することはありません。指示されていない場所には何も送りません。
- **検証済みのリリース**。リリースは公開前に govulncheck で検査し、最新のリリースは毎日あらためて検査します。リリースは公開後に変更できず（v0.14.0
  から）、ビルドの来歴（provenance）が付いています。`gh attestation
  verify` は各バイナリ、チャート、イメージで使えます。v0.28.0 以降の各リリースに残している Sigstore
  バンドルを使えば、バイナリとチャートはオフラインでも検証できます（[リリースの検証](https://github.com/lapilli-project/lapilli/blob/main/SECURITY.md#checking-a-release)）。
- **すべて公開の場で**。脆弱性の報告は GitHub の非公開セキュリティアドバイザリで受け付け、コードは CodeQL と OpenSSF Scorecard
  でスキャンしています。
- **オンコールの誰もが使えるように**。`lapilli serve` のページは WCAG 2.2 AA
  を目標にしています。スクリーンリーダー、キーボードのみの操作、200%
  ズーム、スマートフォンでの利用を想定し、CI
  ではすべての言語でページの構造とコントラストを検証しています。適合を主張するものではなく、あくまで目標です（[アクセシビリティ](https://github.com/lapilli-project/lapilli/blob/main/ACCESSIBILITY.md)）。

## 開発に参加する {#help-build-it}

Lapilli のメンテナーは現在 1
人で、複数の組織からメンテナーを増やしたいと考えています。残っている問いは、どれも取り組みがいのあるものです。バックテストは実際の発火をどこまで忠実に再現できるのか。まばらなインシデント記録から公平な判定を下すには何が必要か。いくつかの領域では、今のメンテナーが持っていないものが必要です。マネージド
Prometheus のある AWS か Azure のアカウント、実運用されている Loki のアラート、インシデント管理ツールです。

- [ロードマップ](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md)：まだできないことを領域ごとに、それぞれがコードのどこにあるかとあわせて
- [初めての Issue](https://github.com/lapilli-project/lapilli/labels/good%20first%20issue)：変更する場所と完了の条件が書かれた小さな変更、そしてアカウントやツールが必要なものを集めた
  [help wanted](https://github.com/lapilli-project/lapilli/labels/help%20wanted)
- [メンテナー](https://github.com/lapilli-project/lapilli/blob/main/MAINTAINERS.md)：ある領域への貢献が、その領域のメンテナンスにどうつながるか
- [ガバナンス](https://github.com/lapilli-project/lapilli/blob/main/GOVERNANCE.md)：原則と、意思決定の進め方
- [貢献ガイド](https://github.com/lapilli-project/lapilli/blob/main/CONTRIBUTING.md)：ビルド、テスト、変更の送り方
- [Discussions](https://github.com/lapilli-project/lapilli/discussions)：質問、アイデア、そして Lapilli があなたのアラートで見つけたこと

いちばん役に立つ報告は、実際の発火とは異なるバックテスト結果になるルールです。

## インストール {#install}

<div class="ways">
<div markdown="1">
### Go

```sh
go install lapilli.dev/cmd/lapilli@latest
lapilli demo
```
</div>
<div markdown="1">
### バイナリ

Linux と macOS の amd64 または arm64
向けに、[リリース]({{ site.repo }}/releases{% if site.version %}/tag/{{ site.version }}{% endif %})から入手できます：

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
{% if site.version %}curl -fsSL {{ site.repo }}/releases/download/{{ site.version }}/lapilli-{{ site.version }}-$os-$arch.tar.gz | tar -xz
./lapilli-{{ site.version }}-$os-$arch/lapilli demo{% else %}gh release download -R lapilli-project/lapilli -p "lapilli-*-$os-$arch.tar.gz"{% endif %}
```
</div>
<div markdown="1">
### コンテナイメージ

```sh
docker run --rm ghcr.io/lapilli-project/lapilli{% if site.version %}:{{ site.version }}{% endif %} demo
```
</div>
<div markdown="1">
### Helm チャート

週に 1 回 CronJob として、または `mode=serve` で毎時実行します：

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
```
</div>
</div>

各リリースは公開前に検査され、公開後は変更できません。ダウンロードしたものは[リリースの検証]({{ site.repo }}/blob/main/SECURITY.md#checking-a-release)の手順で確かめられます。ここから自分のアラートで
1 つ目のレポートを出すまでは、[はじめに]({{ '/docs/getting-started/' | relative_url }})で説明しています。

## 名前の由来 {#the-name}

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> 白と黒の小石で裁くのが、古くからの習わしであった。黒い小石は被告を有罪とし、白い小石は無罪とする。
>
> — オウィディウス『変身物語』XV.41–42

*Lapilli* とは小さな石のことです。発火は一つひとつが
1 票です。週に一度、壺を空けて石を数えます。
