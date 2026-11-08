# fhir-client-agent

院内に常駐し、院内システムの通知をカルテ([fhir-client](../fhir-client))へ中継するエージェント。
Go 製の単一バイナリで、院内機に Go も Ruby も Docker も要りません。

> `fhir-mcp-agent` とは別物です。あちらは Claude から呼ばれる MCP サーバー(利用者の端末で動く短命プロセス)、
> こちらは院内サーバーで動き続ける常駐プロセスです。

```
┌─ 院内 ─────────────────────────────┐
│  院内システム(日レセ など)          │
│        ▲ 連携先ごとのプロトコル      │
│        ▼                            │
│  fhir-client-agent                  │
└────────┼────────────────────────────┘
         │ HTTPS + Bearer(外向きのみ。待ち受けポートなし)
         ▼
   fhir-client backend  POST /integrations/receipt/events
```

カルテは連携先の製品もイベント形式も知りません。連携先の言葉を**中立なイベント**に直すのが
このエージェントの仕事で、連携先を足すときはアダプタを 1 つ書きます(カルテ側は無変更)。

## 対応している連携

| `AGENT_ADAPTER` | 連携先 | 方式 |
|---|---|---|
| `orca-push`(既定) | 日レセ(WebORCA)の PUSH 通知 | WebSocket を購読 |
| `ssmix2-storage` | SS-MIX2 標準化ストレージ(院内の共有フォルダ) | カルテからメッセージを取りに行き、ファイルを書く |

### orca-push が中継するもの

| 日レセのイベント | 中立イベント |
|---|---|
| `patient_infomation` (add / modify) | `patient.changed` |
| `patient_infomation` (delete) | `patient.deleted` |
| `patient_accept` (add / modify / delete) | `reception.created` / `.updated` / `.canceled` |

**患者情報のイベント名は `patient_infomation` で、`r` がありません**(日レセ側の綴り。
実機 20260825-1 で確認)。綴りが直った版でも拾えるように `patient_information` も受けます。

日レセの通知は「何が変わったか」を運びません(患者番号と日時だけ)。
**保険・公費の変更にも専用のイベントは無く、`patient_infomation` の modify として届きます。**
中身はカルテが日レセの API で取り直すので、エージェントは種別と鍵だけを渡します。

```json
{ "event_id": "fddf4bb0-…", "type": "reception.created",
  "occurred_at": "2026-09-20T10:16:00+09:00", "patient_number": "00002",
  "reception": { "key": "2026-09-20:00003", "date": "2026-09-20", "time": "10:16:00",
                 "department_code": "01", "physician_code": "10001", "coverage_set_key": "0001" } }
```

### ssmix2-storage が書くもの

向きが逆のアダプタで、カルテ(fhir-client)が FHIR への書き込みから起こした HL7 v2.5 のメッセージを
`GET /integrations/ssmix2/messages` で取りに行き、カルテの施設設定「SS-MIX2 標準化ストレージ」で決めた
出力先(`root_path`)の下に、標準化ストレージの規則で置きます(設計は fhir-client の `docs/ssmix2-export-design.md`)。

```
<root_path>/患者ID先頭3/患者ID次の3/患者ID/診療日/データ種別/
  患者ID_診療日_データ種別_オーダNo_トランザクション日時_診療科_コンディションフラグ
```

- 新規・修正(`INS`)は同じオーダの有効ファイル(`_1`)を過去履歴(`_2`)に改名してから `_1` で書く。
  取消(`DEL`)は `_1` と `_2` を無効(`_0`)に改名し、取消の電文を `_0` で書く。
- 書けた id を `POST …/messages/ack` で返す。返せなくてもカルテが配り直し、同じファイル名が既にあれば
  何もしないので二重には書かない。
- 出力先はカルテが決める。`SSMIX2_ALLOWED_ROOT` を置くと、その下以外の出力先は拒んで ack せずに残す。
- `SSMIX2_TRANSACTION_DIR` を置くと、置いたメッセージをトランザクションストレージ(構築ガイドライン 3.3)として
  `<dir>/<年>/TR_<書き始めた日時>_<処理系統番号>.DAT` にも追記する(SS-MIX ヘッダー + HL7、1 件ごとに
  `<EOH>` `<EOM>` 付き。日付が変わるか 100MB に達したら次のファイル)。標準化ストレージの再構築や複製の元にする。
- 電文の中身(文字コード CP50221、セグメント)はカルテが組む。このアダプタは触らない。
- カルテの施設設定に拡張ストレージの出力先(`extended_root_path`)があると、確定した診療記録の HL7 CDA R2 も同じポーリングで
  受け取り、`<extended_root_path>/患者ID先頭3/次の3/患者ID/診療日/データ種別フォルダ/コンテンツフォルダ/` に `HL7CDA.xml` と
  `_contents.xml` を置く(拡張ストレージ構築ガイドライン 2.2)。修正は前のコンテンツフォルダを `_2`、削除は `_1` / `_2` を `_0` に改名する
  (削除では新しいフォルダを作らない)。拡張ストレージのルートも `SSMIX2_ALLOWED_ROOT` の下でなければ、その分だけ残す。
- 日レセ連携と同じ院内サーバーで動かすときは、`AGENT_ADAPTER` を変えた **2 つ目のプロセス**として起動する
  (1 プロセス 1 連携)。

## 動かす場所

**院内サーバー 1 台で 1 プロセスだけ。** クライアント PC には入れません。

テナント全体の通知を購読するので、複数台で動かすと同じ通知が台数ぶん転送されます
(取り込みは冪等なので壊れませんが、連携先の API とカルテのレート制限を無駄に使います)。
連携先と同じ網に置き、カルテへは外向き HTTPS で出られれば足ります。

## 設定

環境変数、または**バイナリと同じ場所の `.env`**(環境変数が勝ちます)。雛形は `.env.example`。

| 変数 | 既定 | 内容 |
|---|---|---|
| `AGENT_ADAPTER` | `orca-push` | 動かす連携。1 プロセス 1 連携 |
| `KARTE_EVENTS_URL` | (orca-push で必須) | `https://<カルテ>/integrations/receipt/events` |
| `KARTE_INBOUND_TOKEN` | (必須) | カルテの 管理 > 外部システム連携 >「受信トークンを再発行」で発行 |
| `SPOOL_DIR` | バイナリの隣の `spool/` | 送れなかったイベントの退避先 |
| `HEALTH_ADDR` | (待ち受けない) | 例 `127.0.0.1:8787`。`/healthz` を出す |
| `LOG_LEVEL` | `INFO` | `DEBUG` にすると捨てたイベントも出る |
| `KARTE_SSMIX2_URL` | (ssmix2-storage で必須) | `https://<カルテ>/integrations/ssmix2/messages` |
| `SSMIX2_POLL_INTERVAL` | `10s` | 取りに行く間隔 |
| `SSMIX2_ALLOWED_ROOT` | (制限しない) | カルテが指定した出力先を、この下に限る |
| `SSMIX2_TRANSACTION_DIR` | (持たない) | トランザクションストレージの置き場(絶対パス) |
| `SSMIX2_TRANSACTION_LINE_NO` | `0001` | トランザクションデータファイル名の処理系統番号 |
| `ORCA_WS_URL` | `ws://localhost:8000/ws` | 日レセの PUSH 接続先 |
| `ORCA_TENANT_ID` | `1` | `X-GINBEE-TENANT-ID`。オンプレは 1 固定 |
| `ORCA_TLS_CERT` / `_KEY` / `_CA` | — | クラウド版の SSL クライアント認証 |

| 日レセ | 接続先 | 認証 |
|---|---|---|
| オンプレ Ubuntu 22.04 以降 | `ws://<日レセ>:8000/ws` | 無し |
| オンプレ Ubuntu 20.04 | `ws://<日レセ>:9400/ws` | 無し。`jma-receipt-pusher` の導入が別途要る |
| クラウド | `wss://weborca.cloud.orcamo.jp/ws` | SSL クライアント証明書 |

オンプレの PUSH は認証がありません。**ポートに届く誰もが全患者の通知を受け取れる**ので、
院内網に閉じていることを運用側で担保してください。

## 起動

`make dist` で `dist/` に配布物一式(Linux amd64/arm64・Windows・macOS のバイナリ、`.env.example`、systemd unit)ができます。

### systemd(Linux・推奨)

手順は `deploy/fhir-client-agent.service` の先頭にあります。バイナリ 1 つと設定 1 つを置くだけです。

```
sudo systemctl enable --now fhir-client-agent
journalctl -u fhir-client-agent -f
```

### Windows

`fhir-client-agent-windows-amd64.exe` と `.env` を同じフォルダに置いて実行します。
常駐させるにはタスクスケジューラ(起動時・失敗時再起動)か NSSM などでサービス化してください。

### Docker

```
cd deploy && cp ../.env.example .env && vi .env
docker compose up -d && docker compose logs -f
```

イメージは distroless・nonroot で 10MB 弱です。

### 手元で試す

```
KARTE_EVENTS_URL=http://localhost:3001/integrations/receipt/events \
KARTE_INBOUND_TOKEN=<トークン> go run .

# SS-MIX2 への書き出し(出力先はカルテの施設設定で決める)
AGENT_ADAPTER=ssmix2-storage KARTE_SSMIX2_URL=http://localhost:3001/integrations/ssmix2/messages \
KARTE_INBOUND_TOKEN=<トークン> go run .
```

## 動いているかの確認

ログは 1 行 1 JSON で stdout に出ます。`subscribed` と `connected` が出れば接続完了です。

| ログ | 意味 |
|---|---|
| `connected` | 連携先に繋がり、購読できた |
| `forwarding` → `delivered` | 通知を受け、カルテに届いた |
| `retrying` → `undeliverable; spooled` | カルテに届かず退避した。復旧後に自動で送り直す(`resent from spool`) |
| `rejected; dropped` | カルテが 400 / 422 を返した。形が悪いので送り直さない |
| `disconnected; reconnecting` | 連携先と切れた。1 秒から最大 30 秒まで間隔を広げて繋ぎ直す |

`HEALTH_ADDR` を設定すると `/healthz` が稼働状況を返します(未接続なら 503)。

```json
{"adapter":"orca-push","connected":true,"connected_at":"…","last_event_at":"…","events":12,"spooled":0}
```

### 届かないとき

| 症状 | 原因 |
|---|---|
| `disconnected` が続く(`dial` エラー) | 連携先に届いていない。`ORCA_WS_URL` とポート |
| `closed before subscribed` / `subscribe rejected` | 購読を断られた。`ORCA_TENANT_ID` |
| `HTTP 401` で spool が溜まる | `KARTE_INBOUND_TOKEN` が古い。再発行して入れ直せば、溜まったぶんは自動で届く |
| `HTTP 403` + HTML | カルテ(Rails)のホスト許可に弾かれている。`KARTE_EVENTS_URL` のホスト名を `config.hosts` に足す(開発時のみ) |

## 取りこぼしについて

エージェントが**受け取った**通知は落としません。送れなければディスクに退避し、順番どおりに送り直します
(SIGTERM で止めたときも、未送信ぶんを退避してから終わります)。

一方、**エージェントが止まっている間に連携先で起きた変更は届きません**。日レセは未接続中の通知を
再配達しない(受信確認も未配達保存も無い)ためです。こちらはカルテ側で回復します。

- カルテ > 患者情報 > 保険・公費 の「レセコンから再取込」(患者 1 人)
- `bin/rails integrations:receipt:resync`(取り込み済みの患者を洗い直す。日次などで定期実行するのが実用的)

## 構成

```
main.go                       設定 → アダプタ → 土台を組み立てて起動。SIGTERM を受ける
internal/
  config/                     環境変数 + バイナリ隣の .env。検証は起動時に全部やる
  event/                      中立イベント(カルテとの契約)
  agent/                      常駐の土台: 再接続(バックオフ)、稼働状況、/healthz
  deliver/                    送達: リトライ → 退避 → 順序を保って再送
  adapter/orcapush/           日レセ PUSH 通知。日レセの語彙はここで完結
  adapter/ssmix2/             SS-MIX2 標準化ストレージへの書き出し。フォルダ・ファイル名・フラグの規則はここ
deploy/                       systemd unit、docker-compose
```

### 連携先を足す

`agent.Adapter` を実装し、`main.go` の `buildAdapter` と `config` に 1 行ずつ足します。
再接続・送達・ログ・シャットダウンは土台が持つので、アダプタは「繋いで、直して、`emit` する」だけです。

```go
type Adapter interface {
    Name() string
    // 接続して、切れるまで戻らない。使える状態になったら ready を 1 回呼ぶ。
    Run(ctx context.Context, emit func(event.Event), ready func()) error
}
```

## 開発

```
make test     # go test -race ./...
make lint     # go vet + gofmt
make dist     # 配布物一式
```

依存は WebSocket クライアント(`github.com/coder/websocket`)だけで、あとは標準ライブラリです。
