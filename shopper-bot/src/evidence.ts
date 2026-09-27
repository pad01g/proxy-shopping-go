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

/** JSON evidence hashes the exact bytes it carries, so it can be re-verified. */
export function jsonEvidence(kind: "receipt" | "json", value: unknown): Evidence {
  return evidenceFromBytes(kind, "application/json", Buffer.from(JSON.stringify(value), "utf8"));
}
