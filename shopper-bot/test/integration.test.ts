// Runs the real bot (Chromium) against a running fakeshop.
//
//   FAKESHOP_ADDR=fakeshop:8080 npm run test:integration
//
// Without Caddy there is no DNS for *.test and no TLS, so Chromium maps
// every *.test host to fakeshop (--host-resolver-rules) and shop URLs are
// fetched over plain HTTP (SHOP_SCHEME_OVERRIDE=http). See README.
import { createHash } from "node:crypto";
import { lookup } from "node:dns/promises";
import type { AddressInfo } from "node:net";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createBot, type Bot } from "../src/bot.js";
import { loadConfig } from "../src/config.js";
import type { PurchaseRequest, PurchaseResult, TrackingStatus } from "../src/types.js";
import { purchaseRequest } from "./fixtures.js";

const fakeshop = process.env.FAKESHOP_ADDR;

describe.skipIf(!fakeshop)("shopper-bot against fakeshop", () => {
  let bot: Bot;
  let base: string;

  beforeAll(async () => {
    const [host, port = "8080"] = fakeshop!.split(":");
    const { address } = await lookup(host!); // Chromium's MAP wants an IP
    bot = createBot(
      loadConfig({
        SHOP_SCHEME_OVERRIDE: "http",
        BROWSER_HOST_RESOLVER_RULES: `MAP *.test ${address}:${port}`,
        ACTION_TIMEOUT_MS: "10000",
      }),
    );
    await new Promise<void>((resolve) => bot.server.listen(0, "127.0.0.1", resolve));
    base = `http://127.0.0.1:${(bot.server.address() as AddressInfo).port}`;
  });

  afterAll(async () => {
    await bot?.close();
  });

  async function call<T>(path: string, body?: unknown): Promise<{ status: number; body: T }> {
    const res = await fetch(base + path, body === undefined ? {} : { method: "POST", body: JSON.stringify(body) });
    return { status: res.status, body: (await res.json()) as T };
  }

  const purchase = async (over: Partial<PurchaseRequest>) => {
    const res = await call<PurchaseResult>("/v1/purchase", purchaseRequest(over));
    expect(res.status).toBe(200);
    return res.body;
  };

  async function waitForTracking(shop_url: string, shop_order_id: string, want: string): Promise<TrackingStatus> {
    const seen: string[] = [];
    for (let i = 0; i < 40; i++) {
      const res = await call<TrackingStatus>("/v1/tracking", { shop_url, shop_order_id });
      expect(res.status).toBe(200);
      if (seen.at(-1) !== res.body.status) seen.push(res.body.status);
      if (res.body.status === want) return res.body;
      await new Promise((r) => setTimeout(r, 250));
    }
    throw new Error(`tracking never reached ${want}; saw ${seen.join(" → ")}`);
  }

  it("reports its drivers", async () => {
    const res = await call<{ drivers: string[] }>("/v1/capabilities");
    expect(res.body).toEqual({ version: "1", drivers: ["cash-store.test", "safe-shop.test", "us-shop.test"] });
  });

  it("buys at safe-shop.test with a card and tracks delivery", async () => {
    const result = await purchase({ items: [{ sku: "A-100", qty: 1 }], max_amount: { amount: "4000", currency: "JPY" } });
    expect(result).toMatchObject({ status: "ok", total: { amount: "4000", currency: "JPY" } });
    expect(result.shop_order_id).toMatch(/^SS-/);

    const [shot, receipt] = result.evidence;
    expect(shot?.kind).toBe("screenshot");
    const png = Buffer.from(shot!.data_b64!, "base64");
    expect(png.subarray(1, 4).toString()).toBe("PNG");
    expect(createHash("sha256").update(png).digest("hex")).toBe(shot!.sha256);
    expect(JSON.parse(Buffer.from(receipt!.data_b64!, "base64").toString())).toMatchObject({
      shop_order_id: result.shop_order_id,
      total: { amount: "4000", currency: "JPY" },
    });

    const shipped = await waitForTracking("https://safe-shop.test/", result.shop_order_id!, "shipped");
    expect(shipped.carrier).toBe("Yamato");
    expect(shipped.tracking_no).toMatch(/^\d{12}$/);
    expect(shipped.evidence[0]?.kind).toBe("json");
    await waitForTracking("https://safe-shop.test/", result.shop_order_id!, "delivered");
  }, 30_000);

  it("buys at us-shop.test in USD", async () => {
    const result = await purchase({
      shop_url: "https://us-shop.test/",
      items: [{ sku: "U-100", qty: 1 }],
      max_amount: { amount: "40.00", currency: "USD" },
    });
    expect(result).toMatchObject({ status: "ok", total: { amount: "35.00", currency: "USD" } });
  }, 30_000);

  it("stops before paying when the total exceeds max_amount", async () => {
    const result = await purchase({ items: [{ sku: "A-200", qty: 1 }], max_amount: { amount: "12000", currency: "JPY" } });
    expect(result.status).toBe("failed");
    expect(result.error).toContain("exceeds max_amount");
    expect(result.shop_order_id).toBeUndefined();
    expect(result.evidence[0]?.kind).toBe("screenshot");
  }, 30_000);

  it("reports a declined card as failed", async () => {
    const result = await purchase({ payment_ref: "card:declined" });
    expect(result.status).toBe("failed");
    expect(result.error).toContain("card_declined");
  }, 30_000);

  it("reports a FAIL- product's shipment as failed", async () => {
    const result = await purchase({ items: [{ sku: "FAIL-100", qty: 1 }] });
    expect(result.status).toBe("ok");
    await waitForTracking("https://safe-shop.test/", result.shop_order_id!, "failed");
  }, 30_000);

  it("pays cash at the cash-store.test counter", async () => {
    const result = await purchase({
      shop_url: "https://cash-store.test/",
      items: [{ sku: "C-100", qty: 2 }],
      payment_ref: "cash",
      max_amount: { amount: "4000", currency: "JPY" },
    });
    expect(result).toMatchObject({ status: "ok", total: { amount: "4000", currency: "JPY" } });
    const receipt = JSON.parse(Buffer.from(result.evidence[1]!.data_b64!, "base64").toString());
    expect(receipt).toMatchObject({ payment: "cash" });
    expect(receipt.receipt_no).toMatch(/^R\d{8}-/);
    await waitForTracking("https://cash-store.test/", result.shop_order_id!, "delivered");
  }, 30_000);

  it("will not pay a cash-only store by card", async () => {
    const result = await purchase({ shop_url: "https://cash-store.test/", items: [{ sku: "C-100", qty: 1 }] });
    expect(result).toMatchObject({ status: "failed" });
  });

  it("hands unknown shops to a human", async () => {
    const result = await purchase({ shop_url: "http://risky-shop.test/", items: [{ sku: "R-100", qty: 1 }] });
    expect(result.status).toBe("needs_human");
  });

  it("answers 404 for an unknown order", async () => {
    const res = await call("/v1/tracking", { shop_url: "https://safe-shop.test/", shop_order_id: "SS-NOPE" });
    expect(res.status).toBe(404);
  });
});
