import { getJSON, type JSONRecord } from "./api";
import type { ConsoleMode } from "./mode";
import { asList, asRecord, numberValue, stringValue } from "./records";
import type { PickerOption } from "../components/DataTable";

// The console reads principals and keys from the server's listings, a page
// at a time, rather than from the state, which lists neither.

export type PrincipalFilter = { kinds?: string[]; status?: string; projectID?: string; ids?: string[]; q?: string; limit?: number; offset?: number; sort?: string; descending?: boolean };

export function principalsPath(filter: PrincipalFilter): string {
  const query = new URLSearchParams();
  for (const id of filter.ids ?? []) query.append("id", id);
  for (const kind of filter.kinds ?? []) query.append("kind", kind);
  if (filter.status) query.set("status", filter.status);
  if (filter.projectID) query.set("project_id", filter.projectID);
  if (filter.q?.trim()) query.set("q", filter.q.trim());
  if (filter.sort) {
    query.set("sort", filter.sort);
    query.set("order", filter.descending ? "desc" : "asc");
  }
  query.set("limit", String(filter.limit ?? 20));
  if (filter.offset) query.set("offset", String(filter.offset));
  return `/principals?${query.toString()}`;
}

// principalLabel names a principal, with its kind when showKind is set, and
// its status when it is not active.
export function principalLabel(principal: JSONRecord, showKind = false): string {
  const name = stringValue(principal.display_name, stringValue(principal.email, stringValue(principal.id)));
  const notes = [showKind ? stringValue(principal.kind) : "", stringValue(principal.status, "active") === "active" ? "" : stringValue(principal.status)].filter(Boolean);
  return notes.length ? `${name} (${notes.join(", ")})` : name;
}

// A search finds the first page of options that match a term; resolve names
// one value. filterKey changes exactly when the search's filter does.
export type RemoteSearch = { search: (term: string) => Promise<{ options: PickerOption[]; total: number }>; resolve: (value: string) => Promise<string | null>; filterKey: string };

// principalSearch searches the principals filter selects, by name.
export function principalSearch(mode: ConsoleMode, filter: PrincipalFilter, { showKind = false } = {}): RemoteSearch {
  return {
    search: async (term) => {
      const payload = await getJSON<JSONRecord>(mode, principalsPath({ ...filter, q: term }));
      return { options: asList(payload.principals).map(asRecord).map((principal) => ({ value: stringValue(principal.id), label: principalLabel(principal, showKind) })), total: numberValue(payload.total) };
    },
    resolve: async (id) => {
      const payload = await getJSON<JSONRecord>(mode, principalsPath({ ids: [id], limit: 1 }));
      const principal = asList(payload.principals).map(asRecord)[0];
      return principal ? principalLabel(principal, showKind) : null;
    },
    filterKey: JSON.stringify(filter),
  };
}

export type KeyFilter = { status?: string; projectID?: string; grant?: string; q?: string; limit?: number; offset?: number; sort?: string; descending?: boolean };

export function keysPath(filter: KeyFilter): string {
  const query = new URLSearchParams({ status: filter.status ?? "all" });
  if (filter.projectID) query.set("project_id", filter.projectID);
  if (filter.grant) query.set("grant", filter.grant);
  if (filter.q?.trim()) query.set("q", filter.q.trim());
  if (filter.sort) {
    query.set("sort", filter.sort);
    query.set("order", filter.descending ? "desc" : "asc");
  }
  query.set("limit", String(filter.limit ?? 20));
  if (filter.offset) query.set("offset", String(filter.offset));
  return `/keys?${query.toString()}`;
}

export function keyLabel(key: JSONRecord): string {
  return stringValue(key.name, stringValue(key.prefix, stringValue(key.id)));
}

// keyCount is how many keys data knows of: the administrator state counts
// every key, and a portal user's lists the user's own.
export function keyCount(data: JSONRecord): number {
  return data.counts === undefined ? asList(data.keys).length : numberValue(asRecord(data.counts).keys);
}

// keySearch searches the keys filter selects, newest first. A key cannot be
// found by its ID, so one the search does not list keeps its ID as its label.
export function keySearch(mode: ConsoleMode, filter: KeyFilter): RemoteSearch {
  return {
    search: async (term) => {
      const payload = await getJSON<JSONRecord>(mode, keysPath({ ...filter, q: term }));
      return { options: asList(payload.keys).map(asRecord).map((key) => ({ value: stringValue(key.id), label: keyLabel(key) })), total: numberValue(payload.total) };
    },
    resolve: async (id) => {
      const payload = await getJSON<JSONRecord>(mode, keysPath({ ...filter, q: id, limit: 1 }));
      const key = asList(payload.keys).map(asRecord).find((candidate) => stringValue(candidate.id) === id);
      return key ? keyLabel(key) : null;
    },
    filterKey: JSON.stringify(filter),
  };
}
