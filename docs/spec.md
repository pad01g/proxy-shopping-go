# proxy-shopping プロトコル仕様 v1

暗号通貨で、現金や特定の電子決済しか受け付けない店の買い物を代行してもらう P2P 網の仕様。
Go 実装（`proxy-shopping-go`）と TypeScript 実装（`proxy-shopping-web`）は、この文書のワイヤ形式に従う。

- 「必須」「任意」と書いたもの以外は、この文書の既定値を使う。
- 数値の金額はすべて **10 進の文字列**（JSON number は使わない）。単位は各所に明記する。
- 公開鍵は特記がなければ Nostr 形式（x-only 32 byte の hex, 小文字）。
- 時刻は UNIX 秒。

## 0. 構成

```
ブラウザ（proxy-shopping-web）             Go ノード（proxy-shopping-go/node）
  鍵はブラウザ内・署名もブラウザ       shopper / escrow / operator / coordinator / relay
        │ WSS（外向きのみ）                       │ WSS            │ libp2p
        ▼                                          ▼                ▼
   Nostr リレー群（オペレータが一覧で指定） ◀──▶  Go ノード同士の網（gossipsub, circuit relay v2, DCUtR）
   = 常時オンラインの口 + メールボックス
```

| 役割 | 常時オンライン | 動かし方 | 責任 |
|---|---|---|---|
| user | いいえ | ブラウザ | 注文・入金・受取確認・紛争の申立 |
| shopper | **はい** | Go ノード + 自動操作ツール（shopper-bot） | 見積・代理購入・配送の報告 |
| escrow | いいえ | ブラウザ または Go ノード | 前払い手数料を受けた注文の紛争を T1 までに裁定 |
| operator | いいえ | ブラウザ または Go ノード / `psctl` | 地域ごとに信頼できる shopper × escrow の組み合わせ一覧を署名 |
| coordinator | いいえ | ブラウザ または `psctl` | オペレータへの委任書を署名 |
| mailbox | はい | Nostr リレー（`psrelay`, khatru） | 暗号文の一時保管 |
| p2p relay | はい | Go ノード（`-role relay`） | NAT の内側の Go ノードのための circuit relay v2 |

ブラウザ（user / escrow / operator / coordinator）は Nostr だけを使う。libp2p を使うのは Go ノード同士だけ。

## 1. 鍵

BIP39 のニーモニック（12 または 24 語, パスフレーズ無し）→ BIP32 の根の鍵。すべて secp256k1。

| 用途 | 導出パス | 備考 |
|---|---|---|
| 身元（Nostr 鍵） | `m/44'/1237'/0'/0/0` | NIP-06。一覧・プロフィール・メッセージの署名鍵。公開鍵は x-only |
| libp2p の鍵 | `m/7333'/0'/0'` | Go ノードのみ。libp2p の secp256k1 鍵として使う |
| BTC 注文鍵（user / shopper） | `m/7333'/1'/{idx}'` | idx は §1.1 |
| BTC 注文鍵（escrow） | `m/7333'/2'/{idx}` | **非 hardened**。escrow はオフラインでもよいので、`m/7333'/2'` の xpub をプロフィールに載せ、相手が導出する |
| BTC 財布（入金元・受取先） | `m/84'/1'/0'/0/0` | P2WPKH（`tb1q…`）。受取・返金・手数料の受け口もこのアドレス |
| EVM アカウント | `m/44'/60'/0'/0/0` | Safe の所有者・USDC の送受 |

### 1.1 注文ごとの idx

`idx = uint32_be(SHA-256(order_id の 16 byte))[0..4] & 0x7fffffff`

`order_id` は 16 byte の乱数を hex（32 文字）で表したもの。user が作る。

### 1.2 NIP-07

ブラウザでは身元の署名と NIP-44 の暗号化を NIP-07 拡張（`window.nostr`）に任せてもよい。
その場合も BTC / EVM の鍵はローカルのニーモニックから導出する（身元とは別のニーモニックでもよい）。

## 2. 信頼の委譲と一覧

yacy の trust bundle と同じ考え方。委譲は 1 段、有効期限は持たず、バージョンで管理する。
すべて Nostr のイベント（署名付き）で表し、Nostr リレーと libp2p gossipsub の両方で配る。

```
coordinator 鍵（利用者が設定。複数可、先頭ほど優先）
  └─ 委任書 kind 30500（coordinator が署名）: 「この operator に、この網の一覧を作らせる」
       └─ 一覧 kind 30501（operator が署名）: 地域 × shopper × escrow の組み合わせ
```

### 2.1 共通のタグ

| タグ | 意味 |
|---|---|
| `["d", <id>]` | addressable event の識別子 |
| `["v", "<整数>"]` | バージョン。**created_at ではなくこれで新旧を決める** |
| `["network", <名前>]` | 網の名前。例 `ps-lab`（e2e）, `ps-main` |

同じ `(kind, pubkey, d)` のイベントは `v` の最大のものを採る。同じ `v` が複数あれば `id` の辞書順で小さいもの。

### 2.2 委任書 kind 30500

```json
{"kind": 30500, "pubkey": "<coordinator>",
 "tags": [["d", "<operator pk>"], ["v", "3"], ["network", "ps-lab"], ["p", "<operator pk>"], ["revoked", "false"]],
 "content": "{\"note\":\"任意\"}"}
```

- `revoked` が `"true"` の版が最新なら、その operator の一覧は（この coordinator の下では）無効。

### 2.3 一覧 kind 30501

`d` = network 名。content は JSON:

```json
{
  "network": "ps-lab",
  "name": "Kanto operator",
  "regions": ["JP-13", "JP-14"],
  "relays": [{"url": "wss://relay-1.test", "retention_days": 30}],
  "chain": {
    "btc": {"network": "signet", "esplora": ["https://esplora.test"]},
    "evm": {"chain_id": 31337, "rpc": ["https://evm.test"], "usdc": "0x…", "safe": {"singleton": "0x…", "factory": "0x…", "fallback_handler": "0x…", "multisend_call_only": "0x…", "module": "0x…", "setup": "0x…"}}
  },
  "entries": [
    {
      "region": "JP-13",
      "shopper": "<pk>",
      "escrow": "<pk>",
      "shops": ["*"],
      "payments": ["btc-signet", "usdc-evm"],
      "tags": [],
      "escrow_sla_days": 14
    }
  ],
  "donation": {"btc_address": "tb1…", "evm_address": "0x…", "bps": 0},
  "report_to": "<operator pk>"
}
```

- `shops`: 対応する店のホスト名。`"*"` はすべて。店の危険度判定は shopper 自身の方針で行うので、オペレータ側は通常 `"*"`。
- `payments`: `btc-signet` / `usdc-evm` のいずれか。
- `escrow_sla_days`: escrow が紛争の申立から裁定までにかける日数の上限。**T1 より前に裁定する**ことが escrow の義務。
- `donation`: 任意の寄付欄。user のクライアントは `bps > 0` のとき支払いのトランザクションに出力を足すことを **提案**する（既定オフ、利用者が選ぶ）。プロトコル上の義務ではない。
- `chain`: 推奨の接続先。利用者は設定で上書きしてよい。
- 掲載料・預かり金（bond）・没収は **プロトコルの外** の、operator と shopper / escrow の間の規約。参考実装を `contracts/examples/bond` に置く。

### 2.4 実効の組み合わせ

1. 利用者の設定 `coordinators = [C1, C2, …]`（先頭ほど優先）。空なら何も信頼しない（`署名のみ` モードは持たない）。
2. 各 Ci について、Ci が署名した最新の委任書のうち `revoked=false` の operator を集める。
3. 各 operator の最新の一覧（network 一致）を読み、`entries` を集める。
4. 同じ `(region, shopper, escrow)` は先に決まったもの（優先度の高い coordinator、同じ coordinator 内では operator pk の辞書順）を採る。
5. 結果の各行には、出所の `(coordinator, operator, list version)` を付ける。

利用者が coordinator を「解任」するのは、設定からその鍵を外すこと。

### 2.5 地域コード

階層のある文字列を `-` でつなぎ、**前方一致**で照合する。

| 段 | 例 | 規則 |
|---|---|---|
| 国 | `JP` | ISO 3166-1 alpha-2 |
| 都道府県 | `JP-13` | ISO 3166-2 |
| 市区町村 | `JP-13-13104` | 日本は全国地方公共団体コード（JIS X 0402）の 5 桁。日本以外は当面なし |

`covers(R, X)` = `X == R` または `X` が `R + "-"` で始まる。

- 一覧の `entries[].region` が店の地域をカバーしていること。
- 現金のみの店は、shopper の `cash_regions` のどれかが店の地域をカバーしていること（§3.1）。

## 3. プロフィール

### 3.1 shopper kind 30502

`d` = network。content:

```json
{
  "name": "shopper-1",
  "payments": ["btc-signet", "usdc-evm"],
  "currencies": ["JPY", "USD"],
  "cash_regions": ["JP-13"],
  "fee": {"bps": 500, "min": {"amount": "300", "currency": "JPY"}},
  "max_order": {"amount": "200000", "currency": "JPY"},
  "delivery_days": 5,
  "evm_address": "0x…",
  "btc_address": "tb1q…",
  "p2p": {"peer_id": "16Uiu2…", "addrs": ["/ip4/…/tcp/4001"]}
}
```

### 3.2 escrow kind 30503

```json
{
  "name": "escrow-1",
  "btc_xpub": "tpub…（m/7333'/2' の拡張公開鍵, testnet の版番号）",
  "btc_fee_address": "tb1q…",
  "evm_address": "0x…",
  "upfront_fee": {"bps": 50, "min_sats": "1000", "min_usdc": "0.50"},
  "dispute_fee_bps": 200,
  "p2p": {"peer_id": "…", "addrs": []}
}
```

- **前払い手数料（upfront）を入金時に受け取らなかった注文について、escrow は裁定の義務を負わない。**
- 紛争手数料（`dispute_fee_bps`）は裁定の配分から差し引く。

### 3.3 受信箱のリレー kind 10050

NIP-17 と同じ。各主体は自分宛てのメッセージを受け取るリレーを `["relay", "wss://…"]` タグで公開する。
無ければ、関係する一覧の `relays` を使う。

## 4. 1 対 1 のメッセージ

### 4.1 包み方

NIP-59 の 3 層に従う。ただし中身（rumor）は **署名付き** にする。紛争のとき第三者（escrow）が検証できる証拠にするため。

1. **inner**（kind 5400, 送り手の身元鍵で署名）
   ```json
   {"kind": 5400, "pubkey": "<sender>", "created_at": 1790000000,
    "tags": [["p", "<recipient>"], ["o", "<order_id>"], ["t", "<type>"]],
    "content": "<body の JSON 文字列>", "id": "…", "sig": "…"}
   ```
2. **seal**（kind 13, 送り手が署名, tags なし）: content = NIP-44 v2(送り手→受け手, JSON(inner))
3. **wrap**（kind 1059, 使い捨て鍵で署名）: tags = `[["p", "<recipient>"]]`, content = NIP-44 v2(使い捨て→受け手, JSON(seal))

受け手は `seal.pubkey == inner.pubkey` と両方の署名を確かめる。`created_at` はずらさない（v1）。

### 4.2 届け方

- 送り手は受け手の受信箱リレー（§3.3）のうち **k 個以上**（既定 k=2, 足りなければ全部）へ wrap を送る。
- 受け手は `{"kinds":[1059], "#p":[自分]}` を購読する。
- 受け手は受け取った inner の `id` を `ack` で返す（ack 自身には ack しない）。
- 送り手は ack が来るまで、オンラインの間 30 秒ごとに再送する（上限 7 日）。inner の `id` で重複を除く。
- リレーの `OK` 応答には署名がないので、受領の証明には使わない。

### 4.3 メッセージの種類

`t` タグと body の対応。金額は §5 の単位。

| type | 送り手 → 受け手 | body |
|---|---|---|
| `ack` | 誰でも | `{"ids": ["<inner id>", …]}` |
| `order.request` | user → shopper | §4.4 |
| `order.quote` | shopper → user | §4.5 |
| `order.accept` | user → shopper | `{"quote_id": "<quote の inner id>"}` |
| `order.cancel` | user ↔ shopper | `{"reason": "…"}`（入金前のみ） |
| `order.funded` | user → shopper | §4.6 |
| `escrow.notice` | user → escrow | `{"request": <inner>, "quote": <inner>, "accept": <inner>, "funded": <inner>}`（署名付き inner をそのまま入れる） |
| `order.purchased` | shopper → user | `{"shop_order_id": "…", "total": {"amount","currency"}, "evidence": [Evidence]}` |
| `order.shipping` | shopper → user | `{"status": "shipped"\|"delivered"\|"failed", "tracking": TrackingStatus}` |
| `order.release` | user → shopper | `{"asset": …, "psbt": "<base64>"}` または `{"asset": …, "safe_tx": SafeTx, "signature": "0x…"}` |
| `order.refund` | shopper → user | release と同じ形（払い戻し先が user）。任意の協力的払い戻し |
| `order.completed` | shopper → user | `{"txid": "…"}`（BTC の txid または EVM の tx hash） |
| `dispute.open` | user または shopper → escrow（写しを相手へ） | §4.7 |
| `dispute.evidence_request` | escrow → 当事者 | `{"want": ["…"]}` |
| `dispute.evidence` | 当事者 → escrow | §4.7 の `evidence` と同じ形 |
| `dispute.ruling` | escrow → user と shopper | §4.8 |
| `dispute.countersigned` | 当事者 → 相手と escrow | `{"txid": "…"}` |
| `report` | 誰でも → operator | `{"subject": "<pk>", "order_id": "…", "text": "…", "evidence": [<inner> …]}` |
| `chat` | 当事者 ↔ 当事者 | `{"text": "…"}` |

### 4.4 order.request

```json
{
  "shop_url": "https://safe-shop.test/",
  "shop_region": "JP-13-13104",
  "items": [{"sku": "A-100", "qty": 1}],
  "payment": "btc-signet",
  "escrow": "<escrow pk>",
  "operator": "<operator pk>",
  "coordinator": "<coordinator pk>",
  "delivery": {
    "ciphertext": "<base64(nonce24 || XChaCha20-Poly1305(K, nonce, JSON(Address), aad=order_id の 16 byte))>",
    "key_for_shopper": "<NIP-44 v2(user→shopper, hex(K))>",
    "key_for_escrow_sha256": "<hex(SHA-256(key_for_escrow の文字列))>"
  },
  "key_proof": "<§4.4.1>",
  "user_btc_pubkey": "<33 byte 圧縮公開鍵 hex>（btc のとき）",
  "user_btc_address": "tb1q…（返金先）",
  "user_evm_address": "0x…（usdc のとき）",
  "relays": ["wss://…"]
}
```

`Address = {"name","postal_code","address","phone"}`。

- K は 32 byte の乱数。
- `key_for_escrow = NIP-44 v2(user→escrow, hex(K))` は **order.request に入れない**。
  - 入れると `escrow.notice` に写った request から、紛争の無い注文でも escrow が住所を読めてしまう。
  - user は request の直後に、`order.escrow_key {"key_for_escrow"}` を shopper へ送る。shopper はそれを紛争に備えて保存する。
  - **紛争になったら、shopper（または user）が escrow に渡す**（`order.escrow_key` の署名付き inner を証拠に入れる）。
  - escrow は `key_for_escrow_sha256` と照らし合わせ、**署名付きの request の `ciphertext` だけ**を復号する（相手が差し出した別の ciphertext は使わない）。
- `order_id` は inner の `o` タグ。

#### 4.4.1 key_proof（身元とチェーンの鍵の結び付け）

Nostr の身元（inner の署名者）と、多重署名に入れるチェーンの鍵が同じ人のものであることを示す。
これが無いと、別の身元が user の公開鍵を写した request を作り、escrow に「自分が user だ」と名乗れる。

- 対象のメッセージ `m = "ps-key-proof-v1|" + order_id + "|" + user の Nostr 公開鍵（hex）"`
- `btc-signet`: `key_proof = hex(BIP340 Schnorr 署名(SHA-256(m)))`。鍵は `user_btc_pubkey` の秘密鍵（§1 の注文鍵）。検証は x-only 公開鍵で行う。
- `usdc-evm`: `key_proof = hex(EIP-191 personal_sign(m))`（65 byte）。署名者は `user_evm_address` と一致すること。
- shopper と escrow は、`key_proof` が検証できない request を拒否する。

### 4.5 order.quote

```json
{
  "accept": true,
  "reject_reason": "risk|region|payment|limit|unavailable|…（accept=false のとき）",
  "detail": "人が読む説明",
  "expires_at": 1790000900,
  "price": {"items": {"amount": "12000", "currency": "JPY"}, "shipping": {"amount": "800", "currency": "JPY"}, "shopper_fee": {"amount": "640", "currency": "JPY"}},
  "fx": {"pair": "BTC/JPY", "rate": "15000000", "sources": [{"name": "coingecko", "rate": "15000000", "at": 1790000000}], "at": 1790000000},
  "asset": "btc-signet",
  "lock_amount": "89600",
  "escrow_upfront_fee": "1000",
  "payout_fee_reserve": "1000",
  "timelock": {"t1": 1234, "t2": 3456},
  "shopper_btc_pubkey": "<33 byte hex>",
  "shopper_btc_address": "tb1q…",
  "escrow_btc_pubkey": "<xpub から導出した 33 byte hex>",
  "escrow_btc_fee_address": "tb1q…",
  "shopper_evm_address": "0x…",
  "escrow_evm_address": "0x…",
  "escrow_address": "tb1q…（P2WSH）または 0x…（Safe）"
}
```

- 金額の単位: BTC は sats（整数）、USDC は 6 桁の基本単位（整数）。`lock_amount` は多重署名に入れる額（payout の手数料の予備を含む）。
- 換算: `lock_amount = ceil((items + shipping + shopper_fee) / rate × 10^decimals) + payout_fee_reserve`（USDC の payout_fee_reserve は 0）。
- `timelock`: BTC はブロック高、USDC は UNIX 秒。既定 `t1 = 今 + (delivery_days + 21) 日`, `t2 = t1 + 14 日`（BTC は 600 秒 = 1 ブロックで換算）。
- **user 側の検証:**
  - レート: user は自分で設定した取得元で同じ pair を計算し、乖離 `|quote − own| / own` を出す。
    - 3% を超えたら注意を出す。
    - 10% を超えたら強い警告を出し、**利用者が明示的に確認するまで承諾できない**。
  - 組み合わせ: escrow の組み合わせが実効の一覧（§2.4）に無ければ受け付けない。
  - アドレス: `escrow_address` を自分で計算し直して一致を確かめる。
  - 次のどれかに当たる見積は **エラー** とし、承諾できない。
    - `lock_amount` が、見積の価格とレートから計算し直した値と一致しない。
    - `payout_fee_reserve` が上限を超える。BTC は `min(20000 sats, max(2000 sats, lock_amount の 5%))`（少額の注文でも 2000 sats までは認める）、USDC は 0。
    - `escrow_upfront_fee` が、escrow のプロフィールの `max(bps × lock, min)` の 2 倍を超える。
    - タイムロックが user の方針を満たさない（§4.5.1）。

#### 4.5.1 タイムロックの方針（user 側）

shopper は T1 以降に単独で受け取れるので、T1 が近すぎる見積は、買わずに持ち逃げする準備になる。
user のクライアントは、**今のブロック高 / チェーンの時刻** と比べて次を確かめる。

| 値 | 既定（公開網） | 意味 |
|---|---|---|
| `min_t1` | (`delivery_days` + 14) 日 | T1 は今からこれ以上先 |
| `min_gap` | 7 日 | T2 − T1 の下限 |
| `max_t2` | 120 日 | T2 は今からこれ以下 |

- BTC は 600 秒 = 1 ブロックで換算する。
- BTC の T1 / T2 は 500000000 未満（ブロック高）であること。
- 利用者はこの方針を設定で変えられる。lab の `config.json` は短い値を配る（`timelock_policy`）。

### 4.6 order.funded

```json
{"asset": "btc-signet", "txid": "…", "vout": 0, "amount": "89600", "fee_txid": "…（前払い手数料の tx, BTC は同じ tx）"}
{"asset": "usdc-evm", "safe": "0x…", "deploy_tx": "0x…", "fund_tx": "0x…", "fee_tx": "0x…", "amount": "…"}
```

shopper は 1 承認（既定）を確認してから購入する。加えて、次を確かめる。

- `order.accept` を受け取っていること、入金の承認時刻が `expires_at` + 猶予（既定 1 時間）より前であること。
- 購入を始める時点で、T1 までに `delivery_days` + 猶予が残っていること（残っていなければ協力的な払い戻しを申し出る）。
- **同じ出力（txid:vout）・同じ Safe・同じ前払い手数料の取引を、別の注文で使っていないこと。**
- 前払い手数料:
  - BTC は **同じ入金 tx** の中に escrow の `btc_fee_address` への出力があること（`fee_txid` は `txid` と同じ）。
  - USDC は `fee_tx` が `user_evm_address` から escrow の `evm_address` への USDC の transfer で、承認済みであること。
  - escrow も同じ確認をし、使い回された手数料の注文には裁定の義務を負わない。

### 4.7 dispute.open

```json
{
  "claim": "not_delivered|wrong_item|not_released|other",
  "text": "…",
  "requested_split": {"user": "…", "shopper": "…"},
  "evidence": {
    "messages": [<署名付き inner> …],
    "tracking": [TrackingStatus …],
    "purchase_evidence": [Evidence …],
    "delivery_key_for_escrow": "<order.escrow_key の key_for_escrow>"
  }
}
```

escrow は **すべての証拠**（店の注文番号・配送状況・双方の申告と署名付きメッセージ）を揃えてから裁定する。足りなければ `dispute.evidence_request` で求める。

**escrow が注文を組み立てる規則:**

- 注文の request・quote・accept・funded は、**`escrow.notice` で受け取ったもの**を使う。
- 証拠に入った同じ種類のメッセージが、それと食い違う（id が違う）ときは、証拠のほうを無視して記録に残す。
- notice が無い注文は、証拠から組み立てる。同じ注文 id に異なる request が 2 つ以上あれば、その紛争は受けない。
- user は request の署名者で、`key_proof`（§4.4.1）が検証できること。
- funded の出力（BTC の scriptPubKey）と Safe のアドレスが、request と quote から計算し直した P2WSH / Safe と一致すること。
  - Safe は、所有者・しきい値・モジュールの設定もチェーン上で確かめる。
- 前払い手数料が §4.6 を満たすこと（使い回しは不可）。

### 4.8 dispute.ruling

```json
{
  "split": {"user": "40000", "shopper": "46600", "escrow_fee": "2000"},
  "reason": "…",
  "asset": "btc-signet",
  "psbt": "<escrow が署名済みの PSBT>",
  "safe_tx": SafeTx, "signature": "0x…（usdc のとき）"
}
```

- `split` の合計 = 多重署名の残高 − payout_fee_reserve。
- `escrow_fee` ≤ `dispute_fee_bps` × （残高 − payout_fee_reserve）。
- **escrow は 1 件の紛争に 1 回だけ裁定する**（署名済みの tx は取り消せないので、2 回目は両立しない配分を生む）。
- どちらか一方の当事者が連署して放送する（2-of-3）。
- 当事者は、開いている紛争の無い注文への裁定と、`escrow_fee` が上限を超える裁定を連署しない。
- `dispute.countersigned` と `order.completed` を受けても、**チェーン上で多重署名の出力が使われたこと**（BTC は outspend、USDC は Safe の残高 0 と tx の receipt）を確かめるまで、状態を終わりにしない。

### 4.9 大きさの上限と添付

NIP-44 が暗号化できるのは 64 KiB までで、wrap の中の seal の中に inner が base64 で入るので、
**inner（署名付きの JSON 全体）は 30000 byte 以下**にする。超えるものは送る前に拒否する。

- `Evidence.data_b64` は、元のデータが 8 KiB 以下のときだけメッセージに入れる。それより大きいものは `sha256` と `mime` だけを載せる。
- 大きい証拠の本体（スクリーンショットなど）は、紛争のとき shopper が escrow へ `attachment` で分けて送る。
  `{"sha256", "mime", "index", "total", "data_b64"}`（1 通あたり元データ 12 KiB 以下。wrap の content は元の約 2.3 倍になり、多くのリレーは content を 65535 byte までに制限する）。
  受け手は全部そろったら結合して `sha256` を確かめる。リレーは順番を保たないので、証拠より先に届いた添付も保持する。
- `dispute.evidence` の `messages` が上限を超えるときは、複数の `dispute.evidence` に分けて送る（受け手は当事者ごとに追記する）。

| type | 送り手 → 受け手 | body |
|---|---|---|
| `attachment` | 当事者 → escrow | 上記 |

### 4.10 受け取ったメッセージの扱い

- body はスキーマで検証する（型・桁・文字列の長さ）。合わないものは捨てる。
- 相手が主張しただけの状態（完了・決着・取り消し）で、T2 の返金や紛争の操作を隠さない。
- 連署して自動で実行してよいのは、**決まった形の取引だけ**。
  - BTC:
    - 入力は funded の出力 1 つだけで、witnessScript が一致すること。
    - 出力は決まった宛先だけであること。
    - 手数料は `payout_fee_reserve` 以下であること。
  - USDC:
    - `operation 0` かつ `to = usdc` の `transfer`、または `operation 1` かつ `to = MultiSendCallOnly` で USDC の transfer だけを並べたもの。
    - `value = safeTxGas = baseGas = gasPrice = 0`、`gasToken = refundReceiver = 0`。
    - nonce が Safe の今の nonce であること。
    - 合計が Safe の残高であること。
- user のクライアントは、協力的な払い戻し（`order.refund`）を含め、**資金を動かす署名には利用者の操作を求める**。
- 受信箱のリレー（kind 10050）は先頭の 8 個までを使う。
- ack と返信は、相手の受信箱のうち k 個までに送る。
- 既定では、ループバック・リンクローカル・プライベートのアドレスのリレーには接続しない（lab は設定で許す）。
- shopper は `shop_url` についても同じ扱いをする（プライベートのアドレスには接続しない。lab は設定で許す）。
- 当事者でない相手（注文の無い相手）への返信は、再送しない。

## 5. BTC（signet, P2WSH）

### 5.1 witness script

鍵の順序は **U（user）, S（shopper）, E（escrow）** 固定（並べ替えない）。

```
OP_IF
  OP_2 <U> <S> <E> OP_3 OP_CHECKMULTISIG
OP_ELSE
  OP_IF
    <T1> OP_CHECKLOCKTIMEVERIFY OP_DROP <S> OP_CHECKSIG
  OP_ELSE
    <T2> OP_CHECKLOCKTIMEVERIFY OP_DROP <U> OP_CHECKSIG
  OP_ENDIF
OP_ENDIF
```

- T1, T2 はブロック高（< 500000000）を最小の CScriptNum で符号化。`T1 < T2`。
- アドレス = P2WSH（`tb1q…`, 32 byte の SHA-256）。

| 経路 | witness（下から上） | nLockTime / nSequence |
|---|---|---|
| 2-of-3 | `<> <sig1> <sig2> <0x01> <script>`（sig は鍵の順序 U,S,E に従う） | 0 / 0xffffffff |
| T1 以降 shopper だけ | `<sigS> <0x01> <> <script>` | T1 以上 / 0xfffffffe |
| T2 以降 user だけ | `<sigU> <> <> <script>` | T2 以上 / 0xfffffffe |

### 5.2 取引

- 入金（user が作る）: 出力 0 = P2WSH（`lock_amount`）、出力 1 = escrow の `btc_fee_address`（`escrow_upfront_fee`）、残りはおつり。
- 支払い（release）: 入力 = 多重署名の出力、出力 = shopper の `btc_address` に `lock_amount − payout_fee_reserve`（寄付を選んだときはそこから寄付の出力を分ける）。手数料 = `payout_fee_reserve`。
- 裁定: 出力 = user の返金先、shopper の受取先、escrow の `btc_fee_address`（0 の出力は作らない）。
- 部分署名の受け渡しは PSBT（BIP174, base64）。SIGHASH_ALL, BIP143。

## 6. USDC（EVM, Safe v1.4.1）

### 6.1 コントラクト

| 名前 | 内容 |
|---|---|
| `MockUSDC` | ERC-20, 6 桁, `mint(address,uint256)` は誰でも（lab 用） |
| Safe v1.4.1 | `SafeL2`（singleton）, `SafeProxyFactory`, `CompatibilityFallbackHandler`, `MultiSendCallOnly`。CREATE2 deployer `0x4e59b44847b379578588920ca78fbf26c0b4956c`、salt 0 で置く（決まったアドレス） |
| `PSEscrowModule` | タイムロックの Safe モジュール（1 つを全注文で共用） |
| `PSSafeSetup` | Safe の `setup` から delegatecall され、モジュールの有効化と登録をする |
| `MockAggregatorV3` | Chainlink AggregatorV3 形式の模擬オラクル（BTC/USD, JPY/USD, USDC/USD）。`setAnswer(int256)` |
| `examples/bond/PSBond` | 参考: operator と escrow の任意の規約。escrow が預け、operator が没収して補償に回す |

アドレスは `contracts/deployments/<chain_id>.json` と一覧の `chain.evm` に載る。

### 6.2 注文ごとの Safe

- owners = `[U, S, E]`（EVM アドレス）、threshold = 2。
- initializer = `Safe.setup(owners, 2, PSSafeSetup, abi.encodeCall(PSSafeSetup.setup, (module, usdc, U, S, t1, t2)), fallbackHandler, 0, 0, 0)`
- saltNonce = `uint256(keccak256(order_id の 16 byte))`
- アドレスは `SafeProxyFactory.createProxyWithNonce` の CREATE2 規則でオフラインに計算できる（`proxyCreationCode` と singleton から）。

### 6.3 PSEscrowModule

```solidity
function register(address token, address user, address shopper, uint64 t1, uint64 t2) external; // msg.sender = Safe, 一度だけ
function claimByShopper(address safe) external; // msg.sender == shopper && block.timestamp >= t1 → token 残高を全額 shopper へ
function refundToUser(address safe) external;   // msg.sender == user && block.timestamp >= t2 → 全額 user へ
function config(address safe) external view returns (address token, address user, address shopper, uint64 t1, uint64 t2);
```

### 6.4 SafeTx と署名

- release: `{to: usdc, value: 0, data: transfer(S, lock_amount), operation: 0, safeTxGas: 0, baseGas: 0, gasPrice: 0, gasToken: 0, refundReceiver: 0, nonce: 0}`
- 裁定: `to = MultiSendCallOnly`, `operation = 1`（delegatecall）, data = `multiSend(…)` で user / shopper / escrow への transfer を並べる。
- 署名: EIP-712（domain `{chainId, verifyingContract: safe}`）の 65 byte 署名（v = 27/28）。`execTransaction` には **署名者のアドレス昇順**で連結して渡す。
- 実行（ガス代）は受け取る側（release なら shopper、裁定なら連署した側）が払う。
- JSON の `SafeTx` はフィールド名を上記のまま、数値は 10 進文字列、`data` は 0x hex。

## 7. レート

- 取得元は差し替えられる（Go: `fx.Provider`、TS: `RateSource`）。
  - `frankfurter`: `GET {base}/latest?from=USD&to=JPY` → `{"rates":{"JPY":150.1}}`（法定通貨同士）
  - `coingecko`: `GET {base}/api/v3/simple/price?ids=bitcoin,usd-coin&vs_currencies=usd,jpy`
  - `chainlink`: AggregatorV3 の `latestRoundData()`（8 桁）。feed のアドレスは設定
  - `static`: 固定値（テスト用）
- pair は `BTC/JPY`, `BTC/USD`, `USDC/JPY`, `USDC/USD`。直接の値が無ければ USD を介して合成する。
- 複数の取得元があれば中央値を使う。
- e2e は外部に接続しない（`ratemock` が frankfurter / coingecko 互換の応答を返し、`ratefeed` が同じ値を模擬オラクルへ書く）。

## 8. 店の危険度（shopper の方針）

shopper ノードは見積の前に店を点数化する（0〜100）。既定のしきい値は 70。

| 規則 | 点 |
|---|---|
| 店のホストが shopper の許可リストにある | +50 |
| HTTPS で、証明書が検証できる | +20 |
| 決済画面（`<meta name="ps-payment-gateway" content="<host>">`）が既知のゲートウェイ | +30 |

- 現金のみの店（`<meta name="ps-payment" content="cash-only">` と `<meta name="ps-region" content="JP-13-13104">`）は、危険度の代わりに `cash_regions` で判定する。
- しきい値未満 → `reject_reason: "risk"`、地域外 → `"region"`。

## 9. shopper-bot の API（自動操作）

Playwright 版と将来の AI 版は同じ入出力を持つ。JSON Schema は `shopper-bot/schema/`。カード情報は bot の中だけに置く。

- `GET /v1/capabilities` → `{"version":"1","drivers":["safe-shop.test", …]}`
- `POST /v1/purchase` `PurchaseRequest` → `PurchaseResult`
  - **冪等:** bot は `request_id` ごとに結果を保存し、同じ `request_id` の要求には、店をもう一度操作せず保存した結果を返す。
  - 購入の途中なら `409 in_progress` を返す。ノードは再起動後や通信エラーのあと、同じ `request_id` で問い合わせ直す。
  - 4xx（要求の誤り）は再試行しない。
- `POST /v1/tracking` `TrackingQuery` → `TrackingStatus`

```ts
PurchaseRequest = {request_id, order_id, shop_url, items: [{sku, qty}], shipping: Address, payment_ref: "card:default" | "cash", max_amount: {amount, currency}}
PurchaseResult  = {request_id, status: "ok" | "failed" | "needs_human", shop_order_id?, total?: {amount, currency}, evidence: Evidence[], error?}
Evidence        = {kind: "screenshot" | "receipt" | "html" | "json", sha256, mime, data_b64?}
TrackingQuery   = {shop_url, shop_order_id}
TrackingStatus  = {status: "processing" | "shipped" | "delivered" | "failed", carrier?, tracking_no?, updated_at, evidence: Evidence[]}
```

## 10. libp2p（Go ノード同士）

- 鍵は §1 の libp2p の鍵。transport は TCP と WebSocket。AutoNAT, circuit relay v2（client）, DCUtR を有効にする。`-role relay` のノードは relay service を動かす。
- gossipsub の topic:
  - `/ps/<network>/trust/1`: kind 30500 / 30501 のイベント（JSON）
  - `/ps/<network>/profiles/1`: kind 30502 / 30503 / 10050
  受け取ったノードは検証して保存し、より新しい版なら自分の Nostr リレーにも転送する。
- **受け入れる範囲:** 保存・中継するのは、設定した coordinator から辿れる著者のイベントだけにする。
  - coordinator の委任書
  - 委任された operator の一覧
  - 実効の一覧に載った shopper / escrow のプロフィールと 10050
  - それ以外の 10050（注文の相手の user など）は、必要なときに取りに行き、保存数に上限を設け、中継しない。
- stream プロトコル:
  - `/ps/status/1.0.0`: 要求なし → `{"pubkey","role","network","reachability","trust":{"<operator pk>": v, …},"at"}` を身元鍵で署名した kind 5401 のイベント
  - `/ps/trust-sync/1.0.0`: 自分が持つ trust / profiles のイベントを全部送る。委任書 → 一覧 → プロフィールの順に送る（受け手が件数の上限で打ち切っても、信頼の根から先に届くように）

## 11. 流れ

```
user                     shopper                   escrow               operator
 │ order.request ──────▶ │ 危険度・地域・決済手段を判定
 │ ◀────── order.quote   │ レート取得・Safe/P2WSH アドレス計算
 │ レート検証・アドレス再計算
 │ order.accept ───────▶ │
 │ 入金（多重署名 + escrow 前払い手数料）
 │ order.funded ───────▶ │ 1 承認を確認
 │ escrow.notice ────────────────────────────────▶ │（記録）
 │                       │ shopper-bot で購入
 │ ◀──── order.purchased │
 │ ◀──── order.shipping  │ 追跡を定期取得
 │ order.release（部分署名）▶ │ 連署して放送
 │ ◀──── order.completed │
 ── 紛争 ──
 │ dispute.open ─────────────────────────────────▶ │ 証拠を集める
 │                       │ ◀── evidence_request ── │
 │                       │ dispute.evidence ─────▶ │（key_for_escrow を含む）
 │ ◀──────────────────────────────── dispute.ruling│（escrow 署名済み）
 │ 連署して放送（または shopper が）
 ── 不正な裁定 ──
 │ report ───────────────────────────────────────────────────────────▶ │ 一覧の新しい版から escrow を外す
 │                                                                     │ （規約次第で bond を没収して補償）
```

## 12. 既知の制約

- lab の `evm.test`（anvil の RPC をそのまま公開）と `faucet.test` は、誰でも時刻を進めたり残高を作ったりできる。lab 専用で、公開網で同じ構成にしてはならない。

- 3 者が一覧の外で直接取引することは検知できない。一覧が売っているのは「見つけてもらえること」。
- 紛争中に escrow が応答しないまま T1 を過ぎると、shopper が単独で受け取れる（shopper を先にしている）。escrow の義務（SLA）違反として operator に通報する。
- wrap の `created_at` をずらしていないので、リレーは送受の時刻を知る。
- リレーは暗号文を消せる（k 個に送ることで薄める）。
