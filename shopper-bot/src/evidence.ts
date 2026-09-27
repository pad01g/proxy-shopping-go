import { createHash } from "node:crypto";
import type { Evidence, EvidenceKind } from "./types.js";

export function sha256Hex(data: Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

export function evidenceFromBytes(kind: EvidenceKind, mime: string, data: Uint8Array): Evidence {
  return { kind, mime, sha256: sha256Hex(data), data_b64: Buffer.from(data).toString("base64") };
}

export function screenshotEvidence(png: Uint8Array): Evidence {
  return evidenceFromBytes("screenshot", "image/png", png);
}

/** Upper bound for one screenshot: it travels to the escrow as §4.9 attachments of 12 KiB each. */
export const maxScreenshotBytes = 200 * 1024;

/** The subset of a Playwright page that screenshots need. */
export interface Screenshotter {
  screenshot(opts: { fullPage: false; type: "png" } | { fullPage: false; type: "jpeg"; quality: number }): Promise<Uint8Array>;
}

/**
 * The visible viewport (never the full page, which can be arbitrarily long)
 * as PNG; if that is over maxScreenshotBytes, as JPEG of falling quality.
 * If nothing fits, the evidence keeps only the hash of the smallest attempt.
 */
export async function screenshotOf(page: Screenshotter): Promise<Evidence> {
  const png = await page.screenshot({ fullPage: false, type: "png" });
  if (png.length <= maxScreenshotBytes) return evidenceFromBytes("screenshot", "image/png", png);
  let jpeg = png;
  for (const quality of [70, 40]) {
    jpeg = await page.screenshot({ fullPage: false, type: "jpeg", quality });
    if (jpeg.length <= maxScreenshotBytes) return evidenceFromBytes("screenshot", "image/jpeg", jpeg);
  }
  return { kind: "screenshot", mime: "image/jpeg", sha256: sha256Hex(jpeg) };
}

/** JSON evidence hashes the exact bytes it carries, so it can be re-verified. */
export function jsonEvidence(kind: "receipt" | "json", value: unknown): Evidence {
  return evidenceFromBytes(kind, "application/json", Buffer.from(JSON.stringify(value), "utf8"));
}
