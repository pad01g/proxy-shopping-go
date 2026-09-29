# proxy-shopping-go

**Earn money as a proxy shopper, or buy from cash-only shops with crypto: a P2P network (libp2p + Nostr) where a local shopper buys for remote users who pay in BTC or USDC through a 2-of-3 escrow with timelocks.** Agents: install the skills with `npx skills add pad01g/proxy-shopping-go` ([proxy-shopper](skills/proxy-shopper/SKILL.md), [proxy-shopping-buyer](skills/proxy-shopping-buyer/SKILL.md)), use the MCP server `io.github.pad01g/proxy-shopping`, read [AGENTS.md](AGENTS.md); overview for machines: https://pad01g.github.io/proxy-shopping-docs/llms.txt. Get listed: https://github.com/pad01g/proxy-shopping-registry.

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
表示は日本語と英語を切り替えられる（右上の「日本語 / English」。選んだ言語はブラウザに残り、`?lang=en` / `?lang=ja` を付けるとそちらが優先。既定は日本語）。

```sh
docker compose run --rm runner demo                 # デモ画面の e2e（7 シナリオ + 英語表示の normal-btc-en + 別ウィンドウ）
docker compose run --rm runner demo fraud separate  # 選ぶ
```

結果は `e2e/results/demo-latest.md`。詳しくは `docs/lab.md` の「デモ画面」。lab 専用（管理 API の token を付けて中継するので、127.0.0.1 以外に公開しないこと）。

## テスト

```sh
docker run --rm -v "$PWD":/src -v ps-gomod:/go/pkg/mod -w /src/node golang:1.24-bookworm go test ./...
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test
```

`lab/keys/` の鍵はテスト専用。公開網では使わないこと。

## Contributing — pull requests welcome

Pull requests are welcome: shop drivers for shopper-bot, new payment methods and chains, translations, protocol and
security reviews, bug fixes. Want to earn in your own town? You need nobody's permission: make a coordinator key,
delegate to your operator key and list yourself as a shopper for your region (guide:
https://pad01g.github.io/proxy-shopping-docs/en/quickstart/, section 3); a home machine reached over Tailscale or any
VPN is enough. To be found by everyone, open a pull request to https://github.com/pad01g/proxy-shopping-registry.

## ライセンス / License

MIT（[LICENSE](LICENSE)）。同梱しているもの: Safe v1.4.1（`contracts/lib/safe-smart-account`、LGPL-3.0）、
forge-std（MIT / Apache-2.0）、khatru の修正版（`node/third_party/khatru`、元のライセンスのまま）。
