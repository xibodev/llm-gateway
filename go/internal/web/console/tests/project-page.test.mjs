import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, mount, settle, text } from "./hook-harness.mjs";
import { fakeKeyListing } from "./key-listing.mjs";

const stubs = [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args); export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" },
  { filter: /\/useDialogFocus$/, contents: "export const useDialogFocus = () => ({ current: null });" },
];
const { ProjectDetail } = await bundle(fileURLToPath(new URL("../src/pages/ProjectDetail.tsx", import.meta.url)), stubs);
const { Access } = await bundle(fileURLToPath(new URL("../src/pages/Access.tsx", import.meta.url)), stubs);
const { Settings } = await bundle(fileURLToPath(new URL("../src/pages/Settings.tsx", import.meta.url)), stubs);

const data = {
  projects: [{ id: "prj 1", name: "Research", slug: "research", status: "active" }, { id: "prj-2", name: "Other", slug: "other", status: "active" }],
  // Memberships name their principal, and the state counts keys and
  // principals rather than listing them.
  memberships: [
    { project_id: "prj 1", principal_id: "prn-ada", role: "owner", principal_name: "Ada", principal_kind: "human", principal_status: "active" },
    { project_id: "prj-2", principal_id: "prn-bot", role: "member", principal_name: "Batch", principal_kind: "service", principal_status: "active" },
  ],
  counts: { keys: 3, principals: 2, active_principals: 2, active_humans: 1, project_owners: 1 },
};
// The key listing serves the project's keys, each naming its owner.
const keys = [
  { id: "key-1", name: "notebook", prefix: "llmgw_ab", project_id: "prj 1", principal_id: "prn-ada", principal: "Ada", status: "active", last_used_at: 1798783140 },
  { id: "key-2", name: "retired", prefix: "llmgw_cd", project_id: "prj 1", principal_id: "prn-ada", principal: "Ada", status: "revoked" },
  { id: "key-3", name: "elsewhere", prefix: "llmgw_ef", project_id: "prj-2", principal_id: "prn-bot", principal: "Batch", status: "active" },
];
const limits = [{ scope: "project", field: "daily_requests", metric: "requests", period: "day", limit: 100, used: 40, resets_at: 1798761600, closest: true }];
const usage = {
  series: [{ start: 1, requests: 30, errors: 2, input_tokens: 1000, output_tokens: 500, cost_microusd: 2_000_000 }, { start: 2, requests: 10, errors: 0, input_tokens: 0, output_tokens: 0, cost_microusd: 0 }],
  control_plane: { groups: { key: [{ key_id: "key-1", key_name: "notebook", requests: 38, errors: 2, input_tokens: 1000, output_tokens: 500, cost_microusd: 2_000_000 }, { key_id: "", requests: 2 }] } },
};

function fakeAPI() {
  const paths = [];
  const sent = [];
  const listing = fakeKeyListing(keys);
  globalThis.__api = {
    async getJSON(mode, path) { paths.push(`${mode} ${path}`); if (path.startsWith("/keys?")) return listing.answer(path); return path.includes("/limits") ? { limits } : usage; },
    async sendJSON(mode, path, method) { sent.push(`${method} ${path}`); return {}; },
  };
  return { paths, sent };
}

test("a project's page shows its limits, keys, members and usage", async () => {
  const { paths, sent } = fakeAPI();
  const navigated = [];
  let changed = 0;
  const render = mount(() => ProjectDetail({ projectID: "prj 1", data, onChanged: async () => { changed += 1; }, onNavigate: (page, detail) => navigated.push([page, detail]), onBack() { navigated.push(["back"]); } }));
  render();
  await settle();
  let tree = render();
  assert.equal(text(find(tree, (node) => node.type === "h1")), "Research");
  assert.equal(paths[0], "admin /projects/prj%201/limits");
  const usagePath = new URLSearchParams(paths[1].split("?")[1]);
  assert.equal(usagePath.get("project_id"), "prj 1");
  assert.equal(Number(usagePath.get("to")) - Number(usagePath.get("from")), 30 * 24 * 60 * 60);
  assert.equal(paths[2], "admin /keys?status=all&project_id=prj+1&limit=25", "the server pages the project's keys, of every status");

  const table = find(tree, (node) => typeof node.type === "function" && Array.isArray(node.props?.limits));
  assert.deepEqual(table.props.limits, limits, "the project's limits are shown with their usage");
  const policy = find(tree, (node) => typeof node.type === "function" && node.props?.projectID === "prj 1");
  assert.ok(policy, "the project's budgets and allowlists are edited on its page");

  const rows = findAll(tree, (node) => node.type === "tr").map(text);
  assert.ok(rows.some((row) => row.startsWith("notebookllmgw_abAdaactive")), "an active key of the project is listed");
  assert.ok(rows.some((row) => row.startsWith("retiredllmgw_cdAdarevoked")), "so is a revoked one");
  assert.ok(!rows.some((row) => row.includes("elsewhere")), "another project's key is not");
  assert.ok(rows.some((row) => row.startsWith("Adahumanowner")), "the project's member is listed");
  assert.ok(!rows.some((row) => row.startsWith("Batch")), "another project's member is not");
  assert.ok(text(tree).includes(`Requests40Failed2Tokens${(1500).toLocaleString()}Estimated cost$2.0000`), "the last 30 days are summed");
  assert.ok(rows.some((row) => row.startsWith(`notebook382${(1500).toLocaleString()}`)), "usage is broken down by key");
  assert.ok(rows.some((row) => row.startsWith("Playground or external keys2")), "usage without a key is named");

  find(tree, (node) => node.type === "button" && text(node).includes("Manage keys")).props.onClick();
  assert.deepEqual(navigated.at(-1), ["keys", "project=prj%201"]);
  find(tree, (node) => node.type === "button" && node.props?.["aria-label"] === "Remove Ada from Research").props.onClick();
  await settle();
  assert.deepEqual(sent, ["DELETE /memberships?project_id=prj%201&principal_id=prn-ada"]);
  assert.equal(changed, 1);

  policy.props.onSaved("Project policy saved. Empty fields mean no limit.");
  await settle();
  tree = render();
  assert.equal(paths.filter((path) => path.includes("/limits")).length, 2, "saving the policy shows the limits it set");
  assert.match(text(find(tree, (node) => node.props?.role === "status")), /Project policy saved/);
});

test("an unknown project says so and leads back to Access", async () => {
  fakeAPI();
  const navigated = [];
  const render = mount(() => ProjectDetail({ projectID: "prj-gone", data, onChanged: async () => {}, onNavigate() {}, onBack() { navigated.push("back"); } }));
  render();
  await settle();
  const tree = render();
  assert.ok(find(tree, (node) => node.props?.title === "Unknown project"));
  find(tree, (node) => node.type === "button" && text(node).includes("Access")).props.onClick();
  assert.deepEqual(navigated, ["back"]);
});

test("Access opens a project's page, and Settings keeps gateway-wide options", () => {
  fakeAPI();
  const navigated = [];
  const list = mount(() => Access({ data, mode: "admin", onChanged: async () => {}, onNavigate: (page, detail) => navigated.push([page, detail]) }));
  find(list(), (node) => node.type === "button" && node.props?.["aria-label"] === "Open Research").props.onClick();
  assert.deepEqual(navigated, [["access", "prj 1"]]);

  const page = mount(() => Access({ data, mode: "admin", detail: "prj 1", onChanged: async () => {}, onNavigate: (next, detail) => navigated.push([next, detail]) }));
  const detail = page();
  assert.equal(detail.props.projectID, "prj 1", "a detail opens that project's page");
  detail.props.onBack();
  assert.deepEqual(navigated.at(-1), ["access", undefined]);

  const settings = mount(() => Settings({ data, mode: "admin", onNavigate() {} }));
  const tree = settings();
  assert.doesNotMatch(text(tree), /Budgets and allowlists/);
  assert.match(text(tree), /each project's page holds its budgets and allowlists/);
});
