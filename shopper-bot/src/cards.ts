import { readFileSync } from "node:fs";
import type { PaymentRef } from "./types.js";

export interface Card {
  number: string;
  exp: string; // MM/YY
  cvc: string;
  name: string;
}

/**
 * Card data lives only in the bot's own file. Requests carry a reference
 * ("card:default"), which is resolved here and nowhere else.
 */
export class CardVault {
  private constructor(private readonly cards: ReadonlyMap<string, Card>) {}

  static fromFile(path: string): CardVault {
    const parsed: unknown = JSON.parse(readFileSync(path, "utf8"));
    return CardVault.fromObject(parsed);
  }

  static fromObject(parsed: unknown): CardVault {
    const cards = new Map<string, Card>();
    const entries = (parsed as { cards?: Record<string, unknown> })?.cards;
    if (!entries || typeof entries !== "object") throw new Error('cards file must look like {"cards": {"default": {...}}}');
    for (const [name, value] of Object.entries(entries)) {
      const c = value as Partial<Card>;
      if (![c.number, c.exp, c.cvc, c.name].every((f) => typeof f === "string" && f !== "")) {
        throw new Error(`card ${name}: number, exp, cvc and name are required`);
      }
      cards.set(name, c as Card);
    }
    return new CardVault(cards);
  }

  /** Resolves "card:<name>"; throws for "cash" or unknown names. */
  resolve(ref: PaymentRef): Card {
    if (!ref.startsWith("card:")) throw new Error(`payment_ref ${ref} is not a card`);
    const card = this.cards.get(ref.slice("card:".length));
    if (!card) throw new Error(`no card configured for ${ref}`);
    return card;
  }

  names(): string[] {
    return [...this.cards.keys()];
  }
}
