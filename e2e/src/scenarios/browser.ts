// 公開の Web 画面（https://app.test）を、NAT の内側の Chromium で操作する。
// testid は proxy-shopping-web/apps/web/TESTIDS.md。
import { readFileSync } from 'node:fs';
import { chromium, type Page } from 'playwright';

const T = 120_000;

async function waitAttr(page: Page, testId: string, attr: string, value: string, timeout = T): Promise<void> {
  await page.getByTestId(testId).and(page.locator(`[${attr}="${value}"]`)).waitFor({ timeout });
}

export async function browserHappyPath(): Promise<{ orderId: string; txid: string }> {
  const mnemonic = readFileSync('/keys/user-browser.mnemonic', 'utf8').trim();
  const browser = await chromium.launch();
  const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'ja-JP' });
  const page = await context.newPage();
  const errors: string[] = [];
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  try {
    await page.goto('https://app.test/');
    await page.getByTestId('onboarding-import-toggle').click();
    await page.getByTestId('onboarding-mnemonic-input').fill(mnemonic);
    await page.getByTestId('onboarding-import-submit').click();
    await page.getByTestId('whoami').and(page.locator('[data-pubkey]')).waitFor();

    await page.getByTestId('nav-new-order').click();
    await page.getByTestId('order-shop-url').fill('https://safe-shop.test/');
    await page.getByTestId('order-region').fill('JP-13-13104');
    await page.getByTestId('order-payment').selectOption('btc-signet');
    await page.getByTestId('order-item-sku-0').fill('A-100');
    await page.getByTestId('order-item-qty-0').fill('1');
    await page.getByTestId('order-search').click();
    await page.getByTestId('offers').and(page.locator(':not([data-count="0"])')).waitFor({ timeout: T });
    await page.getByTestId('offer-select-0').check();
    await page.getByTestId('address-name').fill('山田太郎');
    await page.getByTestId('address-postal-code').fill('160-0022');
    await page.getByTestId('address-address').fill('東京都新宿区新宿1-1-1');
    await page.getByTestId('address-phone').fill('03-0000-0000');
    await page.getByTestId('order-submit').click();

    await waitAttr(page, 'order-status', 'data-status', 'quoted');
    await waitAttr(page, 'quote-fx-banner', 'data-level', 'ok');
    await page.getByTestId('quote-check-ok').waitFor();
    await page.getByTestId('quote-accept').click();
    await waitAttr(page, 'order-status', 'data-status', 'accepted');
    await page.getByTestId('order-faucet').click();
    await waitAttr(page, 'order-faucet', 'data-busy', 'false');
    await waitAttr(page, 'fund-balance', 'data-enough', 'true');
    await page.getByTestId('order-fund').click();
    await waitAttr(page, 'order-status', 'data-status', 'delivered', 180_000);
    await page.getByTestId('order-release').click();
    await waitAttr(page, 'order-status', 'data-status', 'completed');
    const orderId = (await page.getByTestId('order-id').textContent())!.trim();
    const txid = (await page.getByTestId('order-completed-txid').textContent())!.trim();
    await page.screenshot({ path: 'results/browser-order.png', fullPage: true });
    return { orderId, txid };
  } catch (e) {
    await page.screenshot({ path: 'results/browser-failure.png', fullPage: true }).catch(() => undefined);
    throw new Error(`${(e as Error).message}\nbrowser console errors: ${errors.slice(-5).join(' | ')}`);
  } finally {
    await browser.close();
  }
}
