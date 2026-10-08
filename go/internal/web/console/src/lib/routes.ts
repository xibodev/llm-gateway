import type { JSONRecord } from "./api";
import { keyLabel, keysPath } from "./directory";
import { asList, asRecord, numberValue } from "./records";

export type RouteSavePlan =
  | { kind: "refused"; message: string }
  | { kind: "create" | "update"; name: string }
  | { kind: "rename"; name: string; from: string };

export type RouteRequest = (path: string, method: "POST" | "DELETE", body?: unknown) => Promise<unknown>;

const sameRouteName = (left: string, right: string) => left.toLowerCase() === right.toLowerCase();

// planRouteSave decides what saving the route editor means. Saving is an upsert
// keyed on the name, and the server compares names without letter case, so a
// rename is a create plus a delete of the original, and neither a new route nor
// a rename may land on a name another route already uses.
export function planRouteSave(originalName: string, draftName: string, routeNames: string[]): RouteSavePlan {
  const name = draftName.trim();
  if (originalName && name === originalName) return { kind: "update", name };
  if (originalName && sameRouteName(name, originalName)) {
    return { kind: "refused", message: `Route names ignore letter case, so ${name} still names ${originalName}. Keep the current name or choose a different one.` };
  }
  const taken = routeNames.find((existing) => existing !== originalName && sameRouteName(existing, name));
  if (taken) {
    return { kind: "refused", message: originalName
      ? `A route named ${taken} already exists. Renaming never replaces another route; choose a different name.`
      : `A route named ${taken} already exists. Edit it from the route list, or choose a different name.` };
  }
  if (!originalName) return { kind: "create", name };
  return { kind: "rename", name, from: originalName };
}

// GrantedKeys are the unrevoked keys whose grants name a route: the names of
// the first few, and how many there are in all.
export type GrantedKeys = { names: string[]; total: number };

// keysGrantingRoute asks the server for the unrevoked keys whose allowed
// routes or allowed models name a route. Grants store route names, so they
// keep naming the old route after a rename.
export async function keysGrantingRoute(load: (path: string) => Promise<JSONRecord>, route: string): Promise<GrantedKeys> {
  const payload = await load(keysPath({ grant: route, status: "unrevoked", limit: 5 }));
  return { names: asList(payload.keys).map(asRecord).map(keyLabel), total: numberValue(payload.total) };
}

export function renameConfirmation(plan: { name: string; from: string }, granted: GrantedKeys): string {
  const count = granted.total;
  const unnamed = count - granted.names.length;
  const shown = granted.names.join(", ") + (unnamed > 0 ? `, and ${unnamed} more` : "");
  const keys = count
    ? `${count} API key${count === 1 ? " names" : "s name"} ${plan.from} in ${count === 1 ? "its" : "their"} grants (${shown}). Grants are not moved: until ${count === 1 ? "that key is" : "those keys are"} updated on the API keys page, ${count === 1 ? "it" : "they"} cannot call ${plan.name}.`
    : `No API key names ${plan.from} in its grants.`;
  return `Rename route ${plan.from} to ${plan.name}? ${plan.name} is created with this failover chain, then ${plan.from} is deleted, so clients calling ${plan.from} must switch to ${plan.name}. ${keys} Project policies that list ${plan.from} are not updated either.`;
}

// commitRouteSave writes a planned save. A rename creates the new route before
// deleting the original, so a failure never leaves neither route in place; when
// only the delete fails, the outcome names the route that is left to remove.
export async function commitRouteSave(plan: Exclude<RouteSavePlan, { kind: "refused" }>, failover: JSONRecord[], principalQuery: string, send: RouteRequest): Promise<string> {
  await send(`/endpoints${principalQuery}`, "POST", { name: plan.name, failover });
  if (plan.kind !== "rename") return "Route saved in the selected failover order.";
  try {
    await send(`/endpoints/${encodeURIComponent(plan.from)}`, "DELETE");
  } catch (cause) {
    const detail = (cause instanceof Error ? cause.message : "the request failed").replace(/[.\s]+$/, "");
    return `Route ${plan.name} was created, but ${plan.from} could not be deleted: ${detail}. Delete ${plan.from} from the route list to finish the rename.`;
  }
  return `Route ${plan.from} was renamed to ${plan.name}.`;
}
