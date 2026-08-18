export type ConsoleMode = "admin" | "portal";

export function modeForPath(pathname = window.location.pathname): ConsoleMode {
  return pathname === "/portal" || pathname.startsWith("/portal/")
    ? "portal"
    : "admin";
}

export function apiBaseForMode(mode: ConsoleMode): string {
  return mode === "portal" ? "/user/api" : "/admin/api";
}
