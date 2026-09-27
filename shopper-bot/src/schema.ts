import { readFileSync, readdirSync } from "node:fs";
import { Ajv2020, type ErrorObject, type ValidateFunction } from "ajv/dist/2020.js";
import type { PurchaseRequest, PurchaseResult, TrackingQuery, TrackingStatus } from "./types.js";

// schema/ sits next to src/ and dist/, so the same relative path works for
// the compiled server and for tests running from source.
const schemaDir = new URL("../schema/", import.meta.url);

const ajv = new Ajv2020({ allErrors: true, strict: true, strictRequired: false }) // "then: required" refers to properties declared at the top level;
for (const file of readdirSync(schemaDir).filter((f) => f.endsWith(".json"))) {
  ajv.addSchema(JSON.parse(readFileSync(new URL(file, schemaDir), "utf8")));
}

function validator<T>(name: string): ValidateFunction<T> {
  const v = ajv.getSchema<T>(`https://github.com/pad01g/proxy-shopping-go/shopper-bot/schema/${name}.json`);
  if (!v) throw new Error(`schema ${name} not found in ${schemaDir.pathname}`);
  return v;
}

export const validate = {
  purchaseRequest: validator<PurchaseRequest>("PurchaseRequest"),
  purchaseResult: validator<PurchaseResult>("PurchaseResult"),
  trackingQuery: validator<TrackingQuery>("TrackingQuery"),
  trackingStatus: validator<TrackingStatus>("TrackingStatus"),
};

/** Human-readable summary of ajv errors, e.g. "/items/0/qty must be >= 1". */
export function describeErrors(errors: ErrorObject[] | null | undefined): string {
  return (errors ?? []).map((e) => `${e.instancePath || "/"} ${e.message ?? "is invalid"}`).join("; ");
}
