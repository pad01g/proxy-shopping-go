import type { Page } from "playwright";
import type { CardVault } from "../cards.js";
import type { PurchaseRequest, PurchaseResult, TrackingQuery, TrackingStatus } from "../types.js";

/**
 * What a driver gets from the bot. Drivers never see configuration they do
 * not need, and card data only through `cards`.
 */
export interface DriverContext {
  /** Runs fn on a fresh, isolated browser page. */
  withPage<T>(fn: (page: Page) => Promise<T>): Promise<T>;
  cards: CardVault;
  /** Scheme + host to use for a shop URL (applies SHOP_SCHEME_OVERRIDE). */
  origin(shopUrl: string): string;
  log(message: string): void;
}

/**
 * A way to shop at one or more hosts. The Playwright drivers follow scripted
 * selectors; a future AI driver (see ai.ts) takes the same inputs and must
 * return the same results, so the server does not care which one runs.
 *
 * purchase() reports expected outcomes (declined card, over max_amount, …)
 * in the result. It may throw; the server then answers "needs_human", since
 * it cannot know whether money has already left.
 */
export interface Driver {
  /** For logs and /v1/capabilities, e.g. "playwright:card-shop". */
  readonly name: string;
  readonly hosts: readonly string[];
  purchase(req: PurchaseRequest, ctx: DriverContext): Promise<PurchaseResult>;
  tracking(q: TrackingQuery, ctx: DriverContext): Promise<TrackingStatus>;
}

/** The shop does not know the order (answered as HTTP 404). */
export class NotFoundError extends Error {}

export function hostOf(shopUrl: string): string {
  return new URL(shopUrl).hostname.toLowerCase();
}
