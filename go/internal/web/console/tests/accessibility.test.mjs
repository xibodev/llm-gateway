import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount } from "./hook-harness.mjs";

const picker = await bundle(fileURLToPath(new URL("../src/components/ModelPicker.tsx", import.meta.url)));
const styles = readFileSync(new URL("../src/styles/base.css", import.meta.url), "utf8");

function luminance(hex) {
  const [red, green, blue] = [1, 3, 5].map((start) => parseInt(hex.slice(start, start + 2), 16) / 255)
    .map((channel) => channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4);
  return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
}
const contrast = (left, right) => {
  const [light, dark] = [luminance(left), luminance(right)].sort((a, b) => b - a);
  return (light + 0.05) / (dark + 0.05);
};

test("focus rings are opaque, shown on search fields, and reach 3:1 in both themes", () => {
  const light = styles.match(/^button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-visible \{ outline: 3px solid (#[0-9a-f]{6}); outline-offset: 2px; \}/m)?.[1];
  const dark = styles.match(/^:root\[data-theme="dark"\] \.search-field:focus-within \{ outline-color: (#[0-9a-f]{6}); \}/m)?.[1];
  assert.ok(light && dark, "focus ring colours not found");
  assert.match(styles, new RegExp(`^\\.search-field:focus-within \\{ outline: 3px solid ${light}; outline-offset: 2px; \\}`, "m"));
  assert.match(styles, /^:root\[data-theme="dark"\] :is\(button, input, select, textarea\):focus-visible,$/m);
  // Page and surface backgrounds the ring sits on, light and dark.
  for (const background of ["#ffffff", "#f7f7f5"]) assert.ok(contrast(light, background) >= 3, `${light} on ${background}`);
  for (const background of ["#111418", "#171a1f"]) assert.ok(contrast(dark, background) >= 3, `${dark} on ${background}`);
});

test("the model picker is an ARIA 1.2 combobox over a listbox of options", () => {
  const models = picker.catalogModels({ data: ["a/one", "a/two", "a/three"].map((id) => ({ id, owned_by: "a" })) });
  let chosen = "";
  const render = mount(() => picker.ModelCombo({ models, filter: picker.emptyModelFilter, value: "", onChange: (id) => { chosen = id; }, pageSize: 2 }));
  const view = () => {
    const tree = render();
    return {
      box: find(tree, (node) => node.type === "input"),
      list: find(tree, (node) => node.props?.role === "listbox"),
      options: findAll(tree, (node) => node.props?.role === "option"),
    };
  };

  let { box, list, options } = view();
  assert.equal(box.props.role, "combobox");
  assert.equal(box.props["aria-autocomplete"], "list");
  assert.equal(box.props["aria-expanded"], false);
  assert.equal(box.props["aria-controls"], list.props.id);
  assert.equal(list.props.hidden, true);
  assert.equal(box.props["aria-activedescendant"], undefined);
  assert.equal(options.length, 0);

  box.props.onFocus();
  ({ box, list, options } = view());
  assert.equal(box.props["aria-expanded"], true);
  assert.equal(list.props.hidden, false);
  assert.equal(options.length, 2);
  assert.equal(new Set(options.map((option) => option.props.id)).size, 2);
  assert.ok(options.every((option) => option.props.id.startsWith(list.props.id)));
  assert.equal(box.props["aria-activedescendant"], options[0].props.id);
  assert.deepEqual(options.map((option) => option.props["aria-selected"]), [true, false]);
  assert.equal(findAll(list, (node) => node.type === "button").length, 0, "options are not separate tab stops");
  assert.equal(find(list, (node) => node.props?.class === "model-combo__more").props.role, "presentation");

  box.props.onKeyDown({ key: "ArrowDown", preventDefault() {} });
  ({ box, options } = view());
  assert.equal(box.props["aria-activedescendant"], options[1].props.id);
  box.props.onKeyDown({ key: "Enter", preventDefault() {} });
  ({ box } = view());
  assert.equal(chosen, "a/two");
  assert.equal(box.props["aria-expanded"], false);
  assert.equal(box.props["aria-activedescendant"], undefined);

  box.props.onKeyDown({ key: "ArrowDown", preventDefault() {} });
  assert.equal(view().box.props["aria-expanded"], true, "ArrowDown reopens a closed list");
});
