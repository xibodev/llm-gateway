import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const dates = await bundle(fileURLToPath(new URL("../src/lib/key-dates.ts", import.meta.url)));
const { ApiKeys } = await bundle(fileURLToPath(new URL("../src/pages/ApiKeys.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" },
]);
const now = new Date(2026, 9, 3, 12, 0).getTime();

test("a blank expiry creates a key that never expires", () => {
  assert.deepEqual(dates.keyExpiryFromInput(""), { expiresAt: 0, error: "" });
  assert.deepEqual(dates.keyExpiryFromInput("   "), { expiresAt: 0, error: "" });
});

test("an expiry is the chosen local date and time in Unix seconds", () => {
  assert.deepEqual(dates.keyExpiryFromInput("2026-12-31T23:59", now), { expiresAt: new Date(2026, 11, 31, 23, 59).getTime() / 1000, error: "" });
  assert.equal(dates.keyExpiryFromInput("2026-12-31T23:59:30", now).expiresAt, new Date(2026, 11, 31, 23, 59, 30).getTime() / 1000);
});

test("past, impossible and malformed expiries are refused", () => {
  assert.match(dates.keyExpiryFromInput("2026-10-03T12:00", now).error, /must be in the future/);
  assert.match(dates.keyExpiryFromInput("2025-01-01T00:00", now).error, /must be in the future/);
  for (const value of ["2026-02-31T10:00", "2026-13-01T00:00", "2026-12-31", "tomorrow", "1798783140"]) {
    assert.match(dates.keyExpiryFromInput(value, now).error, /valid date and time/, value);
  }
});

test("key timestamps read both the admin and the portal field names", () => {
  assert.deepEqual(dates.keyTimes({ created: 10, expires_at: 20, last_used_at: 30 }), { created: 10, expires: 20, lastUsed: 30 });
  assert.deepEqual(dates.keyTimes({ created_at: 11 }), { created: 11, expires: 0, lastUsed: 0 });
  assert.equal(dates.formatKeyTime(0, "Never"), "Never");
  assert.equal(dates.formatKeyTime(1798783140, "Never"), new Date(1798783140000).toLocaleString([], { dateStyle: "medium", timeStyle: "short" }));
});

const data = {
  projects: [{ id: "project-1", name: "Project one", status: "active" }],
  principals: [{ id: "user-1", kind: "human", status: "active", display_name: "Ada" }],
  memberships: [{ project_id: "project-1", principal_id: "user-1", role: "owner", status: "active" }],
  keys: [{ id: "key-1", name: "ci", status: "active", project_id: "project-1", principal_id: "user-1", created: 1798700000, expires_at: 0, last_used_at: 1798783140 }],
};

function keysPage() {
  const sent = [];
  globalThis.__api = { sendJSON: async (mode, path, method, body) => { sent.push({ path, method, body }); return { token: "fixture-token" }; } };
  const render = mount(() => ApiKeys({ data, mode: "admin", onChanged: async () => {}, initialContext: { projectID: "project-1" } }));
  return { sent, render };
}
const submit = (tree) => find(tree, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });

test("a new key carries the chosen expiry and an invalid one is never sent", async () => {
  const { sent, render } = keysPage();
  let tree = render();
  find(tree, (node) => node.props?.name === "expires_at").props.onInput(input("2001-01-01T00:00"));
  tree = render();
  submit(tree);
  await settle();
  tree = render();
  assert.equal(sent.length, 0);
  assert.match(text(find(tree, (node) => node.props?.role === "status")), /The expiry must be in the future\./);

  find(tree, (node) => node.props?.name === "expires_at").props.onInput(input("2999-01-01T00:00"));
  tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.length, 1);
  assert.equal(sent[0].path, "/keys");
  assert.equal(sent[0].body.expires_at, new Date(2999, 0, 1).getTime() / 1000);
});

test("a key without an expiry omits it, and the list shows created, expiry and last use", async () => {
  const { sent, render } = keysPage();
  const tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.length, 1);
  assert.equal("expires_at" in sent[0].body, false);
  const headings = findAll(tree, (node) => node.type === "th").map(text);
  assert.deepEqual(headings.slice(5, 9), ["Status", "Created", "Expires", "Last used"]);
  const cells = findAll(find(tree, (node) => node.type === "tr" && node.key === "key-1"), (node) => node.type === "td").map(text);
  assert.deepEqual(cells.slice(6, 9), [dates.formatKeyTime(1798700000, "—"), "Never", dates.formatKeyTime(1798783140, "Never")]);
});
