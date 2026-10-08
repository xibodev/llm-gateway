import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";
import { fakeKeyListing } from "./key-listing.mjs";

const dates = await bundle(fileURLToPath(new URL("../src/lib/key-dates.ts", import.meta.url)));
const { ApiKeys } = await bundle(fileURLToPath(new URL("../src/pages/ApiKeys.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);\nexport const getJSON = (...args) => globalThis.__api.getJSON(...args);" },
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

test("an expiry shows in the field as the local date and time it reads back as", () => {
  assert.equal(dates.keyExpiryInputValue(0), "");
  const expiry = new Date(2026, 11, 31, 23, 59).getTime() / 1000;
  assert.equal(dates.keyExpiryInputValue(expiry), "2026-12-31T23:59");
  assert.equal(dates.keyExpiryFromInput(dates.keyExpiryInputValue(expiry), now).expiresAt, expiry);
  assert.equal(dates.keyExpiryInputValue(new Date(2027, 0, 2, 3, 4, 59).getTime() / 1000), "2027-01-02T03:04");
});

const keys = [{ id: "key-1", name: "ci", status: "active", project_id: "project-1", principal_id: "user-1", created: 1798700000, expires_at: 0, last_used_at: 1798783140 }];
const data = {
  projects: [{ id: "project-1", name: "Project one", status: "active" }],
  memberships: [{ project_id: "project-1", principal_id: "user-1", role: "owner", principal_name: "Ada", principal_kind: "human", principal_status: "active" }],
  counts: { keys: keys.length },
};

function keysPage() {
  const sent = [];
  const listing = fakeKeyListing(keys);
  globalThis.__api = {
    getJSON: async (mode, path) => listing.answer(path),
    sendJSON: async (mode, path, method, body) => { sent.push({ path, method, body }); return { token: "fixture-token" }; },
  };
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
  render();
  await settle();
  const tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.length, 1);
  assert.equal("expires_at" in sent[0].body, false);
  const headings = findAll(tree, (node) => node.type === "th").map(text);
  assert.deepEqual(headings.slice(6, 10), ["Status", "Created", "Expires", "Last used"]);
  const cells = findAll(find(tree, (node) => node.type === "tr" && node.key === "key-1"), (node) => node.type === "td").map(text);
  assert.deepEqual(cells.slice(7, 10), [dates.formatKeyTime(1798700000, "—"), "Never", dates.formatKeyTime(1798783140, "Never")]);
});

// A narrow keys table scrolls: its prefixes stay on one line and its row
// actions keep their width, with rules that outrank base.css, which loads
// after the keys styles.
test("the keys table keeps prefixes on one line and its actions within it", async () => {
  const css = readFileSync(new URL("../src/styles/keys.css", import.meta.url), "utf8");
  assert.match(css, /\.key-list-table \.key-value \.technical \{[^}]*white-space: nowrap/);
  assert.match(css, /\.key-list-table \.table-actions \{[^}]*width: max-content/);
  const { render } = keysPage();
  render();
  await settle();
  const table = find(render(), (node) => node.type === "table");
  assert.equal(table.props.class, "key-list-table");
  assert.ok(findAll(table, (node) => node.props?.class === "key-value").length > 0);
  assert.ok(findAll(table, (node) => node.props?.class === "table-actions").length > 0);
});

// An active key's expiry is edited with its policy and sent only when it
// changed; an expired key stays expired, so its editor offers no expiry.
test("an active key's expiry can change and an expired key's cannot", async () => {
  const later = new Date(2999, 0, 1, 10, 30).getTime() / 1000;
  const keys = [
    { id: "key-live", name: "live", status: "active", project_id: "project-1", principal_id: "user-1", created: 1798700000, expires_at: later },
    { id: "key-gone", name: "gone", status: "active", project_id: "project-1", principal_id: "user-1", created: 1798700000, expires_at: 1000000000 },
  ];
  const sent = [];
  const listing = fakeKeyListing(keys);
  globalThis.__api = {
    getJSON: async (mode, path) => listing.answer(path),
    sendJSON: async (mode, path, method, body) => { sent.push({ path, method, body }); return { ok: true }; },
  };
  const render = mount(() => ApiKeys({ data: { ...data, keys }, mode: "admin", onChanged: async () => {} }));
  render();
  await settle();
  let tree = render();
  const editKey = (name) => { find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === `Edit ${name}`).props.onClick(); tree = render(); };
  const field = () => find(tree, (node) => node.props?.name === "expires_at");

  editKey("live");
  assert.equal(field().props.value, "2999-01-01T10:30");
  submit(tree);
  await settle();
  assert.equal(sent.at(-1).path, "/keys/update");
  assert.equal("expires_at" in sent.at(-1).body, false, "an untouched expiry is not sent");

  editKey("live");
  field().props.onInput(input("2999-06-01T08:00"));
  tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.at(-1).body.expires_at, new Date(2999, 5, 1, 8, 0).getTime() / 1000);

  editKey("live");
  field().props.onInput(input(""));
  tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.at(-1).body.expires_at, 0, "a cleared expiry means the key never expires");

  // Expired keys are listed once the status filter shows them.
  find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "all").length).props.onChange(input("all"));
  render();
  await settle();
  tree = render();
  editKey("gone");
  assert.equal(findAll(tree, (node) => node.props?.name === "expires_at").length, 0);
  assert.match(text(tree), /An expired key stays expired; create a new key to replace it\./);
  submit(tree);
  await settle();
  assert.equal(sent.at(-1).body.id, "key-gone");
  assert.equal("expires_at" in sent.at(-1).body, false);
});
