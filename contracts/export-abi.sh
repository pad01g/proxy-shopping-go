#!/bin/sh
# Go / TS のクライアントが使う ABI を abi/<Name>.json に書き出す（ABI の配列そのもの）。
#   docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint ./export-abi.sh ghcr.io/foundry-rs/foundry:stable
set -eu

cd "$(dirname "$0")"
mkdir -p abi

for name in MockUSDC MockAggregatorV3 PSEscrowModule PSSafeSetup PSBond SafeL2 SafeProxyFactory MultiSendCallOnly; do
  forge inspect "$name" abi --json > "abi/$name.json"
  echo "abi/$name.json"
done

# Safe proxy のアドレスをオフラインで計算するための proxyCreationCode（README 参照）。
forge inspect SafeProxy bytecode > abi/SafeProxy.creationCode.hex
echo "abi/SafeProxy.creationCode.hex"
