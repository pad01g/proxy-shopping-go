# lab（docker compose の e2e 環境）

`proxy-shopping-go/compose.yaml` で閉じた網を立てる。外部（インターネット）には接続しない。
`proxy-shopping-web` は `../proxy-shopping-web` に置く（`WEB_DIR` で変更可）。

## ネットワーク

| 名前 | サブネット | 役 |
|---|---|---|
| `pub` | 172.40.0.0/24 | 公開側（インターネットの代わり） |
| `home` | 172.41.0.0/24 | NAT の内側。`router`（172.40.0.254 / 172.41.0.254）が MASQUERADE する |

`home` の中のコンテナは `extra_hosts` で `*.test` を `edge`（172.40.0.2）へ向ける。

## ホスト名（すべて `edge` = Caddy。`tls internal` の自己署名 CA）

| ホスト | 転送先 | 内容 |
|---|---|---|
| `app.test` | `web:80` | proxy-shopping-web（静的）。`/config.json` と `/deployments/31337.json` も配る |
| `relay-1.test`, `relay-2.test` | `relay-1:7777`, `relay-2:7777` | Nostr リレー（psrelay） |
| `esplora.test` | `esplora:3000` | Esplora 互換 API（esplora-lite） |
| `evm.test` | `anvil:8545` | EVM RPC |
| `rates.test` | `fakeshop:8080` | レートの模擬（`/frankfurter/…`, `/coingecko/…`） |
| `faucet.test` | `faucet:8080` | lab 専用の蛇口 |
| `safe-shop.test` | `fakeshop:8080` | 安全と判定される店（JPY, cardgw） |
| `us-shop.test` | `fakeshop:8080` | 安全と判定される店（USD, cardgw） |
| `cash-store.test` | `fakeshop:8080` | 現金のみの実店舗（`JP-13-13104` 新宿区）。店頭端末 `/pos` |
| `cardgw.test` | `fakeshop:8080` | 仮想のカード決済ゲートウェイ |
| `risky-shop.test` | `fakeshop:8080` | **HTTP のみ**。未知の決済画面。危険と判定される店（USD） |

Caddy のルート CA は volume `caddy_data` の `caddy/pki/authorities/local/root.crt`。各コンテナは `caddy_data` を `/caddy-data` に読み取り専用で付けるので、パスは `/caddy-data/caddy/pki/authorities/local/root.crt`。
Go は設定 `tls.extra_ca`、Node は `NODE_EXTRA_CA_CERTS`、Playwright は `ignoreHTTPSErrors` を使う。

## サービス

| サービス | IP | 内容 |
|---|---|---|
| `edge` | 172.40.0.2 | Caddy |
| `relay-1`, `relay-2` | .10, .11 | psrelay（khatru）。下の「psrelay の制限」。`edge` と `demo` の `X-Forwarded-For` を信じる |
| `p2p-relay` | .12 | psnode `-role relay`（circuit relay v2, gossipsub の中継） |
| `bitcoind` | .20 | Bitcoin Core 29, `-signet -signetchallenge=51`（OP_TRUE の独自 signet） |
| `esplora` | .21 | esplora-lite（ブロックごとに親のハッシュを確かめ、合わなければ分岐点まで巻き戻す） |
| `anvil` | .22 | anvil（chain id 31337） |
| `deployer` | ― | 一度だけ動く。コントラクトを置き `/deployments/31337.json` を書く |
| `faucet` | .23 | labfaucet（BTC/ETH/USDC の配布、採掘、時刻送り） |
| `fakeshop` | .26 | 架空の店・cardgw・レート模擬（Host ヘッダで振り分け） |
| `ratefeed` | .25 | レートを模擬オラクルへ書く。送信は anvil の account 1（`evm.key`。account 0 は deployer と faucet が使うので、共有すると nonce が衝突する） |
| `shopper-1` / `bot-1` | .30 / .31 | shopper（`cash_regions: [JP-27]` = 大阪府。現金の店は地域外） |
| `shopper-2` / `bot-2` | .32 / .33 | shopper（`cash_regions: [JP-13]` = 東京都） |
| `escrow-1` | .40 | 誠実な escrow |
| `escrow-2` | .41 | シナリオ c で不正な裁定をする escrow（bond を預けている） |
| `operator-1` | .50 | operator ノード（通報の受信箱） |
| `operator-2` | .51 | operator ノード（シナリオ d で委任を失効させられる） |
| `web` | .60 | nginx（proxy-shopping-web の dist） |
| `demo` | .61（別名 `demo`） | デモ画面（proxy-shopping-web/apps/demo）と、その中継。ホストの `127.0.0.1:8888` に公開（下の「デモ画面」） |
| `natbox` | 172.41.0.10 | NAT の内側の名前空間 |
| `escrow-nat` | natbox を共有 | NAT の内側の escrow（libp2p は relay 経由でのみ届く） |
| `runner` | 172.41.0.20（`home`） | e2e の実行（profile `run`）。NAT の内側から動かし、Playwright（NAT の内側のブラウザ利用者）も中で動く。docker の socket は持たない（シナリオ h は管理 API の `POST /admin/pause` を使う） |

`bot-1` / `bot-2` は購入結果を `request_id` ごとに volume `bot1_data` / `bot2_data`（`/data`, `BOT_DATA_DIR`）に保存する（spec §9 の冪等）。

最初からやり直すときは volume ごと消す: `docker compose down -v`（チェーン・リレー・各ノードの bbolt・bot の結果・Caddy の CA がすべて消える）。

### ヘルスチェック

`docker compose up` は次が healthy になるまで依存するサービスを待つ（`depends_on: condition: service_healthy`）。`runner` は全部を待つ。

| サービス | 検査 |
|---|---|
| `edge` | ルート CA があり、`127.0.0.1:443` で listen している（`nc -z`） |
| `relay-1`, `relay-2` | `GET http://127.0.0.1:7777/healthz` |
| psnode（`p2p-relay`, shopper, escrow, operator） | `GET http://127.0.0.1:8080/healthz`（認証なし。起動が終わると 200） |
| `esplora` | `GET /blocks/tip/height` |
| `faucet` | `GET /height` |
| `web` | `GET /`（nginx） |

## psrelay のフラグと制限

フラグ（括弧は環境変数と既定値）。制限はすべて 1 分あたりで、`-1` で無効。

| フラグ | 内容 |
|---|---|
| `-listen`（`PSRELAY_LISTEN`, `0.0.0.0:7777`） | listen するアドレス |
| `-data`（`PSRELAY_DATA`, `data`） | イベントを置くディレクトリ（badger） |
| `-kinds`（`PSRELAY_KINDS`, `5,1059,10050,30500-30503`） | 受け取る kind（一覧と範囲）。kind 0 は受け取らない。kind 5 は自分のイベントだけを消せる |
| `-retention-days`（`PSRELAY_RETENTION_DAYS`, 30） | gift wrap（kind 1059）を保つ日数。1 時間ごとに古いものを消す |
| `-max-event-size`（`PSRELAY_MAX_EVENT_SIZE`, 262144） | イベント（直列化したもの）の最大 byte |
| `-name`（`PSRELAY_NAME`, `psrelay`） | NIP-11 の名前 |
| `-events-per-minute`（`PSRELAY_EVENTS_PER_MINUTE`, 3000） | 接続元アドレスごとの EVENT |
| `-conn-events-per-minute`（`PSRELAY_CONN_EVENTS_PER_MINUTE`, 300） | 接続ごとの EVENT |
| `-conn-messages-per-minute`（`PSRELAY_CONN_MESSAGES_PER_MINUTE`, 1200） | 接続ごとのフレーム（EVENT・REQ・CLOSE など全部）。解析と署名の検証の前に数える。超えた分は NOTICE を返して捨てる |
| `-reqs-per-minute`（`PSRELAY_REQS_PER_MINUTE`, 3000） | 接続元アドレスごとの REQ の filter |
| `-conns-per-minute`（`PSRELAY_CONNS_PER_MINUTE`, 600） | 接続元アドレスごとの新しい接続 |
| `-max-subscriptions`（`PSRELAY_MAX_SUBSCRIPTIONS`, 32） | 接続ごとの開いている REQ |
| `-max-limit`（`PSRELAY_MAX_LIMIT`, 1000） | filter の `limit` の上限 |
| `-trusted-proxies`（`PSRELAY_TRUSTED_PROXIES`, なし） | `X-Forwarded-For` を信じる逆プロキシ（IP / CIDR, カンマ区切り）。lab は `edge`（172.40.0.2） |

- 「接続元アドレス」は IPv4 のアドレス、IPv6 は /64 の prefix（1 契約者が /64 を丸ごと持つのが普通なので、アドレスごとでは制限にならない）。
- アドレスごとの既定値は大きめにしてある（NAT の内側の多数の利用者が 1 つのアドレスを共有する）。主な歯止めは接続ごとの制限。
- 逆プロキシの後ろに置くときは `-trusted-proxies` に必ずそのアドレスを書く。書かないと全員がプロキシのアドレスの制限を共有する。信頼していない相手から `X-Forwarded-For` が届くと、起動後に一度だけ ERROR のログを出す（`X-Forwarded-For from a peer that is not a trusted proxy`）。
- `GET /healthz` は 200 `ok`（ヘルスチェック用）。
- 置き換え可能な kind（10050, 30500–30503）は最新の 1 件だけを保つ。新旧は `v` タグで決める（spec §2.1。同じ `v` なら `id` の小さいもの）。`v` の無い 10050 は `created_at` で決める。
- COUNT（NIP-45）と NIP-50 の検索には応じない。

## デモ画面（http://localhost:8888/）

lab を動かしたまま、1 つの画面で全員の役割を演じて流れを追うためのページ。`docker compose up -d --build` で `demo` も立ち上がる。
ブラウザで <http://localhost:8888/> を開く（localhost なので Web Crypto / Web Locks が使える安全な文脈になる）。

- 役割ごとに鍵を持つ: 利用者・escrow・operator は初回に BIP39 の鍵を作ってブラウザの localStorage に置き（平文。lab 専用）、coordinator は lab の固定の
  デモ用の鍵（`lab/keys/coordinator-demo.mnemonic`, 公開鍵 `07da142f…9084`）を使う。lab のノードはすべてこの公開鍵を `trust.coordinators` に持つ。
  各役割は別々の Session（別の IndexedDB `ps-demo-<役割>`、別のリレー接続）で同時に動く。shopper は常時オンラインの Go ノード `shopper-1`。
- 画面: 左がガイド（選んだシナリオの手順。役割のバッジ、説明、✓ / いま / まだ、「この操作へ」でタブを開いて押すボタンを光らせる）。右が役割のタブ
  （`利用者` `shopper（ノード）` `escrow` `operator` `coordinator` `lab 操作`）。フォームはシナリオに合わせて入力済みなので、ガイドのボタンと確認ダイアログを押すだけで進む。
- 準備（全シナリオ共通。済んでいれば ✓）: coordinator が operator に委任 → operator が一覧（shopper-1 × デモの escrow、JP-13 と US）を公開 →
  escrow がプロフィールを公開 → shopper-1 ノードが一覧とプロフィールを受け取る → 利用者が蛇口から BTC・USDC・ETH を受け取る。
- シナリオ:

| id | 内容 |
|---|---|
| `normal-btc` | 正常系（BTC）: safe-shop A-100 → 見積 → 承諾 → 2-of-3 の P2WSH に入金（+ escrow の前払い手数料）→ shopper が bot で購入・配達 → 利用者が支払いに署名 → shopper が連署して放送 → チェーンで完了 |
| `normal-usdc` | 正常系（USDC）: us-shop U-100 を注文ごとの Safe で。shopper-1 にガス代が無ければ lab 操作で ETH を送る手順が入る |
| `dispute-refund` | FAIL-100 → 配送失敗 → 利用者が紛争 → escrow が証拠を確かめ、届け先を復号し、全額を利用者へ裁定 → 利用者が連署 → 精算 |
| `sold-out` | SOLDOUT-100 → shopper が購入に失敗して払い戻しを申し出る → 利用者が確かめて連署 → 返金 |
| `risky` | risky-shop R-100 → shopper が `risk` で断る |
| `fraud` | FAIL-100 → 紛争 → escrow が全額を shopper へ（不正な裁定のデモ）→ shopper が自動で連署 → 利用者が通報 → operator が escrow を一覧から外す → 候補から消える |
| `timelock-t2` | 見積の承諾後に lab 操作で shopper-1 を一時停止 → 入金 → T2 まで採掘 → 利用者が一人で取り戻す → shopper-1 を再開 |

- 表示言語: ヘッダー右の「日本語 / English」（`lang-ja` / `lang-en`）で切り替える。画面の文言・ガイドの手順と説明・シナリオ名・確認ダイアログ・
  入力済みの値（届け先の例 `Taro Yamada / 1-1-1 Shinjuku, Shinjuku-ku, Tokyo`、紛争の説明など）がその言語になる。選んだ言語は localStorage
  （`ps-demo-lang`。「デモを初期化」でも消えない）に残り、URL の `?lang=en` / `?lang=ja` はそれより優先する（保存はしない）。既定は日本語で、`<html lang>` も合わせる。
  core の経過（timeline）は種類（kind）から選んだ言語で表示する。相手が書いた文（紛争の説明・通報の本文・プロフィールの名前・委任書のメモ）や
  ノード・core のエラーはデータなのでそのまま出す（`data-i18n-exempt` を付けている）。e2e の `normal-btc-en` は `?lang=en` で normal-btc を最後までたどり、
  各手順・確認ダイアログ・最後に全タブで日本語の文字が無いことを確かめる。

- 別々のウィンドウ: `?role=user`、`?role=escrow,operator,coordinator` のように役割を選ぶと、そのウィンドウではその役割だけが動く（同じブラウザなら鍵と
  ガイドの進み具合は localStorage で共有される）。ガイドはどのウィンドウにも出て、ほかのウィンドウの役割の手順は「別のウィンドウで … が操作」と表示する。
  同じ役割は 1 つのウィンドウでしか動かない（Web Locks）。`shopper（ノード）` と `lab 操作` のタブはどのウィンドウにもある。
- 「デモを初期化」: このブラウザのデモの鍵と記録（localStorage と IndexedDB）をすべて消す。チェーンやノードの記録は消えない。
  coordinator の鍵は固定なので、前の operator への委任書は残る（coordinator タブで失効させられる）。
- 「最初から」: 同じシナリオを新しい注文でやり直す（準備の手順は済んでいれば ✓ のまま）。
- testid は `proxy-shopping-web/apps/demo/TESTIDS.md`。

### デモのサーバー（`lab/demo/nginx.conf`, `lab/demo/demo-config.json`）

ブラウザはホストにいて `*.test` の名前を引けないので、ページが使うものをすべて `demo` の 1 つの origin にまとめる。

| パス | 転送先 |
|---|---|
| `/` | デモ画面（静的ファイル） |
| `/demo-config.json` | `lab/demo/demo-config.json`（マウント） |
| `/deployments/` | volume `deployments` |
| `/relay-1`, `/relay-2` | `relay-1:7777`, `relay-2:7777`（websocket） |
| `/esplora/` | `esplora:3000` |
| `/evm` | `anvil:8545` |
| `/faucet/` | `faucet:8080` |
| `/rates/` | `fakeshop:8080`（`Host: rates.test`、`/rates/admin/rates` には `X-Admin-Token: lab` を付ける） |
| `/node/shopper-1/…`, `/node/shopper-2/…` | 各 shopper の管理 API。`Authorization: Bearer lab` を付け、`Host` をサービス名にし、`Origin` を外し、`Content-Type: application/json` にする（管理 API の CSRF 対策の検査を通すため） |

プロトコルに現れるリレーの URL は論理名（`wss://relay-1.test`）のまま（受信箱の kind 10050、依頼の `relays`、一覧の `relays`）なので、docker の中の
Go ノードも同じリレーに届く。ブラウザの実際の接続だけを `ws://<ページのホスト>/relay-1` に向ける（core の `MappedTransport`。対応の無いリレーには接続しない）。
Esplora・EVM RPC・faucet もページと同じ origin のパスを使う。

**注意（lab 専用）:** このサーバーは shopper ノードの管理 API を token 付きで中継し、faucet（採掘・時刻送り・残高の作成）とレートの管理 API も開いている。
このポートに届く人は誰でも shopper を止めたりチェーンを進めたりできる。compose は `127.0.0.1:8888` にだけ公開する。ほかのアドレスに公開しないこと。

### デモの e2e

```sh
docker compose run --rm runner demo                    # 7 シナリオ + normal-btc-en + 別々のウィンドウ（separate）
docker compose run --rm runner demo dispute-refund     # 選ぶ（id は上の表と normal-btc-en・separate）
```

runner（NAT の内側）の Chromium が `http://demo` を開き、最初に「デモを初期化」してから、シナリオごとにページを開き直してガイドの指示どおりに押す
（「この操作へ」→ ボタン →（確認ダイアログなら）OK、待ちの手順は進むまで待つ）。最後に画面の表示（txid・状態・配分・候補の有無）を確かめる。
`normal-btc-en` は `?lang=en` のページで normal-btc をたどり、手順が変わるたびにヘッダー・ガイド・タブ・開いているパネル、確認ダイアログ、
最後に 6 つのタブすべてに日本語（ひらがな・カタカナ・漢字・和文の記号）が無いことを確かめる（`data-i18n-exempt` の要素は除く）。
`separate` は `?role=user` と `?role=escrow,operator,coordinator` の 2 つのページで `dispute-refund` を進め、各手順をその役割のページで押す。
`http://demo` は安全な文脈ではないので、Chromium は `--unsafely-treat-insecure-origin-as-secure=http://demo` で起動する。
結果は `e2e/results/demo-latest.md`（と `demo-<時刻>.md`、各シナリオの画面 `demo-<id>.png`）。

## lab の鍵（テスト専用。公開網で使わないこと）

`sha256("ps-lab:" + 名前)` の先頭 16 byte を entropy にした BIP39 の 12 語。`lab/keys/<名前>.mnemonic` に置く。

| 名前 | ニーモニック |
|---|---|
| coordinator-1 | pizza champion puzzle wrestle curtain galaxy vendor pluck town mixture original gorilla |
| coordinator-2 | sphere nation isolate asthma phrase this discover detect judge start poverty measure |
| operator-1 | donkey burger catch disease lens parrot visa manage explain corn million toast |
| operator-2 | boss any wide kitchen hotel famous stable sugar tenant brief dish dentist |
| shopper-1 | december art feature luxury renew grape champion meadow wage weird aunt unaware |
| shopper-2 | suit total usual cloud problem alcohol habit conduct mystery alone cotton fetch |
| escrow-1 | blue salt fault plastic fault bargain word lady icon actual speed reflect |
| escrow-2 | fossil frown theory pyramid suit basket arrest crazy stand bunker couch rate |
| escrow-nat | broccoli crater library receive battle since poverty guard claim afraid foster defense |
| user-1 | news hybrid corn purchase public hedgehog clay survey able alter supreme shove |
| user-2 | sting unknown cabbage detect artist gate judge virus mutual forum return garlic |
| user-browser | injury raise enable film tissue approve code topple unlock busy candy embark |
| coordinator-demo | message occur banana believe spring shadow deer manage ginger mistake mad process（デモ画面の coordinator） |
| relay-p2p | dream entry clutch old reform gain tortoise slab kitchen mother rebuild lunch |
| faucet | defense girl explain south shine scissors view soup code talk fence town |

## lab の値

- レート: BTC/USD = 100000, USD/JPY = 150, USDC/USD = 1（したがって BTC/JPY = 15000000）。`POST https://rates.test/admin/rates` で変えられる。
- タイムロック（shopper の lab 設定）: BTC `t1 = 高さ + 100`, `t2 = 高さ + 150`。EVM `t1 = 今 + 3600 秒`, `t2 = 今 + 7200 秒`。
- 採掘: faucet が 1 秒ごとに mempool を見て、取引があれば 1 ブロック掘る。`POST /mine {"blocks": n}` で追加で掘る。
- 店の配送: 注文の 2 秒後に `shipped`、さらに 3 秒後に `delivered`。SKU が `FAIL-` で始まる商品は `shipped` の後 `failed` になる。

## 店の商品

| 店 | SKU | 名前 | 価格 |
|---|---|---|---|
| safe-shop.test | `A-100` | 抹茶ティーセット | 3200 JPY |
| safe-shop.test | `A-200` | 南部鉄器の急須 | 12000 JPY |
| safe-shop.test | `FAIL-100` | 配送に失敗する商品 | 2000 JPY |
| safe-shop.test | `SOLDOUT-100` | 在庫切れの商品（注文の確定で失敗する。支払いの前に止まる） | 2500 JPY |
| us-shop.test | `U-100` | Coffee beans 1kg | 25.00 USD |
| cash-store.test | `C-100` | 店頭限定の和菓子 | 1500 JPY |
| risky-shop.test | `R-100` | Too-cheap headphones | 9.99 USD |

送料: safe-shop 800 JPY、us-shop 10.00 USD、cash-store 1000 JPY、risky-shop 0。

## 蛇口 `faucet.test`

| 要求 | 内容 |
|---|---|
| `POST /btc {"address","sats"}` | 送って 1 ブロック掘る → `{"txid"}` |
| `POST /evm {"address","eth":"1","usdc":"1000"}` | ETH は `anvil_setBalance`、USDC は `mint` |
| `POST /mine {"blocks": n}` | n ブロック掘る → `{"height"}` |
| `POST /evm/time {"seconds": n}` | `evm_increaseTime` + `evm_mine` |
| `GET /height` | `{"btc": 高さ, "evm_time": 秒}` |

## psnode の設定（YAML）

```yaml
role: shopper            # shopper | escrow | operator | relay
name: shopper-1
network: ps-lab
mnemonic_file: /keys/shopper-1.mnemonic
data_dir: /data
admin: {listen: "0.0.0.0:8080", token: "lab", hosts: [shopper-1]}   # ループバック以外で listen するなら token は必須。hosts は下を参照
tls: {extra_ca: /caddy-data/caddy/pki/authorities/local/root.crt}
nostr:
  relays: ["wss://relay-1.test", "wss://relay-2.test"]
  k: 2
  allow_private_relays: true   # 既定 false: ループバック・リンクローカル・プライベートのアドレスのリレーに接続しない
p2p:
  listen: ["/ip4/0.0.0.0/tcp/4001", "/ip4/0.0.0.0/tcp/4002/ws"]
  bootstrap: ["/ip4/172.40.0.12/tcp/4001/p2p/<relay-p2p の peer id>"]
  relays: ["/ip4/172.40.0.12/tcp/4001/p2p/<同上>"]
  reachability: auto     # auto | public | private
  announce: []           # 任意: 追加で広告するアドレス（multiaddr）
trust:
  coordinators: ["<coordinator-1 pk>", "<coordinator-2 pk>"]
  bundle_urls: []        # 任意: trust bundle（署名済みイベントの JSON, 例 https://pad01g.github.io/proxy-shopping-registry/events.json）
chain:
  btc: {network: signet, esplora: "https://esplora.test"}
  evm: {chain_id: 31337, rpc: "https://evm.test", deployments: /deployments/31337.json}
fx:
  sources:
    - {type: coingecko, base: "https://rates.test/coingecko"}
    - {type: frankfurter, base: "https://rates.test/frankfurter"}
shopper:
  bot_url: "http://bot-1:7000"
  payments: [btc-signet, usdc-evm]
  currencies: [JPY, USD]
  cash_regions: [JP-27]
  fee: {bps: 500, min: {amount: "300", currency: JPY}}
  max_order: {amount: "200000", currency: JPY}
  delivery_days: 5                 # 85 まで（既定の T2 = delivery_days + 35 日が user の上限 120 日に収まるように）
  risk: {allowlist: [safe-shop.test, us-shop.test, cash-store.test], known_gateways: [cardgw.test], threshold: 70}
  timelock: {btc_t1_blocks: 100, btc_t2_blocks: 150, evm_t1_seconds: 3600, evm_t2_seconds: 7200}
  confirmations: 1                 # 入金の承認数（既定 1）
  payout_confirmations: 1          # 既定 3: 自分の BTC の支払いを確定とみなす承認数。それまで見張り、mempool から消えたら放送し直す。lab は 1（faucet は mempool に取引があるときだけ掘るので 3 には届かない）
  tracking_poll_seconds: 2         # 既定 60
  accept_rulings: always           # always（既定） | favorable
  quote_ttl_seconds: 900           # 既定 900: 見積の有効期間（expires_at）
  payout_fee_reserve_sats: 1000    # 既定 1000: BTC の payout の手数料の予備（lock_amount に含める）。上限 20000
  min_t1_remaining_seconds: 1800   # 既定 (delivery_days + 7) 日。T1 までこれ未満なら買わずに協力的な払い戻しを申し出る
  allow_private_shops: true        # 既定 false: shop_url がループバック・リンクローカル・プライベートのアドレスなら取りに行かない
escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
```

- `admin.token` が空で `admin.listen` がループバック（`127.0.0.1`, `[::1]`, `localhost`）以外なら、設定の読み込みで失敗する。ループバックで `admin.token` が空なら、最初の起動で乱数の token を作って `data_dir/admin.token`（0600）に保存し、その場所をログに出す。管理 API は `GET /healthz` を除いて常に token を求める。
- 管理 API はブラウザの他のページからの要求（CSRF, DNS rebinding）を断る: `Host` は `localhost`・IP アドレス・`admin.hosts` の名前だけ（それ以外は 403）、別の origin の `Origin` ヘッダは 403、POST は `Content-Type: application/json` だけ（それ以外は 415）。lab は compose のサービス名（`shopper-1` など）を `admin.hosts` に書く。
- `trust.coordinators` は `relay` 以外の役割で必須（空は何も信頼しないので、設定の読み込みで失敗する。spec §2.4）。要素は hex の公開鍵。`role: relay` は coordinators を見ず、正しい署名のイベントをすべて保存・中継する（保存は最大 20000 件、gossip は相手ごとに 1 分 600 件まで）。
- `trust.bundle_urls`（任意）: 登録簿の `events.json`（`{"events": [...]}` または配列）を起動時と 10 分ごとに取得し、リレーから届いたイベントと同じ検査（署名・版・網・受け入れる範囲）をして取り込み、新しいものは gossip する。bundle 自体は何も信頼を足さない。https のみ（`nostr.allow_private_relays` のときは http も可）。公開網 `ps-main` の例は `lab/examples/ps-main-shopper.yaml`。
- `nostr.allow_private_relays` は接続する瞬間のアドレスで判定する（名前の解決先が後で変わっても回り込めない）。lab のノードはすべて `true`。
- `shopper.allow_private_shops` も同じく接続ごと（リダイレクト先も含む）に判定する。店の証明書が検証できなければ読まない（検証を外して読み直すことはしない）。`http://` の店は TLS の点が付かないだけ（リダイレクトの途中に `http://` があっても同じ。別のホストへリダイレクトした店には許可リストの点も付かない）。NAT64（`64:ff9b::/96`, `64:ff9b:1::/48`）と 6to4（`2002::/16`）のアドレスにも接続しない。見積の拒否理由には接続の詳細を書かない（ログにだけ残す）。
- shopper の設定の検査: `timelock` の T1（BTC は 600 秒 = 1 ブロックで換算）は `min_t1_remaining_seconds` より先、`payout_fee_reserve_sats` は 20000 以下（spec §4.5）、`delivery_days` は 85 以下。見積では予備が `min(20000, max(2000, lock_amount の 5%))` sats を超える注文を `limit` で断る（user のクライアントが受け付ける上限。少額でも 2000 sats までは認める）。
- shopper の注文（spec §4.6, §4.8, §9）: `order.funded` は `order.accept` の後だけ受け付ける（先に届いたものは accept の後に処理し直す）。入金の出力・Safe・前払い手数料の tx は 1 つの注文にしか使えない（bbolt の `funding_uses`）。承認時刻が `expires_at` + 1 時間より後の入金、購入の開始時に T1 まで `min_t1_remaining_seconds` 未満の注文は買わずに払い戻しを申し出る。`expires_at` + 1 時間までに入金の無い `accepted` の注文は `cancelled` にする（その後に届いた入金は買わずに払い戻しを申し出る）。USDC は `fund_tx` の中の Safe への transfer だけで lock_amount 以上が要る（その tx の承認時刻で遅れを判定）。txid は BTC が 64 桁の小文字 hex、EVM が 0x + 64 桁の小文字 hex でなければ受け付けない。チェーンに問い合わせられないとき（通信・5xx・未索引）は `funding` のまま再試行し、食い違いがはっきりしたときだけ `accepted` に戻す。24 時間承認されない入金は買わず、10 分ごとに見続けて承認されたら払い戻しを申し出る。bot が `ok` でも `total` の無い結果は `needs_human` にする（`order.purchased` には必ず total を入れる）。bot への購入は `request_id` = 注文 id で、409 は待ち、4xx は `purchase_failed`（払い戻しの申し出）、それ以外は同じ `request_id` で問い合わせ直す。
- shopper の見積: 見積と状態（`quoted` / `rejected`）を保存してから送る。送った見積は messenger の outbox に残るので、送った直後に落ちても 2 つ目の見積は作らない。見積の記録より先に届いた `order.accept` は記録の後に処理し直す。断った依頼への返事は再送しない（`shopper.Engine.WantsRetry`）。
- shopper の保留中の処理（`pending`）: 見積の送信、払い戻しの申し出、release の連署、裁定の連署、T1 の受け取り。失敗しても注文に残り、3 秒から倍々に最大 10 分の間隔で再試行する（はっきり失敗したものと、7 日たっても終わらないものは `error` と履歴に残して捨てる。ただし tx を送った処理は、チェーンで決着するまで捨てない）。送った tx の完了待ちは試行に数えない。送った tx は放送前に記録し、次の試行はそれを探す。BTC は `payout_confirmations` の承認まで見張り、mempool から消えたら同じ tx を放送し直す（別の tx に使われたら `closed`）。EVM は 10 分待って mined でなければ、同じ nonce で手数料を 1/8 以上上げて送り直す。
- shopper の USDC の決着: Safe の残高が lock_amount 未満になり、Safe からの USDC の Transfer がある tx が見つかったら決着とみなす（後から送り付けられた少額は Safe に残ってよい）。裁定の連署は `lock_amount ≤ 合計 ≤ いまの残高` なら受け入れる。
- shopper は裁定を、開いている紛争（user の `dispute.open` の写し、または注文の escrow からの `dispute.evidence_request`）があり、`escrow_fee` が escrow のプロフィールの `dispute_fee_bps` 以下のときだけ自動で連署する。紛争を知る前に届いた裁定は保留し、紛争を知った時点で判断する。`dispute.countersigned` はチェーン上で escrow の出力が使われたのを見るまで状態を変えない。連署中の裁定がある注文は T1 の受け取りをしない（連署しなかった裁定は受け取りを止めない）。watch のループは注文のロックを待たない。寄付（§2.3）は見積の時点の一覧のものを注文に保存して使う。紛争の証拠は 1 通目（messages 以外）も大きさの上限に収め、入りきらないデータ（購入・配送の証拠）は添付で送る（escrow の上限どおり 4 件・1 件 64 チャンクまで）。
- escrow の事件（`GET /cases`）の状態: `fee_pending`（入金か前払い手数料をまだ確かめられない。5 秒から倍々に最大 1 時間の間隔と、裁定の要求・新しい notice / dispute.open のときに確かめ直す。7 日で `no_obligation`）、`open`、`no_obligation`（注文として名指された合意のどれも escrow 自身の注文鍵への入金と一致しない、手数料が 0 か escrow 自身の `max(bps × lock, min)` 未満、入金 tx の外）、`ruled`（裁定は 1 回だけ。2 回目は 409）、`closed`（チェーン上で escrow の出力が使われた。USDC は残高が lock_amount 未満になり Safe からの Transfer がある）。事件は request の id で組み立てる（spec §4.7）: notice は同じ注文 id で異なる request のものも 4 件まで保持し、チェーンで入金が確かめられた合意（`verified`）だけを使う。確かめられるまでは要点だけの控え（stub）で、証拠・添付は保存せず、開いた時点で messenger の受信箱から読み直す。確かめられた合意の後に別の request を名指すものは無視して履歴に残す。同じ前払い手数料が確かめられた別の事件で使われていれば `fee_pending` のまま確かめ直す。裁定は当事者ごとに送り、送れなかった相手には 5 秒ごとの見回りで送り直す（`ruling_sent`）。`dispute.evidence` は inner の id で 1 回だけ数える。見回りは同時に 8 件まで。添付は 1 当事者 4 件・1 件 64 チャンクまで、事件の文書とは別の bucket に置き、`GET /cases/{order_id}` の `attachments` に `data_b64` 付きで出す。
- 受け入れる範囲（spec §10）: ノードが保存・gossip・Nostr との中継をするのは、`trust.coordinators` の委任書、委任された operator の一覧、実効の一覧に載った shopper / escrow のプロフィールと 10050、自分自身のものだけ。Nostr の購読もこの著者に絞り、信頼が変わると購読し直す（範囲から外れたイベントは消す）。範囲の外の正しいイベント（委任書より先に届いた一覧やプロフィールなど）は 1000 件まで脇に置き、範囲が広がったら取り込む。範囲が変わると libp2p の相手と trust-sync をやり直す。gossip は相手ごとに 1 分 600 件まで。それ以外の 10050（注文の相手の user など）は送るときに問い合わせ、上限付き（1000 件, 10 分）でメモリに持つ。自分の 10050 は `v` タグを付けずに署名する（版は `created_at`）。`v` の付いた 10050 も受け付ける。一覧は `d`・`network` タグ・content の `network` が一致すること。
- メッセージ（spec §4）: 受け取ったものは先に保存（未処理）してから ack し、ハンドラが終わったら処理済みにする。再起動すると未処理のものをもう一度処理する。ハンドラは注文ごとに 1 件ずつ、全体で 8 並列、1 件 2 分まで（時間切れのハンドラが戻るまで、その注文の次のメッセージは待つ。ほかの注文は別の worker が進める）。
  - wrap は `id` が内容のハッシュと一致し署名が正しいものだけを扱い、復号と検証を通ったものだけを「見た」と記録する（リレーが別の wrap の id を名乗って本物を捨てさせることはできない）。
  - 既に保存した inner（再送の新しい wrap、別のリレーの写し）は ack し直すだけで、送り手の枠を使わない。
  - 注文の相手（shopper なら注文の user と escrow、escrow なら事件の user と shopper）からのものは送り手ごと・全体の制限の対象外（相手と注文ごとに 1 分 300 件、瞬間 600 件の歯止めだけ）。それ以外の送り手は、送り手ごとに 1 分 30 件、全体で 1 分 60 件まで（超えた分は保存も ack もしない。送り手の再送で後から届く）。
  - 注文の相手でない送り手からは、役割が受け付ける種類だけを受け取る: shopper は `order.request`、escrow は自分を名指す request を含む `escrow.notice` と `dispute.open`、operator は `report`。それ以外は保存も ack もしない（再接続のときにもう一度見る）。
  - 購読は新しいほうから 1000 件を読み、起動時と 1 時間ごとに保存済みの wrap を `until` で 14 日前まで遡って読む（新しいゴミが 1000 件あっても古いメッセージに届く）。
  - ack は送り手と注文ごとにまとめて送る（捨てない）。宛先は送り手の 10050、無ければ注文の request の `relays` と届いたリレー。
  - 送る wrap の content が 65535 byte を超えるものは送る前に失敗する（spec §4.9）。`o` タグは ack 以外で必須で、注文の無いメッセージには乱数の `o` を付ける。
  - 10050 と宛先の候補は、使えないもの（プライベートのアドレス・`ws(s)://` でない URL）を除いてから先頭 8 個まで、送るのは k 個まで（失敗したら次の候補へ）。再送は同じ wrap を使う。注文の無い相手（shopper なら断った依頼の相手）へは再送しない。
  - wrap の id は 14 日、処理済みの受信と ack 済み・期限切れの送信は注文があれば 120 日（T2 より長く。なければ 30 日）で消す。
  - リレーへの接続は最大 64 本（使っていないものから閉じる）。リレーが購読を CLOSED で断ると、間隔を倍にしながら（最大 30 秒）購読し直す。

`p2p-relay` の peer id は `relay-p2p` の鍵から決まる（`psctl keys` で表示）。lab の各鍵の公開鍵と peer id は `lab/keys/public.json`。実際の設定は `lab/nodes/*.yaml`。NAT の内側からは名前が引けないので、relay は IP で書く。

## psnode の管理 API（`admin.listen`, ヘッダ `Authorization: Bearer <token>`）

| 要求 | 役 | 内容 |
|---|---|---|
| `GET /status` | 全部 | `{pubkey, role, network, peer_id, addrs, reachability, trust: {operator: version}, relays}` |
| `GET /trust` | 全部 | 保持している委任書・一覧と、実効の組み合わせ |
| `POST /events` | 全部 | 署名済みのイベント（30500/30501/30502/30503/10050）、またはその配列を受け取り、検証・保存・gossip・Nostr へ公開。`null` の要素や受け入れる範囲の外の著者は 400 |
| `GET /healthz` | 全部 | **認証なし**。起動が終わると 200 `{"ok": true, "role"}`、それまでは 503 |
| `POST /admin/pause {"seconds": n}` | 全部 | n 秒（1〜86400）メッセージの送受信（購読・送信・再送・ack）と役割の定期処理（入金の確認・追跡・支払い・T1 の受け取り）を止める → `{"paused": true, "paused_until": UNIX 秒}`。送るものは outbox に残り、再開後に送る。libp2p・信頼の中継・管理 API は動き続ける。もう一度呼ぶと期限を置き換える。lab のシナリオ h で「shopper が消えた」状態を作るのに使う |
| `POST /admin/resume` | 全部 | 止めていたものをすぐ再開する → `{"paused": false, "was_paused"}`。`GET /status` の `paused_until` は止めている間だけ出る |
| `POST /p2p/status {"peer_id"}` | 全部 | libp2p の `/ps/status/1.0.0` で相手に問い合わせた結果 |
| `GET /orders`, `GET /orders/{id}` | shopper | 注文の状態と履歴（`pending` は保留中の処理） |
| `POST /orders/{id}/resolve` | shopper | `needs_human` の注文を人が片付ける。`{"action":"purchased","shop_order_id","total":{"amount","currency"}}`（手で買った: `order.purchased` を送る。total は必須で、無い・正の数でなければ 400）または `{"action":"refund"}`（買わなかった: 払い戻しを申し出る）。`needs_human` 以外は 409 |
| `GET /cases`, `GET /cases/{order_id}` | escrow | 紛争の一覧・証拠・復号した住所 |
| `POST /cases/{order_id}/rule {"user","shopper","reason"}` | escrow | 裁定して署名済みの tx を両者へ送る。入金の出力が注文の P2WSH か、Safe が予測どおり（所有者・しきい値・モジュール）かをチェーンで確かめる。BTC は 546 sats 未満の配分（0 以外）と、予備が最小のリレー手数料に足りない裁定を 400 で断る（escrow の手数料が 546 sats 未満なら取らない。予備は §4.5 の上限までに抑える）。USDC は署名時の Safe の残高を分ける。手数料の無い事件と 2 回目の裁定は 409 |
| `GET /reports` | operator | 受け取った通報 |

## 開発時のコマンド（ホストに Go / Node が無くても動く）

```sh
# Go
docker run --rm -v "$PWD":/src -v ps-gomod:/go/pkg/mod -v ps-gocache:/root/.cache/go-build -w /src/node golang:1.24-bookworm go test ./...
# Node
docker run --rm -v "$PWD":/src -v ps-npm:/root/.npm -w /src/shopper-bot node:22-bookworm npm test
# Foundry
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test
```
