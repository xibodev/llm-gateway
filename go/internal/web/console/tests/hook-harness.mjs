import { build } from "esbuild";

// bundle transpiles one console module in memory. preact/hooks imported from
// console sources resolves to the recorded slots of mount(), and each stub
// replaces the modules its filter matches, so a component runs as a plain
// function whose element tree can be inspected without a DOM.
export async function bundle(entry, stubs = []) {
  const { outputFiles } = await build({
    entryPoints: [entry], bundle: true, write: false, format: "esm", platform: "node",
    jsx: "automatic", jsxImportSource: "preact", loader: { ".css": "empty" },
    plugins: [{ name: "console-test-boundaries", setup(builder) {
      builder.onResolve({ filter: /^preact\/hooks$/ }, ({ importer }) => importer.includes("node_modules") ? undefined : { path: "hooks", namespace: "stub" });
      stubs.forEach(({ filter }, index) => builder.onResolve({ filter }, () => ({ path: String(index), namespace: "stub" })));
      builder.onLoad({ filter: /.*/, namespace: "stub" }, ({ path }) => ({ contents: path === "hooks" ? hooks : stubs[Number(path)].contents }));
    } }],
  });
  return import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);
}

const hooks = [
  "export const useState = (value) => globalThis.__hooks.state(value);",
  "export const useEffect = (effect, deps) => globalThis.__hooks.effect(effect, deps);",
  "export const useMemo = (factory, deps) => globalThis.__hooks.memo(factory, deps);",
  "export const useRef = (value) => globalThis.__hooks.ref(value);",
  "export const useCallback = (callback, deps) => globalThis.__hooks.memo(() => callback, deps);",
].join("\n");

// mount returns a render function for one component instance. Hook state lives
// in call-order slots across renders, and effects whose dependencies changed
// run after each render, as they would after a commit.
export function mount(component) {
  const slots = [];
  const pending = [];
  let cursor = 0;
  const changed = (previous, next) => !previous || !next || previous.length !== next.length || next.some((value, index) => !Object.is(value, previous[index]));
  const recorder = {
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
    globalThis.__hooks = recorder;
    cursor = 0;
    const tree = component();
    for (const effect of pending.splice(0)) effect();
    return tree;
  };
}

export const settle = async () => { for (let turn = 0; turn < 10; turn += 1) await new Promise((resolve) => setImmediate(resolve)); };

export function nodes(node) {
  if (node == null || typeof node === "boolean") return [];
  if (Array.isArray(node)) return node.flatMap(nodes);
  return typeof node === "object" ? [node, ...nodes(node.props?.children)] : [node];
}
export const findAll = (tree, predicate) => nodes(tree).filter((node) => typeof node === "object" && predicate(node));
export const find = (tree, predicate) => findAll(tree, predicate)[0];
export const text = (tree) => nodes(tree).filter((node) => typeof node === "string" || typeof node === "number").join("");
export const input = (value) => ({ currentTarget: { value } });
