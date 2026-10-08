import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, input, mount, settle, text } from "./hook-harness.mjs";

const { ProjectPolicyEditor } = await bundle(fileURLToPath(new URL("../src/pages/ProjectDetail.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args); export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" },
  { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" },
]);

function fakeAPI() {
  const pending = new Map();
  const sent = [];
  globalThis.__api = {
    getJSON: (mode, path) => new Promise((resolve) => pending.set(path, resolve)),
    sendJSON: async (mode, path, method, body) => { sent.push({ path, body }); return {}; },
  };
  return { pending, sent };
}
const limit = (tree, key) => find(find(tree, (node) => node.type === "label" && node.key === key), (node) => node.type === "input");
const submit = (tree) => find(tree, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });

test("a late policy response for a project the operator left is discarded", async () => {
  const { pending, sent } = fakeAPI();
  let projectID = "alpha";
  const render = mount(() => ProjectPolicyEditor({ projectID, onSaved() {} }));
  let tree = render();
  projectID = "beta";
  tree = render();
  pending.get("/projects/beta/policy")({ rpm: 20 });
  await settle();
  pending.get("/projects/alpha/policy")({ rpm: 10, allowed_models: ["alpha-only"] });
  await settle();
  tree = render();
  assert.equal(limit(tree, "rpm").props.value, "20");
  assert.doesNotMatch(JSON.stringify(find(tree, (node) => node.type === "form").props), /alpha-only/);

  submit(tree);
  await settle();
  assert.equal(sent.length, 1);
  assert.equal(sent[0].path, "/projects/beta/policy");
  assert.equal(sent[0].body.rpm, 20);
  assert.deepEqual(sent[0].body.allowed_models, []);
});

test("project limits accept only plain non-negative whole numbers", async () => {
  const { pending, sent } = fakeAPI();
  const render = mount(() => ProjectPolicyEditor({ projectID: "alpha", onSaved() {} }));
  render();
  pending.get("/projects/alpha/policy")({});
  await settle();
  let tree = render();
  for (const value of ["1.5", "-1", "1e3", "0x10", "9007199254740992"]) {
    limit(tree, "daily_requests").props.onInput(input(value));
    tree = render();
    submit(tree);
    await settle();
    tree = render();
    assert.match(text(find(tree, (node) => node.props?.role === "alert")), /Daily requests must be a nonnegative whole number\./, value);
  }
  assert.equal(sent.length, 0, "an invalid limit is never sent");

  limit(tree, "daily_requests").props.onInput(input(" 25 "));
  tree = render();
  submit(tree);
  await settle();
  assert.equal(sent.length, 1);
  assert.equal(sent[0].body.daily_requests, 25);
  assert.equal(sent[0].body.rpm, 0, "a blank limit is sent as no limit");
});

test("a policy is saved once while its save is in flight", async () => {
  const { pending } = fakeAPI();
  const sent = [];
  let release;
  globalThis.__api.sendJSON = (mode, path) => { sent.push(path); return new Promise((resolve) => { release = resolve; }); };
  const render = mount(() => ProjectPolicyEditor({ projectID: "alpha", onSaved() {} }));
  render();
  pending.get("/projects/alpha/policy")({});
  await settle();
  let tree = render();
  submit(tree);
  tree = render();
  assert.equal(find(tree, (node) => node.type === "button" && node.props?.type === "submit").props.disabled, true);
  submit(tree);
  assert.equal(sent.length, 1, "a second submit while saving sends nothing");
  release({});
  await settle();
  pending.get("/projects/alpha/policy")({});
  await settle();
  assert.equal(find(render(), (node) => node.type === "button" && node.props?.type === "submit").props.disabled, false);
});
