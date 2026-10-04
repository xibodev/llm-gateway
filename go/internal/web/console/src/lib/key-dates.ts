import type { JSONRecord } from "./api";
import { numberValue } from "./records";

// keyExpiryFromInput turns the optional expiry field, a local date and time as
// a datetime-local input reports it, into the Unix seconds the key API stores.
// Blank means the key never expires. The server accepts any timestamp, so a
// date that does not exist or is not in the future is refused here.
export function keyExpiryFromInput(value: string, now = Date.now()): { expiresAt: number; error: string } {
  const raw = value.trim();
  if (!raw) return { expiresAt: 0, error: "" };
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(raw);
  const [year, month, day, hour, minute, second] = (match ?? []).slice(1).map((part) => Number(part ?? 0));
  const date = new Date(year, month - 1, day, hour, minute, second);
  if (!match || date.getFullYear() !== year || date.getMonth() !== month - 1 || date.getDate() !== day || date.getHours() !== hour || date.getMinutes() !== minute) {
    return { expiresAt: 0, error: "Enter the expiry as a valid date and time." };
  }
  if (date.getTime() <= now) return { expiresAt: 0, error: "The expiry must be in the future." };
  return { expiresAt: Math.floor(date.getTime() / 1000), error: "" };
}

// keyTimes reads a key's Unix-second timestamps. Admin state names the
// creation time "created" and portal state "created_at"; zero means unset.
export function keyTimes(key: JSONRecord): { created: number; expires: number; lastUsed: number } {
  return {
    created: numberValue(key.created, numberValue(key.created_at)),
    expires: numberValue(key.expires_at),
    lastUsed: numberValue(key.last_used_at),
  };
}

export function formatKeyTime(seconds: number, unset: string): string {
  return seconds > 0 ? new Date(seconds * 1000).toLocaleString([], { dateStyle: "medium", timeStyle: "short" }) : unset;
}
