import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { PurchaseRequest, PurchaseResult } from "./types.js";

/**
 * Where a purchase stands (spec §9, idempotency by request_id):
 * - "started": the bot began driving the shop, no payment was submitted yet;
 * - "payment_submitted": the card form / cash button was about to be used, so
 *   money may have moved;
 * - "done": result holds the answer every later request with this id gets.
 */
export type PurchaseState = "started" | "payment_submitted" | "done";

export interface PurchaseRecord {
  request_id: string;
  /** sha256 of the canonical request, to tell a retry from a different request reusing the id. */
  fingerprint: string;
  state: PurchaseState;
  started_at: number;
  updated_at: number;
  result?: PurchaseResult;
}

/**
 * One JSON file per request_id under <dir>/purchases, written atomically
 * (temp file + rename), so a crash leaves either the old or the new record.
 */
export class PurchaseStore {
  private readonly dir: string;

  constructor(dataDir: string) {
    this.dir = join(dataDir, "purchases");
    mkdirSync(this.dir, { recursive: true, mode: 0o700 });
  }

  private path(requestId: string): string {
    // request ids are free-form strings; hash them into safe file names
    return join(this.dir, `${createHash("sha256").update(requestId, "utf8").digest("hex")}.json`);
  }

  get(requestId: string): PurchaseRecord | undefined {
    let raw: string;
    try {
      raw = readFileSync(this.path(requestId), "utf8");
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
      throw err;
    }
    return JSON.parse(raw) as PurchaseRecord;
  }

  put(rec: PurchaseRecord): void {
    const path = this.path(rec.request_id);
    const tmp = `${path}.${process.pid}.tmp`;
    writeFileSync(tmp, JSON.stringify(rec), { mode: 0o600 });
    renameSync(tmp, path);
  }
}

/** Hash of the request with sorted keys, independent of the JSON key order the caller used. */
export function fingerprint(req: PurchaseRequest): string {
  return createHash("sha256").update(canonical(req), "utf8").digest("hex");
}

function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v !== null && typeof v === "object") {
    const o = v as Record<string, unknown>;
    return `{${Object.keys(o)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${canonical(o[k])}`)
      .join(",")}}`;
  }
  return JSON.stringify(v);
}
