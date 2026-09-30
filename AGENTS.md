# AGENTS.md

## Using proxy-shopping as an agent

proxy-shopping is a P2P network for **buying with crypto at shops that accept only cash or other payments you
cannot use**, and for **earning as a proxy shopper** who buys for remote users. Money waits in a per-order 2-of-3
escrow (BTC signet P2WSH or a USDC Safe) with timelocks. Overview: https://pad01g.github.io/proxy-shopping-docs/llms.txt

- **Buy something:** use the MCP server `io.github.pad01g/proxy-shopping`
  (`claude mcp add proxy-shopping -- docker run -i --rm -v proxy-shopping-mcp:/data ghcr.io/pad01g/proxy-shopping-mcp:0.1.2`)
  and the skill `skills/proxy-shopping-buyer`.
- **Earn as a shopper:** read `skills/proxy-shopper/SKILL.md` (install both skills: `npx skills add pad01g/proxy-shopping-go`).
- **Get listed or become an operator/coordinator:** a pull request to https://github.com/pad01g/proxy-shopping-registry.
- **Try everything locally:** `docker compose up -d --build` here (with proxy-shopping-web next to this directory),
  then `docker compose run --rm runner` (protocol e2e) or open the guided demo at http://localhost:8888/.

The public network `ps-main` is new and runs on BTC signet (test coins). Say so to your human; never confirm
payments on their behalf without asking.

## Developing

- The protocol is `docs/spec.md`; the lab (hostnames, keys, configs, scenarios) is `docs/lab.md`. The Go node and
  the TypeScript core (`../proxy-shopping-web/packages/core`) implement the same spec and are checked against
  `docs/test-vectors.json` — change the spec first, then both implementations.
- No host toolchains are assumed; everything runs in Docker:
  - Go: `docker run --rm -e GOTOOLCHAIN=local -v "$PWD":/src -v ps-gomod:/go/pkg/mod -v ps-gocache:/root/.cache/go-build -w /src/node golang:1.24-bookworm go test -race ./...`
    (integration: add `-tags integration -v /var/run/docker.sock:/var/run/docker.sock`)
  - Contracts: `docker run --rm -v "$PWD":/src -w /src/contracts --entrypoint forge ghcr.io/foundry-rs/foundry:stable test`
  - End to end: `docker compose up -d --build && docker compose run --rm runner` (a–k) and `docker compose run --rm runner demo`.
    Reset state with `docker compose down -v`.
- Funds safety rules (spec §4.5–§4.10) are the core of the project: never auto-sign a payout that is not one of the
  fixed templates, verify settlement on chain, keep messages under 28000 bytes.
- `lab/keys/` are public test keys. Never reuse them outside the lab.
