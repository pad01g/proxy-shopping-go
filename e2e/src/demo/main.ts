// デモ画面の e2e: docker compose run --rm runner demo [シナリオ id …] [separate]
// NAT の内側の Chromium で http://demo（デモのサーバー）を開き、ガイドの指示どおりに操作する。
// 結果は results/demo-<時刻>.md と results/demo-latest.md。1 つでも失敗すれば終了コード 1。
import { mkdirSync, writeFileSync } from 'node:fs';
import { chromium, type Browser, type BrowserContext, type Page } from 'playwright';
import { followGuide, openDemo, resetDemo, startScenario, type DemoWindow } from './driver.js';
import { DEMO_SCENARIOS } from './scenarios.js';

const BASE = process.env.DEMO_URL ?? 'http://demo';
const SEPARATE = 'separate';

const wanted = process.argv.slice(2);
const selected = wanted.length ? DEMO_SCENARIOS.filter((s) => wanted.includes(s.id)) : DEMO_SCENARIOS;
const runSeparate = !wanted.length || wanted.includes(SEPARATE);
const unknown = wanted.filter((w) => w !== SEPARATE && !DEMO_SCENARIOS.some((s) => s.id === w));
if (unknown.length) {
  console.error(`unknown demo scenarios: ${unknown.join(' ')} (have ${DEMO_SCENARIOS.map((s) => s.id).join(' ')} ${SEPARATE})`);
  process.exit(2);
}

const head: string[] = [];
const details: string[] = [];
const say = (l: string, to = head) => {
  console.log(l);
  to.push(l);
};

/** http://demo is not a secure context; the page uses Web Locks, which browsers only give secure contexts. */
const launch = () => chromium.launch({ args: [`--unsafely-treat-insecure-origin-as-secure=${BASE}`] });

async function newPage(context: BrowserContext, name: string): Promise<Page> {
  const page = await context.newPage();
  page.on('console', (m) => {
    if (m.type() === 'error') console.log(`[${name} console] ${m.text().slice(0, 300)}`);
  });
  page.on('pageerror', (e) => console.log(`[${name} pageerror] ${e.message}`));
  return page;
}

async function shot(page: Page, file: string): Promise<void> {
  await page.screenshot({ path: `results/${file}`, fullPage: true }).catch(() => undefined);
}

async function runScenario(context: BrowserContext, id: string, check: (p: Page) => Promise<string>): Promise<void> {
  const page = await newPage(context, id);
  try {
    await openDemo(page, BASE);
    await startScenario(page, id);
    await followGuide([{ page, name: 'all' }], { log: (l) => say(`  - ${l}`, details) });
    say(`  - ${await check(page)}`, details);
    await shot(page, `demo-${id}.png`);
  } catch (e) {
    await shot(page, `demo-${id}-failure.png`);
    throw e;
  } finally {
    await page.close();
  }
}

/** Two windows of one browser: the user in one, escrow + operator + coordinator in the other. */
async function runSeparateWindows(context: BrowserContext): Promise<void> {
  const userPage = await newPage(context, 'user-window');
  const othersPage = await newPage(context, 'others-window');
  const windows: DemoWindow[] = [
    { page: userPage, roles: ['user'], name: 'user-window' },
    { page: othersPage, roles: ['escrow', 'operator', 'coordinator'], name: 'others-window' },
  ];
  try {
    await openDemo(userPage, `${BASE}/?role=user`);
    await openDemo(othersPage, `${BASE}/?role=escrow,operator,coordinator`);
    // Each window runs only its roles: the user tab is not in the other window and vice versa.
    if (await othersPage.getByTestId('tab-user').count()) throw new Error('the escrow window shows the user tab');
    if (await userPage.getByTestId('tab-escrow').count()) throw new Error('the user window shows the escrow tab');
    await startScenario(userPage, 'dispute-refund');
    await othersPage.locator('[data-testid="guide"][data-scenario="dispute-refund"][data-complete="false"]').waitFor({ timeout: 30_000 });
    await followGuide(windows, { log: (l) => say(`  - ${l}`, details) });
    await othersPage.locator('[data-testid="guide"][data-complete="true"]').waitFor({ timeout: 30_000 });
    const status = await userPage.getByTestId('order-status').getAttribute('data-status');
    if (status !== 'settled') throw new Error(`status ${status}`);
    say('  - both windows show the scenario complete; the user window countersigned, the other one ruled', details);
    await shot(userPage, 'demo-separate-user.png');
    await shot(othersPage, 'demo-separate-others.png');
  } catch (e) {
    await shot(userPage, 'demo-separate-user-failure.png');
    await shot(othersPage, 'demo-separate-others-failure.png');
    throw e;
  } finally {
    await userPage.close();
    await othersPage.close();
  }
}

async function main(): Promise<number> {
  const started = new Date();
  mkdirSync('results', { recursive: true });
  say(`# proxy-shopping demo e2e (${started.toISOString()})`);
  say(`- page: ${BASE}`);
  let browser: Browser | undefined;
  const rows: string[] = [];
  let failed = 0;
  try {
    browser = await launch();
    const context = await browser.newContext({ locale: 'ja-JP', viewport: { width: 1400, height: 1000 } });
    // Start from scratch: new keys for the user, escrow and operator.
    const first = await newPage(context, 'reset');
    await openDemo(first, BASE);
    await resetDemo(first);
    await first.close();
    say('- demo storage reset (fresh keys)');

    const run = async (id: string, title: string, fn: () => Promise<void>) => {
      const t0 = Date.now();
      say(`- **${id}**: ${title}`, details);
      try {
        await fn();
        rows.push(`| ${id} | PASS | ${((Date.now() - t0) / 1000).toFixed(0)} | ${title} |`);
      } catch (e) {
        failed++;
        say(`  - FAIL: ${((e as Error).stack ?? String(e)).split('\n').slice(0, 6).join(' / ')}`, details);
        rows.push(`| ${id} | **FAIL** | ${((Date.now() - t0) / 1000).toFixed(0)} | ${title} |`);
      }
    };
    for (const s of selected) await run(s.id, s.title, () => runScenario(context, s.id, s.check));
    if (runSeparate) await run(SEPARATE, '別々のウィンドウ（?role=user と ?role=escrow,operator,coordinator）で dispute-refund', () => runSeparateWindows(context));
  } finally {
    await browser?.close();
  }
  rows.forEach((r) => console.log(r));
  const md = [...head, '', '| id | result | seconds | scenario |', '|---|---|---:|---|', ...rows, '', '## 詳細', '', ...details].join('\n') + '\n';
  writeFileSync(`results/demo-${started.toISOString().replace(/[:.]/g, '-')}.md`, md);
  writeFileSync('results/demo-latest.md', md);
  console.log(failed ? `\n${failed} demo scenario(s) failed` : '\nall demo scenarios passed');
  return failed ? 1 : 0;
}

main().then(
  (code) => process.exit(code),
  (e) => {
    console.error(e);
    process.exit(1);
  },
);
