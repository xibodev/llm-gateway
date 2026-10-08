import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount, settle, text } from "./hook-harness.mjs";

const { ApiKeys } = await bundle(fileURLToPath(new URL("../src/pages/ApiKeys.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);\nexport const getJSON = (...args) => globalThis.__api.getJSON(...args);" },
]);

const key = (id, status, expiresAt = 0) => ({ id, name: id, status, project_id: "project-1", principal_id: "user-1", created: 1798700000, expires_at: expiresAt });
const data = {
  projects: [{ id: "project-1", name: "Project one", status: "active" }],
  principals: [{ id: "user-1", kind: "human", status: "active", display_name: "Ada" }],
  memberships: [{ project_id: "project-1", principal_id: "user-1", role: "owner", status: "active" }],
  keys: [key("live", "active"), key("paused", "disabled"), key("gone", "revoked"), key("lapsed", "active", 1000000000), key("old", "revoked")],
};

function keysPage() {
  const sent = [];
  const confirmations = [];
  globalThis.window = { confirm: (message) => { confirmations.push(message); return true; } };
  globalThis.__api = { sendJSON: async (mode, path, method, body) => { sent.push({ mode, path, method, body }); return { deleted: body?.ids ?? [], refused: [] }; } };
  const render = mount(() => ApiKeys({ data, mode: "admin", onChanged: async () => {} }));
  return { sent, confirmations, render };
}
const shown = (tree) => findAll(tree, (node) => node.type === "tr" && node.key).map((row) => row.key);
const statusSelect = (tree) => find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "ended").length);
const button = (tree, label) => find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === label);
const check = (tree, label, checked = true) => find(tree, (node) => node.type === "input" && node.props?.["aria-label"] === label).props.onChange({ currentTarget: { checked } });

test("the key list hides revoked and expired keys until the status filter shows them", () => {
  const { render } = keysPage();
  let tree = render();
  assert.deepEqual(shown(tree), ["live", "paused"]);
  statusSelect(tree).props.onChange({ currentTarget: { value: "ended" } });
  tree = render();
  assert.deepEqual(shown(tree), ["gone", "lapsed", "old"]);
  statusSelect(tree).props.onChange({ currentTarget: { value: "all" } });
  tree = render();
  assert.deepEqual(shown(tree), ["live", "paused", "gone", "lapsed", "old"]);
});

test("a key that works is revoked and one that no longer works is deleted", async () => {
  const { sent, render } = keysPage();
  let tree = render();
  statusSelect(tree).props.onChange({ currentTarget: { value: "all" } });
  tree = render();
  for (const id of ["live", "paused"]) {
    assert.ok(button(tree, `Revoke ${id}`), `${id} can be revoked`);
    assert.equal(button(tree, `Delete ${id}`), undefined, `${id} still works, so it cannot be deleted`);
  }
  for (const id of ["gone", "lapsed", "old"]) {
    assert.ok(button(tree, `Delete ${id}`), `${id} can be deleted`);
    assert.equal(button(tree, `Revoke ${id}`), undefined, `${id} no longer works, so revoking it means nothing`);
  }
  const statusActions = findAll(tree, (node) => node.type === "button" && ["Enable", "Disable"].includes(text(node))).map(text);
  assert.deepEqual(statusActions, ["Disable", "Enable"], "only the keys that still work can be enabled or disabled");

  button(tree, "Revoke live").props.onClick();
  await settle();
  assert.deepEqual(sent.at(-1), { mode: "admin", path: "/keys?id=live", method: "DELETE", body: undefined });
  tree = render();
  button(tree, "Delete lapsed").props.onClick();
  await settle();
  assert.deepEqual(sent.at(-1), { mode: "admin", path: "/keys/delete", method: "POST", body: { ids: ["lapsed"] } });
});

test("revoked and expired keys are deleted in bulk, only those the list shows", async () => {
  const { sent, confirmations, render } = keysPage();
  let tree = render();
  statusSelect(tree).props.onChange({ currentTarget: { value: "all" } });
  tree = render();
  const selectable = findAll(tree, (node) => node.type === "input" && String(node.props?.["aria-label"]).startsWith("Select ") && !node.props.disabled)
    .map((node) => node.props["aria-label"]);
  assert.deepEqual(selectable, ["Select every revoked or expired key shown", "Select gone", "Select lapsed", "Select old"]);

  check(tree, "Select gone");
  tree = render();
  check(tree, "Select old");
  tree = render();
  const bulk = find(tree, (node) => node.type === "button" && /Delete 2 selected/.test(text(node)));
  assert.ok(bulk);
  bulk.props.onClick();
  await settle();
  assert.deepEqual(sent.at(-1).body, { ids: ["gone", "old"] });
  assert.match(confirmations.at(-1), /Delete 2 keys\?/);
  tree = render();
  assert.match(text(find(tree, (node) => node.props?.role === "status")), /2 keys deleted\./);

  check(tree, "Select every revoked or expired key shown");
  tree = render();
  statusSelect(tree).props.onChange({ currentTarget: { value: "revoked" } });
  tree = render();
  assert.equal(findAll(tree, (node) => node.type === "button" && /selected/.test(text(node))).length, 0, "changing the status filter clears the selection");
  check(tree, "Select every revoked or expired key shown");
  tree = render();
  find(tree, (node) => node.type === "button" && /Delete 2 selected/.test(text(node))).props.onClick();
  await settle();
  assert.deepEqual(sent.at(-1).body, { ids: ["gone", "old"] }, "the expired key the filter hides is not deleted");
});
