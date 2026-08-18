import { apiBaseForMode, type ConsoleMode } from "./mode";

export type JSONRecord = Record<string, unknown>;

const staticAdminStorageKey = "llmgw.console.static-admin-key";

export class APIError extends Error {
  readonly status: number;
  readonly detail: unknown;
  readonly code: string;

  constructor(status: number, message: string, detail: unknown, code = "") {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.detail = detail;
    this.code = code;
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
    const message = typeof errorValue === "string"
      ? errorValue
      : typeof errorValue === "object" && errorValue !== null && "message" in errorValue
        ? String((errorValue as JSONRecord).message)
        : `Request failed with status ${response.status}.`;
    throw new APIError(response.status, message, payload);
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

export function sendJSON<T>(mode: ConsoleMode, path: string, method: "POST" | "DELETE", body?: unknown): Promise<T> {
  return requestJSON<T>(mode, path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}
