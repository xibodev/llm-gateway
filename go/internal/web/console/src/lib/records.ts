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
