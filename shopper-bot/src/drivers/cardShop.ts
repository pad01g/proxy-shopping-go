import type { Page } from "playwright";
import type { Card } from "../cards.js";
import { exceedsMax, formatMoney } from "../money.js";
import type { PurchaseRequest, PurchaseResult, TrackingQuery, TrackingStatus } from "../types.js";
import type { Driver, DriverContext } from "./driver.js";
import { failed, fillAddress, needsHuman, readMoney, receipt, screenshot, trackViaShopApi } from "./fakeshop.js";

export interface CardShopOptions {
  hosts: string[];
  /** The only host card data may be typed into. */
  gatewayHost: string;
}

/**
 * Online shops that take cards through a hosted payment page (lab:
 * safe-shop.test and us-shop.test via cardgw.test).
 *
 * Flow: product pages → cart → checkout (check the total against
 * max_amount) → gateway (check host and amount again) → confirmation.
 */
export class CardShopDriver implements Driver {
  readonly name = "playwright:card-shop";
  readonly hosts: readonly string[];
  private readonly gatewayHost: string;

  constructor(opts: CardShopOptions) {
    this.hosts = opts.hosts;
    this.gatewayHost = opts.gatewayHost;
  }

  async purchase(req: PurchaseRequest, ctx: DriverContext): Promise<PurchaseResult> {
    if (!req.payment_ref.startsWith("card:")) {
      return failed(req, `this shop takes cards only, not ${req.payment_ref}`);
    }
    let card: Card;
    try {
      card = ctx.cards.resolve(req.payment_ref);
    } catch (err) {
      return failed(req, (err as Error).message);
    }
    return ctx.withPage((page) => this.run(page, req, card, ctx));
  }

  tracking(q: TrackingQuery, ctx: DriverContext): Promise<TrackingStatus> {
    return trackViaShopApi(q, ctx);
  }

  private async run(page: Page, req: PurchaseRequest, card: Card, ctx: DriverContext): Promise<PurchaseResult> {
    const origin = ctx.origin(req.shop_url);
    let paymentSubmitted = false;
    try {
      for (const item of req.items) {
        const res = await page.goto(`${origin}/products/${encodeURIComponent(item.sku)}`);
        if (res?.status() === 404) return failed(req, `the shop has no product ${item.sku}`);
        await page.fill("#qty", String(item.qty));
        await Promise.all([page.waitForURL(/\/cart$/), page.click("#add-to-cart")]);
      }

      await page.goto(`${origin}/checkout`);
      const total = await readMoney(page, "#checkout-total");
      const tooMuch = exceedsMax(total, req.max_amount);
      if (tooMuch) return failed(req, tooMuch, [await screenshot(page)]);

      await fillAddress(page, req.shipping);
      await Promise.all([page.waitForURL(/\/pay\//), page.click("#place-order")]);

      // Never type card data into a page we did not expect.
      const payHost = new URL(page.url()).hostname;
      if (payHost !== this.gatewayHost) {
        return failed(req, `checkout sent us to ${payHost}, not ${this.gatewayHost}`, [await screenshot(page)]);
      }
      const charged = await readMoney(page, "#pay-amount");
      if (charged.amount !== total.amount || charged.currency !== total.currency) {
        return failed(req, `gateway asks ${formatMoney(charged)} but checkout showed ${formatMoney(total)}`, [await screenshot(page)]);
      }

      await page.fill("#card-number", card.number);
      await page.fill("#card-exp", card.exp);
      await page.fill("#card-cvc", card.cvc);
      await page.fill("#card-name", card.name);
      await ctx.beforePayment();
      paymentSubmitted = true;
      await page.click("#pay");

      const outcome = await firstOf(
        page.waitForURL(/\/checkout\/complete\?/).then(() => "complete" as const),
        page.waitForSelector("#pay-error").then(() => "error" as const),
      );
      if (outcome === "error") {
        const reason = (await page.getAttribute("#pay-error", "data-reason")) ?? "";
        const text = (await page.textContent("#pay-error"))?.trim() ?? "";
        // The gateway refused before charging: nothing was paid.
        return failed(req, `payment refused (${reason}): ${text}`, [await screenshot(page)]);
      }

      const shopOrderId = (await page.textContent("#order-id"))?.trim();
      if (!shopOrderId) throw new Error("confirmation page has no #order-id");
      const paid = await readMoney(page, "#order-total");
      ctx.log(`${req.request_id}: ordered ${shopOrderId} for ${formatMoney(paid)}`);
      return {
        request_id: req.request_id,
        status: "ok",
        shop_order_id: shopOrderId,
        total: paid,
        evidence: [await screenshot(page), receipt(req, page, shopOrderId, paid)],
      };
    } catch (err) {
      const msg = (err as Error).message;
      const evidence = await screenshot(page).then((e) => [e], () => []);
      // Once the card form is submitted we cannot tell whether a charge went
      // through, so a person has to look.
      return paymentSubmitted ? needsHuman(req, `after submitting payment: ${msg}`, evidence) : failed(req, msg, evidence);
    }
  }
}

/** Promise.race that does not leave the loser's rejection unhandled. */
function firstOf<T>(...waiters: Promise<T>[]): Promise<T> {
  for (const w of waiters) w.catch(() => {});
  return Promise.race(waiters);
}
