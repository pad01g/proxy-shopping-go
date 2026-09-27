#!/bin/sh
# コントラクトを CREATE2 deployer から salt 0 で置き、アドレスを JSON に書く。
# 既に置いてあれば置き直さず、JSON だけ書き直す。
#
#   RPC_URL      EVM の RPC（既定 http://localhost:8545）
#   OUT          JSON の出力先（既定 deployments/<chain_id>.json のみ）
#   PRIVATE_KEY  送信に使う鍵（既定 anvil の account 0）
set -eu

cd "$(dirname "$0")"

RPC_URL="${RPC_URL:-http://localhost:8545}"
# anvil の既定の account 0（lab 専用）
PRIVATE_KEY="${PRIVATE_KEY:-0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80}"

echo "waiting for $RPC_URL"
i=0
until chain_id="$(cast chain-id --rpc-url "$RPC_URL" 2>/dev/null)"; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "RPC not reachable: $RPC_URL" >&2
    exit 1
  fi
  sleep 1
done

forge script script/Deploy.s.sol:Deploy \
  --rpc-url "$RPC_URL" \
  --private-key "$PRIVATE_KEY" \
  --broadcast \
  --slow

written="deployments/${chain_id}.json"
if [ -n "${OUT:-}" ] && [ "$OUT" != "$written" ]; then
  mkdir -p "$(dirname "$OUT")"
  cp "$written" "$OUT"
  echo "copied to $OUT"
fi
cat "$written"
