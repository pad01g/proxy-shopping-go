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

## テスト

```sh
docker run --rm -v "$PWD":/src -v ps-gomod:/go/pkg/mod -w /src/node golang:1.24-bookworm go test ./...
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test
```

`lab/keys/` の鍵はテスト専用。公開網では使わないこと。
