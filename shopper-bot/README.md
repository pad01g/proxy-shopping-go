# shopper-bot

shopper ノードの代わりに店で買い物をする自動操作ツール。API は `docs/spec.md` §9。

```
psnode (shopper) ──HTTP──▶ shopper-bot :7000 ──Chromium──▶ 店 / 決済画面
```

## API

| 要求 | 入力 | 出力 |
|---|---|---|
| `GET /v1/capabilities` | ― | `{"version":"1","drivers":["cash-store.test","safe-shop.test","us-shop.test"]}` |
| `POST /v1/purchase` | `PurchaseRequest` | `PurchaseResult` |
| `POST /v1/tracking` | `TrackingQuery` | `TrackingStatus`（注文が無ければ 404） |

型は `schema/*.json`（JSON Schema 2020-12）。要求も応答も ajv で検証する。不正な要求は 400、ドライバが不正な結果を返したら 500。

### 冪等（`request_id`）

- 結果は `request_id` ごとに `BOT_DATA_DIR/purchases/<sha256(request_id)>.json` に保存する（一時ファイル + rename）。同じ `request_id` の要求には、店を操作せず保存した結果を返す（再起動の後も）。
- 購入の途中に同じ `request_id` が来たら `409 {"status":"in_progress","request_id"}`。ノードは少し待って同じ要求を送り直す。
- 同じ `request_id` で中身の違う要求は `422`（要求の誤り。再試行しない）。
- 店を操作する前に `started`、支払いの操作（カードの送信・現金の受け取り）の直前に `payment_submitted` を記録する。`payment_submitted` のまま止まった要求は二度と実行せず `needs_human` を返す。`started` のまま止まった要求（まだ払っていない）はやり直す。
- 支払いの後に失敗した要求は `needs_human` として保存され、同じ `request_id` でも再実行しない。

`PurchaseResult.status`:

- `ok`: 買えた。`shop_order_id` と `total`（店の表示額）、証拠（確認画面のスクリーンショットと JSON の receipt）付き。スクリーンショットは表示範囲（1280×900）だけの PNG。200 KiB を超えるときは JPEG（品質 70、次に 40）、それでも超えれば `sha256` と `mime` だけにする。
- `failed`: **支払っていない** ことが分かっている失敗。店の合計が `max_amount` を超えた、カードが拒否された、商品が無い、など。
- `needs_human`: 人が確かめる必要がある。担当のドライバが無い店、または支払いの操作の後に何かが起きて支払われたか分からないとき。

## 守っていること

- **カード情報は bot の中だけ。** 要求の `payment_ref` は参照（`card:default`）で、`BOT_CARDS_FILE` の中身に解決する。既定は `lab/cards.json`（lab の試験用カード）。
- **`max_amount` を超えたら払わない。** 店のレジ画面の合計を読み、通貨が違うか超えていれば、支払い画面に進む前に `failed` を返す。決済画面の金額がレジと違う場合も払わない。
- **想定外の決済画面にはカードを入れない。** カードの店は決まったゲートウェイ（`cardgw.test`）に移ったことを確かめてから入力する。
- 購入ごとに新しいブラウザコンテキスト（cookie を共有しない）。

## ドライバ

`src/drivers/driver.ts` の `Driver` を実装し、`DriverRegistry` に店のホストで登録する。

| ドライバ | ホスト | 内容 |
|---|---|---|
| `CardShopDriver` | `safe-shop.test`, `us-shop.test` | 商品ページ → カート → レジ → `cardgw.test` → 確認画面 |
| `CashStoreDriver` | `cash-store.test` | 店頭端末 `/pos` を操作する（＝店頭で現金を払う場面を模す）。`payment_ref` は `cash` |
| `AIDriver`（`ai.ts`） | ― | 未実装。**既定では登録しない** |

`risky-shop.test` はわざと登録していない（shopper ノードが危険度で断る店。来ても `needs_human`）。

### AI ドライバを足すとき

AI 版も同じ入出力（`PurchaseRequest` → `PurchaseResult`, `TrackingQuery` → `TrackingStatus`）を守る。サーバは結果をスキーマで検証するので、形の違う結果は通らない。
カードは `ctx.cards.resolve(req.payment_ref)` からだけ受け取り、`max_amount` を超えたら払わず、払ったか分からなくなったら `needs_human` を返し、スクリーンショットと receipt を証拠に付ける。
登録は `src/drivers/index.ts` の `defaultRegistry()` で、担当させるホストを明示する。

## 設定（環境変数）

| 変数 | 既定 | 内容 |
|---|---|---|
| `PORT` / `HOST` | `7000` / `0.0.0.0` | 待ち受け |
| `BOT_CARDS_FILE` | `lab/cards.json` | `{"cards": {"default": {"number","exp","cvc","name"}}}` |
| `BOT_DATA_DIR` | `data`（イメージでは `/data`） | 購入結果の保存先（冪等のため。lab は volume `bot1_data` / `bot2_data`） |
| `NODE_EXTRA_CA_CERTS` | ― | lab の Caddy のルート CA（Chromium は `ignoreHTTPSErrors` で動く） |
| `PURCHASE_TIMEOUT_MS` | `120000` | 1 回の購入の上限 |
| `ACTION_TIMEOUT_MS` | `15000` | 1 操作の上限 |
| `HEADLESS` | `true` | `false` で画面を出す（手元の調査用） |
| `SHOP_SCHEME_OVERRIDE` | ― | **試験専用。** `http` にすると店の URL を HTTP で開く（Caddy 無しで fakeshop に直接つなぐとき） |
| `BROWSER_HOST_RESOLVER_RULES` | ― | **試験専用。** Chromium の `--host-resolver-rules`。例 `MAP *.test 172.18.0.2:8080` |

## 試験

```sh
# 単体（スキーマ・max_amount・サービス）
docker run --rm -v "$PWD":/src -v ps-npm:/root/.npm -w /src/shopper-bot node:22-bookworm sh -c 'npm ci && npm test'

# 結合: fakeshop を立てて、Playwright のコンテナから本物の Chromium で買う
docker network create ps-bot-test
docker build -t ps-fakeshop:dev fakeshop
docker run -d --rm --name fakeshop --network ps-bot-test ps-fakeshop:dev
docker run --rm --network ps-bot-test -v "$PWD":/src -w /src/shopper-bot -e FAKESHOP_ADDR=fakeshop:8080 \
  mcr.microsoft.com/playwright:v1.55.0-noble npm run test:integration
docker rm -f fakeshop && docker network rm ps-bot-test
```

結合試験では Caddy も DNS も無いので、`*.test` を fakeshop の IP へ向け（`BROWSER_HOST_RESOLVER_RULES`）、HTTP で開く（`SHOP_SCHEME_OVERRIDE=http`）。
fakeshop は `X-Forwarded-Proto` が無ければ `http://cardgw.test/…` へ転送するので、決済画面との往復もそのまま動く。
追跡（`/v1/tracking`）も店の JSON API をブラウザ経由で読むので、同じ設定で届く。
`FAKESHOP_ADDR` が無ければ結合試験は飛ばす（`npm test` は単体だけになる）。
