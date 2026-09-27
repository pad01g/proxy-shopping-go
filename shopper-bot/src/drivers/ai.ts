import type { PurchaseRequest, PurchaseResult, TrackingQuery, TrackingStatus } from "../types.js";
import type { Driver, DriverContext } from "./driver.js";

/**
 * Placeholder for a model-driven driver that can shop at sites nobody has
 * scripted. It is not registered by default.
 *
 * Whatever it does internally, it must honour the same contract as the
 * Playwright drivers:
 *   - take a PurchaseRequest, return a PurchaseResult that passes
 *     schema/PurchaseResult.json (the server checks);
 *   - get card data only from ctx.cards.resolve(req.payment_ref);
 *   - refuse to pay when the shop's total exceeds req.max_amount;
 *   - await ctx.beforePayment() right before any step that can move money;
 *   - return "needs_human" whenever it cannot tell whether it has paid;
 *   - attach a screenshot and a receipt as evidence.
 */
export class AIDriver implements Driver {
  readonly name = "ai";

  constructor(readonly hosts: readonly string[]) {}

  purchase(_req: PurchaseRequest, _ctx: DriverContext): Promise<PurchaseResult> {
    return Promise.reject(new Error("not implemented"));
  }

  tracking(_q: TrackingQuery, _ctx: DriverContext): Promise<TrackingStatus> {
    return Promise.reject(new Error("not implemented"));
  }
}
