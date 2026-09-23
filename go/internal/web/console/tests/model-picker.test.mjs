import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";

const { outputFiles } = await build({
  entryPoints: [new URL("../src/components/ModelPicker.tsx", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1")],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
  jsx: "automatic",
  jsxImportSource: "preact",
});
const picker = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);

test("model option labels always lead with the exact catalog id", () => {
  assert.equal(picker.modelOptionLabel("codex/gpt-5-codex", "GPT-5 Codex"), "codex/gpt-5-codex — GPT-5 Codex");
  assert.equal(picker.modelOptionLabel("codex/gpt-5-codex", "codex/gpt-5-codex"), "codex/gpt-5-codex");
  assert.equal(picker.modelOptionLabel("codex/gpt-5-codex", "  GPT-5   Codex  "), "codex/gpt-5-codex — GPT-5 Codex");
});

test("model option labels omit marketing copy rather than replacing the exact id", () => {
  const description = "Our most advanced agentic coding model for long-running software engineering work across your entire organization";
  assert.equal(picker.modelOptionLabel("codex/gpt-5-codex", description), "codex/gpt-5-codex");
});

test("catalog projection keeps provider/model id separate from its display name", () => {
  const [model] = picker.catalogModels({ data: [{ id: "codex/gpt-5-codex", owned_by: "codex", display_name: "GPT-5 Codex" }] });
  assert.equal(model.id, "codex/gpt-5-codex");
  assert.equal(model.provider, "codex");
  assert.equal(model.label, "GPT-5 Codex");
});

test("missing capability metadata remains unknown instead of becoming chat", () => {
  assert.deepEqual(picker.capabilitiesFor({ id: "provider/opaque" }), []);
  assert.deepEqual(picker.capabilitiesFor({ supported_surfaces: ["/v1/images/generations"] }), ["image"]);
  assert.deepEqual(picker.capabilitiesFor({ capabilities: { tts: true } }), ["tts"]);
  assert.deepEqual(picker.capabilitiesFor({ typed_capabilities: { operations: { chat: "supported" } } }), ["chat"]);
  assert.deepEqual(picker.capabilitiesFor({ typed_capabilities: { operations: { image: "supported" }, inputs: { image: "unknown" } } }), ["image"]);
  assert.deepEqual(picker.capabilitiesFor({ supported_surfaces: ["/v1/embeddings"] }), ["embedding"]);
  assert.deepEqual(picker.capabilitiesFor({ typed_capabilities: { operations: { embeddings: "supported" } } }), ["embedding"]);
});

test("catalog projection preserves publication and freshness evidence", () => {
  const [model] = picker.catalogModels({ data: [{
    id: "anonymous/candidate",
    owned_by: "anonymous",
    capabilities: { image: true },
    publication_state: "unverified",
    published: true,
    disabled: false,
    failure_code: "verification_failed",
    typed_capabilities: { freshness: { discovered_at: "2026-09-22T00:00:00Z", verified_at: "" } },
  }] });
  assert.equal(model.publicationState, "unverified");
  assert.equal(model.published, true);
  assert.equal(model.disabled, false);
  assert.equal(model.failureCode, "verification_failed");
  assert.equal(model.discoveredAt, "2026-09-22T00:00:00Z");
  assert.equal(model.verifiedAt, "");
});
