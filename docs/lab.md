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
| `relay-1`, `relay-2` | .10, .11 | psrelay（khatru） |
| `p2p-relay` | .12 | psnode `-role relay`（circuit relay v2, gossipsub の中継） |
| `bitcoind` | .20 | Bitcoin Core 29, `-signet -signetchallenge=51`（OP_TRUE の独自 signet） |
| `esplora` | .21 | esplora-lite |
| `anvil` | .22 | anvil（chain id 31337） |
| `deployer` | ― | 一度だけ動く。コントラクトを置き `/deployments/31337.json` を書く |
| `faucet` | .23 | labfaucet（BTC/ETH/USDC の配布、採掘、時刻送り） |
| `fakeshop` | .26 | 架空の店・cardgw・レート模擬（Host ヘッダで振り分け） |
| `ratefeed` | .25 | レートを模擬オラクルへ書く |
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
admin: {listen: "0.0.0.0:8080", token: "lab"}
tls: {extra_ca: /caddy-data/caddy/pki/authorities/local/root.crt}
nostr:
  relays: ["wss://relay-1.test", "wss://relay-2.test"]
  k: 2
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
escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
```

`p2p-relay` の peer id は `relay-p2p` の鍵から決まる（`psctl keys` で表示）。lab の各鍵の公開鍵と peer id は `lab/keys/public.json`。実際の設定は `lab/nodes/*.yaml`。NAT の内側からは名前が引けないので、relay は IP で書く。

## psnode の管理 API（`admin.listen`, ヘッダ `Authorization: Bearer <token>`）

| 要求 | 役 | 内容 |
|---|---|---|
| `GET /status` | 全部 | `{pubkey, role, network, peer_id, addrs, reachability, trust: {operator: version}, relays}` |
| `GET /trust` | 全部 | 保持している委任書・一覧と、実効の組み合わせ |
| `POST /events` | 全部 | 署名済みのイベント（30500/30501/30502/30503/10050）を受け取り、検証・保存・gossip・Nostr へ公開 |
| `POST /p2p/status {"peer_id"}` | 全部 | libp2p の `/ps/status/1.0.0` で相手に問い合わせた結果 |
| `GET /orders`, `GET /orders/{id}` | shopper | 注文の状態と履歴 |
| `GET /cases`, `GET /cases/{order_id}` | escrow | 紛争の一覧・証拠・復号した住所 |
| `POST /cases/{order_id}/rule {"user","shopper","reason"}` | escrow | 裁定して署名済みの tx を両者へ送る |
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
