import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const activity = await bundle(fileURLToPath(new URL("../src/lib/activity.ts", import.meta.url)));
const apiStub = [{ filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args);" }];
const { Requests } = await bundle(fileURLToPath(new URL("../src/pages/Requests.tsx", import.meta.url)), apiStub);
const { AuditLog } = await bundle(fileURLToPath(new URL("../src/pages/AuditLog.tsx", import.meta.url)), apiStub);
const navigation = await bundle(fileURLToPath(new URL("../src/lib/navigation.ts", import.meta.url)));

test("listing paths carry only the filters that select something", () => {
  assert.equal(activity.requestsPath(activity.emptyRequestFilter), "/requests?limit=50");
  assert.equal(
    activity.requestsPath({ ...activity.emptyRequestFilter, status: "error", provider: "openai", model: " gpt ", requestID: "req_1" }, 42),
    "/requests?limit=50&status=error&provider=openai&model=gpt&request_id=req_1&before_id=42",
  );
  assert.equal(activity.auditPath(activity.emptyAuditFilter), "/audit?limit=50");
  assert.equal(
    activity.auditPath({ ...activity.emptyAuditFilter, action: "provider.", result: "denied", targetID: "x" }, 7),
    "/audit?limit=50&action=provider.&result=denied&target_id=x&before_id=7",
  );
});

test("statuses, results, models, tokens and actors read as an operator needs them", () => {
  assert.equal(activity.statusTone(200), "ready");
  assert.equal(activity.statusTone(429), "attention");
  assert.equal(activity.statusTone(0), "attention");
  assert.equal(activity.auditTone("success"), "ready");
  assert.equal(activity.auditTone("denied"), "attention");
  assert.equal(activity.modelSummary({ requested_model: "fast", routed_model: "openai/gpt-5.6" }), "fast → openai/gpt-5.6");
  assert.equal(activity.modelSummary({ requested_model: "m", routed_model: "m" }), "m");
  assert.equal(activity.modelSummary({}), "—");
  assert.equal(activity.tokenSummary({ input_tokens: 1200, output_tokens: 3 }), `${(1200).toLocaleString()} in / 3 out`);
  assert.equal(activity.tokenSummary({}), "—");
  assert.equal(activity.auditActor({ actor_principal_id: "p1", detail: {} }), "p1");
  assert.equal(activity.auditActor({ detail: { actor_source: "static_key", actor_key_fingerprint: "abcdef012345" } }), "static key abcdef012345");
  assert.deepEqual(activity.safeAuditDetail({ provider: "x", api_key_hint: "y", refresh_token: "z" }), { provider: "x" });
});

test("the audit log is an administrator page and the requests page serves both", () => {
  const admin = navigation.navigationFor("admin").map((item) => item.id);
  const portal = navigation.navigationFor("portal").map((item) => item.id);
  assert.ok(admin.includes("audit"));
  assert.ok(!portal.includes("audit"));
  assert.ok(admin.includes("requests"));
  assert.ok(portal.includes("requests"));
});

test("the portal requests page lists the user's requests without the gateway's failover chains", async () => {
  const paths = [];
  globalThis.__api = {
    async getJSON(mode, path) {
      paths.push(`${mode} ${path}`);
      return {
        requests: [{ id: 2, ts: 2, request_id: "req_mine", endpoint: "chat", status_code: 502, provider: "beta" }],
        next_before_id: 0, providers: ["alpha", "beta"],
      };
    },
  };
  const render = mount(() => Requests({ data: { projects: [] }, mode: "portal" }));
  render();
  await settle();
  let tree = render();
  assert.deepEqual(paths, ["portal /requests?limit=50"], "the portal asks for nothing but its own listing");
  assert.match(text(tree), /req_mine/);
  assert.match(text(tree), /Portal/, "a request without a key or project came from the portal");
  assert.doesNotMatch(text(tree), /Recent failover chains/);
  const providerSelect = find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "beta").length);
  assert.deepEqual(findAll(providerSelect, (node) => node.type === "option").map((option) => option.props.value), ["all", "alpha", "beta"]);
});

test("the requests page lists, filters and pages recorded requests", async () => {
  const paths = [];
  globalThis.__api = {
    async getJSON(mode, path) {
      paths.push(`${mode} ${path}`);
      if (path === "/telemetry") return { recent: [{ ts: 1, requested: "fast", served: "openai/gpt", attempts: [{ provider: "slow", model: "m", ok: false, error: "timeout" }, { provider: "openai", model: "gpt", ok: true }] }] };
      if (path.includes("before_id=2")) return { requests: [{ id: 1, ts: 1, request_id: "req_old", status_code: 200, endpoint: "chat" }], next_before_id: 0 };
      return {
        requests: [
          { id: 3, ts: 3, request_id: "req_new", endpoint: "chat", status_code: 502, error_code: "upstream", provider: "openai", requested_model: "fast", routed_model: "gpt", key_id: "k1", key_name: "ci key", latency_ms: 812, input_tokens: 11, output_tokens: 3 },
          { id: 2, ts: 2, request_id: "req_mid", endpoint: "messages", status_code: 200, provider: "anthropic", key_id: "k-deleted", key_name: "retired key" },
        ],
        next_before_id: 2,
      };
    },
  };
  const data = { providers: [{ id: "openai" }], projects: [] };
  const render = mount(() => Requests({ data, mode: "admin" }));
  render();
  await settle();
  let tree = render();
  assert.deepEqual(paths.slice(0, 2).sort(), ["admin /requests?limit=50", "admin /telemetry"]);
  const rows = () => findAll(tree, (node) => node.type === "tr");
  assert.match(text(tree), /req_new/);
  assert.match(text(tree), /ci key/, "a key is named, not shown by ID");
  assert.match(text(tree), /retired key/, "a deleted key is named as the request records it");
  assert.match(text(tree), /fast → gpt/);
  const pill = find(tree, (node) => node.type === "span" && text(node).startsWith("502"));
  assert.match(pill.props.class, /status-pill--attention/);
  assert.match(text(tree), /slow\/m timeout; openai\/gpt ok/);

  find(tree, (node) => node.type === "button" && text(node) === "Load older requests").props.onClick();
  await settle();
  tree = render();
  assert.match(paths.at(-1), /before_id=2/);
  assert.match(text(tree), /req_old/);
  assert.equal(find(tree, (node) => node.type === "button" && text(node) === "Load older requests"), undefined, "the last page offers no older one");
  assert.ok(rows().length >= 4);

  const status = find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "error").length);
  status.props.onInput(input("error"));
  tree = render();
  find(tree, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });
  await settle();
  assert.match(paths.at(-1), /status=error/);
});

test("the audit log filters, pages and colours results", async () => {
  const paths = [];
  globalThis.__api = {
    async getJSON(mode, path) {
      paths.push(path);
      if (path.includes("before_id=5")) return { events: [{ id: 4, ts: 1, action: "key.issue", result: "success", detail: {} }], next_before_id: 0 };
      return {
        events: [{ id: 5, ts: 2, action: "provider.delete", target_type: "provider", target_id: "old", result: "denied", detail: { actor_source: "static_key", actor_key_fingerprint: "abcdef012345", secret_value: "x" } }],
        next_before_id: 5,
      };
    },
  };
  const render = mount(() => AuditLog({ mode: "admin" }));
  render();
  await settle();
  let tree = render();
  assert.equal(paths[0], "/audit?limit=50");
  assert.match(text(tree), /static key abcdef012345/);
  assert.match(text(tree), /provider old/);
  assert.doesNotMatch(text(tree), /secret_value/);
  assert.match(find(tree, (node) => node.type === "span" && text(node) === "denied").props.class, /status-pill--attention/);

  find(tree, (node) => node.type === "button" && text(node) === "Load older events").props.onClick();
  await settle();
  tree = render();
  assert.match(paths.at(-1), /before_id=5/);
  assert.match(text(tree), /key\.issue/);

  const action = find(tree, (node) => node.type === "input" && node.props?.placeholder?.startsWith("Prefix"));
  action.props.onInput(input("key."));
  tree = render();
  find(tree, (node) => node.type === "form").props.onSubmit({ preventDefault() {} });
  await settle();
  assert.equal(paths.at(-1), "/audit?limit=50&action=key.");
});
