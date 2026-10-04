import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const stubs = [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args); export const sendJSON = (...args) => globalThis.__api.sendJSON(...args); export const sendForm = (...args) => globalThis.__api.sendForm(...args);" },
  { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" },
];
const models = await bundle(fileURLToPath(new URL("../src/lib/models.ts", import.meta.url)));
const { ProviderDetail } = await bundle(fileURLToPath(new URL("../src/pages/ProviderDetail.tsx", import.meta.url)), stubs);
const { ConnectDialog } = await bundle(fileURLToPath(new URL("../src/components/providers/ProviderHub.tsx", import.meta.url)), stubs);

function fakeAPI(responses) {
  const requests = [];
  const pending = new Map();
  globalThis.__api = {
    getJSON(mode, path) {
      requests.push(path);
      if (path in responses) return Promise.resolve(responses[path]);
      return new Promise((resolve) => pending.set(path, resolve));
    },
    sendJSON: async () => ({}),
    sendForm: async () => ({}),
  };
  return { requests, pending };
}
const detail = (entryID, data) => mount(() => ProviderDetail({ entryID, data, mode: "admin", onChanged: async () => {}, onBack() {} }));
const heading = (tree) => text(find(tree, (node) => node.type === "h1"));

test("a namespaced upstream model keeps its namespace and appears once", () => {
  assert.equal(models.upstreamModelID("openrouter/anthropic/claude-x"), "anthropic/claude-x");
  assert.equal(models.upstreamModelID("groq/llama-3"), "llama-3");
  assert.equal(models.upstreamModelID("bare-model"), "bare-model");
  assert.deepEqual(models.verifyModelChoices(["openrouter/anthropic/claude-x", "openrouter-2/anthropic/claude-x", "openrouter-2/meta/llama"]), ["anthropic/claude-x", "meta/llama"]);
});

test("the test completion picker offers each upstream model id once", async () => {
  fakeAPI({ "/models?diagnostics=1": { data: [
    { id: "openrouter/anthropic/claude-x", owned_by: "openrouter" },
    { id: "openrouter-2/anthropic/claude-x", owned_by: "openrouter-2" },
    { id: "openrouter-2/meta/llama", owned_by: "openrouter-2" },
  ] } });
  const render = detail("openrouter", {
    provider_registry: [{ id: "openrouter", label: "OpenRouter", auth_methods: ["api_key"], availability: "available" }],
    provider_statuses: [{ id: "openrouter", configured_provider_ids: ["openrouter", "openrouter-2"], instances: [{ id: "openrouter" }, { id: "openrouter-2" }] }],
    providers: [{ id: "openrouter" }, { id: "openrouter-2" }],
  });
  render();
  await settle();
  const picker = find(render(), (node) => node.type === "label" && node.props?.class === "verify-model-select");
  assert.deepEqual(findAll(picker, (node) => node.type === "option").map((option) => option.props.value), ["", "anthropic/claude-x", "meta/llama"]);
});

const candidate = { id: "endpoint-0123456789abcdef", name: "Example", protocol: "openai", base_url: "https://api.example.com/v1", auth: "none", offer: "free_tier", setup: "compatible", state: "active" };
const registry = [{ id: "custom_openai", label: "Custom OpenAI-compatible", auth_methods: ["api_key", "none"], availability: "available" }];

test("a roster candidate is read from the roster and connects through its adapter", async () => {
  const { requests, pending } = fakeAPI({});
  const render = detail(`roster:${candidate.id}`, { provider_registry: registry, provider_statuses: [], providers: [] });
  let tree = render();
  assert.ok(find(tree, (node) => node.props?.title === "Loading roster candidate"), "the page waits for the roster instead of reporting an unknown integration");
  assert.deepEqual(requests, ["/provider-roster"]);
  pending.get("/provider-roster")({ entries: [candidate], revision: 7 });
  await settle();
  tree = render();
  assert.equal(heading(tree), "Example");
  assert.ok(find(tree, (node) => Array.isArray(node.props?.entries) && node.props.entries[0] === candidate), "roster details are shown");
  const connect = find(tree, (node) => node.type === "button" && text(node).includes("Connect") && node.props.disabled === false);
  connect.props.onClick();
  tree = render();
  const dialog = find(tree, (node) => node.props?.takenIDs && node.props?.entry);
  assert.equal(dialog.props.entry.id, "custom_openai", "the provider is created from the adapter, never from the roster id");
  assert.equal(dialog.props.entry.default_base_url, candidate.base_url);
  assert.equal(dialog.props.mode, "create");
});

test("a roster candidate without quick setup explains why instead of offering a failing connect", async () => {
  fakeAPI({ "/provider-roster": { entries: [{ ...candidate, auth: "unknown" }] } });
  const render = detail(`roster:${candidate.id}`, { provider_registry: registry, provider_statuses: [], providers: [] });
  render();
  await settle();
  const tree = render();
  const connect = find(tree, (node) => node.type === "button" && text(node).includes("Connect"));
  assert.equal(connect.props.disabled, true);
  assert.match(connect.props.title, /Quick setup requires/);
});

test("a roster candidate that is already connected shows its configured instance", async () => {
  fakeAPI({ "/provider-roster": { entries: [candidate] }, "/models?diagnostics=1": { data: [] } });
  const render = detail(`roster:${candidate.id}`, {
    provider_registry: registry,
    provider_statuses: [{ id: "custom_openai", configured_provider_ids: ["example"], instances: [{ id: "example", registry_id: "custom_openai", base_url: "https://api.example.com/v1", status: "verified" }] }],
    providers: [{ id: "example" }],
  });
  render();
  await settle();
  const tree = render();
  assert.equal(heading(tree), "example");
  assert.match(text(tree), /Checks and lifecycle/);
  assert.equal(find(tree, (node) => Array.isArray(node.props?.entries)), undefined);
});

test("other unknown entries never request the roster", async () => {
  const { requests } = fakeAPI({});
  const render = detail("missing", { provider_registry: registry, provider_statuses: [], providers: [] });
  render();
  await settle();
  assert.ok(find(render(), (node) => node.props?.title === "Unknown integration"));
  assert.deepEqual(requests, []);
});

test("a service account upload carries the API adaptation setting", async () => {
  for (const enabled of [true, false]) {
    const forms = [];
    globalThis.__api = { sendJSON: async () => ({}), sendForm: async (mode, path, form) => { forms.push({ path, form }); return {}; } };
    const render = mount(() => ConnectDialog({
      entry: { id: "vertex_ai", label: "Vertex AI", auth_methods: ["api_key", "gcp_service_account"], requires_api_key: true, onboarding_fields: ["project", "location"], provider_config: { id: "vertex", project: "example-project", location: "global", force_api_support: true, api_key_set: true } },
      mode: "edit", onClose() {}, onConfigured: async () => {},
    }));
    let tree = render();
    find(tree, (node) => node.type === "select").props.onChange(input("gcp_service_account"));
    tree = render();
    if (!enabled) find(tree, (node) => node.props?.type === "checkbox").props.onChange({ currentTarget: { checked: false } });
    const file = new File([JSON.stringify({ type: "service_account" })], "service-account.json", { type: "application/json" });
    find(tree, (node) => node.props?.type === "file").props.onChange({ currentTarget: { files: [file] } });
    await settle();
    find(render(), (node) => node.type === "form").props.onSubmit({ preventDefault() {} });
    await settle();
    assert.equal(forms.length, 1);
    assert.equal(forms[0].path, "/providers");
    assert.equal(forms[0].form.get("id"), "vertex");
    assert.equal(forms[0].form.get("force_api_support"), String(enabled));
  }
});
