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
| `relay-1`, `relay-2` | .10, .11 | psrelay（khatru）。下の「psrelay の制限」 |
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
| `natbox` | 172.41.0.10 | NAT の内側の名前空間 |
| `escrow-nat` | natbox を共有 | NAT の内側の escrow（libp2p は relay 経由でのみ届く） |
| `browser` | 172.41.0.20 | Playwright（NAT の内側のブラウザ利用者） |
| `runner` | pub + home | e2e の実行（profile `run`） |

`bot-1` / `bot-2` は購入結果を `request_id` ごとに volume `bot1_data` / `bot2_data`（`/data`, `BOT_DATA_DIR`）に保存する（spec §9 の冪等）。やり直すときは volume ごと消す。

## psrelay の制限

フラグ（括弧は環境変数と既定値）。制限はすべて 1 分あたりで、`-1` で無効。

| フラグ | 内容 |
|---|---|
| `-kinds`（`PSRELAY_KINDS`, `5,1059,10050,30500-30503`） | 受け取る kind。kind 0 は受け取らない。kind 5 は自分のイベントだけを消せる |
| `-events-per-minute`（`PSRELAY_EVENTS_PER_MINUTE`, 600） | 接続元アドレスごとの EVENT |
| `-conn-events-per-minute`（`PSRELAY_CONN_EVENTS_PER_MINUTE`, 300） | 接続ごとの EVENT |
| `-reqs-per-minute`（`PSRELAY_REQS_PER_MINUTE`, 600） | 接続元アドレスごとの REQ の filter |
| `-conns-per-minute`（`PSRELAY_CONNS_PER_MINUTE`, 120） | 接続元アドレスごとの新しい接続 |
| `-max-subscriptions`（`PSRELAY_MAX_SUBSCRIPTIONS`, 32） | 接続ごとの開いている REQ |
| `-max-limit`（`PSRELAY_MAX_LIMIT`, 1000） | filter の `limit` の上限 |
| `-trusted-proxies`（`PSRELAY_TRUSTED_PROXIES`, なし） | `X-Forwarded-For` を信じる逆プロキシ（IP / CIDR）。lab は `edge`（172.40.0.2） |

置き換え可能な kind（10050, 30500–30503）は最新の 1 件だけを保つ。COUNT（NIP-45）と NIP-50 の検索には応じない。

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
admin: {listen: "0.0.0.0:8080", token: "lab"}   # ループバック以外で listen するなら token は必須
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
trust:
  coordinators: ["<coordinator-1 pk>", "<coordinator-2 pk>"]
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
  delivery_days: 5
  risk: {allowlist: [safe-shop.test, us-shop.test, cash-store.test], known_gateways: [cardgw.test], threshold: 70}
  timelock: {btc_t1_blocks: 100, btc_t2_blocks: 150, evm_t1_seconds: 3600, evm_t2_seconds: 7200}
  confirmations: 1
  tracking_poll_seconds: 2
  min_t1_remaining_seconds: 1800   # 既定 (delivery_days + 7) 日。T1 までこれ未満なら買わずに協力的な払い戻しを申し出る
  allow_private_shops: true        # 既定 false: shop_url がループバック・リンクローカル・プライベートのアドレスなら取りに行かない
escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
```

- `admin.token` が空で `admin.listen` がループバック（`127.0.0.1`, `[::1]`, `localhost`）以外なら、設定の読み込みで失敗する。
- `nostr.allow_private_relays` は接続する瞬間のアドレスで判定する（名前の解決先が後で変わっても回り込めない）。lab のノードはすべて `true`。
- `shopper.allow_private_shops` も同じく接続ごと（リダイレクト先も含む）に判定する。店の証明書が検証できなければ読まない（検証を外して読み直すことはしない）。`http://` の店は TLS の点が付かないだけ。見積の拒否理由には接続の詳細を書かない（ログにだけ残す）。
- shopper の設定の検査: `timelock` の T1（BTC は 600 秒 = 1 ブロックで換算）は `min_t1_remaining_seconds` より先、`payout_fee_reserve_sats` は 20000 以下（spec §4.5）。見積では予備が lock の 5% を超える注文を `limit` で断る。
- shopper の注文（spec §4.6, §4.8, §9）: `order.funded` は `order.accept` の後だけ受け付ける（先に届いたものは accept の後に処理し直す）。入金の出力・Safe・前払い手数料の tx は 1 つの注文にしか使えない（bbolt の `funding_uses`）。承認時刻が `expires_at` + 1 時間より後の入金、購入の開始時に T1 まで `min_t1_remaining_seconds` 未満の注文は買わずに払い戻しを申し出る。チェーンに問い合わせられないとき（通信・5xx・未索引）は `funding` のまま再試行し、食い違いがはっきりしたときだけ `accepted` に戻す。bot への購入は `request_id` = 注文 id で、409 は待ち、4xx は `purchase_failed`（払い戻しの申し出）、それ以外は同じ `request_id` で問い合わせ直す。
- shopper の保留中の処理（`pending`）: 見積の送信、払い戻しの申し出、release の連署、裁定の連署、T1 の受け取り。失敗しても注文に残り、3 秒ごとに再試行する（はっきり失敗したもの、50 回失敗したものは `error` と履歴に残して捨てる）。送った tx は放送前に記録し、次の試行はそれを探す（EVM は 10 分待って mined でなければ送り直す）。
- shopper は裁定を、開いている紛争（user の `dispute.open` の写し）があり、`escrow_fee` が escrow のプロフィールの `dispute_fee_bps` 以下のときだけ自動で連署する。`dispute.countersigned` はチェーン上で escrow の出力が使われたのを見るまで状態を変えない。裁定のある注文は T1 の受け取りをしない。
- escrow の事件（`GET /cases`）の状態: `fee_pending`（前払い手数料をまだ確かめられない。5 秒ごとと裁定の要求のときに確かめ直す）、`open`、`no_obligation`（手数料が無い・入金 tx の外・別の注文で使用済み）、`ruled`（裁定は 1 回だけ。2 回目は 409）、`closed`（チェーン上で escrow の出力が使われた）。添付は 1 当事者 4 件・1 件 64 チャンクまで、事件の文書とは別の bucket に置き、`GET /cases/{order_id}` の `attachments` に `data_b64` 付きで出す。
- 受け入れる範囲（spec §10）: ノードが保存・gossip・Nostr との中継をするのは、`trust.coordinators` の委任書、委任された operator の一覧、実効の一覧に載った shopper / escrow のプロフィールと 10050、自分自身のものだけ。Nostr の購読もこの著者に絞り、信頼が変わると購読し直す（範囲から外れたイベントは消す）。それ以外の 10050（注文の相手の user など）は送るときに問い合わせ、上限付き（1000 件, 10 分）でメモリに持つ。
- メッセージ（spec §4）: 受け取ったものは先に保存（未処理）してから ack し、ハンドラが終わったら処理済みにする。再起動すると未処理のものをもう一度処理する。ハンドラは注文ごとに 1 件ずつ、全体で 8 並列、1 件 2 分まで。送り手ごとに 1 分 30 件まで（超えた分は ack せず捨てる。送り手の再送で後から届く）。10050 と宛先の候補は先頭 8 個まで、送るのは k 個まで（失敗したら次の候補へ）。再送は同じ wrap を使う。注文の無い相手（shopper なら断った依頼の相手）へは再送しない。wrap の id は 14 日、処理済みの受信と ack 済み・期限切れの送信は注文があれば 60 日（なければ 30 日）で消す。

`p2p-relay` の peer id は `relay-p2p` の鍵から決まる（`psctl keys` で表示）。lab の各鍵の公開鍵と peer id は `lab/keys/public.json`。実際の設定は `lab/nodes/*.yaml`。NAT の内側からは名前が引けないので、relay は IP で書く。

## psnode の管理 API（`admin.listen`, ヘッダ `Authorization: Bearer <token>`）

| 要求 | 役 | 内容 |
|---|---|---|
| `GET /status` | 全部 | `{pubkey, role, network, peer_id, addrs, reachability, trust: {operator: version}, relays}` |
| `GET /trust` | 全部 | 保持している委任書・一覧と、実効の組み合わせ |
| `POST /events` | 全部 | 署名済みのイベント（30500/30501/30502/30503/10050）、またはその配列を受け取り、検証・保存・gossip・Nostr へ公開。`null` の要素や受け入れる範囲の外の著者は 400 |
| `POST /p2p/status {"peer_id"}` | 全部 | libp2p の `/ps/status/1.0.0` で相手に問い合わせた結果 |
| `GET /orders`, `GET /orders/{id}` | shopper | 注文の状態と履歴（`pending` は保留中の処理） |
| `POST /orders/{id}/resolve` | shopper | `needs_human` の注文を人が片付ける。`{"action":"purchased","shop_order_id","total":{"amount","currency"}}`（手で買った: `order.purchased` を送る）または `{"action":"refund"}`（買わなかった: 払い戻しを申し出る）。`needs_human` 以外は 409 |
| `GET /cases`, `GET /cases/{order_id}` | escrow | 紛争の一覧・証拠・復号した住所 |
| `POST /cases/{order_id}/rule {"user","shopper","reason"}` | escrow | 裁定して署名済みの tx を両者へ送る。入金の出力が注文の P2WSH か、Safe が予測どおり（所有者・しきい値・モジュール）かをチェーンで確かめる。手数料の無い事件と 2 回目の裁定は 409 |
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
