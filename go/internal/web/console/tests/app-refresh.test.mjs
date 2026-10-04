import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, mount, nodes, settle, text } from "./hook-harness.mjs";

// App runs as a plain function over recorded hooks. Pages are never mounted, so
// the tree shows exactly what App decides to put on screen.
const { App } = await bundle(fileURLToPath(new URL("../src/App.tsx", import.meta.url)));

globalThis.window = {
  location: { hash: "#keys", pathname: "/admin", href: "https://gateway.example/admin", origin: "https://gateway.example", assign() {} },
  addEventListener() {}, removeEventListener() {},
};
let responses = [];
globalThis.fetch = async () => responses.shift();
const ok = (payload) => new Response(JSON.stringify(payload), { status: 200, headers: { "Content-Type": "application/json" } });
const unavailable = () => new Response(JSON.stringify({ error: "State store unavailable." }), { status: 503, headers: { "Content-Type": "application/json" } });

const page = (tree) => find(tree, (node) => typeof node.props?.onChanged === "function");
const errorPage = (tree) => find(tree, (node) => node.props?.title === "Workspace state is unavailable");
const notice = (tree) => find(tree, (node) => node.props?.role === "alert");

test("a failed refresh keeps the loaded page on screen and rejects to its caller", async () => {
  const render = mount(App);
  responses = [ok({ keys: [{ id: "key-1" }] }), unavailable(), ok({ keys: [{ id: "key-2" }] })];
  render();
  await settle();
  const loaded = page(render());
  assert.ok(loaded, "the keys page renders once state loads");

  await assert.rejects(loaded.props.onChanged(), /request succeeded, but the console could not reload the latest gateway state: State store unavailable\./);
  const afterFailure = render();
  const kept = page(afterFailure);
  assert.ok(kept, "the page stays mounted after a failed refresh");
  assert.strictEqual(kept.props.data, loaded.props.data, "the last good state is kept");
  assert.equal(errorPage(afterFailure), undefined, "a refresh failure never takes over the page");
  assert.match(text(notice(afterFailure)), /Refresh failed/);
  assert.match(text(notice(afterFailure)), /State store unavailable\./);

  nodes(notice(afterFailure)).find((node) => node.type === "button").props.onClick();
  await settle();
  const recovered = render();
  assert.equal(notice(recovered), undefined, "a successful retry clears the notice");
  assert.deepEqual(page(recovered).props.data, { keys: [{ id: "key-2" }] });
});

test("only a console with nothing loaded yet shows the full error page", async () => {
  const render = mount(App);
  responses = [unavailable()];
  render();
  await settle();
  const tree = render();
  assert.ok(errorPage(tree), "the first load failure gets the full error page");
  assert.equal(page(tree), undefined);
});

test("a successful refresh resolves", async () => {
  const render = mount(App);
  responses = [ok({ keys: [] }), ok({ keys: [{ id: "key-3" }] })];
  render();
  await settle();
  await page(render()).props.onChanged();
  assert.deepEqual(page(render()).props.data, { keys: [{ id: "key-3" }] });
});
