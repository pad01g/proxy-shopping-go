#!/bin/sh
# runner は NAT の内側（home）にいる。外へは router 経由でしか出られない（docs/lab.md）。
#   runner [シナリオ id …]          プロトコルの e2e（a〜k）
#   runner demo [シナリオ id …]     デモ画面の e2e（src/demo, http://demo）
set -e
ip route replace default via 172.41.0.254
if [ "$1" = "demo" ]; then
  shift
  exec npx tsx src/demo/main.ts "$@"
fi
exec npx tsx src/main.ts "$@"
