import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { CardVault } from "../src/cards.js";
import type { Driver, DriverContext } from "../src/drivers/driver.js";
import { NotFoundError } from "../src/drivers/driver.js";
import { DriverRegistry } from "../src/drivers/registry.js";
import { AIDriver } from "../src/drivers/ai.js";
import { defaultRegistry } from "../src/drivers/index.js";
import { BotService } from "../src/service.js";
import { PurchaseStore } from "../src/store.js";
import type { PurchaseResult } from "../src/types.js";
import { purchaseRequest } from "./fixtures.js";

const cards = CardVault.fromObject({ cards: { default: { number: "4242424242424242", exp: "12/30", cvc: "123", name: "X" } } });

const ctx: DriverContext = {
  withPage: () => Promise.reject(new Error("no browser in unit tests")),
  cards,
  origin: (u) => new URL(u).origin,
  log: () => {},
  beforePayment: async () => {},
};

const store = () => new PurchaseStore(mkdtempSync(join(tmpdir(), "bot-")));

function fakeDriver(purchase: Driver["purchase"], tracking?: Driver["tracking"]): Driver {
  return {
    name: "fake",
    hosts: ["safe-shop.test"],
    purchase,
    tracking: tracking ?? (() => Promise.reject(new NotFoundError("nope"))),
  };
}

describe("BotService", () => {
  it("lists registered hosts", () => {
    const svc = new BotService(defaultRegistry(), ctx, store());
    expect(svc.capabilities().body).toEqual({ version: "1", drivers: ["cash-store.test", "safe-shop.test", "us-shop.test"] });
  });

  it("rejects invalid requests before any driver runs", async () => {
    let called = false;
    const svc = new BotService(new DriverRegistry().register(fakeDriver(async () => ((called = true), {} as PurchaseResult))), ctx, store());
    const reply = await svc.purchase({ ...purchaseRequest(), max_amount: { amount: 1, currency: "JPY" } });
    expect(reply.status).toBe(400);
    expect(called).toBe(false);
  });

  it("returns needs_human for shops without a driver", async () => {
    const svc = new BotService(defaultRegistry(), ctx, store());
    const reply = await svc.purchase(purchaseRequest({ shop_url: "http://risky-shop.test/" }));
    expect(reply.status).toBe(200);
    expect(reply.body).toMatchObject({ status: "needs_human", request_id: "req-1" });
  });

  it("turns a crashing driver into needs_human", async () => {
    const svc = new BotService(new DriverRegistry().register(fakeDriver(() => Promise.reject(new Error("boom")))), ctx, store());
    expect((await svc.purchase(purchaseRequest())).body).toMatchObject({ status: "needs_human", error: "boom" });
  });

  it("does not pass on a malformed driver result", async () => {
    const svc = new BotService(new DriverRegistry().register(fakeDriver(async (r) => ({ request_id: r.request_id, status: "ok", evidence: [] }))), ctx, store());
    expect((await svc.purchase(purchaseRequest())).status).toBe(500);
  });

  it("maps unknown orders to 404", async () => {
    const svc = new BotService(new DriverRegistry().register(fakeDriver(async () => ({}) as PurchaseResult)), ctx, store());
    expect((await svc.tracking({ shop_url: "https://safe-shop.test/", shop_order_id: "X" })).status).toBe(404);
  });

  it("keeps the AI driver out of the default registry", async () => {
    expect(defaultRegistry().hosts()).not.toContain("any-shop.test");
    await expect(new AIDriver(["any-shop.test"]).purchase(purchaseRequest(), ctx)).rejects.toThrow("not implemented");
  });
});

describe("CardVault", () => {
  it("resolves references and nothing else", () => {
    expect(cards.resolve("card:default").number).toBe("4242424242424242");
    expect(() => cards.resolve("card:other")).toThrow(/no card/);
    expect(() => cards.resolve("cash")).toThrow(/not a card/);
  });
});
