import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { issueEntryID } from "../../../../../scripts/provider-roster-community/github.mjs";
import { canonicalURL } from "../../../../../scripts/provider-roster/normalize.mjs";

// Transpile the real UI in memory; no dist output or browser/network dependency.
// Hook slots let us drive the optional request lifecycle and its exposed callback.
const { outputFiles } = await build({
  entryPoints: [new URL("../src/components/providers/ProviderRoster.tsx", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1")],
  bundle: true, write: false, format: "esm", platform: "node", jsx: "automatic", jsxImportSource: "preact",
  loader: { ".css": "empty" },
  plugins: [{ name: "roster-test-boundaries", setup(builder) {
    builder.onResolve({ filter: /^preact\/hooks$/ }, () => ({ path: "hooks", namespace: "test" }));
    builder.onResolve({ filter: /\/lib\/api$/ }, () => ({ path: "api", namespace: "test" }));
    builder.onLoad({ filter: /.*/, namespace: "test" }, ({ path }) => ({ contents: path === "api"
      ? "export const getJSON = (...args) => globalThis.__rosterAPI.getJSON(...args); export const sendJSON = (...args) => globalThis.__rosterAPI.sendJSON(...args);"
      : "export const useState = value => globalThis.__rosterHooks.state(value); export const useRef = value => globalThis.__rosterHooks.ref(value); export const useEffect = effect => globalThis.__rosterHooks.effect(effect);" }));
  } }],
});
const ui = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);
const id = "endpoint-0123456789abcdef";
const candidate = { id, name: "Example", protocol: "openai", base_url: "https://api.example.com/v1", auth: "api_key", offer: "free_tier", setup: "compatible", state: "active" };
const registry = [{ id: "custom_openai" }, { id: "custom_anthropic", requires_api_key: true }];
const builtin = { id: "example", protocol: "openai", default_base_url: candidate.base_url, configured: true, provider_config: { base_url: "https://configured.example.com/v1" } };

function nodes(node) {
  if (node == null || typeof node === "boolean") return [];
  if (Array.isArray(node)) return node.flatMap(nodes);
  return typeof node === "object" ? [node, ...nodes(node.props?.children)] : [node];
}
const text = node => nodes(node).filter(item => typeof item === "string" || typeof item === "number").join("");

test("new report is accepted by the actual community parser and carries no configuration", () => {
  const url = new URL(ui.rosterReportURL({ ...candidate, api_key: "sensitive-key", provider_config: builtin.provider_config, report: { issue_url: "https://github.com/other/repo/issues/1" } }, 42));
  assert.equal(url.pathname, "/xibodev/llm-gateway/issues/new");
  assert.equal(issueEntryID(url.searchParams.get("body")), id);
  assert.match(url.searchParams.get("body"), /### Roster revision\n\n42/);
  assert.match(url.searchParams.get("body"), /Reports are public: never include credentials/);
  assert.doesNotMatch(url.href, /sensitive-key|configured\.example/);
  assert.equal(ui.rosterReportURL({ ...candidate, report: { issue_url: "https://github.com/xibodev/llm-gateway/issues/17?discard=yes" } }, 42), "https://github.com/xibodev/llm-gateway/issues/17");
});

test("endpoint merging agrees with producer canonicalization without erasing routes", () => {
  for (const [left, right] of [
    ["https://api.example.com/v1", "https://API.example.com:443/v1/"],
    ["https://api.example.com/~user/v1", "https://api.example.com/%7euser/%761/"],
    ["https://api.example.com/v1", "https://api.example.com/v1//"],
    ["https://api.example.com/v1/", "https://api.example.com/v1///"],
    ["https://api.example.com/a/b", "https://api.example.com/a%2fb"],
  ]) {
    const merged = ui.mergeProviderRoster([{ ...builtin, default_base_url: left }], [{ ...candidate, base_url: right }]);
    assert.equal(merged.length, canonicalURL(left) === canonicalURL(right) ? 1 : 2, `${left} vs ${right}`);
    assert.deepEqual(merged[0].provider_config, builtin.provider_config);
  }
  assert.equal(ui.mergeProviderRoster([builtin], [{ ...candidate, id: builtin.id, base_url: "https://api.example.com/v2" }]).length, 2);
});

test("unknown candidates remain discoverable without a protocol compatibility claim", () => {
  const merged = ui.mergeProviderRoster([], [{ ...candidate, protocol: "unknown" }]);
  assert.equal(ui.shelfFor(merged[0]), "Other candidates");
  assert.ok(ui.providerShelves.includes(ui.shelfFor(merged[0])));
  assert.equal(ui.matchesDiscoveryFilter(merged[0], "all"), true);
  assert.equal(ui.rosterSetupEntry(merged[0], registry), null);
});

test("setup blocks anonymous Anthropic while retaining supported key/no-key adapters", () => {
  assert.equal(ui.rosterSetupEntry({ ...candidate, protocol: "anthropic", auth: "none" }, registry), null);
  assert.match(ui.rosterSetupUnavailableReason({ ...candidate, protocol: "anthropic", auth: "none" }, registry), /Anthropic adapter requires an API key/);
  for (const [protocol, auth] of [["openai", "none"], ["openai", "api_key"], ["anthropic", "api_key"]]) {
    const entry = ui.rosterSetupEntry({ ...candidate, protocol, auth }, registry);
    assert.equal(entry.id, `custom_${protocol}`);
    assert.equal(entry.requires_api_key, auth === "api_key");
    assert.deepEqual(entry.configured_provider_ids, []);
  }
  for (const patch of [{ state: "quarantined" }, { state: "withdrawn" }, { auth: "unknown" }, { setup: "candidate" }]) assert.equal(ui.rosterSetupEntry({ ...candidate, ...patch }, registry), null);
});

test("free filters require explicit offer and auth; unavailable entries stay inspectable", () => {
  for (const auth of ["api_key", "unknown", "none"]) {
    const [entry] = ui.mergeProviderRoster([], [{ ...candidate, auth }]);
    assert.equal(ui.matchesDiscoveryFilter(entry, "no-key"), auth === "none");
  }
  for (const state of ["quarantined", "withdrawn"]) {
    const [entry] = ui.mergeProviderRoster([], [{ ...candidate, state }]);
    assert.equal(ui.matchesDiscoveryFilter(entry, "all"), false);
    assert.equal(ui.matchesDiscoveryFilter(entry, "unavailable"), true);
  }
});

test("portal cannot refresh even through callback; admin can refresh with auto-refresh off", async () => {
  for (const mode of ["portal", "admin"]) {
    const slots = []; let cursor = 0; let effect; let firstRender = true;
    globalThis.__rosterHooks = {
      state(value) { const index = cursor++; if (!(index in slots)) slots[index] = value; return [slots[index], next => { slots[index] = next; }]; },
      ref(value) { const index = cursor++; return slots[index] ??= { current: value }; },
      effect(callback) { if (firstRender) effect = callback; },
    };
    const calls = [];
    let revision = 1;
    globalThis.__rosterAPI = {
      async getJSON(...args) { calls.push(["GET", ...args]); return { configured: true, entries: [], revision, auto_refresh: false }; },
      async sendJSON(...args) { calls.push(["POST", ...args]); revision = 2; return {}; },
    };
    const render = () => { cursor = 0; const result = ui.useProviderRoster(mode); firstRender = false; return result; };
    render(); const cleanup = effect();
    await new Promise(resolve => setImmediate(resolve));
    const roster = render();
    assert.equal(roster.busy, false);
    const status = ui.RosterStatus({ roster });
    assert.equal(nodes(status).some(node => node.type === "button"), mode === "admin");
    if (mode === "portal") assert.match(text(status), /Only an administrator can refresh/);
    await roster.refresh();
    assert.equal(calls.filter(call => call[0] === "POST").length, mode === "admin" ? 1 : 0);
    assert.equal(render().state.revision, mode === "admin" ? 2 : 1);
    cleanup();
  }
});

test("degraded sources are readable and ordinary metadata has no logo attribution", () => {
  const status = ui.RosterStatus({ roster: { state: { configured: true, revision: 1, stale: true, sources: [{ status: "ok" }, { status: "stale" }] }, busy: false, error: "", canRefresh: false, refresh() {} } });
  assert.match(text(status), /Stale snapshot/);
  assert.match(text(status), /1 discovery source is degraded/);
  const metadata = ui.RosterMetadata({ entries: [{ ...candidate, logo: { license: "LICENSE-NOTICE" } }], revision: 1 });
  assert.doesNotMatch(text(metadata), /LICENSE-NOTICE/);
  assert.match(text(metadata), /public GitHub issue/);
});
