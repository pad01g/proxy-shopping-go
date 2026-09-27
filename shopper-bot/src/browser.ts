import { chromium, type Browser, type Page } from "playwright";

export interface BrowserOptions {
  headless: boolean;
  hostResolverRules?: string;
  actionTimeoutMs: number;
}

/**
 * One Chromium process shared by all jobs; every job gets its own context,
 * so cookies and carts never leak between purchases.
 */
export class BrowserPool {
  private browser?: Promise<Browser>;

  constructor(private readonly opts: BrowserOptions) {}

  private launch(): Promise<Browser> {
    if (!this.browser) {
      const args = this.opts.hostResolverRules ? [`--host-resolver-rules=${this.opts.hostResolverRules}`] : [];
      const launching = chromium.launch({ headless: this.opts.headless, args });
      launching.then(
        (b) => b.on("disconnected", () => (this.browser = undefined)),
        () => (this.browser = undefined),
      );
      this.browser = launching;
    }
    return this.browser;
  }

  /**
   * Runs fn with a fresh page. After timeoutMs the context is closed, which
   * makes whatever fn is waiting on fail.
   */
  async withPage<T>(timeoutMs: number, fn: (page: Page) => Promise<T>): Promise<T> {
    const browser = await this.launch();
    // The lab's shops sit behind Caddy's internal CA.
    const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 900 } });
    context.setDefaultTimeout(this.opts.actionTimeoutMs);
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      void context.close();
    }, timeoutMs);
    try {
      return await fn(await context.newPage());
    } catch (err) {
      if (timedOut) throw new Error(`gave up after ${timeoutMs} ms`, { cause: err });
      throw err;
    } finally {
      clearTimeout(timer);
      await context.close().catch(() => {});
    }
  }

  async close(): Promise<void> {
    const b = this.browser;
    this.browser = undefined;
    if (b) await (await b).close().catch(() => {});
  }
}
