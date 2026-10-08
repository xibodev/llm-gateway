import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, findAll, input, mount, settle, text } from "./hook-harness.mjs";
import { fakeKeyListing, fakePrincipalListing } from "./key-listing.mjs";

// Pages import the API as ../lib/api and the directory imports it as ./api.
const apiStub = { filter: /(\/lib\/api|^\.\/api)$/, contents: "export const getJSON = (...args) => globalThis.__api.getJSON(...args);" };
const { RemoteSearchSelect } = await bundle(fileURLToPath(new URL("../src/components/DataTable.tsx", import.meta.url)), [apiStub]);
const directory = await bundle(fileURLToPath(new URL("../src/lib/directory.ts", import.meta.url)), [apiStub]);

const people = [
  { id: "prn-ada", kind: "human", display_name: "Ada", status: "active" },
  { id: "prn-bob", kind: "human", display_name: "bob", status: "active" },
  { id: "prn-cy", kind: "human", display_name: "Cy", status: "disabled" },
  { id: "prn-bot", kind: "service", display_name: "Batch", status: "active" },
];
const options = (tree) => findAll(tree, (node) => node.type === "option").map((option) => [option.props.value, text(option)]);

// owners mounts a picker of the active people, as the console's owner
// pickers are, answered by listing; getJSON may hold an answer back.
async function owners(listing, { value = "", getJSON, searchAt } = {}) {
  const chosen = { value };
  globalThis.__api = { getJSON: getJSON ?? (async (mode, path) => { assert.equal(mode, "admin"); return listing.answer(path); }) };
  const render = mount(() => RemoteSearchSelect({
    label: "Human owner", noun: "owners", value: chosen.value, emptyLabel: "Select a human owner", searchAt,
    ...directory.principalSearch("admin", { kinds: ["human"], status: "active" }), onChange: (next) => { chosen.value = next; },
  }));
  render();
  await settle();
  return { chosen, render: async () => { render(); await settle(); return render(); } };
}

test("an owner picker lists the first page of active people the server finds", async () => {
  const listing = fakePrincipalListing(people);
  const { chosen, render } = await owners(listing);
  const tree = await render();
  assert.deepEqual(listing.requests, ["/principals?kind=human&status=active&limit=20"]);
  assert.deepEqual(options(tree), [["", "Select a human owner"], ["prn-ada", "Ada"], ["prn-bob", "bob"]]);
  assert.equal(find(tree, (node) => node.type === "input"), undefined, "a short list needs no search");
  find(tree, (node) => node.type === "select").props.onChange(input("prn-bob"));
  assert.equal(chosen.value, "prn-bob");
});

test("a chosen principal the page does not list is named by the server, or marked unavailable", async () => {
  const listing = fakePrincipalListing(people);
  let tree = await (await owners(listing, { value: "prn-cy" })).render();
  assert.ok(listing.requests.includes("/principals?id=prn-cy&limit=1"), "the chosen principal is looked up by ID");
  assert.deepEqual(options(tree).slice(0, 2), [["", "Select a human owner"], ["prn-cy", "Cy (disabled)"]], "a disabled choice keeps its value and says so");

  tree = await (await owners(fakePrincipalListing(people), { value: "prn-gone" })).render();
  assert.deepEqual(options(tree)[1], ["prn-gone", "prn-gone (unavailable)"]);
});

test("a search asks the server again, and an answer to an older search is dropped", async () => {
  const listing = fakePrincipalListing(people);
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let calls = 0;
  const getJSON = async (mode, path) => {
    calls += 1;
    const answer = listing.answer(path);
    if (calls === 1) await held;
    return answer;
  };
  // A negative searchAt shows the search before the first answer arrives.
  const { render } = await owners(listing, { getJSON, searchAt: -1 });
  let tree = await render();
  find(tree, (node) => node.type === "input").props.onInput(input("bo"));
  tree = await render();
  assert.equal(listing.requests.at(-1), "/principals?kind=human&status=active&q=bo&limit=20");
  assert.deepEqual(options(tree), [["", "Select a human owner"], ["prn-bob", "bob"]]);
  release();
  tree = await render();
  assert.deepEqual(options(tree), [["", "Select a human owner"], ["prn-bob", "bob"]], "the first search's late answer does not replace the newer one");
});

test("more matches than one page are counted, and the picker offers a search", async () => {
  const many = Array.from({ length: 25 }, (_, index) => ({ id: `prn-${index}`, kind: "human", display_name: `Person ${String(index).padStart(2, "0")}`, status: "active" }));
  const tree = await (await owners(fakePrincipalListing(many))).render();
  assert.equal(options(tree).length, 21, "the empty choice and the first page of 20");
  assert.match(text(tree), /5 more owners match; search to narrow them\./);
  assert.ok(find(tree, (node) => node.type === "input" && node.props?.type === "search"));
});

test("a key picker searches every key, and names a chosen key by searching for it", async () => {
  const listing = fakeKeyListing([{ id: "k-1", name: "ci", status: "active" }, { id: "k-2", name: "old", status: "revoked" }]);
  globalThis.__api = { getJSON: async (mode, path) => listing.answer(path) };
  const keys = directory.keySearch("admin", {});
  assert.deepEqual(await keys.search(""), { options: [{ value: "k-1", label: "ci" }, { value: "k-2", label: "old" }], total: 2 });
  assert.equal(await keys.resolve("k-2"), "old");
  assert.equal(await keys.resolve("k-gone"), null);
  assert.deepEqual(listing.requests, ["/keys?status=all&limit=20", "/keys?status=all&q=k-2&limit=1", "/keys?status=all&q=k-gone&limit=1"]);
});

test("the administrator state's counts stand in for its key list, and a user's own keys count in the portal", () => {
  assert.equal(directory.keyCount({ counts: { keys: 7 } }), 7);
  assert.equal(directory.keyCount({ keys: [{ id: "k-1" }, { id: "k-2" }] }), 2);
});
