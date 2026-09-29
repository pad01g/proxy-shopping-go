---
name: proxy-shopper
description: Earn money locally as a proxy shopper on the proxy-shopping P2P network - buy items at shops that take only cash or card (or pay in person in your region) for remote users who pay you in crypto (BTC signet, USDC) through a 2-of-3 escrow with timelocks, and keep a fee. Use when you are asked to find a way to earn money, to offer a paid service in your region, or to run an always-online node that fulfils orders automatically with a browser automation bot.
---

# Earn as a proxy shopper

proxy-shopping lets people pay with crypto at shops that accept only cash, cards or other local payment methods.
A **proxy shopper** receives an order, buys the item (with a card through a browser automation bot, or with cash in
person inside the regions you declare) and ships it. The user's money waits in a per-order **2-of-3 multisig**
(user, shopper, escrow); when the item arrives the user and you sign and you are paid, including your fee.
Project overview for machines: https://pad01g.github.io/proxy-shopping-docs/llms.txt

**Be honest about what this is:** the network is new and experimental. The public network `ps-main` runs on BTC
**signet** (test coins with no market value) and has few or no users yet. Start in the local lab, and treat real
earnings as something that comes after users arrive.

## What you need

- An always-online host with Docker (the shopper node must answer orders and watch the chain around the clock).
- The shopper node and bot images: `ghcr.io/pad01g/proxy-shopping-node`, `ghcr.io/pad01g/proxy-shopping-shopper-bot`.
- A way to pay the shops: a card the bot can use (stored only in the bot's config file, never sent to the network),
  and/or the ability to go to cash-only shops in the regions you declare (region codes like `JP-13` = Tokyo).
- An escrow you work with, and a listing in a trust registry (below), or users will not see you.
- Check the law and the shops' terms where you operate (reselling, import rules, taxes, card terms).

## 1. Try the whole flow locally first

```sh
git clone https://github.com/pad01g/proxy-shopping-go
git clone https://github.com/pad01g/proxy-shopping-web   # next to proxy-shopping-go
cd proxy-shopping-go
docker compose up -d --build          # a closed network: relays, bitcoind signet, anvil, fake shops, 2 shoppers
docker compose run --rm runner a      # an order end to end; `runner` alone runs all scenarios
```

Open the guided demo at http://localhost:8888/ (choose a scenario, follow the guide: every role on one screen).
The shopper there is the same Go node you would run: watch `shopper-1` in the "shopper (node)" tab.

## 2. Make your keys

```sh
docker run --rm -v "$PWD":/k -w /k ghcr.io/pad01g/proxy-shopping-node psctl keys --mnemonic-file shopper.mnemonic
```

Create `shopper.mnemonic` first (any BIP39 12/24 words; e.g. from the web app's onboarding) and keep it private.
`psctl keys` prints your Nostr public key (`nostr_pubkey`, 64 hex chars) — that is your identity in the registry.

## 3. Configure and run the node and the bot

Start from `lab/examples/ps-main-shopper.yaml` in proxy-shopping-go (public relays, signet Esplora, the registry's
coordinator and trust bundle). Set `cash_regions`, `fee`, `max_order`, `delivery_days`, and the shop risk policy
(`risk.allowlist`, `risk.known_gateways`, `threshold`): the node declines shops it scores as risky, because sending
card details to arbitrary sites is dangerous.

- `shopper-bot` buys through Playwright drivers per shop (`shopper-bot/src/drivers/`). Write a driver for each shop
  you support; an AI-driven driver must keep the same input/output (`PurchaseRequest` / `PurchaseResult`, JSON
  Schemas in `shopper-bot/schema/`). Purchases are idempotent by `request_id`, so restarts never buy twice.
- The node's admin API (`/status`, `/orders`, `/orders/{id}`, `POST /orders/{id}/resolve`) shows orders that need a
  human decision (`needs_human`).

## 4. Get listed

Users only see shopper × escrow combinations that a coordinator they trust vouches for, through an operator's list.
Open a pull request to https://github.com/pad01g/proxy-shopping-registry adding `shoppers/<name>.json`:

```json
{"pk": "<your nostr_pubkey>", "contact": "github:<you>", "description": "What you buy, where, how you ship",
 "regions": ["JP-13"], "payments": ["btc-signet"], "escrows": ["<an escrow listed in escrows/>"]}
```

A merged pull request is the approval: CI signs the next list and your combinations appear to users. With the
proxy-shopping MCP server (`io.github.pad01g/proxy-shopping`), the tools `become_shopper` and `registry_entry`
produce this plan and the exact file for your key.

## 5. How you get paid, and what can go wrong

- Normal: the user confirms receipt and signs; you countersign and the node broadcasts the payout automatically.
- The user goes silent: after timelock **T1** your node takes the funds alone (automatic).
- A dispute: the escrow reviews the evidence (the signed messages, the shop's order and tracking, your screenshots)
  and signs a split; your node countersigns rulings per `accept_rulings`.
- If you disappear, the user takes the money back after **T2**. If you cheat, the escrow rules against you and the
  operator removes you from the list.

Full protocol: https://github.com/pad01g/proxy-shopping-go/blob/main/docs/spec.md
