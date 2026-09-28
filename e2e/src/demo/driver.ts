// デモ画面のガイドに従って操作する（testid は proxy-shopping-web/apps/demo/TESTIDS.md）。
// ガイドの「いまの手順」を読み、操作があれば「この操作へ」→ そのボタン →（確認ダイアログがあれば）OK、
// 待ちの手順なら進むまで待つ。これを guide の data-complete="true" まで繰り返す。
import type { Page } from 'playwright';

export interface DemoWindow {
  page: Page;
  /** Session roles this window runs; undefined = every role (the default page). */
  roles?: string[];
  name: string;
}

export interface CurrentStep {
  step: string;
  role: string;
  action: string;
  tab: string;
  confirm: boolean;
}

const SESSION_ROLES = ['user', 'escrow', 'operator', 'coordinator'];

export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export async function currentStep(page: Page): Promise<CurrentStep | 'complete' | undefined> {
  const guide = page.getByTestId('guide');
  if ((await guide.getAttribute('data-complete', { timeout: 5000 }).catch(() => null)) === 'true') return 'complete';
  const cur = page.getByTestId('guide-current');
  if (!(await cur.count())) return undefined;
  const attr = async (n: string) => (await cur.getAttribute(n, { timeout: 2000 }).catch(() => null)) ?? '';
  return { step: await attr('data-step'), role: await attr('data-role'), action: await attr('data-action'), tab: await attr('data-tab'), confirm: (await attr('data-confirm')) === 'true' };
}

async function text(page: Page, testid: string): Promise<string> {
  return ((await page.getByTestId(testid).first().textContent({ timeout: 1000 }).catch(() => '')) ?? '').trim();
}

/** The window that runs the role owning the control (the shopper and lab tabs are in every window). */
function owner(windows: DemoWindow[], tab: string): DemoWindow {
  if (!SESSION_ROLES.includes(tab)) return windows[0];
  const w = windows.find((x) => !x.roles || x.roles.includes(tab));
  if (!w) throw new Error(`no window runs ${tab}`);
  return w;
}

async function waitForStepChange(page: Page, step: string, ms: number): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    const cur = await currentStep(page);
    if (cur === 'complete' || (cur && cur.step !== step)) return;
    await sleep(500);
  }
}

export interface FollowOptions {
  /** Whole scenario. */
  timeoutMs?: number;
  /** One step (waiting steps included: mining to T2 and the shop's delivery take a while). */
  stepTimeoutMs?: number;
  log: (line: string) => void;
  /** Called on the main window whenever the guide shows a new current step (e.g. to check its texts). */
  onStep?: (page: Page, step: CurrentStep) => Promise<void>;
  /** Called on the acting window while a confirmation dialog is open, before its OK. */
  onConfirm?: (page: Page, action: string) => Promise<void>;
}

/** Follow the guide of the selected scenario until it is complete. Returns the ids of the steps passed. */
export async function followGuide(windows: DemoWindow[], opts: FollowOptions): Promise<string[]> {
  const main = windows[0].page;
  const deadline = Date.now() + (opts.timeoutMs ?? 15 * 60_000);
  const stepTimeout = opts.stepTimeoutMs ?? 240_000;
  const seen: string[] = [];
  let last = '';
  let since = Date.now();
  let attempts = 0;
  for (;;) {
    if (Date.now() > deadline) throw new Error(`scenario timed out at step ${last}`);
    const cur = await currentStep(main);
    if (cur === 'complete') return seen;
    if (!cur) {
      await sleep(500);
      continue;
    }
    if (cur.step !== last) {
      opts.log(`${cur.step} (${cur.role}${cur.action ? ` → ${cur.action}` : ', waiting'})`);
      seen.push(cur.step);
      last = cur.step;
      since = Date.now();
      attempts = 0;
      await opts.onStep?.(main, cur);
    }
    if (Date.now() - since > stepTimeout) {
      throw new Error(`step ${cur.step} did not finish in ${stepTimeout / 1000}s: ${await text(main, 'guide-progress')}${await errorsOf(windows)}`);
    }
    if (!cur.action) {
      await sleep(1000);
      continue;
    }
    if (++attempts > 4) throw new Error(`step ${cur.step}: ${cur.action} was pressed ${attempts - 1} times without effect${await errorsOf(windows)}`);
    const w = owner(windows, cur.tab || cur.role);
    // The owning window learns of the step through the other windows' snapshots: let it catch up.
    // The guide may also have moved on meanwhile (a wait step finishing): then just read it again.
    const caughtUp = await w.page
      .locator(`[data-testid="guide-current"][data-step="${cur.step}"]`)
      .waitFor({ timeout: 30_000 })
      .then(() => true, () => false);
    if (!caughtUp) {
      const now = await currentStep(main);
      if (now === 'complete' || (now && now.step !== cur.step)) {
        attempts = 0;
        continue;
      }
      throw new Error(`step ${cur.step} never showed in the ${w.name} window${await errorsOf(windows)}`);
    }
    await w.page.getByTestId('guide-go').click();
    await w.page.getByTestId(cur.action).click({ timeout: 120_000 });
    if (cur.confirm) {
      await w.page.locator(`[data-testid="confirm-dialog"][data-action="${cur.action}"]`).waitFor({ timeout: 30_000 });
      await opts.onConfirm?.(w.page, cur.action);
      await w.page.getByTestId('confirm-ok').click();
    }
    await waitForStepChange(main, cur.step, 30_000);
  }
}

/** Visible action errors and order errors, for failure messages. */
export async function errorsOf(windows: DemoWindow[]): Promise<string> {
  const out: string[] = [];
  for (const w of windows) {
    for (const loc of await w.page.locator('[data-testid$="-error"], [data-testid="order-last-error"]').all()) {
      const t = ((await loc.textContent().catch(() => '')) ?? '').trim();
      if (t) out.push(`${w.name}: ${t}`);
    }
  }
  return out.length ? ` / errors: ${out.join(' | ')}` : '';
}

/** Open the demo and wait until the guide is there. */
export async function openDemo(page: Page, url: string): Promise<void> {
  await page.goto(url);
  await page.getByTestId('guide').waitFor({ timeout: 60_000 });
}

/** Select a scenario and start it over (a new run: nothing latched, the order is created fresh). */
export async function startScenario(page: Page, id: string): Promise<void> {
  await page.getByTestId('scenario-select').selectOption(id);
  await page.getByTestId('scenario-restart').click();
  await page.locator(`[data-testid="guide"][data-scenario="${id}"][data-complete="false"]`).waitFor({ timeout: 30_000 });
}

/** "デモを初期化": wipe the keys and data of every role, then the page reloads. */
export async function resetDemo(page: Page): Promise<void> {
  await page.getByTestId('demo-reset').click();
  await page.locator('[data-testid="confirm-dialog"][data-action="demo-reset"]').waitFor();
  await Promise.all([page.waitForEvent('load', { timeout: 60_000 }), page.getByTestId('confirm-ok').click()]);
  await page.getByTestId('guide').waitFor({ timeout: 60_000 });
}
