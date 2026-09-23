import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";

const { outputFiles } = await build({
  stdin: {
    contents: `
      export { modeFor, modesFor, playgroundFailure } from "./src/pages/Playground.tsx";
      export { APIError, requestJSON } from "./src/lib/api.ts";
    `,
    resolveDir: new URL("..", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1"),
    sourcefile: "playground-test-entry.ts",
    loader: "ts",
  },
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
  jsx: "automatic",
  jsxImportSource: "preact",
});
const subject = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);

test("playground surfaces only affirmatively supported controls", () => {
  assert.equal(subject.modeFor(undefined), "unknown");
  assert.equal(subject.modeFor({ capabilities: [] }), "unknown");
  assert.equal(subject.modeFor({ capabilities: ["image"] }), "image");
  assert.equal(subject.modeFor({ capabilities: ["tts"] }), "tts");
  assert.equal(subject.modeFor({ capabilities: ["chat", "image"] }), "image");
  assert.deepEqual(subject.modesFor({ capabilities: ["chat", "image"] }), ["image", "chat"]);
  assert.equal(subject.modeFor({ capabilities: ["embedding"] }), "embedding");
});

test("playground failure keeps safe status, code, retry, and action", () => {
  const failure = subject.playgroundFailure(new subject.APIError(
    429,
    "Upstream provider request failed.",
    {},
    "provider_invocation_failed",
    "17",
    true,
    "Wait for the provider window.",
  ));
  assert.deepEqual(failure, {
    message: "Upstream provider request failed.",
    status: 429,
    code: "provider_invocation_failed",
    retry: "After 17",
    action: "Wait for the provider window.",
  });
});

test("API client preserves the server error envelope and Retry-After", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { href: "https://gateway.example/admin", origin: "https://gateway.example", assign() {} } };
  globalThis.fetch = async () => new Response(JSON.stringify({
    error: {
      message: "Safe provider detail.",
      code: "provider_busy",
      retryable: true,
      action: "Retry against this provider later.",
    },
  }), { status: 503, headers: { "Content-Type": "application/json", "Retry-After": "9" } });
  try {
    await assert.rejects(
      subject.requestJSON("portal", "/playground"),
      (error) => error instanceof subject.APIError &&
        error.status === 503 &&
        error.message === "Safe provider detail." &&
        error.code === "provider_busy" &&
        error.retryAfter === "9" &&
        error.retryable === true &&
        error.action === "Retry against this provider later.",
    );
  } finally {
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});
