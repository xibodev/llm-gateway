import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { bundle, text } from "./hook-harness.mjs";

const { clientSnippets } = await bundle(fileURLToPath(new URL("../src/pages/ModelsEndpoints.tsx", import.meta.url)));
const { Overview } = await bundle(fileURLToPath(new URL("../src/pages/Overview.tsx", import.meta.url)));

test("setup snippets use a project key and never name the administrator key", () => {
  const snippets = clientSnippets("https://gateway.example");
  assert.deepEqual(snippets.map((snippet) => snippet.title), ["OpenAI SDKs", "Claude Code", "Codex", "Copilot CLI BYOK"]);
  for (const { title, value } of snippets) {
    assert.doesNotMatch(value, /LLMGW_API_KEYS?\b/, title);
    assert.match(value, /'<GATEWAY_PROJECT_KEY>'/, title);
    assert.match(value, /https:\/\/gateway\.example/, title);
  }
});

test("the Codex snippet matches the documented profile and stays valid TOML", () => {
  const codex = clientSnippets("https://gateway.example").find((snippet) => snippet.title === "Codex").value;
  const clients = readFileSync(new URL("../../../../../docs/CLIENTS.md", import.meta.url), "utf8");
  assert.match(codex, /env_key = "LLMGW_PROJECT_KEY"/);
  assert.match(clients, /env_key = "LLMGW_PROJECT_KEY"/);
  for (const line of codex.split("\n")) assert.match(line, /^(?:#.*|\[[\w.]+\]|\w+ = .+)$/, line);
});

test("the overview reads the boolean SSO flag the admin state reports", () => {
  const auth = (sso) => text(Overview({ data: { sso }, mode: "admin", onNavigate() {} }));
  assert.match(auth({ enabled: true }), /AuthSSO enabled/);
  assert.match(auth({ enabled: false }), /AuthGateway policy/);
  assert.match(auth(undefined), /AuthGateway policy/);
});
