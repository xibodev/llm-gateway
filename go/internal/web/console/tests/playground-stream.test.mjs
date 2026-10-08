import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, find, input, mount, settle, text } from "./hook-harness.mjs";

const api = await bundle(fileURLToPath(new URL("../src/lib/api.ts", import.meta.url)));

test("server-sent events are read whole, however the stream splits them", () => {
  let taken = api.takeServerSentEvents('event: route\ndata: {"served":{"provider":"echo"}}\n\nevent: delta\r\ndata: {"text":"Hel');
  assert.deepEqual(taken.events, [{ event: "route", data: { served: { provider: "echo" } } }]);
  taken = api.takeServerSentEvents(`${taken.rest}lo"}\r\n\r\ndata: not json\n\nevent: done\ndata: {"a":\ndata: 1}\n\n`);
  assert.deepEqual(taken.events, [{ event: "delta", data: { text: "Hello" } }, { event: "done", data: { a: 1 } }], "an event's data may span lines; one that is not JSON is skipped");
  assert.equal(taken.rest, "");
});

test("a stream is read as it arrives, and a refusal before it opens rejects as any request's does", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { href: "https://gateway.example/portal", origin: "https://gateway.example", assign() {} } };
  const encoder = new TextEncoder();
  const sent = [];
  try {
    globalThis.fetch = async (path, init) => {
      sent.push({ path, accept: init.headers.get("Accept"), body: JSON.parse(init.body) });
      return new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode('event: delta\ndata: {"text":"a"}\n\nevent: del'));
          controller.enqueue(encoder.encode('ta\ndata: {"text":"b"}\n\n'));
          controller.close();
        },
      }), { status: 200, headers: { "Content-Type": "text/event-stream" } });
    };
    const events = [];
    await api.streamEvents("portal", "/playground/v1/chat/completions", { stream: true }, (event) => events.push(event));
    assert.deepEqual(events.map((event) => event.data.text), ["a", "b"]);
    assert.deepEqual(sent, [{ path: "/user/api/playground/v1/chat/completions", accept: "text/event-stream", body: { stream: true } }]);

    globalThis.fetch = async () => new Response(JSON.stringify({ error: { message: "project requests/day quota exceeded (limit 1)" } }), { status: 429, headers: { "Content-Type": "application/json", "Retry-After": "60" } });
    await assert.rejects(
      api.streamEvents("portal", "/playground/v1/chat/completions", {}, () => {}),
      (error) => error instanceof api.APIError && error.status === 429 && error.retryAfter === "60" && /quota exceeded/.test(error.message),
    );
  } finally {
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});

const { Playground } = await bundle(fileURLToPath(new URL("../src/pages/Playground.tsx", import.meta.url)), [{
  filter: /\/lib\/api$/,
  contents: [
    "export class APIError extends Error {}",
    "export const getJSON = (...args) => globalThis.__api.getJSON(...args);",
    "export const sendJSON = (...args) => globalThis.__api.sendJSON(...args);",
    "export const requestJSON = (...args) => globalThis.__api.requestJSON(...args);",
    "export const streamEvents = (...args) => globalThis.__api.streamEvents(...args);",
  ].join("\n"),
}]);

const data = {
  principal: { id: "user-1" },
  projects: [{ id: "project-1", name: "Research", status: "active" }],
  memberships: [{ project_id: "project-1", principal_id: "user-1", role: "owner" }],
};
const catalog = { data: [{ id: "echo/echo-default", owned_by: "echo", supported_surfaces: ["/v1/chat/completions"], typed_capabilities: { operations: { chat: "supported" }, streaming: "supported" } }] };

// playground mounts the portal playground with its catalog loaded and a
// message drafted; stream stands in for the gateway's stream.
async function playground(stream) {
  const streams = [];
  globalThis.__api = {
    getJSON: async () => catalog,
    sendJSON: async () => { throw new Error("a streamed request was sent without streaming"); },
    streamEvents: (mode, path, body, onEvent, signal) => { streams.push({ mode, path, body }); return stream(onEvent, signal); },
  };
  const render = mount(() => Playground({ data, mode: "portal", principalID: "", onPrincipalIDChange() {} }));
  for (let turn = 0; turn < 6; turn += 1) { render(); await settle(); }
  let tree = render();
  // preact/compat, which the page uses, renames a textarea's onInput.
  const composer = find(tree, (node) => node.type === "textarea" && /Send a message/.test(node.props?.placeholder ?? ""));
  (composer.props.onInput ?? composer.props.oninput)(input("Hi there"));
  tree = render();
  find(tree, (node) => node.type === "form" && node.props?.class === "chat-composer surface").props.onSubmit({ preventDefault() {} });
  return { render, streams };
}
const thread = (tree) => find(tree, (node) => typeof node.type === "function" && Array.isArray(node.props?.turns)).props;

test("an answer streams into the conversation as it arrives, then settles as the full answer", async () => {
  let finish;
  const { render, streams } = await playground((onEvent) => {
    onEvent({ event: "route", data: { served: { provider: "echo", model: "echo-default" }, fallback_trace: [{ provider: "echo", model: "echo-default", status: "served" }] } });
    onEvent({ event: "delta", data: { text: "Hel" } });
    onEvent({ event: "delta", data: { text: "lo", reasoning: "brief" } });
    return new Promise((resolve) => { finish = () => {
      onEvent({ event: "done", data: { served: { provider: "echo", model: "echo-default" }, latency_ms: 42, usage: { prompt_tokens: 3, completion_tokens: 2 }, raw_response: { choices: [{ message: { role: "assistant", content: "Hello" } }] } } });
      resolve();
    }; });
  });
  await settle();
  let tree = render();
  assert.equal(streams.length, 1);
  assert.deepEqual(streams[0].body.stream, true);
  assert.deepEqual(streams[0].body.stream_options, { include_usage: true }, "a Chat stream asks for its usage");
  assert.deepEqual(streams[0].body.messages, [{ role: "user", content: "Hi there" }]);
  let turns = thread(tree).turns;
  assert.equal(turns.length, 2);
  assert.deepEqual({ ...turns[1] }, { role: "assistant", content: "Hello", reasoning: "brief", streaming: true, served: "echo/echo-default" });
  assert.match(text(tree), /Streaming…/, "the routed result is still streaming");
  assert.match(text(tree), /echo\/echo-default/, "the route members tried are shown as the stream opens");
  assert.ok(find(tree, (node) => node.type === "button" && text(node).includes("Stop")), "a stream can be stopped");

  finish();
  await settle();
  tree = render();
  turns = thread(tree).turns;
  assert.equal(turns[1].content, "Hello");
  assert.equal(turns[1].streaming, undefined);
  assert.equal(turns[1].latency, 42);
  assert.match(text(tree), /42 ms/);
  assert.equal(find(tree, (node) => node.type === "button" && text(node).includes("Stop")), undefined);
});

test("stopping a stream keeps what arrived, and a stream that fails is reported", async () => {
  const { render } = await playground((onEvent, signal) => {
    onEvent({ event: "route", data: { served: { provider: "echo", model: "echo-default" } } });
    onEvent({ event: "delta", data: { text: "Partial" } });
    return new Promise((resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("stopped", "AbortError"))));
  });
  await settle();
  let tree = render();
  find(tree, (node) => node.type === "button" && text(node).includes("Stop")).props.onClick();
  await settle();
  tree = render();
  const turns = thread(tree).turns;
  assert.equal(turns.length, 2);
  assert.equal(turns[1].content, "Partial");
  assert.equal(turns[1].stopped, true);
  assert.equal(turns[1].streaming, false);
  assert.equal(find(tree, (node) => node.props?.title === "Playground request did not complete"), undefined, "a stop is not a failure");

  const failing = await playground((onEvent) => {
    onEvent({ event: "route", data: { served: { provider: "echo", model: "echo-default" } } });
    onEvent({ event: "delta", data: { text: "Par" } });
    onEvent({ event: "error", data: { error: { message: "Upstream provider stream failed." } } });
    return Promise.resolve();
  });
  await settle();
  tree = failing.render();
  assert.equal(thread(tree).turns.length, 0, "a failed answer leaves the conversation as it was");
  assert.equal(find(tree, (node) => node.type === "textarea" && /Send a message/.test(node.props?.placeholder ?? "")).props.value, "Hi there", "the message waits to be sent again");
  assert.ok(find(tree, (node) => node.props?.detail === "Upstream provider stream failed."), "the stream's failure is reported");
});
