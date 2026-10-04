import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";

const usage = await bundle(fileURLToPath(new URL("../src/lib/usage.ts", import.meta.url)));
const { UsageQuotas } = await bundle(fileURLToPath(new URL("../src/pages/UsageQuotas.tsx", import.meta.url)), [
  { filter: /\/lib\/api$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args);" },
]);
const day = 24 * 60 * 60;

test("usage totals add up the filtered series", () => {
  assert.deepEqual(usage.usageTotals([{ requests: 3, errors: 1 }, { requests: 0 }, { requests: 4, errors: 2 }]), { requests: 7, errors: 3 });
  assert.deepEqual(usage.usageTotals([]), { requests: 0, errors: 0 });
});

test("hour buckets are allowed only for ranges the server will chart", () => {
  assert.equal(usage.usageBucketAllowed(7 * day, "hour"), true);
  assert.equal(usage.usageBucketAllowed(30 * day, "hour"), true);
  // 41 days touch at most 985 hourly buckets, 42 days at least 1008.
  assert.equal(usage.usageBucketAllowed(41 * day, "hour"), true);
  assert.equal(usage.usageBucketAllowed(42 * day, "hour"), false);
  assert.equal(usage.usageBucketAllowed(90 * day, "hour"), false);
  assert.equal(usage.usageBucketAllowed(90 * day, "day"), true);
  assert.equal(usage.usageBucketAllowed(90 * day, "week"), true);
  assert.equal(usage.usageBucketAllowed(day, "minute"), false);
});

test("bucket labels name the UTC bucket whatever the browser's time zone", () => {
  const previous = process.env.TZ;
  process.env.TZ = "America/Los_Angeles";
  try {
    const midnight = new Date(Date.UTC(2026, 9, 3));
    const dayLabel = usage.usageBucketLabel(midnight.getTime() / 1000, "day");
    assert.equal(dayLabel, midnight.toLocaleDateString([], { month: "short", day: "numeric", timeZone: "UTC" }));
    assert.notEqual(dayLabel, midnight.toLocaleDateString([], { month: "short", day: "numeric" }), "a local label would name the previous day");
    const early = new Date(Date.UTC(2026, 9, 3, 5));
    const hourLabel = usage.usageBucketLabel(early.getTime() / 1000, "hour");
    assert.equal(hourLabel, early.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", timeZone: "UTC" }));
    assert.notEqual(hourLabel, early.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
  } finally {
    if (previous === undefined) delete process.env.TZ;
    else process.env.TZ = previous;
  }
});

test("the usage page totals the filtered series and keeps bucket choices the server accepts", async () => {
  const requests = [];
  const start = Date.UTC(2026, 9, 3) / 1000;
  globalThis.__api = {
    async getJSON(mode, path) {
      requests.push(path);
      return {
        control_plane: { totals: { requests: 999, errors: 99 } },
        series: [{ start, requests: 3, errors: 1 }, { start: start + day, requests: 2, errors: 0 }],
      };
    },
  };
  const render = mount(() => UsageQuotas({ data: { providers: [], keys: [], projects: [] }, mode: "admin" }));
  render();
  await settle();
  let tree = render();
  const card = (label) => text(find(tree, (node) => node.type === "article" && text(node).includes(label)));
  assert.match(card("Recorded requests"), /Recorded requests5Filtered/);
  assert.match(card("Failed requests"), /Failed requests1Recorded/);

  const selects = () => findAll(tree, (node) => node.type === "select");
  const bucketSelect = () => selects().find((node) => findAll(node, (option) => option.props?.value === "hour").length);
  const rangeSelect = () => selects().find((node) => findAll(node, (option) => option.props?.value === "90").length);
  bucketSelect().props.onInput(input("hour"));
  tree = render();
  assert.equal(bucketSelect().props.value, "hour");
  rangeSelect().props.onInput(input("90"));
  tree = render();
  assert.equal(bucketSelect().props.value, "day", "a range too long for hourly buckets falls back to days");
  assert.equal(find(bucketSelect(), (node) => node.props?.value === "hour").props.disabled, true);

  bucketSelect().props.onInput(input("week"));
  tree = render();
  assert.match(text(tree), /Requests by day/, "the chart describes the report it shows until filters are applied");
  const labels = findAll(tree, (node) => node.type === "small" && node.props?.children === usage.usageBucketLabel(start, "day"));
  assert.equal(labels.length, 1);

  find(tree, (node) => node.type === "button" && text(node) === "Apply filters").props.onClick();
  await settle();
  tree = render();
  assert.match(requests.at(-1), /bucket=week/);
  assert.match(text(tree), /Requests by week/);
});
