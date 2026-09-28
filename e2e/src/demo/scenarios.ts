// シナリオごとの結果の確認（画面に出ているものだけを見る）。
import type { Page } from 'playwright';
import { assertEnglishPage } from './i18n.js';

export interface DemoScenario {
  id: string;
  title: string;
  /** The page's scenario to follow (default: id). */
  scenario?: string;
  /** Query string of the page (e.g. "?lang=en"). */
  query?: string;
  /** English mode: the guide, header, tabs and panels must show no Japanese UI text at any step. */
  english?: boolean;
  /** Checks on the finished scenario, from the all-roles page; returns a line for the report. */
  check(page: Page): Promise<string>;
}

function assert(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new Error(`assertion failed: ${msg}`);
}

async function tab(page: Page, id: string): Promise<void> {
  await page.getByTestId(`tab-${id}`).click();
  await page.getByTestId(`panel-${id}`).waitFor();
}

async function attr(page: Page, testid: string, name: string): Promise<string> {
  return (await page.getByTestId(testid).first().getAttribute(name, { timeout: 10_000 })) ?? '';
}

async function txt(page: Page, testid: string): Promise<string> {
  return ((await page.getByTestId(testid).first().textContent({ timeout: 10_000 })) ?? '').trim();
}

/** The scenario's order in the user tab (the detail view follows the scenario's order). */
async function order(page: Page): Promise<{ id: string; status: string }> {
  await tab(page, 'user');
  return { id: await txt(page, 'order-id'), status: await attr(page, 'order-status', 'data-status') };
}

const BTC_TXID = /^[0-9a-f]{64}$/;
const EVM_TX = /^0x[0-9a-f]{64}$/;

async function normalBtcCompleted(page: Page): Promise<string> {
  const o = await order(page);
  assert(o.status === 'completed', `status ${o.status}`);
  const txid = await txt(page, 'order-completed-txid');
  assert(BTC_TXID.test(txid), `payout txid ${txid}`);
  return `order ${o.id.slice(0, 8)} completed, payout ${txid}`;
}

export const DEMO_SCENARIOS: DemoScenario[] = [
  {
    id: 'normal-btc',
    title: '正常系（BTC）: 購入 → 配達 → 利用者と shopper の署名でロック解除',
    check: normalBtcCompleted,
  },
  {
    id: 'normal-usdc',
    title: '正常系（USDC）: Safe に入金 → 配達 → 支払い',
    async check(page) {
      const o = await order(page);
      assert(o.status === 'completed', `status ${o.status}`);
      const tx = await txt(page, 'order-completed-txid');
      assert(EVM_TX.test(tx), `payout tx ${tx}`);
      return `order ${o.id.slice(0, 8)} completed, Safe payout ${tx}`;
    },
  },
  {
    id: 'dispute-refund',
    title: '配達失敗 → 紛争 → escrow が全額を利用者へ → 連署して返金',
    async check(page) {
      const o = await order(page);
      assert(o.status === 'settled', `status ${o.status}`);
      const user = BigInt(await attr(page, 'ruling', 'data-split-user'));
      const shopper = BigInt(await attr(page, 'ruling', 'data-split-shopper'));
      assert(user > 0n && shopper === 0n, `split user ${user} shopper ${shopper}`);
      const txid = await txt(page, 'ruling-settled-txid');
      assert(BTC_TXID.test(txid), `settlement txid ${txid}`);
      await tab(page, 'escrow');
      const address = await txt(page, 'escrow-address');
      assert(address.includes('東京都新宿区'), `decrypted address ${address}`);
      return `order ${o.id.slice(0, 8)} settled: ${user} sats back to the user, tx ${txid}; escrow decrypted "${address}"`;
    },
  },
  {
    id: 'sold-out',
    title: '在庫切れ → shopper の協力的な払い戻し → 利用者が連署',
    async check(page) {
      const o = await order(page);
      assert(o.status === 'refunded', `status ${o.status}`);
      const txid = await txt(page, 'order-refund-txid');
      assert(BTC_TXID.test(txid), `refund txid ${txid}`);
      return `order ${o.id.slice(0, 8)} refunded, tx ${txid}`;
    },
  },
  {
    id: 'risky',
    title: '危険な店 → shopper が risk で断る',
    async check(page) {
      const o = await order(page);
      assert(o.status === 'rejected', `status ${o.status}`);
      const reason = await attr(page, 'quote-rejected', 'data-reason');
      assert(reason === 'risk', `reason ${reason}`);
      return `order ${o.id.slice(0, 8)} rejected: ${reason}`;
    },
  },
  {
    id: 'fraud',
    title: '不正な escrow（全額を shopper へ）→ 通報 → operator が一覧から外す → 候補から消える',
    async check(page) {
      await tab(page, 'escrow');
      const escrow = await page.getByTestId('identity-escrow').locator('[data-pubkey]').getAttribute('data-pubkey');
      assert(escrow && /^[0-9a-f]{64}$/.test(escrow), `escrow pubkey ${escrow}`);
      const o = await order(page);
      assert(o.status === 'settled', `status ${o.status}`);
      const user = BigInt(await attr(page, 'ruling', 'data-split-user'));
      const shopper = BigInt(await attr(page, 'ruling', 'data-split-shopper'));
      assert(user === 0n && shopper > 0n, `split user ${user} shopper ${shopper}`);
      // Search again and wait for the answer, so an empty list is the relays' answer and not "not loaded yet".
      await page.getByTestId('order-search').click();
      await page.locator('[data-testid="order-search"][data-busy="false"]').waitFor({ timeout: 30_000 });
      const offered = await page.locator(`[data-testid="offer"][data-escrow="${escrow}"]`).count();
      assert(offered === 0, `the removed escrow is still offered (${offered})`);
      await tab(page, 'operator');
      await page.locator(`[data-testid="operator-report"][data-order-id="${o.id}"]`).first().waitFor({ timeout: 30_000 });
      await page.getByTestId('operator-entries').waitFor({ timeout: 30_000 });
      const listed = await page.locator(`[data-testid="operator-entry"][data-escrow="${escrow}"]`).count();
      assert(listed === 0, `escrow still in the operator list (${listed})`);
      return `order ${o.id.slice(0, 8)}: ${shopper} to the shopper by the fraudulent ruling; reported; escrow removed from the list and no longer offered`;
    },
  },
  {
    id: 'timelock-t2',
    title: 'shopper が消えたら T2 の後に利用者が一人で取り戻す（BTC）',
    async check(page) {
      const o = await order(page);
      assert(o.status === 'refunded', `status ${o.status}`);
      const txid = await txt(page, 'order-refund-txid');
      assert(BTC_TXID.test(txid), `refund txid ${txid}`);
      await tab(page, 'lab');
      assert((await attr(page, 'lab-shopper-state', 'data-paused')) === 'false', 'shopper-1 resumed');
      return `order ${o.id.slice(0, 8)} refunded alone after T2, tx ${txid}; shopper-1 resumed`;
    },
  },
  {
    id: 'normal-btc-en',
    title: '英語表示（?lang=en）で normal-btc: ガイド・ヘッダー・タブ・ボタン・ラベルに日本語が無い',
    scenario: 'normal-btc',
    query: '?lang=en',
    english: true,
    async check(page) {
      const done = await normalBtcCompleted(page);
      const english = await assertEnglishPage(page, ['user', 'shopper', 'escrow', 'operator', 'coordinator', 'lab']);
      return `${done}; ${english}`;
    },
  },
];
