import type { DriverContext } from "./drivers/driver.js";
import { NotFoundError } from "./drivers/driver.js";
import type { DriverRegistry } from "./drivers/registry.js";
import { describeErrors, validate } from "./schema.js";
import type { PurchaseResult, TrackingStatus } from "./types.js";

/** An API answer: HTTP status and JSON body. */
export interface Reply {
  status: number;
  body: unknown;
}

const badRequest = (error: string): Reply => ({ status: 400, body: { error } });

/**
 * The transport-independent core of the API (spec §9): validates input,
 * picks the driver by shop host, and validates what the driver returns.
 */
export class BotService {
  constructor(
    private readonly registry: DriverRegistry,
    private readonly ctx: DriverContext,
  ) {}

  capabilities(): Reply {
    return { status: 200, body: { version: "1", drivers: this.registry.hosts() } };
  }

  async purchase(input: unknown): Promise<Reply> {
    if (!validate.purchaseRequest(input)) {
      return badRequest(`invalid PurchaseRequest: ${describeErrors(validate.purchaseRequest.errors)}`);
    }
    const req = input;
    const driver = this.registry.forUrl(req.shop_url);
    let result: PurchaseResult;
    if (!driver) {
      result = { request_id: req.request_id, status: "needs_human", error: `no driver for ${new URL(req.shop_url).hostname}`, evidence: [] };
    } else {
      this.ctx.log(`${req.request_id}: purchase at ${req.shop_url} with ${driver.name}`);
      try {
        result = await driver.purchase(req, this.ctx);
      } catch (err) {
        // We do not know how far the driver got, so a person must check.
        result = { request_id: req.request_id, status: "needs_human", error: (err as Error).message, evidence: [] };
      }
    }
    this.ctx.log(`${req.request_id}: ${result.status}${result.error ? ` (${result.error})` : ""}`);
    return this.checked(result, validate.purchaseResult, "PurchaseResult");
  }

  async tracking(input: unknown): Promise<Reply> {
    if (!validate.trackingQuery(input)) {
      return badRequest(`invalid TrackingQuery: ${describeErrors(validate.trackingQuery.errors)}`);
    }
    const driver = this.registry.forUrl(input.shop_url);
    if (!driver) return { status: 404, body: { error: `no driver for ${new URL(input.shop_url).hostname}` } };
    let status: TrackingStatus;
    try {
      status = await driver.tracking(input, this.ctx);
    } catch (err) {
      const code = err instanceof NotFoundError ? 404 : 502;
      return { status: code, body: { error: (err as Error).message } };
    }
    return this.checked(status, validate.trackingStatus, "TrackingStatus");
  }

  /** A driver bug must not leak malformed data to the shopper node. */
  private checked(body: unknown, v: { (x: unknown): boolean; errors?: Parameters<typeof describeErrors>[0] }, name: string): Reply {
    if (!v(body)) {
      const error = `driver returned an invalid ${name}: ${describeErrors(v.errors)}`;
      this.ctx.log(error);
      return { status: 500, body: { error } };
    }
    return { status: 200, body };
  }
}
