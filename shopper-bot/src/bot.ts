import type { Server } from "node:http";
import { BrowserPool } from "./browser.js";
import { CardVault } from "./cards.js";
import type { Config } from "./config.js";
import type { DriverContext } from "./drivers/driver.js";
import { defaultRegistry } from "./drivers/index.js";
import type { DriverRegistry } from "./drivers/registry.js";
import { createHttpServer } from "./http.js";
import { BotService } from "./service.js";

export interface Bot {
  server: Server;
  close(): Promise<void>;
}

/** Wires config, browser, cards and drivers into a (not yet listening) server. */
export function createBot(config: Config, registry: DriverRegistry = defaultRegistry()): Bot {
  const pool = new BrowserPool({
    headless: config.headless,
    hostResolverRules: config.hostResolverRules,
    actionTimeoutMs: config.actionTimeoutMs,
  });
  const ctx: DriverContext = {
    withPage: (fn) => pool.withPage(config.purchaseTimeoutMs, fn),
    cards: CardVault.fromFile(config.cardsFile),
    origin: (shopUrl) => {
      const u = new URL(shopUrl);
      return `${config.shopSchemeOverride ?? u.protocol.replace(":", "")}://${u.host}`;
    },
    log: (message) => console.log(`[bot] ${message}`),
  };
  const server = createHttpServer(new BotService(registry, ctx));
  return {
    server,
    async close() {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      await pool.close();
    },
  };
}
