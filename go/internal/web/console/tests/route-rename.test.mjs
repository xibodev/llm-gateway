import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { build } from "esbuild";

const { outputFiles } = await build({
  entryPoints: [fileURLToPath(new URL("../src/lib/routes.ts", import.meta.url))],
  bundle: true, write: false, format: "esm", platform: "node",
});
const routes = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);

const existing = ["coding", "Research"];
const keys = [
  { name: "ci", status: "active", allowed_routes: ["coding"] },
  { name: "selector", status: "disabled", allowed_models: ["coding", "openai/gpt"] },
  { name: "revoked", status: "revoked", allowed_routes: ["coding"] },
  { name: "other", status: "active", allowed_routes: ["research"] },
  { name: "portal", status: "active", policy: { allowed_routes: ["coding"] } },
];

test("a new route may not take a name already in use, whatever its letter case", () => {
  assert.deepEqual(routes.planRouteSave("", " drafts ", existing, keys), { kind: "create", name: "drafts" });
  const taken = routes.planRouteSave("", "research", existing, keys);
  assert.equal(taken.kind, "refused");
  assert.match(taken.message, /A route named Research already exists/);
});

test("editing a route under its own name is an update", () => {
  assert.deepEqual(routes.planRouteSave("coding", "coding", existing, keys), { kind: "update", name: "coding" });
});

test("a rename names every live key whose grants still name the old route", () => {
  const plan = routes.planRouteSave("coding", "coding-v2", existing, keys);
  assert.equal(plan.kind, "rename");
  assert.equal(plan.from, "coding");
  assert.equal(plan.name, "coding-v2");
  assert.deepEqual(plan.grantedKeys, ["ci", "selector", "portal"]);
  const message = routes.renameConfirmation(plan);
  assert.match(message, /Rename route coding to coding-v2\?/);
  assert.match(message, /coding is deleted/);
  assert.match(message, /3 API keys name coding in their grants \(ci, selector, portal\)/);
  assert.match(message, /cannot call coding-v2/);
  assert.match(routes.renameConfirmation({ ...plan, grantedKeys: [] }), /No API key names coding in its grants/);
});

test("a rename never lands on another route or on its own name in another case", () => {
  const onto = routes.planRouteSave("coding", "RESEARCH", existing, keys);
  assert.equal(onto.kind, "refused");
  assert.match(onto.message, /A route named Research already exists\. Renaming never replaces another route/);
  const caseOnly = routes.planRouteSave("coding", "Coding", existing, keys);
  assert.equal(caseOnly.kind, "refused");
  assert.match(caseOnly.message, /ignore letter case/);
});

test("a rename creates the new route before deleting the old one", async () => {
  const calls = [];
  const send = async (path, method, body) => { calls.push([method, path, body?.name]); };
  const plan = { kind: "rename", name: "coding v2", from: "coding", grantedKeys: [] };
  const outcome = await routes.commitRouteSave(plan, [{ provider: "p", model: "m" }], "?diagnostics=1", send);
  assert.deepEqual(calls, [["POST", "/endpoints?diagnostics=1", "coding v2"], ["DELETE", "/endpoints/coding", undefined]]);
  assert.equal(outcome, "Route coding was renamed to coding v2.");
});

test("a failed create deletes nothing, and a failed delete names the route left behind", async () => {
  const calls = [];
  const plan = { kind: "rename", name: "next", from: "coding", grantedKeys: [] };
  await assert.rejects(routes.commitRouteSave(plan, [], "", async (path, method) => {
    calls.push(method);
    throw new Error("route member 1 has unknown model");
  }), /unknown model/);
  assert.deepEqual(calls, ["POST"]);

  const outcome = await routes.commitRouteSave(plan, [], "", async (path, method) => {
    if (method === "DELETE") throw new Error("Route configuration could not be persisted.");
  });
  assert.equal(outcome, "Route next was created, but coding could not be deleted: Route configuration could not be persisted. Delete coding from the route list to finish the rename.");
});

test("an update or a create writes only the named route", async () => {
  const calls = [];
  const outcome = await routes.commitRouteSave({ kind: "update", name: "coding" }, [], "", async (path, method) => { calls.push([method, path]); });
  assert.deepEqual(calls, [["POST", "/endpoints"]]);
  assert.equal(outcome, "Route saved in the selected failover order.");
});

test("the route editor saves through the plan and titles a new route as new", () => {
  const page = readFileSync(new URL("../src/pages/Routes.tsx", import.meta.url), "utf8");
  assert.match(page, /planRouteSave\(originalName, name,/);
  assert.match(page, /commitRouteSave\(plan, failover, principalQuery,/);
  assert.match(page, /\{originalName \? `Edit \$\{originalName\}` : "Create route"\}/);
  assert.doesNotMatch(page, /\{name \? `Edit \$\{name\}`/);
});
