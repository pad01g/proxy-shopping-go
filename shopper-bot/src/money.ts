import type { Money } from "./types.js";

/** A decimal string as an integer and a number of fraction digits. */
interface Fixed {
  units: bigint;
  scale: number;
}

function toFixed(amount: string): Fixed {
  const m = /^(\d+)(?:\.(\d+))?$/.exec(amount.trim());
  if (!m) throw new Error(`not a decimal amount: ${JSON.stringify(amount)}`);
  const frac = m[2] ?? "";
  return { units: BigInt(m[1]! + frac), scale: frac.length };
}

/** Compares two decimal strings exactly: -1, 0 or 1. */
export function compareDecimal(a: string, b: string): number {
  const x = toFixed(a);
  const y = toFixed(b);
  const scale = Math.max(x.scale, y.scale);
  const xa = x.units * 10n ** BigInt(scale - x.scale);
  const ya = y.units * 10n ** BigInt(scale - y.scale);
  return xa < ya ? -1 : xa > ya ? 1 : 0;
}

export function formatMoney(m: Money): string {
  return `${m.amount} ${m.currency}`;
}

/**
 * Returns why `total` may not be paid under `max`, or undefined when it is
 * within the limit. Different currencies are never comparable: the bot does
 * not convert, the shopper node sets max_amount in the shop's currency.
 */
export function exceedsMax(total: Money, max: Money): string | undefined {
  if (total.currency !== max.currency) {
    return `shop total is in ${total.currency} but max_amount is in ${max.currency}`;
  }
  if (compareDecimal(total.amount, max.amount) > 0) {
    return `shop total ${formatMoney(total)} exceeds max_amount ${formatMoney(max)}`;
  }
  return undefined;
}
