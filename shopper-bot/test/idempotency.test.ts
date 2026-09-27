import { mkdtempSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { CardVault } from "../src/cards.js";
import type { BaseContext, Driver } from "../src/drivers/driver.js";
import { DriverRegistry } from "../src/drivers/registry.js";
import { BotService } from "../src/service.js";
import { PurchaseStore } from "../src/store.js";
import type { PurchaseResult } from "../src/types.js";
import { purchaseRequest } from "./fixtures.js";

const ctx: BaseContext = {
  withPage: () => Promise.reject(new Error("no browser in unit tests")),
  cards: CardVault.fromObject({ cards: { default: { number: "4242424242424242", exp: "12/30", cvc: "123", name: "X" } } }),
  origin: (u) => new URL(u).origin,
  log: () => {},
};

const okResult = (id: string, n: number): PurchaseResult => ({
  request_id: id,
  status: "ok",
  shop_order_id: `SS-${n}`,
  total: { amount: "4000", currency: "JPY" },
  evidence: [],
});

/** A driver counting its purchases; `pay` controls what happens around the payment step. */
function countingDriver(pay: (n: number, beforePayment: () => Promise<void>, id: string) => Promise<PurchaseResult>): { calls: number; driver: Driver } {
  const d: { calls: number; driver: Driver } = {
    calls: 0,
    driver: {
      name: "counting",
      hosts: ["safe-shop.test"],
      purchase: (req, c) => pay(++d.calls, c.beforePayment, req.request_id),
      tracking: () => Promise.reject(new Error("unused")),
    },
  };
  return d;
}

const tmp = () => mkdtempSync(join(tmpdir(), "bot-idem-"));
const service = (driver: Driver, dir: string) => new BotService(new DriverRegistry().register(driver), ctx, new PurchaseStore(dir));

describe("purchase idempotency (spec §9)", () => {
  it("answers a repeated request_id with the stored result, also after a restart", async () => {
    const dir = tmp();
    const d = countingDriver(async (n, before, id) => (await before(), okResult(id, n)));
    const first = await service(d.driver, dir).purchase(purchaseRequest());
    expect(first).toMatchObject({ status: 200, body: { status: "ok", shop_order_id: "SS-1" } });

    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toEqual(first.body);
    expect(d.calls).toBe(1);
    expect(readdirSync(join(dir, "purchases")).filter((f) => f.endsWith(".tmp"))).toEqual([]);

    // another request id is a new purchase
    expect((await service(d.driver, dir).purchase(purchaseRequest({ request_id: "req-2" }))).body).toMatchObject({ shop_order_id: "SS-2" });
  });

  it("answers 409 in_progress while the purchase runs", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const d = countingDriver(async (n, before, id) => (await gate, await before(), okResult(id, n)));
    const svc = service(d.driver, tmp());
    const running = svc.purchase(purchaseRequest());
    const again = await svc.purchase(purchaseRequest());
    expect(again).toEqual({ status: 409, body: { status: "in_progress", request_id: "req-1" } });
    release();
    expect((await running).body).toMatchObject({ status: "ok" });
    expect((await svc.purchase(purchaseRequest())).body).toMatchObject({ status: "ok", shop_order_id: "SS-1" });
    expect(d.calls).toBe(1);
  });

  it("refuses a request_id reused for a different request", async () => {
    const dir = tmp();
    const d = countingDriver(async (n, before, id) => (await before(), okResult(id, n)));
    await service(d.driver, dir).purchase(purchaseRequest());
    const other = await service(d.driver, dir).purchase(purchaseRequest({ items: [{ sku: "A-200", qty: 1 }] }));
    expect(other.status).toBe(422);
    expect(d.calls).toBe(1);
    // the key order of the JSON does not matter
    const { items, ...rest } = purchaseRequest();
    expect((await service(d.driver, dir).purchase({ items, ...rest })).status).toBe(200);
  });

  it("never re-runs a purchase that stopped after submitting payment", async () => {
    const dir = tmp();
    // the first process dies right after the payment step: the driver never returns
    const hung = countingDriver(async (_n, before) => (await before(), new Promise<PurchaseResult>(() => {})));
    void service(hung.driver, dir).purchase(purchaseRequest());
    await new Promise((r) => setTimeout(r, 20));

    const d = countingDriver(async (n, before, id) => (await before(), okResult(id, n)));
    const after = await service(d.driver, dir).purchase(purchaseRequest());
    expect(after).toMatchObject({ status: 200, body: { status: "needs_human", request_id: "req-1" } });
    expect(d.calls).toBe(0);
    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toEqual(after.body);
    expect(d.calls).toBe(0);
  });

  it("keeps a needs_human from a failure after payment", async () => {
    const dir = tmp();
    const d = countingDriver(async (_n, before) => {
      await before();
      throw new Error("page crashed after pay");
    });
    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toMatchObject({ status: "needs_human" });
    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toMatchObject({ status: "needs_human", error: "page crashed after pay" });
    expect(d.calls).toBe(1);
  });

  it("never re-runs an invalid result after payment", async () => {
    const dir = tmp();
    const d = countingDriver(async (_n, before, id) => (await before(), { request_id: id, status: "ok", evidence: [] }));
    expect((await service(d.driver, dir).purchase(purchaseRequest())).status).toBe(500);
    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toMatchObject({ status: "needs_human" });
    expect(d.calls).toBe(1);
  });

  it("resumes a purchase that stopped before any payment", async () => {
    const dir = tmp();
    const hung = countingDriver(() => new Promise<PurchaseResult>(() => {}));
    void service(hung.driver, dir).purchase(purchaseRequest());
    await new Promise((r) => setTimeout(r, 20));
    const d = countingDriver(async (n, before, id) => (await before(), okResult(id, n)));
    expect((await service(d.driver, dir).purchase(purchaseRequest())).body).toMatchObject({ status: "ok" });
    expect(d.calls).toBe(1);
  });

  it("does not pay when the payment marker cannot be written", async () => {
    const dir = tmp();
    const store = new PurchaseStore(dir);
    let paid = false;
    const svc = new BotService(
      new DriverRegistry().register({
        name: "x",
        hosts: ["safe-shop.test"],
        purchase: async (req, c) => {
          store.put = () => {
            throw new Error("disk full");
          };
          try {
            await c.beforePayment();
          } catch (err) {
            return { request_id: req.request_id, status: "failed", error: (err as Error).message, evidence: [] };
          }
          paid = true;
          return okResult(req.request_id, 1);
        },
        tracking: () => Promise.reject(new Error("unused")),
      }),
      ctx,
      store,
    );
    expect((await svc.purchase(purchaseRequest())).body).toMatchObject({ status: "failed", error: "disk full" });
    expect(paid).toBe(false);
  });
});
