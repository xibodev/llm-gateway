import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, text } from "./hook-harness.mjs";

const table = await bundle(fileURLToPath(new URL("../src/components/DataTable.tsx", import.meta.url)));

const columns = [
  { id: "name", header: "Name", cell: (row) => row.name, sortValue: (row) => row.name },
  { id: "count", header: "Count", cell: (row) => String(row.count), sortValue: (row) => row.count },
  { id: "note", header: "Note", cell: (row) => row.note ?? "" },
];
const rows = Array.from({ length: 60 }, (_, index) => ({ id: `row-${index}`, name: `item ${index}`, count: index % 7 }));

test("a header press sorts ascending, then descending, then as the list was", () => {
  let sort = table.nextSort(null, "name");
  assert.deepEqual(sort, { id: "name", descending: false });
  sort = table.nextSort(sort, "name");
  assert.deepEqual(sort, { id: "name", descending: true });
  assert.equal(table.nextSort(sort, "name"), null);
  assert.deepEqual(table.nextSort(sort, "count"), { id: "count", descending: false }, "another column starts ascending");
});

test("rows sort naturally and keep their order where they tie", () => {
  const names = (sorted) => sorted.map((row) => row.name);
  const items = [{ name: "item 10", rank: 1 }, { name: "item 2", rank: 1 }, { name: "Item 1", rank: 2 }];
  assert.deepEqual(names(table.sortRows(items, (row) => row.name, false)), ["Item 1", "item 2", "item 10"]);
  assert.deepEqual(names(table.sortRows(items, (row) => row.rank, false)), ["item 10", "item 2", "Item 1"], "ties keep their order");
  assert.deepEqual(names(table.sortRows(items, (row) => row.rank, true)), ["Item 1", "item 10", "item 2"], "descending too");
});

test("a table the console holds pages and sorts its rows", () => {
  let shown = rows;
  const render = mount(() => table.useTableView(shown, columns));
  let view = render();
  assert.equal(view.total, 60);
  assert.equal(view.rows.length, 25);
  assert.equal(view.rows[0].name, "item 0");
  view.setPage(2);
  view = render();
  assert.deepEqual(view.rows.map((row) => row.id), rows.slice(50).map((row) => row.id));
  view.toggleSort("count");
  view = render();
  assert.equal(view.page, 0, "sorting returns to the first page");
  assert.deepEqual(view.rows.slice(0, 3).map((row) => row.count), [0, 0, 0]);
  view.setPageSize(50);
  view = render();
  assert.equal(view.rows.length, 50);
  view.setPage(1);
  shown = rows.slice(0, 20);
  view = render();
  assert.equal(view.page, 0, "a page past the end of fewer rows shows the last page");
  assert.equal(view.rows.length, 20);
});

test("a table renders sortable headers, labelled cells and paging only when it needs them", () => {
  const sorted = [];
  const view = { rows: rows.slice(0, 2), total: 60, page: 0, pageSize: 25, pageSizes: [25, 50, 100], sort: { id: "name", descending: true }, setPage() {}, setPageSize() {}, toggleSort: (id) => sorted.push(id) };
  const tree = table.dataTable(view, columns, { label: "Items", rowKey: (row) => row.id, class: "items-table" });
  const headers = findAll(tree, (node) => node.type === "th");
  assert.deepEqual(headers.map((header) => header.props["aria-sort"]), ["descending", "none", undefined]);
  find(headers[1], (node) => node.type === "button").props.onClick();
  assert.deepEqual(sorted, ["count"]);
  assert.equal(find(headers[2], (node) => node.type === "button"), undefined, "a column without a sort value has no sort button");
  assert.equal(find(tree, (node) => node.type === "table").props.class, "items-table");
  const row = find(tree, (node) => node.type === "tr" && node.key === "row-0");
  assert.deepEqual(findAll(row, (node) => node.type === "td").map((cell) => cell.props["data-label"]), ["Name", "Count", "Note"]);
  assert.ok(find(tree, (node) => node.props?.class === "data-table__footer"), "60 rows page");
  const small = table.dataTable({ ...view, total: 2 }, columns, { label: "Items", rowKey: (item) => item.id });
  assert.equal(find(small, (node) => node.props?.class === "data-table__footer"), undefined, "rows that fit one page do not");
});

test("a table the server pages asks for a new page when its order or page size changes", () => {
  const states = [];
  const view = table.serverTableView([{ id: "a" }], 120, { page: 3, pageSize: 25, sort: null }, (next) => states.push(next));
  view.toggleSort("created");
  view.setPageSize(100);
  view.setPage(1);
  assert.deepEqual(states, [
    { page: 0, pageSize: 25, sort: { id: "created", descending: false } },
    { page: 0, pageSize: 100, sort: null },
    { page: 1, pageSize: 25, sort: null },
  ]);
});

test("long IDs show their ends and copy whole", () => {
  assert.equal(table.shortID("prj_0123456789abcdef0123"), "prj_012345…0123");
  assert.equal(table.shortID("short-id"), "short-id");
});

test("a long option list can be searched without losing the chosen option", () => {
  const options = Array.from({ length: 12 }, (_, index) => ({ value: `p${index}`, label: index === 5 ? "Grace Hopper" : `Person ${index}` }));
  let chosen = "p0";
  const render = mount(() => table.SearchSelect({ label: "Owner", value: chosen, options, onChange: (value) => { chosen = value; }, noun: "owners" }));
  let tree = render();
  const search = find(tree, (node) => node.type === "input");
  assert.equal(search.props.placeholder, "Search 12 owners");
  search.props.onInput(input("grace"));
  tree = render();
  const shown = () => findAll(tree, (node) => node.type === "option").map((option) => option.props.value);
  assert.deepEqual(shown(), ["p0", "p5"], "the chosen option stays beside the match");
  find(tree, (node) => node.type === "select").props.onChange(input("p5"));
  assert.equal(chosen, "p5");

  const few = mount(() => table.SearchSelect({ label: "Project", value: "a", options: [{ value: "a", label: "Alpha" }], onChange() {} }))();
  assert.equal(find(few, (node) => node.type === "input"), undefined, "a short list needs no search");
  assert.equal(text(few), "ProjectAlpha");
});
