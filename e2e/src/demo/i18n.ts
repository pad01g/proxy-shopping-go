// 英語表示（?lang=en）の確認: 画面の文言に日本語（ひらがな・カタカナ・漢字、和文の記号と全角文字）が残っていないこと。
// data-i18n-exempt の付いた要素（ネットワークやチェーンから来たデータ、言語名「日本語」）は見ない。
import type { Page } from 'playwright';

/** Hiragana, Katakana, CJK ideographs, plus CJK punctuation (、。「」・) and full-width forms (（）：). */
const JAPANESE = '[\\u3000-\\u303F\\u3040-\\u309F\\u30A0-\\u30FF\\u3400-\\u4DBF\\u4E00-\\u9FFF\\uFF00-\\uFFEF]';

/**
 * Runs in the page: Japanese text inside the elements matching the selectors (text nodes, visible attributes and
 * input values), skipping [data-i18n-exempt] subtrees. Plain JS in a string: tsx would wrap named functions in a
 * helper (__name) that does not exist in the page.
 */
const FIND_JAPANESE = `(({ selectors, pattern }) => {
  const re = new RegExp(pattern);
  const found = new Set();
  const exempt = (el) => !!el.closest('[data-i18n-exempt]');
  const where = (el) => {
    const owner = el.closest('[data-testid]');
    return (owner ? '[data-testid="' + owner.getAttribute('data-testid') + '"] ' : '') + '<' + el.tagName.toLowerCase() + '>';
  };
  const note = (el, what, text) => found.add(where(el) + ' ' + what + ': ' + text.trim().replace(/\\s+/g, ' ').slice(0, 120));
  for (const sel of selectors) {
    for (const root of document.querySelectorAll(sel)) {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        const el = n.parentElement;
        if (!el || exempt(el) || el.closest('script, style')) continue;
        if (re.test(n.nodeValue || '')) note(el, 'text', n.nodeValue || '');
      }
      for (const el of [root, ...root.querySelectorAll('*')]) {
        if (exempt(el)) continue;
        for (const a of ['title', 'placeholder', 'aria-label', 'alt']) {
          const v = el.getAttribute(a);
          if (v && re.test(v)) note(el, a, v);
        }
        if ((el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') && re.test(el.value)) note(el, 'value', el.value);
      }
    }
  }
  return [...found];
})`;

/** Japanese text inside the elements matching `selectors`, as "<where> <what>: <text>" lines. */
export async function japaneseIn(page: Page, selectors: string[]): Promise<string[]> {
  return (await page.evaluate(`${FIND_JAPANESE}(${JSON.stringify({ selectors, pattern: JAPANESE })})`)) as string[];
}

/** Fail when the page shows Japanese UI text in English mode (`where` names the moment for the error). */
export async function assertNoJapanese(page: Page, where: string, selectors: string[]): Promise<void> {
  const found = await japaneseIn(page, selectors);
  if (found.length) throw new Error(`Japanese text in English mode (${where}): ${found.slice(0, 12).join(' | ')}${found.length > 12 ? ` … (${found.length} in all)` : ''}`);
}

/** What the guide shows at every step: the header (brand, scenario picker, buttons), the guide, the tabs and the open panel. */
export const STEP_AREAS = ['header.top', '[data-testid="guide"]', 'nav.tabs', '[data-testid="role-about"]', 'main'];

/** Checks of the English page: <html lang>, the title, and every tab's panel. */
export async function assertEnglishPage(page: Page, tabs: string[]): Promise<string> {
  const lang = (await page.evaluate('document.documentElement.lang')) as string;
  if (lang !== 'en') throw new Error(`<html lang> is ${lang}, not en`);
  const title = await page.title();
  if (new RegExp(JAPANESE).test(title)) throw new Error(`title in Japanese: ${title}`);
  for (const t of tabs) {
    await page.getByTestId(`tab-${t}`).click();
    await page.getByTestId(`panel-${t}`).waitFor();
    // Let the panel's live data (balances, cases, node orders) load before reading it.
    await page.waitForTimeout(1500);
    await assertNoJapanese(page, `${t} tab`, ['body']);
  }
  const exempt = await page.locator('[data-i18n-exempt]').count();
  return `English page: <html lang="en">, title "${title}", no Japanese in ${tabs.length} tabs (${exempt} data-i18n-exempt elements skipped)`;
}
