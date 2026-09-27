import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";

const synced: number[] = [];
vi.mock("node:fs", async (importOriginal) => {
  const fs = await importOriginal<typeof import("node:fs")>();
  return {
    ...fs,
    fsyncSync: (fd: number) => {
      synced.push(fd);
      return fs.fsyncSync(fd);
    },
  };
});

const { PurchaseStore } = await import("../src/store.js");

describe("PurchaseStore", () => {
  it("syncs the record and its directory before put returns", () => {
    const store = new PurchaseStore(mkdtempSync(join(tmpdir(), "bot-store-")));
    synced.length = 0;
    store.put({ request_id: "r-1", fingerprint: "f", state: "payment_submitted", started_at: 1, updated_at: 2 });
    // the file (before the rename) and the directory (after it)
    expect(synced.length).toBeGreaterThanOrEqual(2);
    expect(store.get("r-1")?.state).toBe("payment_submitted");
  });
});
