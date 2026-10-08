import { apiBaseForMode, type ConsoleMode } from "./mode";

export type JSONRecord = Record<string, unknown>;

const staticAdminStorageKey = "llmgw.console.static-admin-key";

export class APIError extends Error {
  readonly status: number;
  readonly detail: unknown;
  readonly code: string;
  readonly retryAfter: string;
  readonly retryable: boolean | null;
  readonly action: string;

  constructor(status: number, message: string, detail: unknown, code = "", retryAfter = "", retryable: boolean | null = null, action = "") {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.detail = detail;
    this.code = code;
    this.retryAfter = retryAfter;
    this.retryable = retryable;
    this.action = action;
  }
}

export const authenticationRedirectCode = "authentication_redirect";
let authenticationRedirectStarted = false;

function startBrowserAuthentication(mode: ConsoleMode): void {
  if (typeof window === "undefined" || authenticationRedirectStarted) return;
  authenticationRedirectStarted = true;
  window.location.assign(mode === "portal" ? "/portal" : "/admin");
}

function adminSessionStorage(): Storage | null {
  if (typeof window === "undefined") return null;
  try { return window.sessionStorage; }
  catch { return null; }
}

export function hasStaticAdminKey(): boolean {
  return Boolean(adminSessionStorage()?.getItem(staticAdminStorageKey)?.trim());
}

export function storeStaticAdminKey(key: string): void {
  const storage = adminSessionStorage();
  const value = key.trim();
  if (!storage) return;
  if (value) storage.setItem(staticAdminStorageKey, value);
  else storage.removeItem(staticAdminStorageKey);
}

export function clearStaticAdminKey(): void {
  adminSessionStorage()?.removeItem(staticAdminStorageKey);
}

function staticAdminKey(): string {
  return adminSessionStorage()?.getItem(staticAdminStorageKey)?.trim() ?? "";
}

function apiPath(mode: ConsoleMode, path: string): string {
  return `${apiBaseForMode(mode)}${path.startsWith("/") ? path : `/${path}`}`;
}

async function decode(response: Response): Promise<unknown> {
  const contentType = response.headers.get("content-type") ?? "";
  if (!contentType.includes("application/json")) {
    return response.text();
  }
  return response.json();
}

export async function requestJSON<T>(mode: ConsoleMode, path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  // FormData must set its own multipart boundary; only JSON bodies are typed here.
  if (init.body && !headers.has("Content-Type") && !(init.body instanceof FormData)) {
    headers.set("Content-Type", "application/json");
  }
  if (mode === "admin") {
    const key = staticAdminKey();
    if (key) headers.set("Authorization", `Bearer ${key}`);
  }
  const response = await fetch(apiPath(mode, path), {
    ...init,
    credentials: "same-origin",
    headers,
    redirect: init.redirect ?? "manual",
  });
  return answerOf<T>(mode, response);
}

// answerOf reads a gateway answer as JSON, or rejects with the APIError it
// carries, a sign-in it needs, or one for an answer that is not JSON.
async function answerOf<T>(mode: ConsoleMode, response: Response): Promise<T> {
  const contentType = response.headers.get("content-type") ?? "";
  const finalURL = new URL(response.url || window.location.href, window.location.href);
  if (response.type === "opaqueredirect" ||
    (response.status >= 300 && response.status < 400) ||
    (response.redirected && (
    finalURL.origin !== window.location.origin ||
    finalURL.pathname.includes("/if/flow/") ||
    finalURL.pathname.includes("/application/o/authorize/")
    ))) {
    startBrowserAuthentication(mode);
    throw new APIError(
      401,
      "Your browser session needs authentication.",
      { final_url: finalURL.origin + finalURL.pathname },
      authenticationRedirectCode,
    );
  }
  const payload = await decode(response);
  if (!response.ok) {
    const detail = typeof payload === "object" && payload !== null ? payload : {};
    const errorValue = typeof detail === "object" && "error" in detail ? (detail as JSONRecord).error : undefined;
    const errorRecord = typeof errorValue === "object" && errorValue !== null ? errorValue as JSONRecord : {};
    const message = typeof errorValue === "string"
      ? errorValue
      : "message" in errorRecord
        ? String(errorRecord.message)
        : `Request failed with status ${response.status}.`;
    throw new APIError(
      response.status,
      message,
      payload,
      String(errorRecord.code ?? ""),
      response.headers.get("Retry-After") ?? "",
      typeof errorRecord.retryable === "boolean" ? errorRecord.retryable : null,
      typeof errorRecord.action === "string" ? errorRecord.action : "",
    );
  }
  if (!contentType.includes("application/json")) {
    if (contentType.includes("text/html")) {
      startBrowserAuthentication(mode);
      throw new APIError(
        401,
        "Your browser session needs authentication.",
        { content_type: contentType },
        authenticationRedirectCode,
      );
    }
    throw new APIError(
      502,
      "The gateway returned an unexpected non-JSON response.",
      { content_type: contentType },
      "unexpected_response",
    );
  }
  return payload as T;
}

export function getJSON<T>(mode: ConsoleMode, path: string): Promise<T> {
  return requestJSON<T>(mode, path);
}

export function sendJSON<T>(mode: ConsoleMode, path: string, method: "POST" | "DELETE", body?: unknown, signal?: AbortSignal): Promise<T> {
  return requestJSON<T>(mode, path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
}

// sendForm posts multipart/form-data. requestJSON deliberately leaves
// Content-Type unset for a FormData body so the browser can supply the boundary.
export function sendForm<T>(mode: ConsoleMode, path: string, body: FormData): Promise<T> {
  return requestJSON<T>(mode, path, { method: "POST", body });
}

// A ServerSentEvent is one event of a stream the gateway sends: its name and
// its JSON data.
export type ServerSentEvent = { event: string; data: JSONRecord };

// takeServerSentEvents splits the events a stream's text holds so far from
// the text after them, which ends no event yet. An event whose data is not a
// JSON object is skipped.
export function takeServerSentEvents(text: string): { events: ServerSentEvent[]; rest: string } {
  const records = text.replace(/\r\n/g, "\n").split("\n\n");
  const rest = records.pop() ?? "";
  const events: ServerSentEvent[] = [];
  for (const record of records) {
    let event = "message";
    const data: string[] = [];
    for (const line of record.split("\n")) {
      if (line.startsWith("event:")) event = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data.push(line.slice("data:".length).replace(/^ /, ""));
    }
    if (!data.length) continue;
    try {
      const parsed: unknown = JSON.parse(data.join("\n"));
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) events.push({ event, data: parsed as JSONRecord });
    } catch { /* Not JSON: no event the console reads. */ }
  }
  return { events, rest };
}

// streamEvents posts body as JSON and calls onEvent with each event of the
// stream the answer opens, as it arrives. An answer that opens no stream
// rejects as requestJSON's do; aborting signal ends the stream.
export async function streamEvents(
  mode: ConsoleMode, path: string, body: unknown,
  onEvent: (event: ServerSentEvent) => void, signal?: AbortSignal,
): Promise<void> {
  const headers = new Headers({ Accept: "text/event-stream", "Content-Type": "application/json" });
  if (mode === "admin") {
    const key = staticAdminKey();
    if (key) headers.set("Authorization", `Bearer ${key}`);
  }
  const response = await fetch(apiPath(mode, path), {
    method: "POST", body: JSON.stringify(body), credentials: "same-origin", headers, redirect: "manual", signal,
  });
  if (!response.ok || !response.body || !(response.headers.get("content-type") ?? "").includes("text/event-stream")) {
    await answerOf<JSONRecord>(mode, response);
    throw new APIError(502, "The gateway answered without a stream.", null, "unexpected_response");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let text = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    text += decoder.decode(value, { stream: true });
    const taken = takeServerSentEvents(text);
    text = taken.rest;
    for (const event of taken.events) onEvent(event);
  }
}
