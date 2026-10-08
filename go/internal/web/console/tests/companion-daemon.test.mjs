import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount, settle, text } from "./hook-harness.mjs";

const panel = await bundle(fileURLToPath(new URL("../src/components/providers/CompanionDaemonPanel.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args);" },
]);
const { Overview } = await bundle(fileURLToPath(new URL("../src/pages/Overview.tsx", import.meta.url)));

const reachable = {
  address: "http://127.0.0.1:18888", address_configured: true, secret_set: false,
  dependent_providers: [{ id: "codex", type: "openai_codex" }, { id: "zen", type: "opencode_zen_anonymous", disabled: true }],
  probed: true, reachable: true, latency_ms: 12, version: "1.3.0",
  served: [{ id: "openai_codex", name: "OpenAI Codex" }],
  warnings: ["the companion daemon serves provider(s) codex, zen and LLMGW_EXTENSION_SECRET is empty: start the daemon with a shared secret"],
};

async function rendered(report) {
  const paths = [];
  globalThis.__api = { async getJSON(mode, path) { paths.push(`${mode} ${path}`); return report; } };
  const render = mount(() => panel.CompanionDaemonPanel());
  render();
  await settle();
  return { tree: render(), render, paths };
}

test("the companion daemon panel shows where the daemon is, what it serves and what to fix", async () => {
  const { tree, paths } = await rendered(reachable);
  assert.deepEqual(paths, ["admin /companion-daemon"]);
  const shown = text(tree);
  assert.match(shown, /Reachable/);
  assert.match(shown, /http:\/\/127\.0\.0\.1:18888/);
  assert.match(shown, /Shared secretNot set/);
  assert.match(shown, /Version1\.3\.0/);
  assert.match(shown, /12 ms/);
  assert.match(shown, /codex openai_codex/);
  assert.match(shown, /zen opencode_zen_anonymous \(disabled\)/);
  assert.match(shown, /OpenAI Codex openai_codex/);
  const alert = find(tree, (node) => node.props?.role === "alert");
  assert.match(text(alert), /LLMGW_EXTENSION_SECRET is empty/);
});

test("the panel says when the daemon does not answer, and stays hidden when nothing uses it", async () => {
  const { tree } = await rendered({ ...reachable, reachable: false, version: "", served: [], error: "extension info failed: the extension did not answer", warnings: ["the companion daemon did not answer, so provider(s) codex cannot serve requests"] });
  assert.match(text(tree), /Unreachable/);
  assert.match(text(tree), /No answer/);
  assert.match(text(tree), /the extension did not answer/);
  assert.match(text(tree), /Unknown until the daemon answers\./);

  const unused = await rendered({ address: "http://127.0.0.1:18888", address_configured: false, secret_set: false, dependent_providers: [], probed: false, reachable: false, served: [], warnings: [] });
  assert.equal(unused.tree, null);
  assert.deepEqual(panel.companionDaemonState({ probed: false }), { label: "Not checked", tone: "muted" });
});

test("the overview shows an administrator the warnings the gateway started with", () => {
  const data = { startup_warnings: ["LLMGW_API_KEYS holds 2 static key(s)"], providers: [], keys: [], provider_connections: [], projects: [] };
  const admin = mount(() => Overview({ data, mode: "admin", onNavigate() {} }))();
  assert.match(text(find(admin, (node) => node.props?.role === "alert")), /Startup warning.*LLMGW_API_KEYS holds 2 static key\(s\)/);
  const portal = mount(() => Overview({ data, mode: "portal", onNavigate() {} }))();
  assert.equal(findAll(portal, (node) => node.props?.role === "alert").length, 0);
});
