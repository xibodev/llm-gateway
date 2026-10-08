import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount, settle, text } from "./hook-harness.mjs";

const stubs = [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args); export const sendJSON = (...args) => globalThis.__api.sendJSON(...args); export const sendForm = (...args) => globalThis.__api.sendForm(...args);" },
  { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" },
];
const { Routes } = await bundle(fileURLToPath(new URL("../src/pages/Routes.tsx", import.meta.url)), stubs);
const { ProviderHub } = await bundle(fileURLToPath(new URL("../src/components/providers/ProviderHub.tsx", import.meta.url)), stubs);

test("route tiles are sorted by name, paged, and searched by route or member", async () => {
  globalThis.__api = { getJSON: async () => ({ data: [] }), sendJSON: async () => ({}) };
  const endpoints = Object.fromEntries(Array.from({ length: 30 }, (_, index) => [`route-${String(30 - index).padStart(2, "0")}`, { failover: [{ provider: index === 4 ? "special" : "echo", model: "m" }] }]));
  const render = mount(() => Routes({ data: { endpoints }, mode: "admin", detail: "", onChanged: async () => {}, onNavigate() {} }));
  render();
  await settle();
  let tree = render();
  const tiles = () => findAll(tree, (node) => node.type === "article" && typeof node.key === "string").map((tile) => tile.key);
  assert.equal(tiles().length, 25, "the first page of route tiles");
  assert.equal(tiles()[0], "route-01", "tiles are sorted by name");
  assert.ok(find(tree, (node) => node.props?.class === "data-table__footer"), "more routes than a page are paged");
  const search = find(tree, (node) => node.type === "input" && node.props?.placeholder === "Search routes and their members");
  search.props.onInput({ currentTarget: { value: "special/m" } });
  tree = render();
  assert.deepEqual(tiles(), ["route-26"], "a member's provider and model find its route");
  search.props.onInput({ currentTarget: { value: "nothing" } });
  tree = render();
  assert.ok(find(tree, (node) => node.props?.title === "No route matches this search"));
});

test("a long provider shelf shows its first providers until asked for all", async () => {
  globalThis.__api = { getJSON: async () => ({ entries: [] }), sendJSON: async () => ({}) };
  const provider_registry = Array.from({ length: 30 }, (_, index) => ({ id: `compat-${index}`, label: `Compatible ${index}`, protocol: "openai", auth_methods: ["api_key"], availability: "available" }));
  const render = mount(() => ProviderHub({ data: { provider_registry, provider_statuses: [], providers: [] }, mode: "admin", onChanged: async () => {}, onOpenDetail() {} }));
  render();
  await settle();
  let tree = render();
  const triggers = () => findAll(tree, (node) => node.type === "button" && node.props?.["data-provider-trigger"]);
  assert.equal(triggers().length, 24);
  const more = find(tree, (node) => node.type === "button" && text(node) === "Show all 30");
  assert.ok(more, "the shelf offers the rest");
  more.props.onClick();
  tree = render();
  assert.equal(triggers().length, 30);
  assert.ok(find(tree, (node) => node.type === "button" && text(node) === "Show fewer"));
});
