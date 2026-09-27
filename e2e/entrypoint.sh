#!/bin/sh
# runner は NAT の内側（home）にいる。外へは router 経由でしか出られない（docs/lab.md）。
set -e
ip route replace default via 172.41.0.254
exec npx tsx src/main.ts "$@"
