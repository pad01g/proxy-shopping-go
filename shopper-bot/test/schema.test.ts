import { describe, expect, it } from "vitest";
import { jsonEvidence, screenshotEvidence } from "../src/evidence.js";
import { describeErrors, validate } from "../src/schema.js";
import { purchaseRequest } from "./fixtures.js";

describe("PurchaseRequest schema", () => {
  it("accepts a well-formed request", () => {
    expect(validate.purchaseRequest(purchaseRequest())).toBe(true);
    expect(validate.purchaseRequest(purchaseRequest({ payment_ref: "cash", shop_url: "https://cash-store.test/" }))).toBe(true);
  });

  it.each([
    ["numeric amount", { max_amount: { amount: 5000, currency: "JPY" } }],
    ["negative amount", { max_amount: { amount: "-1", currency: "JPY" } }],
    ["empty items", { items: [] }],
    ["zero quantity", { items: [{ sku: "A-100", qty: 0 }] }],
    ["fractional quantity", { items: [{ sku: "A-100", qty: 1.5 }] }],
    ["card data instead of a reference", { payment_ref: "4242424242424242" }],
    ["short order id", { order_id: "abc" }],
    ["non-http shop url", { shop_url: "ftp://safe-shop.test/" }],
    ["unknown field", { card_number: "4242424242424242" }],
  ])("rejects %s", (_name, over) => {
    const req = { ...purchaseRequest(), ...over };
    expect(validate.purchaseRequest(req)).toBe(false);
    expect(describeErrors(validate.purchaseRequest.errors)).not.toBe("");
  });

  it("rejects an incomplete address", () => {
    const req = purchaseRequest();
    const { phone: _phone, ...shipping } = req.shipping;
    expect(validate.purchaseRequest({ ...req, shipping })).toBe(false);
    expect(describeErrors(validate.purchaseRequest.errors)).toContain("phone");
  });
});

describe("PurchaseResult schema", () => {
  const evidence = [screenshotEvidence(new Uint8Array([1, 2, 3])), jsonEvidence("receipt", { a: 1 })];

  it("requires order id and total when ok", () => {
    expect(validate.purchaseResult({ request_id: "r", status: "ok", evidence })).toBe(false);
    expect(
      validate.purchaseResult({ request_id: "r", status: "ok", shop_order_id: "SS-1", total: { amount: "4000", currency: "JPY" }, evidence }),
    ).toBe(true);
  });

  it("allows failures without order data", () => {
    expect(validate.purchaseResult({ request_id: "r", status: "failed", error: "declined", evidence: [] })).toBe(true);
    expect(validate.purchaseResult({ request_id: "r", status: "maybe", evidence: [] })).toBe(false);
  });

  it("checks evidence digests look like sha256", () => {
    const bad = { kind: "screenshot", sha256: "xyz", mime: "image/png" };
    expect(validate.purchaseResult({ request_id: "r", status: "failed", evidence: [bad] })).toBe(false);
  });
});

describe("tracking schemas", () => {
  it("validates queries and statuses", () => {
    expect(validate.trackingQuery({ shop_url: "https://safe-shop.test/", shop_order_id: "SS-1" })).toBe(true);
    expect(validate.trackingQuery({ shop_url: "https://safe-shop.test/" })).toBe(false);
    expect(validate.trackingStatus({ status: "shipped", carrier: "Yamato", tracking_no: "1", updated_at: 1790000000, evidence: [] })).toBe(true);
    expect(validate.trackingStatus({ status: "lost", updated_at: 1, evidence: [] })).toBe(false);
    expect(validate.trackingStatus({ status: "shipped", updated_at: "1", evidence: [] })).toBe(false);
  });
});

describe("evidence", () => {
  it("hashes exactly the carried bytes", async () => {
    const e = jsonEvidence("json", { x: "y" });
    const { createHash } = await import("node:crypto");
    const bytes = Buffer.from(e.data_b64!, "base64");
    expect(createHash("sha256").update(bytes).digest("hex")).toBe(e.sha256);
    expect(e.mime).toBe("application/json");
  });
});
