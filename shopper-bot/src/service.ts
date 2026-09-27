import type { BaseContext, DriverContext } from "./drivers/driver.js";
import { NotFoundError } from "./drivers/driver.js";
import type { DriverRegistry } from "./drivers/registry.js";
import { describeErrors, validate } from "./schema.js";
import { fingerprint, type PurchaseRecord, type PurchaseStore } from "./store.js";
import type { PurchaseRequest, PurchaseResult, TrackingStatus } from "./types.js";

/** An API answer: HTTP status and JSON body. */
export interface Reply {
  status: number;
  body: unknown;
}

const badRequest = (error: string): Reply => ({ status: 400, body: { error } });

/**
 * The transport-independent core of the API (spec §9): validates input,
 * picks the driver by shop host, and validates what the driver returns.
 *
 * Purchases are idempotent by request_id: the outcome is stored, and a
 * repeated request_id gets the stored result without touching the shop
 * again (409 {"status":"in_progress"} while the first call still runs;
 * 422 when the id is reused for a different request). A purchase that was
 * interrupted after the payment step (bot crash, restart) is never run
 * again: it is answered with needs_human.
 */
export class BotService {
  /** request_id → fingerprint of the purchases running in this process. */
  private readonly running = new Map<string, string>();
  /** Results we could not persist; still answered while the process lives. */
  private readonly unsaved = new Map<string, PurchaseRecord>();

  constructor(
    private readonly registry: DriverRegistry,
    private readonly ctx: BaseContext,
    private readonly store: PurchaseStore,
  ) {}

  capabilities(): Reply {
    return { status: 200, body: { version: "1", drivers: this.registry.hosts() } };
  }

  async purchase(input: unknown): Promise<Reply> {
    if (!validate.purchaseRequest(input)) {
      return badRequest(`invalid PurchaseRequest: ${describeErrors(validate.purchaseRequest.errors)}`);
    }
    const req = input;
    const fp = fingerprint(req);
    const conflict = (): Reply => ({
      status: 422,
      body: { error: `request_id ${req.request_id} was already used for a different request` },
    });

    // Checked and claimed synchronously, so two concurrent calls cannot both start.
    const runningFp = this.running.get(req.request_id);
    if (runningFp !== undefined) {
      return runningFp === fp ? { status: 409, body: { status: "in_progress", request_id: req.request_id } } : conflict();
    }
    const rec = this.unsaved.get(req.request_id) ?? this.store.get(req.request_id);
    if (rec && rec.fingerprint !== fp) return conflict();
    if (rec?.state === "done" && rec.result) {
      this.ctx.log(`${req.request_id}: answering the stored ${rec.result.status} result`);
      return { status: 200, body: rec.result };
    }
    if (rec?.state === "payment_submitted") {
      // A previous run got as far as paying and never finished (the bot stopped).
      const result: PurchaseResult = {
        request_id: req.request_id,
        status: "needs_human",
        error: "the bot stopped after submitting payment; check the shop before retrying",
        evidence: [],
      };
      this.finish(rec, result);
      return { status: 200, body: result };
    }
    // No record, or "started" (stopped before any payment): safe to run.
    this.running.set(req.request_id, fp);
    try {
      return await this.run(req, fp, rec);
    } finally {
      this.running.delete(req.request_id);
    }
  }

  private async run(req: PurchaseRequest, fp: string, previous: PurchaseRecord | undefined): Promise<Reply> {
    const now = () => Math.floor(Date.now() / 1000);
    const rec: PurchaseRecord = { request_id: req.request_id, fingerprint: fp, state: "started", started_at: previous?.started_at ?? now(), updated_at: now() };
    try {
      this.store.put(rec);
    } catch (err) {
      // Without the marker a crash could lead to a second purchase, so do not start.
      return { status: 500, body: { error: `cannot record the purchase: ${(err as Error).message}` } };
    }
    if (previous) this.ctx.log(`${req.request_id}: resuming a purchase that stopped before payment`);

    const driver = this.registry.forUrl(req.shop_url);
    let result: PurchaseResult;
    if (!driver) {
      result = { request_id: req.request_id, status: "needs_human", error: `no driver for ${new URL(req.shop_url).hostname}`, evidence: [] };
    } else {
      this.ctx.log(`${req.request_id}: purchase at ${req.shop_url} with ${driver.name}`);
      const ctx: DriverContext = {
        ...this.ctx,
        beforePayment: async () => {
          rec.state = "payment_submitted";
          rec.updated_at = now();
          this.store.put(rec);
        },
      };
      try {
        result = await driver.purchase(req, ctx);
      } catch (err) {
        // We do not know how far the driver got, so a person must check.
        result = { request_id: req.request_id, status: "needs_human", error: (err as Error).message, evidence: [] };
      }
    }
    this.ctx.log(`${req.request_id}: ${result.status}${result.error ? ` (${result.error})` : ""}`);
    if (!validate.purchaseResult(result)) {
      const error = `driver returned an invalid PurchaseResult: ${describeErrors(validate.purchaseResult.errors)}`;
      this.ctx.log(error);
      if (rec.state === "payment_submitted") {
        // never let a retry pay again
        this.finish(rec, { request_id: req.request_id, status: "needs_human", error, evidence: [] });
      }
      return { status: 500, body: { error } };
    }
    this.finish(rec, result);
    return { status: 200, body: result };
  }

  /** Stores the final answer of a request_id. */
  private finish(rec: PurchaseRecord, result: PurchaseResult): void {
    const done: PurchaseRecord = { ...rec, state: "done", updated_at: Math.floor(Date.now() / 1000), result };
    try {
      this.store.put(done);
      this.unsaved.delete(rec.request_id);
    } catch (err) {
      this.ctx.log(`${rec.request_id}: cannot store the result: ${(err as Error).message}`);
      this.unsaved.set(rec.request_id, done);
    }
  }

  async tracking(input: unknown): Promise<Reply> {
    if (!validate.trackingQuery(input)) {
      return badRequest(`invalid TrackingQuery: ${describeErrors(validate.trackingQuery.errors)}`);
    }
    const driver = this.registry.forUrl(input.shop_url);
    if (!driver) return { status: 404, body: { error: `no driver for ${new URL(input.shop_url).hostname}` } };
    let status: TrackingStatus;
    try {
      status = await driver.tracking(input, { ...this.ctx, beforePayment: () => Promise.reject(new Error("tracking never pays")) });
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
