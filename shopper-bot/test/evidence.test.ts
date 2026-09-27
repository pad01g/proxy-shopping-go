import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import { maxScreenshotBytes, screenshotOf, type Screenshotter } from "../src/evidence.js";

type Opts = Parameters<Screenshotter["screenshot"]>[0];

function fakePage(size: (o: Opts) => number) {
  const calls: Opts[] = [];
  const page: Screenshotter = {
    screenshot: async (o) => (calls.push(o), new Uint8Array(size(o)).fill(7)),
  };
  return { page, calls };
}

describe("screenshot evidence", () => {
  it("takes the viewport, not the full page, as PNG when it is small enough", async () => {
    const { page, calls } = fakePage(() => 50_000);
    const ev = await screenshotOf(page);
    expect(calls).toEqual([{ fullPage: false, type: "png" }]);
    expect(ev.mime).toBe("image/png");
    const data = Buffer.from(ev.data_b64!, "base64");
    expect(data.length).toBe(50_000);
    expect(createHash("sha256").update(data).digest("hex")).toBe(ev.sha256);
  });

  it("falls back to JPEG quality 70 when the PNG is too big", async () => {
    const { page, calls } = fakePage((o) => (o.type === "png" ? 900_000 : 150_000));
    const ev = await screenshotOf(page);
    expect(calls.at(-1)).toEqual({ fullPage: false, type: "jpeg", quality: 70 });
    expect(ev.mime).toBe("image/jpeg");
    expect(Buffer.from(ev.data_b64!, "base64").length).toBeLessThanOrEqual(maxScreenshotBytes);
  });

  it("keeps only the hash when nothing fits", async () => {
    const { page } = fakePage(() => 900_000);
    const ev = await screenshotOf(page);
    expect(ev.data_b64).toBeUndefined();
    expect(ev.sha256).toMatch(/^[0-9a-f]{64}$/);
  });
});
