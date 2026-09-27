import type { Page } from "playwright";
import { exceedsMax, formatMoney } from "../money.js";
import type { PurchaseRequest, PurchaseResult, TrackingQuery, TrackingStatus } from "../types.js";
import type { Driver, DriverContext } from "./driver.js";
import { failed, fillAddress, needsHuman, readMoney, receipt, screenshot, trackViaShopApi } from "./fakeshop.js";

/**
 * Cash-only shops. In the lab this is cash-store.test: the shopper stands
 * at the counter, the clerk rings the items up on the store terminal
 * (/pos), the shopper hands over cash and gets a receipt. The bot plays
 * that scene by driving the terminal itself, so "pressing 現金を受け取った"
 * is the moment the cash changes hands.
 */
export class CashStoreDriver implements Driver {
  readonly name = "playwright:cash-store";
  readonly hosts: readonly string[];

  constructor(hosts: string[]) {
    this.hosts = hosts;
  }

  async purchase(req: PurchaseRequest, ctx: DriverContext): Promise<PurchaseResult> {
    if (req.payment_ref !== "cash") {
      return failed(req, `this store takes cash only, not ${req.payment_ref}`);
    }
    return ctx.withPage((page) => this.run(page, req, ctx));
  }

  tracking(q: TrackingQuery, ctx: DriverContext): Promise<TrackingStatus> {
    return trackViaShopApi(q, ctx);
  }

  private async run(page: Page, req: PurchaseRequest, ctx: DriverContext): Promise<PurchaseResult> {
    let cashHandedOver = false;
    try {
      await page.goto(`${ctx.origin(req.shop_url)}/pos`);
      for (const item of req.items) {
        const field = page.locator(`[id="qty-${item.sku}"]`);
        if ((await field.count()) === 0) return failed(req, `the store has no product ${item.sku}`);
        await field.fill(String(item.qty));
      }
      await fillAddress(page, req.shipping);

      // Ask the terminal for the total before any money moves.
      await page.click("#pos-calc");
      await page.waitForSelector("#pos-total"); // only on the recalculated page
      const total = await readMoney(page, "#pos-total");
      const tooMuch = exceedsMax(total, req.max_amount);
      if (tooMuch) return failed(req, tooMuch, [await screenshot(page)]);

      await ctx.beforePayment();
      cashHandedOver = true;
      await page.click("#cash-received");
      await page.waitForSelector("#receipt-no");

      const shopOrderId = (await page.textContent("#order-id"))?.trim();
      const receiptNo = (await page.textContent("#receipt-no"))?.trim() ?? "";
      if (!shopOrderId) throw new Error("receipt has no #order-id");
      const paid = await readMoney(page, "#order-total");
      ctx.log(`${req.request_id}: cash sale ${shopOrderId} (receipt ${receiptNo}) for ${formatMoney(paid)}`);
      return {
        request_id: req.request_id,
        status: "ok",
        shop_order_id: shopOrderId,
        total: paid,
        evidence: [await screenshot(page), receipt(req, page, shopOrderId, paid, { receipt_no: receiptNo, payment: "cash" })],
      };
    } catch (err) {
      const msg = (err as Error).message;
      const evidence = await screenshot(page).then((e) => [e], () => []);
      return cashHandedOver ? needsHuman(req, `after paying cash: ${msg}`, evidence) : failed(req, msg, evidence);
    }
  }
}
