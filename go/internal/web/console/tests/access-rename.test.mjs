import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const { Access } = await bundle(fileURLToPath(new URL("../src/pages/Access.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" },
  // Dialog focus needs a DOM; the dialog's behaviour does not.
  { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" },
]);

// opened mounts the dialog component the page rendered, which the harness
// leaves unrendered, as its own instance.
function opened(tree, matches) {
  const node = find(tree, (candidate) => typeof candidate.type === "function" && matches(candidate.props ?? {}));
  return node ? mount(() => node.type(node.props)) : null;
}

test("an administrator renames a principal, and the built-in one keeps its name", async () => {
  const sent = [];
  let changed = 0;
  globalThis.__api = { sendJSON: async (mode, path, method, body) => { sent.push({ mode, path, method, body }); return {}; } };
  const data = {
    principals: [
      { id: "prn-1", kind: "human", display_name: "fixture-subject", external_subject: "authentik:fixture-subject", status: "active" },
      { id: "prn-2", kind: "human", display_name: "Grace", external_subject: "authentik:grace", name_set_by_admin: true, status: "active" },
      { id: "prn-sys", kind: "system", display_name: "Gateway system", status: "active" },
    ],
    projects: [], memberships: [],
  };
  const render = mount(() => Access({ data, mode: "admin", onChanged: async () => { changed += 1; } }));
  let tree = render();
  const rename = (name) => find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === `Rename ${name}`);
  assert.equal(rename("Gateway system"), undefined, "the built-in system principal has no rename action");
  const grace = find(tree, (node) => node.type === "tr" && node.key === "prn-2");
  assert.match(text(grace), /Named by an administrator/);
  assert.doesNotMatch(text(find(tree, (node) => node.type === "tr" && node.key === "prn-1")), /Named by an administrator/);

  rename("fixture-subject").props.onClick();
  tree = render();
  const dialog = opened(tree, (props) => props.principal?.id === "prn-1");
  assert.ok(dialog, "the rename dialog opens for the chosen principal");
  let form = dialog();
  const field = () => find(form, (node) => node.type === "input" && node.props?.maxLength === 200);
  assert.equal(field().props.value, "fixture-subject");
  field().props.onInput(input("  "));
  form = dialog();
  find(form, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });
  await settle();
  form = dialog();
  assert.equal(sent.length, 0, "a blank name is not sent");
  assert.match(text(find(form, (node) => node.props?.role === "alert")), /A display name is required\./);

  field().props.onInput(input(" Ada Lovelace "));
  form = dialog();
  find(form, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });
  await settle();
  tree = render();
  assert.deepEqual(sent, [{ mode: "admin", path: "/principals/prn-1/rename", method: "POST", body: { display_name: "Ada Lovelace" } }]);
  assert.equal(changed, 1);
  assert.equal(opened(tree, (props) => props.principal?.id === "prn-1"), null, "the dialog closes");
  const notice = find(tree, (node) => typeof node.type === "function" && node.props?.result);
  assert.match(notice.props.result.detail, /fixture-subject is now Ada Lovelace\./);
});
