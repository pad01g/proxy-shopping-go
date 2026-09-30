---
name: proxy-shopping-buyer
description: Buy from shops that accept only cash, cards or other payment methods you cannot use, by paying in crypto (BTC signet, USDC) to a proxy shopper through a 2-of-3 escrow with timelocks on the proxy-shopping P2P network. Use when a purchase is blocked by the shop's payment methods, when you must pay with crypto, or when you want to try escrowed peer-to-peer purchasing with MCP tools.
---

# Buy through a proxy shopper

A **proxy shopper** buys the item for you at the shop and ships it; you pay in crypto. Your money waits in a
per-order **2-of-3 multisig** (you, the shopper, an escrow):

- item arrives → you and the shopper sign → the shopper is paid;
- dispute → the escrow decides the split with one of you;
- the shopper disappears → after timelock **T2** you take the money back alone.

Which shoppers and escrows you see depends on the **coordinators** you trust (they delegate to operators, who list
shopper × escrow combinations per region). Overview for machines: https://pad01g.github.io/proxy-shopping-docs/llms.txt

**Honest status:** the public network `ps-main` is new, runs on BTC **signet** (test coins) and may have no shoppers
yet. The local lab (below) always has shoppers and fake shops to practise on.

## 1. Connect the MCP server

```sh
claude mcp add proxy-shopping -- docker run -i --rm -v proxy-shopping-mcp:/data ghcr.io/pad01g/proxy-shopping-mcp:0.1.1
```

Registry name `io.github.pad01g/proxy-shopping`. The server keeps its own key and order state in `/data` (mount a
volume). Environment: `PS_NETWORK=ps-main` (default) or `lab` (the local docker compose lab through its demo server;
from Docker set `PS_LAB_URL=http://host.docker.internal:8888`).

## 2. Order

| Step | Tool |
|---|---|
| See the network and trusted combinations | `network_info` |
| Your addresses and balances (lab: `lab_faucet`) | `wallet` |
| Shoppers × escrows for a shop and region | `find_offers` `{shop_url, region, payment}` |
| Ask for a quote (the address is encrypted for the shopper; the escrow can read it only in a dispute) | `request_quote` |
| Check it: rate deviation vs your own sources (>3 % caution, >10 % needs `acknowledge_rate_deviation`), the recomputed escrow address, timelocks far enough ahead | returned by `request_quote` / `get_order` |
| Accept, then fund the multisig and the escrow's upfront fee | `accept_quote`, `fund_order {confirm: true}` |
| Item arrived → pay | `confirm_receipt {confirm: true}` |
| Not arrived / wrong item | `open_dispute`, later `review_ruling`, `countersign_ruling {confirm: true}` |
| Shopper offers a refund | `accept_refund_offer {confirm: true}` |
| Shopper vanished and T2 passed | `refund_after_timelock {confirm: true}` |
| Report a cheating shopper or escrow to the operator | `report` |

Tools that move money or sign do nothing without `confirm: true`; without it they describe what would happen.
Ask your human before confirming real payments.

## 3. Practise in the local lab

```sh
git clone https://github.com/pad01g/proxy-shopping-go && git clone https://github.com/pad01g/proxy-shopping-web
cd proxy-shopping-go && docker compose up -d --build
```

Then use the MCP server with `PS_NETWORK=lab`, or open the guided demo at http://localhost:8888/ where every role
(user, escrow, operator, coordinator, and the shopper node) is on one screen. Lab shops: `https://safe-shop.test/`
(JPY, items `A-100`, `FAIL-100` fails delivery, `SOLDOUT-100` sold out), `https://us-shop.test/` (USD, `U-100`),
`https://cash-store.test/` (cash only, region `JP-13-13104`); region codes are prefix matched (`JP` > `JP-13` >
`JP-13-13104`).

Full protocol: https://github.com/pad01g/proxy-shopping-go/blob/main/docs/spec.md
