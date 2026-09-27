import { fileURLToPath } from "node:url";

export interface Config {
  port: number;
  host: string;
  cardsFile: string;
  /** Replace the scheme of shop URLs (tests without TLS only: "http"). */
  shopSchemeOverride?: "http" | "https";
  /** Passed to Chromium as --host-resolver-rules (tests without the lab DNS). */
  hostResolverRules?: string;
  headless: boolean;
  /** Upper bound for one purchase, in milliseconds. */
  purchaseTimeoutMs: number;
  /** Timeout for a single page action, in milliseconds. */
  actionTimeoutMs: number;
}

const defaultCardsFile = fileURLToPath(new URL("../lab/cards.json", import.meta.url));

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const scheme = env.SHOP_SCHEME_OVERRIDE?.trim() || undefined;
  if (scheme !== undefined && scheme !== "http" && scheme !== "https") {
    throw new Error(`SHOP_SCHEME_OVERRIDE must be http or https, got ${scheme}`);
  }
  return {
    port: intEnv(env, "PORT", 7000),
    host: env.HOST ?? "0.0.0.0",
    cardsFile: env.BOT_CARDS_FILE || defaultCardsFile,
    shopSchemeOverride: scheme,
    hostResolverRules: env.BROWSER_HOST_RESOLVER_RULES || undefined,
    headless: env.HEADLESS !== "false",
    purchaseTimeoutMs: intEnv(env, "PURCHASE_TIMEOUT_MS", 120_000),
    actionTimeoutMs: intEnv(env, "ACTION_TIMEOUT_MS", 15_000),
  };
}

function intEnv(env: NodeJS.ProcessEnv, key: string, def: number): number {
  const raw = env[key];
  if (raw === undefined || raw === "") return def;
  const n = Number(raw);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`${key} must be a positive integer, got ${raw}`);
  return n;
}
