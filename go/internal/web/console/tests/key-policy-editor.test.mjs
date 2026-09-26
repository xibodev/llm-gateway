import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";
import { build } from "esbuild";

const root = resolve(import.meta.dirname, "..");

const { outputFiles } = await build({
  stdin: {
    contents: `
      export { keyQuotaDraftsFor, keyQuotaFields, keyQuotaPolicyFromDrafts } from "./src/lib/key-policy.ts";
    `,
    resolveDir: new URL("..", import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, "$1"),
    sourcefile: "key-policy-test-entry.ts",
    loader: "ts",
  },
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const subject = await import(`data:text/javascript;base64,${Buffer.from(outputFiles[0].text).toString("base64")}`);

test("existing key token quotas can be explicitly cleared to zero", () => {
  const drafts = subject.keyQuotaDraftsFor({
    daily_input_tokens: 10_000_000,
    daily_output_tokens: 2_000_000,
  });
  drafts.daily_input_tokens = "0";
  drafts.daily_output_tokens = "0";
  const result = subject.keyQuotaPolicyFromDrafts(drafts);
  assert.equal(result.error, "");
  assert.equal(result.policy.daily_input_tokens, 0);
  assert.equal(result.policy.daily_output_tokens, 0);
  assert.deepEqual(Object.keys(result.policy), subject.keyQuotaFields);
});

test("blank quota fields mean no additional key limit", () => {
  const result = subject.keyQuotaPolicyFromDrafts({});
  assert.equal(result.error, "");
  assert.deepEqual(Object.values(result.policy), subject.keyQuotaFields.map(() => 0));
});

test("negative and unsafe quota values are rejected", () => {
  const negative = subject.keyQuotaPolicyFromDrafts({ rpm: "-1" });
  assert.equal(negative.policy, null);
  assert.match(negative.error, /nonnegative whole number/);

  const unsafe = subject.keyQuotaPolicyFromDrafts({ rpm: "9007199254740992" });
  assert.equal(unsafe.policy, null);
  assert.match(unsafe.error, /nonnegative whole number/);
});

test("the existing-key editor renders and submits every quota field", () => {
  const keys = readFileSync(resolve(root, "src/pages/ApiKeys.tsx"), "utf8");
  assert.match(keys, /keyQuotaFields\.map\(\(field\) => <label/);
  assert.match(keys, /<input name=\{field\}/);
  assert.match(keys, /form\?\.get\(field\)/);
  assert.match(keys, /return \{ \.\.\.quotas\.policy, allowed_routes:/);
  assert.doesNotMatch(keys, /server preserves all other quota limits/);
});
