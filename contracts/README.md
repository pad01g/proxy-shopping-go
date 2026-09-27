# contracts

spec §6 の EVM 側（Foundry）。Safe v1.4.1 は `lib/safe-smart-account`（タグ `v1.4.1`, commit `bf943f8`）を、
forge-std は `lib/forge-std`（`v1.9.7`）をそのまま置いている（submodule ではなく、`contracts/` 以外は削った）。

| ファイル | 内容 |
|---|---|
| `src/mocks/MockUSDC.sol` | ERC-20, 6 桁, `mint` は誰でも |
| `src/mocks/MockAggregatorV3.sol` | AggregatorV3 形式, 8 桁, `setAnswer(int256)` で次のラウンドを作る |
| `src/PSEscrowModule.sol` | タイムロックの Safe モジュール（全注文で共用） |
| `src/PSSafeSetup.sol` | `Safe.setup` から delegatecall され、モジュールの有効化と登録をする |
| `examples/bond/PSBond.sol` | 参考: escrow の USDC 預け金。operator が没収できる。引き出しは申請の 7 日後 |
| `script/Deploy.s.sol` | すべてを CREATE2 deployer `0x4e59…956c` から salt 0 で置き、`deployments/<chain_id>.json` を書く |
| `abi/` | クライアント用の ABI（`./export-abi.sh` で作り直す）と `SafeProxy.creationCode.hex` |

## コマンド

```sh
# リポジトリの根で
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test
docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint ./export-abi.sh ghcr.io/foundry-rs/foundry:stable

# 置く（既に置いてあれば JSON だけ書き直す）
RPC_URL=http://localhost:8545 OUT=/path/to/31337.json ./deploy.sh
# または lab の deployer イメージ
docker build -t ps-contracts contracts
docker run --rm --network <net> -e RPC_URL=http://anvil:8545 -v <dir>:/deployments ps-contracts
```

コンパイラは solc 0.8.28、メタデータのハッシュは埋め込まない（`bytecode_hash = "none"`）。
そのため、どこでビルドしても同じバイトコード・同じアドレスになる。

## 注文ごとの Safe のアドレス（オフライン計算）

Go / TS のクライアントは、Safe を作る前にアドレスを自分で計算して一致を確かめる（spec §4.5, §6.2）。

```
owners      = [U, S, E]            # この順。並べ替えない
setupData   = abi.encodeCall(PSSafeSetup.setup, (module, usdc, U, S, t1, t2))
initializer = abi.encodeCall(Safe.setup, (owners, 2, setup, setupData, fallback_handler, 0, 0, 0))
saltNonce   = uint256(keccak256(order_id の 16 byte))

salt     = keccak256(keccak256(initializer) ++ uint256(saltNonce))     # 32 byte ++ 32 byte
initCode = proxyCreationCode ++ uint256(uint160(singleton))            # singleton を 32 byte に左詰め 0 埋め
safe     = keccak256(0xff ++ factory ++ salt ++ keccak256(initCode))[12:]
```

- `proxyCreationCode` は `SafeProxyFactory.proxyCreationCode()` の返り値。
  チェーンに問い合わせずに済むよう `abi/SafeProxy.creationCode.hex` に同じものを置いている。
- `factory`, `singleton`, `setup`, `module`, `fallback_handler`, `usdc` は `deployments/31337.json` の値。
- 作るときは `SafeProxyFactory.createProxyWithNonce(singleton, initializer, saltNonce)`。
- テスト `test/PSSafeFlow.t.sol` の `_predictSafe` が同じ計算で、実際に作られたアドレスと一致することを確かめている。

## SafeTx の署名

- EIP-712, domain = `{chainId, verifyingContract: safe}`。`Safe.getTransactionHash(...)` と同じハッシュに署名する。
- 65 byte（`r ++ s ++ v`, v = 27/28）を **署名者のアドレス昇順** に連結して `execTransaction` に渡す。
- 裁定は `to = multisend_call_only`, `operation = 1`。`multiSend` の各要素は
  `uint8(0) ++ to(20 byte) ++ uint256(0) ++ uint256(len(data)) ++ data`。

## タイムロック

- `claimByShopper(safe)`: shopper が `t1` 以降に呼ぶと、Safe の token 残高を全額 shopper へ送る。
- `refundToUser(safe)`: user が `t2` 以降に呼ぶと、全額 user へ送る。
- `register` は Safe 自身が setup の中で一度だけ呼ぶ（`t1 < t2` でなければ失敗）。
