import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { build } from "esbuild";

// Render App as a plain function over recorded hook slots. Pages are never
// mounted, so the tree shows exactly what App decides to put on screen.
const { outputFiles } = await build({
  entryPoints: [fileURLToPath(new URL("../src/App.tsx", import.meta.url))],
  bundle: true, write: false, format: "esm", platform: "node", jsx: "automatic", jsxImportSource: "preact",
  loader: { ".css": "empty" },
  plugins: [{ name: "app-test-hooks", setup(builder) {
    builder.onResolve({ filter: /^preact\/hooks$/ }, ({ importer }) => importer.includes("node_modules") ? undefined : { path: "hooks", namespace: "test" });
    builder.onLoad({ filter: /.*/, namespace: "test" }, () => ({ contents: [
      "export const useState = (value) => globalThis.__appHooks.state(value);",
      "export const useEffect = (effect, deps) => globalThis.__appHooks.effect(effect, deps);",
      "export const useMemo = (factory, deps) => globalThis.__appHooks.memo(factory, deps);",
      "export const useRef = (value) => globalThis.__appHooks.ref(value);",
      "export const useCallback = (callback) => callback;",
    ].join("\n") }));
  } }],
});
const { App } = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);

globalThis.window = {
  location: { hash: "#keys", pathname: "/admin", href: "https://gateway.example/admin", origin: "https://gateway.example", assign() {} },
  addEventListener() {}, removeEventListener() {},
};
let responses = [];
globalThis.fetch = async () => responses.shift();
const ok = (payload) => new Response(JSON.stringify(payload), { status: 200, headers: { "Content-Type": "application/json" } });
const unavailable = () => new Response(JSON.stringify({ error: "State store unavailable." }), { status: 503, headers: { "Content-Type": "application/json" } });

function mount() {
  const slots = [];
  const pending = [];
  let cursor = 0;
  const changed = (previous, next) => !previous || !next || previous.length !== next.length || next.some((value, index) => !Object.is(value, previous[index]));
  globalThis.__appHooks = {
    state(initial) {
      const index = cursor++;
      if (!(index in slots)) slots[index] = typeof initial === "function" ? initial() : initial;
      return [slots[index], (next) => { slots[index] = typeof next === "function" ? next(slots[index]) : next; }];
    },
    effect(effect, deps) {
      const index = cursor++;
      if (changed(slots[index], deps)) { slots[index] = deps; pending.push(effect); }
    },
    memo(factory, deps) {
      const index = cursor++;
      if (!slots[index] || changed(slots[index].deps, deps)) slots[index] = { deps, value: factory() };
      return slots[index].value;
    },
    ref(initial) { const index = cursor++; return slots[index] ??= { current: initial }; },
  };
  return () => {
    cursor = 0;
    const tree = App();
    for (const effect of pending.splice(0)) effect();
    return tree;
  };
}

const settle = async () => { for (let turn = 0; turn < 10; turn += 1) await new Promise((resolve) => setImmediate(resolve)); };
function nodes(node) {
  if (node == null || typeof node === "boolean") return [];
  if (Array.isArray(node)) return node.flatMap(nodes);
  return typeof node === "object" ? [node, ...nodes(node.props?.children)] : [node];
}
const find = (tree, predicate) => nodes(tree).find((node) => typeof node === "object" && predicate(node));
const text = (tree) => nodes(tree).filter((node) => typeof node === "string").join("");
const page = (tree) => find(tree, (node) => typeof node.props?.onChanged === "function");
const errorPage = (tree) => find(tree, (node) => node.props?.title === "Workspace state is unavailable");

test("a failed refresh keeps the loaded page on screen and rejects to its caller", async () => {
  const render = mount();
  responses = [ok({ keys: [{ id: "key-1" }] }), unavailable(), ok({ keys: [{ id: "key-2" }] })];
  render();
  await settle();
  const loaded = page(render());
  assert.ok(loaded, "the keys page renders once state loads");

  await assert.rejects(loaded.props.onChanged(), /request succeeded, but the console could not reload the latest gateway state: State store unavailable\./);
  const afterFailure = render();
  const kept = page(afterFailure);
  assert.ok(kept, "the page stays mounted after a failed refresh");
  assert.strictEqual(kept.props.data, loaded.props.data, "the last good state is kept");
  assert.equal(errorPage(afterFailure), undefined, "a refresh failure never takes over the page");
  const notice = find(afterFailure, (node) => node.props?.role === "alert");
  assert.match(text(notice), /Refresh failed/);
  assert.match(text(notice), /State store unavailable\./);

  find(notice, (node) => node.type === "button").props.onClick();
  await settle();
  const recovered = render();
  assert.equal(find(recovered, (node) => node.props?.role === "alert"), undefined, "a successful retry clears the notice");
  assert.deepEqual(page(recovered).props.data, { keys: [{ id: "key-2" }] });
});

test("only a console with nothing loaded yet shows the full error page", async () => {
  const render = mount();
  responses = [unavailable()];
  render();
  await settle();
  const tree = render();
  assert.ok(errorPage(tree), "the first load failure gets the full error page");
  assert.equal(page(tree), undefined);
});

test("a successful refresh resolves", async () => {
  const render = mount();
  responses = [ok({ keys: [] }), ok({ keys: [{ id: "key-3" }] })];
  render();
  await settle();
  await page(render()).props.onChanged();
  assert.deepEqual(page(render()).props.data, { keys: [{ id: "key-3" }] });
});
