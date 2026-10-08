import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const activity = await bundle(fileURLToPath(new URL("../src/lib/activity.ts", import.meta.url)));
const { Alerts } = await bundle(fileURLToPath(new URL("../src/pages/Alerts.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args); export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);" },
]);

test("a delivery's state and next attempt read as an operator needs them", () => {
  const now = Date.UTC(2026, 9, 8, 12) ;
  const seconds = now / 1000;
  assert.deepEqual(activity.deliveryState({ status: "delivered" }, now), { label: "delivered", tone: "ready" });
  assert.deepEqual(activity.deliveryState({ status: "failed", exhausted: true }, now), { label: "exhausted", tone: "attention" });
  assert.deepEqual(activity.deliveryState({ status: "failed" }, now), { label: "retrying", tone: "attention" });
  assert.deepEqual(activity.deliveryState({ status: "pending", lease_until: seconds + 60 }, now), { label: "claimed", tone: "muted" });
  assert.deepEqual(activity.deliveryState({ status: "pending", lease_until: seconds - 60 }, now), { label: "pending", tone: "muted" });
  assert.equal(activity.nextAttemptLabel({ next_attempt_at: 0 }, now), "—");
  assert.equal(activity.nextAttemptLabel({ next_attempt_at: seconds - 1 }, now), "Due now");
  assert.equal(activity.nextAttemptLabel({ next_attempt_at: seconds + 600 }, now), new Date(now + 600000).toLocaleString());
  assert.equal(activity.deliveriesPath(activity.emptyDeliveryFilter), "/deliveries?limit=50");
  assert.equal(activity.deliveriesPath({ status: "exhausted", kind: "key_expiring" }, 9), "/deliveries?limit=50&status=exhausted&kind=key_expiring&before_id=9");
  assert.equal(activity.deliveryKindLabel("quota_exhausted"), "Quota exhausted");
});

test("the alerts page lists deliveries, filters them and pages back", async () => {
  const paths = [];
  globalThis.__api = {
    async getJSON(mode, path) {
      paths.push(path);
      if (path === "/alerts") return { rules: [] };
      if (path.includes("before_id=7")) return { deliveries: [{ id: 6, ts: 1, kind: "quota_warning", status: "delivered", attempts: 1, max_attempts: 10, delivered_at: 2 }], next_before_id: 0 };
      return {
        deliveries: [
          { id: 8, ts: 3, kind: "key_expiring", status: "failed", attempts: 10, max_attempts: 10, exhausted: true, last_error: "webhook answered 503", project_id: "project-1" },
          { id: 7, ts: 2, kind: "quota_exhausted", status: "failed", attempts: 2, max_attempts: 10, next_attempt_at: 1, last_error: "timeout" },
        ],
        next_before_id: 7,
      };
    },
  };
  const data = { projects: [{ id: "project-1", name: "Project one" }] };
  const render = mount(() => Alerts({ data }));
  render();
  await settle();
  let tree = render();
  assert.deepEqual(paths.slice(0, 2).sort(), ["/alerts", "/deliveries?limit=50"]);
  const row = (id) => text(find(tree, (node) => node.type === "tr" && node.key === id));
  assert.match(row("8"), /Key expiring.*Project one.*exhausted.*10 of 10.*—.*webhook answered 503/);
  assert.match(row("7"), /Quota exhausted.*retrying.*2 of 10.*Due now.*timeout/);

  find(tree, (node) => node.type === "button" && text(node) === "Load older deliveries").props.onClick();
  await settle();
  tree = render();
  assert.match(paths.at(-1), /before_id=7/);
  assert.match(row("6"), /Quota warning.*delivered.*1 of 10/);
  assert.equal(find(tree, (node) => node.type === "button" && text(node) === "Load older deliveries"), undefined);

  const statusSelect = find(tree, (node) => node.type === "select" && findAll(node, (option) => option.props?.value === "exhausted").length);
  statusSelect.props.onInput(input("exhausted"));
  tree = render();
  find(tree, (node) => node.type === "form" && findAll(node, (child) => child.props?.value === "exhausted").length).props.onSubmit({ preventDefault() {} });
  await settle();
  assert.equal(paths.at(-1), "/deliveries?limit=50&status=exhausted");
});
