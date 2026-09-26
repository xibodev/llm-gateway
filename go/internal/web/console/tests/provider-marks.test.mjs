import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { build } from "esbuild";

// Exercise real resolution, JSX and roster precedence without mounting a UI.
// Only framework boundaries are replaced; brand data and component calls stay real.
const { outputFiles } = await build({
  stdin: {
    contents: 'export * from "./ProviderMark"; export { RosterMark } from "./providers/ProviderRoster";',
    resolveDir: fileURLToPath(new URL("../src/components", import.meta.url)),
    loader: "ts",
  },
  bundle: true, write: false, format: "esm", platform: "node",
  jsx: "automatic", jsxImportSource: "preact", loader: { ".css": "empty" },
  plugins: [{ name: "mark-test-boundaries", setup(builder) {
    builder.onResolve({ filter: /^(preact\/jsx-runtime|preact\/hooks|lucide-preact)$/ }, ({ path }) => ({ path, namespace: "test" }));
    builder.onLoad({ filter: /.*/, namespace: "test" }, ({ path }) => ({ contents:
      path === "preact/jsx-runtime"
        ? 'export const jsx = (type, props) => ({ type, props }); export const jsxs = jsx;'
        : path === "preact/hooks"
          ? 'export const useState = value => globalThis.__markState(value); export const useRef = () => { throw new Error("Unexpected ref"); }; export const useEffect = () => { throw new Error("Unexpected effect"); };'
          : 'export const RefreshCw = () => { throw new Error("Unexpected UI icon"); };',
    }));
  } }],
});
const ui = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);
const opaqueID = "endpoint-0123456789abcdef";
const groqURL = "https://api.groq.com/openai/v1";
const png = { mime: "image/png", data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=" };

function nodes(node) {
  if (node == null || typeof node === "boolean") return [];
  if (Array.isArray(node)) return node.flatMap(nodes);
  if (typeof node !== "object") return [node];
  if (typeof node.type === "function") return nodes(node.type(node.props));
  return [node, ...nodes(node.props?.children)];
}
const elements = (tree, type) => nodes(tree).filter(node => node?.type === type);
const text = tree => nodes(tree).filter(node => typeof node === "string").join("");
const paths = tree => elements(tree, "path").map(node => node.props.d);

function rosterRenderer(entry) {
  const slots = []; let cursor = 0;
  return (nextEntry = entry) => {
    cursor = 0;
    globalThis.__markState = initial => {
      const index = cursor++;
      if (!(index in slots)) slots[index] = initial;
      return [slots[index], value => { slots[index] = value; }];
    };
    try {
      let tree = ui.RosterMark({ entry: nextEntry });
      while (typeof tree.type === "function") tree = tree.type(tree.props);
      return tree;
    }
    finally { delete globalThis.__markState; }
  };
}

test("OpenAI and Codex render the same Blossom geometry, never a generic bot", () => {
  const openai = ui.ProviderMark({ id: "openai", label: "OpenAI" });
  const codex = ui.ProviderMark({ id: "openai_codex", label: "Codex" });
  assert.strictEqual(ui.resolveProviderBrand({ id: "openai" }), ui.resolveProviderBrand({ id: "codex" }));
  assert.deepEqual(paths(openai), paths(codex));
  assert.equal(paths(openai).length, 1);
  // Pin a recognizable segment and the kit's artboard, not merely two equal placeholders.
  assert.match(paths(openai)[0], /^M508\.749 317\.399C516\.777 287\.314/);
  assert.equal(elements(openai, "svg")[0].props.viewBox, "155 155 406 406");
  assert.equal(text(openai), "");
  assert.equal(openai.props["aria-label"], "OpenAI");
  assert.equal(codex.props["aria-label"], "Codex");
});

test("Groq exact API hostname resolves its official lightning for opaque roster IDs", () => {
  assert.equal(ui.providerBrandForEndpoint(groqURL), "groq");
  assert.equal(ui.providerBrandForEndpoint("https://API.GROQ.COM:443/openai/v1"), "groq");
  assert.equal(ui.providerBrandID(opaqueID, groqURL), "groq");
  assert.equal(ui.hasProviderMark(opaqueID, groqURL), true);
  const tree = ui.ProviderMark({ id: opaqueID, label: "Remote candidate", baseURL: groqURL });
  assert.deepEqual(paths(tree), ["M165.98 342.21H0L272.4 1.5l-68.75 220.11H369.6L97.23 562.32z"]);
});

test("lookalike hosts, URL credentials, paths and unsupported schemes cannot select Groq", () => {
  for (const baseURL of [
    "https://api.groq.com.evil.invalid/v1", "https://unreviewed.api.groq.com/v1",
    "https://api.groq.com@evil.invalid/v1", "https://user@api.groq.com/v1",
    "https://evil.invalid/api.groq.com?brand=groq#api.groq.com",
    "ftp://api.groq.com/v1", "api.groq.com/v1", "not a URL",
  ]) {
    assert.equal(ui.providerBrandForEndpoint(baseURL), undefined, baseURL);
    assert.equal(ui.hasProviderMark(opaqueID, baseURL), false, baseURL);
    assert.equal(elements(ui.ProviderMark({ id: opaqueID, baseURL, label: "Groq" }), "svg").length, 0, baseURL);
  }
});

test("untrusted display labels cannot turn opaque providers into known brands", () => {
  for (const label of ["OpenAI", "Codex", "Groq", "Anthropic", "Google Gemini"]) {
    const tree = ui.ProviderMark({ id: opaqueID, label, baseURL: "https://untrusted.invalid/v1" });
    assert.equal(tree.props["data-provider-brand"], undefined);
    assert.equal(elements(tree, "svg").length, 0);
    assert.equal(tree.props["aria-label"], label);
    assert.ok(text(tree).length > 0);
  }
  for (const id of ["constructor", "toString", "__proto__"]) assert.equal(ui.hasProviderMark(id), false);
});

test("unknown providers retain readable Unicode initials and accessible names", () => {
  for (const [label, expected] of [["Example Service", "ES"], ["sandbox", "SA"], ["Élan 東京", "É東"], ["東京", "東京"], ["---", "?"]]) {
    const tree = ui.ProviderMark({ id: opaqueID, label });
    assert.equal(text(tree), expected);
    assert.equal(tree.props.role, "img");
    assert.equal(tree.props["aria-label"], label);
    assert.equal(elements(tree, "svg").length, 0);
  }
  assert.equal(text(ui.ProviderMark({ id: "example-service", label: "  " })), "ES");
});

test("custom adapters resolve trusted endpoints while explicit builtins keep their identity", () => {
  assert.equal(ui.providerBrandID("custom_openai", groqURL), "groq");
  assert.equal(ui.providerBrandID("openai", groqURL), "openai");
  assert.equal(ui.providerBrandID("custom_openai", "https://untrusted.invalid"), "custom_openai");
  assert.equal(text(ui.ProviderMark({ id: "custom_openai" })), "API");
});

test("ProviderRoster builtins override a conflicting valid signed-raster field", () => {
  for (const id of ["openai", "openai_codex", "groq", "anthropic", "azure_openai"]) {
    const tree = rosterRenderer({ id, label: "Conflicting candidate", logo: png })();
    assert.equal(elements(tree, "img").length, 0, id);
    assert.equal(tree.props["data-provider-brand"], id);
    assert.deepEqual(paths(tree), paths(ui.ProviderMark({ id })), id);
    if (id === "azure_openai") assert.equal(text(tree), "AZ");
  }
});

test("ProviderRoster passes hashed roster ID and both endpoint field shapes to local marks", () => {
  for (const field of ["base_url", "default_base_url"]) {
    const tree = rosterRenderer({ id: `roster:${opaqueID}`, roster_id: opaqueID, name: "Unrelated label", [field]: groqURL, logo: png })();
    assert.equal(elements(tree, "img").length, 0);
    assert.equal(tree.props["data-provider-brand"], "groq");
    assert.deepEqual(paths(tree), paths(ui.ProviderMark({ id: "groq" })));
  }
});

test("ProviderRoster uses a valid embedded raster only when no local mark is known", () => {
  const tree = rosterRenderer({ id: opaqueID, label: "Groq", base_url: "https://api.groq.com.evil.invalid", logo: png })();
  assert.equal(elements(tree, "svg").length, 0);
  assert.equal(elements(tree, "img")[0].props.src, `data:image/png;base64,${png.data}`);
});

test("ProviderRoster missing or invalid raster falls back to initials without remote image fetches", () => {
  for (const logo of [undefined, { mime: "image/svg+xml", data: png.data }, { ...png, data: "https://untrusted.invalid/logo.png" }, { ...png, data: "broken" }]) {
    const tree = rosterRenderer({ id: opaqueID, label: "Example Service", logo })();
    assert.equal(elements(tree, "img").length, 0);
    assert.equal(elements(tree, "svg").length, 0);
    assert.equal(text(tree), "ES");
  }
});

test("ProviderRoster image errors fall back and a replacement raster can retry", () => {
  const entry = { id: opaqueID, label: "Example Service", logo: png };
  const render = rosterRenderer(entry);
  elements(render(), "img")[0].props.onError();
  assert.equal(elements(render(), "img").length, 0);
  assert.equal(text(render()), "ES");
  const replacement = { mime: "image/jpeg", data: "/9j/2Q==" };
  assert.equal(elements(render({ ...entry, logo: replacement }), "img")[0].props.src, "data:image/jpeg;base64,/9j/2Q==");
});

test("brand artwork notices stay in the artifact rather than provider mark UI", () => {
  const notice = readFileSync(new URL("../src/components/provider-brand-notices.md", import.meta.url), "utf8");
  for (const name of ["OpenAI", "Codex", "Groq", "Cerebras", "Cohere", "Simple Icons"]) assert.ok(notice.includes(name), name);
  assert.match(notice, /trademarks remain the property/);
  for (const id of ["openai", "openai_codex", "groq", "cerebras", "cohere"]) {
    const tree = ui.ProviderMark({ id });
    assert.equal(text(tree), "");
    assert.equal(elements(tree, "a").length, 0);
    assert.equal(elements(tree, "img").length, 0);
  }
});
