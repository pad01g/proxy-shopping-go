import { describe, expect, it } from "vitest";
import { compareDecimal, exceedsMax } from "../src/money.js";

describe("compareDecimal", () => {
  it.each([
    ["4000", "4000", 0],
    ["4000", "3999", 1],
    ["35.00", "35", 0],
    ["35.01", "35", 1],
    ["9.99", "10.00", -1],
    ["0.000001", "0", 1],
    ["123456789012345678901234567890", "123456789012345678901234567889", 1],
  ])("%s vs %s", (a, b, want) => {
    expect(compareDecimal(a, b)).toBe(want);
  });

  it("rejects non-decimals", () => {
    expect(() => compareDecimal("1e3", "1")).toThrow();
  });
});

describe("exceedsMax", () => {
  it("allows totals up to and including the limit", () => {
    expect(exceedsMax({ amount: "4000", currency: "JPY" }, { amount: "4000", currency: "JPY" })).toBeUndefined();
    expect(exceedsMax({ amount: "35.00", currency: "USD" }, { amount: "40", currency: "USD" })).toBeUndefined();
  });

  it("refuses totals above the limit", () => {
    expect(exceedsMax({ amount: "4000", currency: "JPY" }, { amount: "3999", currency: "JPY" })).toMatch(/exceeds max_amount/);
  });

  it("refuses to compare different currencies", () => {
    expect(exceedsMax({ amount: "1", currency: "USD" }, { amount: "1000", currency: "JPY" })).toMatch(/USD.*JPY/);
  });
});
