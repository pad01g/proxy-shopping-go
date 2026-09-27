// Page conventions shared by the lab shops (fakeshop): amounts carry
// data-amount / data-currency, address fields have #ship-* ids and each
// shop has a JSON order API.
import type { Page } from "playwright";
import { evidenceFromBytes, jsonEvidence, screenshotEvidence } from "../evidence.js";
import type { Address, Evidence, Money, PurchaseRequest, PurchaseResult, ShipmentStatus, TrackingQuery, TrackingStatus } from "../types.js";
import { NotFoundError, type DriverContext } from "./driver.js";

export async function readMoney(page: Page, selector: string): Promise<Money> {
  const el = page.locator(selector);
  const [amount, currency] = await Promise.all([el.getAttribute("data-amount"), el.getAttribute("data-currency")]);
  if (!amount || !currency) throw new Error(`${selector} has no data-amount/data-currency`);
  return { amount, currency };
}

export async function fillAddress(page: Page, a: Address): Promise<void> {
  await page.fill("#ship-name", a.name);
  await page.fill("#ship-postal-code", a.postal_code);
  await page.fill("#ship-address", a.address);
  await page.fill("#ship-phone", a.phone);
}

export async function screenshot(page: Page): Promise<Evidence> {
  return screenshotEvidence(await page.screenshot({ fullPage: true, type: "png" }));
}

/** The receipt evidence: what was bought, where, for how much. */
export function receipt(req: PurchaseRequest, page: Page, shopOrderId: string, total: Money, extra: Record<string, string> = {}): Evidence {
  return jsonEvidence("receipt", {
    request_id: req.request_id,
    order_id: req.order_id,
    shop_url: req.shop_url,
    shop_order_id: shopOrderId,
    items: req.items,
    total,
    confirmation_url: page.url(),
    captured_at: Math.floor(Date.now() / 1000),
    ...extra,
  });
}

export function failed(req: PurchaseRequest, error: string, evidence: Evidence[] = []): PurchaseResult {
  return { request_id: req.request_id, status: "failed", error, evidence };
}

export function needsHuman(req: PurchaseRequest, error: string, evidence: Evidence[] = []): PurchaseResult {
  return { request_id: req.request_id, status: "needs_human", error, evidence };
}

const statuses: readonly ShipmentStatus[] = ["processing", "shipped", "delivered", "failed"];

/**
 * Reads GET /api/orders/{id} through the browser, so it takes the same
 * network path (DNS rules, TLS exceptions) as the purchase did.
 */
export async function trackViaShopApi(q: TrackingQuery, ctx: DriverContext): Promise<TrackingStatus> {
  const url = `${ctx.origin(q.shop_url)}/api/orders/${encodeURIComponent(q.shop_order_id)}`;
  return ctx.withPage(async (page) => {
    const res = await page.goto(url);
    if (!res) throw new Error(`no response from ${url}`);
    if (res.status() === 404) throw new NotFoundError(`shop does not know order ${q.shop_order_id}`);
    if (!res.ok()) throw new Error(`${url}: HTTP ${res.status()}`);
    const body = await res.body();
    const order = JSON.parse(body.toString("utf8")) as { tracking?: Record<string, unknown> };
    const t = order.tracking ?? {};
    if (!statuses.includes(t.status as ShipmentStatus) || typeof t.updated_at !== "number") {
      throw new Error(`${url}: unexpected tracking ${JSON.stringify(t)}`);
    }
    const out: TrackingStatus = {
      status: t.status as ShipmentStatus,
      updated_at: t.updated_at,
      evidence: [evidenceFromBytes("json", "application/json", body)],
    };
    if (typeof t.carrier === "string" && t.carrier) out.carrier = t.carrier;
    if (typeof t.tracking_no === "string" && t.tracking_no) out.tracking_no = t.tracking_no;
    return out;
  });
}
