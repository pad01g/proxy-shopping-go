# proxy-shopping-go

暗号通貨（BTC signet / USDC）で、現金や特定の決済しか使えない店の買い物を代行してもらう P2P 網の、
Go ノードと検証環境一式。ブラウザ側は [proxy-shopping-web](https://github.com/pad01g/proxy-shopping-web)。

| ディレクトリ | 中身 |
|---|---|
| `docs/spec.md` | プロトコル仕様（ワイヤ形式・BTC スクリプト・Safe・タイムロック・信頼の委譲） |
| `docs/lab.md` | docker compose の検証環境（ホスト名・IP・lab の鍵・設定） |
| `node/` | Go: `psnode`（shopper / escrow / operator / relay）、`psctl`、`psrelay`（Nostr リレー）、`esplora-lite`、`labfaucet`、`ratefeed` |
| `contracts/` | Foundry: Safe v1.4.1、タイムロックの Safe モジュール、模擬 USDC / オラクル、bond の参考実装 |
| `fakeshop/` | 架空の店 4 つ、仮想のカード決済ゲートウェイ、レートの模擬 |
| `shopper-bot/` | 店を操作する自動操作ツール（Playwright。AI 版と同じ入出力の API） |
| `e2e/` | compose 全体を使うシナリオ（NAT の内側から実行） |
| `lab/` | compose の設定（Caddy、ノード設定、lab の鍵） |

## 動かす

`../proxy-shopping-web` に proxy-shopping-web を置いてから:

```sh
docker compose up -d --build
docker compose run --rm runner          # 全シナリオ
docker compose run --rm runner a e      # シナリオを選ぶ
```

結果は `e2e/results/e2e-latest.md`。シナリオは `docs/lab.md` と `e2e/src/scenarios/index.ts`。

## デモ画面

`docker compose up -d --build` の後、ブラウザで <http://localhost:8888/> を開く。利用者・escrow・operator・coordinator の鍵を
それぞれブラウザに持ち、shopper は lab の Go ノード（shopper-1）が務める。左のガイドに従ってボタンを押すと、本物のリレー・signet・anvil の上で
代理購入と escrow のロック解除まで進む。シナリオは正常系（BTC / USDC）・紛争で返金・在庫切れ・危険な店・不正な escrow・T2 の返金の 7 つ。
役割ごとに別のウィンドウで開くときは `http://localhost:8888/?role=user` と `?role=escrow,operator,coordinator` のように指定する。

```sh
docker compose run --rm runner demo                 # デモ画面の e2e（7 シナリオ + 別ウィンドウ）
docker compose run --rm runner demo fraud separate  # 選ぶ
```

結果は `e2e/results/demo-latest.md`。詳しくは `docs/lab.md` の「デモ画面」。lab 専用（管理 API の token を付けて中継するので、127.0.0.1 以外に公開しないこと）。

## テスト

```sh
docker run --rm -v "$PWD":/src -v ps-gomod:/go/pkg/mod -w /src/node golang:1.24-bookworm go test ./...
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test
```

`lab/keys/` の鍵はテスト専用。公開網では使わないこと。
