import type { JSONRecord } from "./api";

export function asRecord(value: unknown): JSONRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? value as JSONRecord
    : {};
}

export function asList(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

export function stringValue(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

export function numberValue(value: unknown, fallback = 0): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

// endpointsOf reads the gateway's routing layer out of a state payload.
// "endpoints" is the canonical key; "categories" is the deprecated alias the
// server still mirrors it under and whose removal this branch documents, so a
// view that reads the alias would show zero routes the day it goes. Reading
// canonical-first with the alias as fallback also keeps this console working
// against a server from before the rename.
export function endpointsOf(data: JSONRecord): JSONRecord {
  const canonical = asRecord(data.endpoints);
  return Object.keys(canonical).length > 0 ? canonical : asRecord(data.categories);
}
