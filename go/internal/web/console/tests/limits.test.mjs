import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount, settle, text } from "./hook-harness.mjs";

const limits = await bundle(fileURLToPath(new URL("../src/lib/limits.ts", import.meta.url)));
const apiStub = { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args);\nexport const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" };
const focusStub = { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" };
const { KeyLimitsDialog, LimitUsageTable } = await bundle(fileURLToPath(new URL("../src/components/LimitUsage.tsx", import.meta.url)), [apiStub, focusStub]);
const { ApiKeys } = await bundle(fileURLToPath(new URL("../src/pages/ApiKeys.tsx", import.meta.url)), [apiStub, focusStub]);
const { Requests } = await bundle(fileURLToPath(new URL("../src/pages/Requests.tsx", import.meta.url)), [apiStub]);

test("the requests page names the limit that refused a request", async () => {
  globalThis.__api = { async getJSON(mode, path) {
    if (path === "/telemetry") return { recent: [] };
    return { requests: [
      { id: 2, ts: 2, request_id: "req_refused", endpoint: "chat", status_code: 429, error_code: "quota:project:daily_requests" },
      { id: 1, ts: 1, request_id: "req_failed", endpoint: "chat", status_code: 502, error_code: "upstream" },
    ], next_before_id: 0 };
  } };
  const render = mount(() => Requests({ data: { keys: [], projects: [] }, mode: "admin" }));
  render();
  await settle();
  const tree = render();
  const pills = findAll(tree, (node) => node.type === "span" && /status-pill/.test(node.props?.class ?? ""));
  assert.deepEqual(pills.map(text), ["429 Project limit: requests per day", "502 upstream"]);
  assert.equal(pills[0].props.title, "quota:project:daily_requests", "the recorded code stays readable");
});

test("a refused request names the limit that refused it", () => {
  assert.equal(limits.errorCodeLabel("quota:key:rpm"), "Key limit: requests per minute");
  assert.equal(limits.errorCodeLabel("quota:project:monthly_total_tokens"), "Project limit: tokens per month");
  assert.equal(limits.errorCodeLabel("rate_limit"), "Gateway limit: requests per minute per caller");
  assert.equal(limits.errorCodeLabel("upstream"), "upstream", "other codes read as recorded");
});

test("a limit's amounts carry their units and its state follows its share", () => {
  assert.equal(limits.formatLimitAmount(2_500_000, "cost_microusd"), "$2.5000");
  assert.equal(limits.formatLimitAmount(1500, "credits_milli"), `${(1.5).toLocaleString()} credits`);
  assert.equal(limits.formatLimitAmount(1200, "input_tokens"), (1200).toLocaleString());
  assert.equal(limits.limitShare({ limit: 100, used: 150 }), 1, "a limit counted past its end is used up");
  assert.equal(limits.limitShare({ limit: 0, used: 5 }), 0);
  assert.equal(limits.limitState({ limit: 10, used: 10 }).label, "Used up");
  assert.equal(limits.limitState({ limit: 10, used: 8 }).label, "Near its limit");
  assert.deepEqual(limits.limitState({ limit: 10, used: 1 }), { label: "Has room", tone: "ready" });
});

test("open circuits say whose requests they refuse and for how long", () => {
  const until = "2026-10-08T12:00:30Z";
  const at = new Date(Date.parse(until)).toLocaleTimeString();
  const name = (id) => `${id}'s requests`;
  assert.deepEqual(limits.openCircuitsOf([{ open_circuits: [{ failures: 3 }] }, {}]), [{ failures: 3 }]);
  assert.equal(limits.circuitSummary([], name), "");
  assert.equal(limits.circuitSummary([{ failures: 3, open_until: until }], name), `Refusing requests on the gateway's credential until ${at}, after 3 failures in a row.`);
  assert.equal(limits.circuitSummary([{ principal_id: "Ada", failures: 1, open_until: until }], name), `Refusing Ada's requests until ${at}, after 1 failure in a row.`);
  assert.equal(limits.circuitSummary([{ open_until: "2026-10-08T11:00:00Z" }, { principal_id: "Ada", open_until: until }], name), `Refusing the requests of 2 callers until ${at} at the latest.`);
});

const report = {
  key_id: "key-1",
  limits: [
    { scope: "key", field: "daily_requests", metric: "requests", period: "day", limit: 2, used: 2, resets_at: 1798761600, closest: true },
    { scope: "project", field: "monthly_cost_microusd", metric: "cost_microusd", period: "month", limit: 5_000_000, used: 1_000_000, resets_at: 1801440000 },
    { scope: "caller", field: "rate_limit_per_minute", metric: "requests", period: "minute", limit: 60, used: 3, resets_at: 1798700460 },
  ],
};

test("a key's limits show each limit's usage, when its window ends and which is closest", async () => {
  const paths = [];
  let fail = false;
  globalThis.__api = { async getJSON(mode, path) { paths.push(`${mode} ${path}`); if (fail) throw new Error("Quota store unavailable."); return report; } };
  const dialog = mount(() => KeyLimitsDialog({ mode: "portal", apiKey: { id: "key 1", name: "ci" }, onClose() {}, returnFocus: null }));
  let tree = dialog();
  assert.match(text(tree), /Loading limits/);
  await settle();
  tree = dialog();
  assert.deepEqual(paths, ["portal /keys/key%201/limits"]);
  // The table is a nested component: render it as the dialog would.
  const table = find(tree, (node) => typeof node.type === "function" && Array.isArray(node.props?.limits));
  const rendered = table.type(table.props);
  const rows = findAll(rendered, (node) => node.type === "tr").slice(1).map(text);
  assert.equal(rows.length, 3);
  assert.match(rows[0], /^Requests per dayClosest to refusing requestsKey.*2 of 2 \(100%\) Used up/);
  assert.match(rows[1], /Estimated cost per monthProject.*\$1\.0000 of \$5\.0000 \(20%\) Has room/);
  assert.match(rows[2], /Requests per minuteGateway, per caller.*3 of 60 \(5%\)/);
  assert.ok(rows[0].endsWith(new Date(1798761600 * 1000).toLocaleString()), "a window ends at the local time of its UTC end");
  assert.match(text(tree), /per-caller limit counts on the gateway process that answered/);

  fail = true;
  find(tree, (node) => node.type === "button" && text(node).includes("Refresh")).props.onClick();
  await settle();
  tree = dialog();
  assert.equal(paths.length, 2, "refresh asks again");
  assert.match(text(find(tree, (node) => node.props?.role === "alert")), /Quota store unavailable\./);
});

test("a key without limits says none applies", () => {
  assert.equal(text(LimitUsageTable({ limits: [], empty: "No limit applies to this key." })), "No limit applies to this key.");
});

const isLimitsDialog = (node) => typeof node.type === "function" && node.props?.apiKey !== undefined;

test("every key in the list opens its limits", () => {
  globalThis.__api = { async getJSON() { return report; }, async sendJSON() { return {}; } };
  const data = {
    projects: [{ id: "project-1", name: "Project one", status: "active" }],
    principals: [{ id: "user-1", kind: "human", status: "active", display_name: "Ada" }],
    memberships: [{ project_id: "project-1", principal_id: "user-1", role: "owner", status: "active" }],
    keys: [
      { id: "key-1", name: "ci", status: "active", project_id: "project-1", principal_id: "user-1" },
      { id: "key-2", name: "old", status: "revoked", project_id: "project-1", principal_id: "user-1" },
    ],
  };
  const render = mount(() => ApiKeys({ data, mode: "admin", onChanged: async () => {} }));
  let tree = render();
  const opener = find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === "Limits of ci");
  assert.ok(opener, "an active key offers its limits");
  const statuses = find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "revoked").length);
  statuses.props.onChange({ currentTarget: { value: "all" } });
  tree = render();
  assert.ok(find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === "Limits of old"), "so does a revoked one, whose usage still counts");
  const button = { tagName: "BUTTON" };
  opener.props.onClick({ currentTarget: button });
  tree = render();
  const dialog = find(tree, isLimitsDialog);
  assert.equal(dialog.props.apiKey.id, "key-1");
  assert.equal(dialog.props.mode, "admin");
  assert.equal(dialog.props.returnFocus, button, "closing returns focus to the button that opened it");
  dialog.props.onClose();
  tree = render();
  assert.equal(find(tree, isLimitsDialog), undefined);
});
