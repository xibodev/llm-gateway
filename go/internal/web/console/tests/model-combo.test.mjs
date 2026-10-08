import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, text } from "./hook-harness.mjs";

const picker = await bundle(fileURLToPath(new URL("../src/components/ModelPicker.tsx", import.meta.url)));
const model = (id, label = id) => ({ id, label, provider: id.split("/")[0], capabilities: ["chat"], surfaces: [], free: false, isCategory: false, nativeSurfaces: [], emulatedSurfaces: [], disabled: false });

test("matches rank by prefix: the id, the model name, the label, then any word", () => {
  const models = [
    model("y/chatgpt-latest"), model("x/my-gpt"), model("openrouter/gpt-oss"),
    model("a/other", "GPT helper"), model("openai/gpt-4o"), model("gpt-router"), model("z/none"),
  ];
  assert.deepEqual(picker.rankModels(models, " GPT ").map((entry) => entry.id), [
    "gpt-router", "openrouter/gpt-oss", "openai/gpt-4o", "a/other", "x/my-gpt", "y/chatgpt-latest",
  ]);
  assert.deepEqual(picker.rankModels(models, "").map((entry) => entry.id), models.map((entry) => entry.id), "an empty query keeps the order given");
});

function opened(models, onChange = () => {}) {
  const render = mount(() => picker.ModelCombo({ models, filter: picker.emptyModelFilter, value: "", onChange }));
  let tree = render();
  find(tree, (node) => node.props?.role === "combobox").props.onFocus();
  tree = render();
  const state = {
    tree,
    rerender() { state.tree = render(); return state; },
    key(name) { find(state.tree, (node) => node.props?.role === "combobox").props.onKeyDown({ key: name, preventDefault() {} }); return state.rerender(); },
    options() { return findAll(state.tree, (node) => node.props?.role === "option"); },
    active() { return state.options().findIndex((option) => option.props["aria-selected"] === true); },
    activeID() { return find(state.tree, (node) => node.props?.role === "combobox").props["aria-activedescendant"]; },
  };
  return state;
}

test("the list renders a page of matches and more as it is scrolled to its end", () => {
  const models = Array.from({ length: 120 }, (_, index) => model(`fixture/model-${String(index).padStart(3, "0")}`));
  const combo = opened(models);
  assert.equal(combo.options().length, 50);
  assert.equal(combo.options()[0].props["aria-setsize"], 120);
  assert.match(text(combo.tree), /50 of 120 matches — scroll for more/);
  const list = () => find(combo.tree, (node) => node.props?.role === "listbox");
  list().props.onScroll({ currentTarget: { scrollTop: 0, clientHeight: 280, scrollHeight: 2400 } });
  assert.equal(combo.rerender().options().length, 50, "a list not scrolled to its end stays as it is");
  list().props.onScroll({ currentTarget: { scrollTop: 2100, clientHeight: 280, scrollHeight: 2400 } });
  assert.equal(combo.rerender().options().length, 100);
  list().props.onScroll({ currentTarget: { scrollTop: 4500, clientHeight: 280, scrollHeight: 4800 } });
  assert.equal(combo.rerender().options().length, 120);
  assert.doesNotMatch(text(combo.tree), /scroll for more/);
});

test("Home, End, Page Up and Page Down move the highlight through every match", () => {
  const models = Array.from({ length: 120 }, (_, index) => model(`fixture/model-${String(index).padStart(3, "0")}`));
  let chosen = "";
  const combo = opened(models, (id) => { chosen = id; });
  assert.equal(combo.active(), 0);
  combo.key("PageDown");
  assert.equal(combo.active(), picker.modelComboPageStep);
  combo.key("PageUp").key("PageUp");
  assert.equal(combo.active(), 0, "Page Up stops at the first match");
  combo.key("End");
  assert.equal(combo.options().length, 120, "End renders the list through the last match");
  assert.equal(combo.active(), 119);
  assert.match(combo.activeID(), /-option-119$/);
  combo.key("ArrowDown");
  assert.equal(combo.active(), 119, "the highlight stops at the last match");
  combo.key("Home");
  assert.equal(combo.active(), 0);
  combo.key("Enter");
  assert.equal(chosen, "fixture/model-000");

  // The rendered window only grows, so a fresh list shows the next page
  // appearing as the highlight moves past the first.
  const fresh = opened(models, (id) => { chosen = id; });
  for (let step = 0; step < 50; step += 1) fresh.key("ArrowDown");
  assert.equal(fresh.active(), 50);
  assert.equal(fresh.options().length, 100, "moving past the rendered options renders the next page");
  fresh.key("Enter");
  assert.equal(chosen, "fixture/model-050");
});

test("typing ranks prefix matches first and starts at the first of them", () => {
  const models = [model("y/chatgpt-latest"), model("openai/gpt-4o"), model("x/my-gpt")];
  const combo = opened(models);
  combo.key("End");
  find(combo.tree, (node) => node.props?.role === "combobox").props.onInput(input("gpt"));
  combo.rerender();
  assert.deepEqual(combo.options().map((option) => option.key), ["openai/gpt-4o", "x/my-gpt", "y/chatgpt-latest"]);
  assert.equal(combo.active(), 0);
});
