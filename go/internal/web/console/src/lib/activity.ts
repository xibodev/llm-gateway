import type { JSONRecord } from "./api";
import { numberValue, stringValue } from "./records";

// Filters of the request listing; an empty value or "all" selects everything.
export type RequestFilter = {
  status: string;
  provider: string;
  model: string;
  keyID: string;
  projectID: string;
  requestID: string;
};

export const emptyRequestFilter: RequestFilter = { status: "all", provider: "all", model: "", keyID: "all", projectID: "all", requestID: "" };

// Filters of the audit listing; an empty value or "all" selects everything.
export type AuditFilter = {
  action: string;
  actor: string;
  result: string;
  targetType: string;
  targetID: string;
};

export const emptyAuditFilter: AuditFilter = { action: "", actor: "", result: "all", targetType: "", targetID: "" };

export const activityPageSize = 50;

function setIf(query: URLSearchParams, name: string, value: string) {
  const trimmed = value.trim();
  if (trimmed && trimmed !== "all") query.set(name, trimmed);
}

// requestsPath is the request listing for filter, after the row beforeID when
// a later page is wanted.
export function requestsPath(filter: RequestFilter, beforeID = 0, limit = activityPageSize): string {
  const query = new URLSearchParams({ limit: String(limit) });
  setIf(query, "status", filter.status);
  setIf(query, "provider", filter.provider);
  setIf(query, "model", filter.model);
  setIf(query, "key_id", filter.keyID);
  setIf(query, "project_id", filter.projectID);
  setIf(query, "request_id", filter.requestID);
  if (beforeID > 0) query.set("before_id", String(beforeID));
  return `/requests?${query.toString()}`;
}

// auditPath is the audit listing for filter, after the event beforeID when a
// later page is wanted.
export function auditPath(filter: AuditFilter, beforeID = 0, limit = activityPageSize): string {
  const query = new URLSearchParams({ limit: String(limit) });
  setIf(query, "action", filter.action);
  setIf(query, "actor", filter.actor);
  setIf(query, "result", filter.result);
  setIf(query, "target_type", filter.targetType);
  setIf(query, "target_id", filter.targetID);
  if (beforeID > 0) query.set("before_id", String(beforeID));
  return `/audit?${query.toString()}`;
}

// statusTone colours a response status: a success, or anything a client
// would have to act on.
export function statusTone(code: number): "ready" | "attention" {
  return code > 0 && code < 400 ? "ready" : "attention";
}

// auditTone colours an audit result: a completed action, or anything else.
export function auditTone(result: string): "ready" | "attention" {
  return result === "" || result === "success" ? "ready" : "attention";
}

// tokenSummary shows the input and output tokens of a request, or a dash
// when it reported none.
export function tokenSummary(request: JSONRecord): string {
  const input = numberValue(request.input_tokens);
  const output = numberValue(request.output_tokens);
  return input || output ? `${input.toLocaleString()} in / ${output.toLocaleString()} out` : "—";
}

// modelSummary shows the model a client asked for and, when routing chose
// another, the model that served it.
export function modelSummary(request: JSONRecord): string {
  const requested = stringValue(request.requested_model);
  const routed = stringValue(request.routed_model);
  if (!routed || routed === requested) return requested || routed || "—";
  return requested ? `${requested} → ${routed}` : routed;
}

// nextCursor reads a listing's cursor of its next page, 0 when none follows.
export function nextCursor(listing: JSONRecord | null): number {
  return listing ? numberValue(listing.next_before_id) : 0;
}

// safeAuditDetail drops detail fields whose names suggest a credential. The
// server never stores one; this keeps a future field from being shown.
export function safeAuditDetail(value: unknown): JSONRecord {
  const detail = value && typeof value === "object" && !Array.isArray(value) ? value as JSONRecord : {};
  const out: JSONRecord = {};
  for (const [key, child] of Object.entries(detail)) {
    const lower = key.toLowerCase();
    if (lower.includes("token") || lower.includes("secret") || lower.includes("authorization") || lower.includes("api_key")) continue;
    out[key] = child;
  }
  return out;
}

// auditActor names who took an audited action: the principal or key, with
// the static administrator key's fingerprint when it was that key.
export function auditActor(event: JSONRecord): string {
  const detail = safeAuditDetail(event.detail);
  const principal = stringValue(event.actor_principal_id);
  const key = stringValue(event.actor_key_id);
  const fingerprint = stringValue(detail.actor_key_fingerprint);
  const source = stringValue(detail.actor_source);
  if (principal) return principal;
  if (key) return key;
  if (fingerprint) return `static key ${fingerprint}`;
  return source || "gateway";
}
