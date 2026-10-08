import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import {
  buildRoutePlans,
  chatCompletionPassed,
  classifications,
  classifyObservation,
  classifyPairedObservation,
  evaluatePolicy,
  directProviderObservation,
  gatewayProviderTimeoutSeconds,
  isFreeModel,
  playgroundPayload,
  requiredChecks,
  routeResultPassed,
  schema,
  selectHealthyModels,
  sendWithReplay,
} from "./core.mjs";

test("a Playground answer is read whether it streamed or not", () => {
  assert.deepEqual(playgroundPayload("application/json", '{"served":{"provider":"a"}}'), { served: { provider: "a" } });
  assert.equal(playgroundPayload("application/json", "not json"), null);
  const stream = 'event: route\ndata: {"served":{"provider":"a"}}\n\nevent: delta\ndata: {"text":"ok"}\n\nevent: done\r\ndata: {"served":{"provider":"a"},"project_id":"p"}\r\n\r\n';
  assert.deepEqual(playgroundPayload("text/event-stream", stream), { served: { provider: "a" }, project_id: "p" }, "a stream's payload is its done event");
  assert.equal(playgroundPayload("text/event-stream", 'event: route\ndata: {}\n\nevent: error\ndata: {"error":{"message":"failed"}}\n\n'), null, "a stream that failed has none");
});

test("the gateway gives up on a provider before the harness gives up on the gateway", () => {
  assert.equal(gatewayProviderTimeoutSeconds(180_000), 120);
  assert.equal(gatewayProviderTimeoutSeconds(5_000), 3);
  assert.equal(gatewayProviderTimeoutSeconds(1_000), 1);
  for (const limit of [5_000, 60_000, 180_000, 600_000]) {
    assert.ok(gatewayProviderTimeoutSeconds(limit) * 1000 < limit, `limit ${limit}`);
  }
});

test("an unusable 2xx answer is replayed once and errors never are", async () => {
  const passed = (result) => result.status === 200 && Boolean(result.text);
  const sender = (...answers) => {
    let calls = 0;
    const send = async () => answers[Math.min(calls++, answers.length - 1)];
    return { send, calls: () => calls };
  };

  let probe = sender({ status: 200, text: "hello" });
  let outcome = await sendWithReplay(probe.send, passed);
  assert.equal(probe.calls(), 1);
  assert.deepEqual(outcome, { result: { status: 200, text: "hello" }, first: null });

  probe = sender({ status: 200, text: "", finish_reason: "length" }, { status: 200, text: "hello" });
  outcome = await sendWithReplay(probe.send, passed);
  assert.equal(probe.calls(), 2);
  assert.equal(outcome.result.text, "hello");
  assert.equal(outcome.first.finish_reason, "length");

  probe = sender({ status: 200, text: "" }, { status: 200, text: "" });
  outcome = await sendWithReplay(probe.send, passed);
  assert.equal(probe.calls(), 2, "a replay is sent only once");
  assert.equal(passed(outcome.result), false);

  for (const status of [400, 429, 500, 0]) {
    probe = sender({ status, text: "" });
    outcome = await sendWithReplay(probe.send, passed);
    assert.equal(probe.calls(), 1, `status ${status} is not replayed`);
    assert.equal(outcome.first, null);
  }
});

test("chat evidence requires text from the expected served model", () => {
  const result = (content, model = "model") => ({
    status: 200,
    json: { model, choices: [{ message: { content } }] },
  });
  assert.equal(chatCompletionPassed(result("ok"), "model"), true);
  assert.equal(chatCompletionPassed(result(""), "model"), false);
  assert.equal(chatCompletionPassed(result(null), "model"), false);
  assert.equal(chatCompletionPassed(result("ok", "other"), "model"), false);
  assert.equal(chatCompletionPassed({ ...result("ok"), status: 502 }, "model"), false);
});

test("free model selection is provider-specific", () => {
  assert.equal(isFreeModel("opencode-zen", "deepseek-v4-flash-free"), true);
  assert.equal(isFreeModel("opencode-zen", "big-pickle"), true);
  assert.equal(isFreeModel("opencode-zen", "paid-model"), false);
  assert.equal(isFreeModel("kilo-code", "vendor/model:free"), true);
  assert.equal(isFreeModel("kilo-code", "kilo-auto/free"), true);
  assert.equal(isFreeModel("openai", "model:free"), false);
});

test("direct provider soft errors are never valid success envelopes", () => {
	const observation = directProviderObservation({ status: 200, json: { error: { message: "overloaded", code: 502 } }, text: '{"error":{"message":"overloaded","code":502}}' }, "", "model");
  assert.equal(observation.validEnvelope, false);
  assert.equal(observation.error, "overloaded");
	assert.equal(observation.status, 502);
	assert.equal(classifyObservation(observation), classifications.dependencyOutage);
});

test("a router model's direct answer under another model name is a served answer", () => {
  const routed = directProviderObservation(
    { status: 200, json: { model: "vendor/chosen-model", choices: [{ message: { content: "hi" } }] } },
    "hi", "router/auto",
  );
  assert.equal(routed.validEnvelope, true);
  assert.equal(routed.servedModel, "vendor/chosen-model");
  // A gateway timeout beside a provider that answers directly is an outage of
  // the gateway's attempt, not an unattributable result.
  assert.equal(
    classifyPairedObservation({ status: 0, timedOut: true, error: "timeout" }, routed),
    classifications.dependencyOutage,
  );
  const empty = directProviderObservation({ status: 200, json: { model: "vendor/chosen-model" } }, "", "router/auto");
  assert.equal(empty.validEnvelope, false);
});

test("live results distinguish external availability from product regressions", () => {
  assert.equal(classifyObservation({ status: 200, validEnvelope: true }), classifications.pass);
  assert.equal(classifyObservation({ status: 200, validEnvelope: false }), classifications.productRegression);
  assert.equal(classifyObservation({ status: 429 }), classifications.rateLimited);
  assert.equal(classifyObservation({ status: 503 }), classifications.dependencyOutage);
  assert.equal(classifyObservation({ timedOut: true }), classifications.dependencyOutage);
  assert.equal(classifyObservation({ status: 400, error: "Model is unavailable" }), classifications.modelDrift);
  assert.equal(classifyObservation({ status: 401 }), classifications.authConfig);
  assert.equal(classifyObservation({ status: 400, error: "invalid messages array" }), classifications.productRegression);
  assert.equal(classifyPairedObservation(
    { status: 502, error: "gateway failed" },
    { status: 200, validEnvelope: true },
	), classifications.dependencyOutage);
	assert.equal(classifyPairedObservation(
	  { status: 200, validEnvelope: false },
	  { status: 200, validEnvelope: true },
	), classifications.productRegression);
  assert.equal(classifyPairedObservation(
    { status: 400, error: "Model is unavailable" },
    { status: 400, error: "Model is unavailable" },
  ), classifications.modelDrift);
  assert.equal(classifyPairedObservation(
    { status: 200, validEnvelope: false },
    { status: 200, validEnvelope: false },
  ), classifications.providerContractDrift);
  assert.equal(classifyPairedObservation(
    { status: 200, validEnvelope: false },
    { status: 429, validEnvelope: false },
  ), classifications.attributionInconclusive);
  assert.equal(classifyPairedObservation(
    { status: 503, error: "circuit breaker open" },
    { status: 429, error: "Rate limit exceeded" },
  ), classifications.rateLimited);
});

test("healthy cohort prefers distinct providers and builds required routes", () => {
  const observations = [
    { provider: "zen", model: "a", classification: classifications.pass, text: "ok" },
    { provider: "zen", model: "b", classification: classifications.pass, text: "ok" },
    { provider: "kilo", model: "c", classification: classifications.pass, text: "ok" },
  ];
  const healthy = selectHealthyModels(observations);
  assert.deepEqual(healthy.map((item) => item.provider), ["zen", "kilo", "zen"]);
  const routes = buildRoutePlans(healthy);
  assert.deepEqual(routes.map((route) => [route.name, route.members.length, route.expectedAttempts]), [
    ["live-healthy", 3, 1],
    ["live-one-broken", 2, 2],
    ["live-two-broken", 3, 3],
  ]);
  assert.equal(routeResultPassed({
    served: { provider: "zen", model: "b" },
    fallback_trace: [{}, {}],
  }, routes[0].members), true);
  assert.equal(routeResultPassed({
    served: { provider: "fault-429", model: "fault-model" },
    fallback_trace: [{}],
  }, routes[1].members, routes[1].expectedAttempts), false);
  assert.equal(routeResultPassed({
    served: { provider: "zen", model: "a" },
    fallback_trace: [{}],
  }, routes[1].members, routes[1].expectedAttempts), false);
});

test("browser acceptance selects the message composer rather than tool editors", async () => {
  const source = await import("node:fs/promises").then(({ readFile }) =>
    readFile(new URL("./run.mjs", import.meta.url), "utf8"),
  );
  assert.match(source, /textarea\[placeholder\^=\"Send a message\"\]/);
  assert.doesNotMatch(source, /locator\("\.chat-composer textarea"\)/);
  assert.match(source, /\/admin\/api\/playground\/v1\/chat\/completions/);
  assert.match(source, /commandWithInput\(claudeCommand/);
  assert.match(source, /child\.stdin\.end\(input\)/);
  assert.match(source, /CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT: "1"/);
  assert.match(source, /modelOverrides: \{ "claude-sonnet-4-6": model \}/);
  assert.match(source, /"--settings", JSON\.stringify\(settings\)/);
  assert.match(source, /alwaysThinkingEnabled: false/);
  assert.match(source, /autoCompactEnabled: false/);
});

test("human-like UAT uses a gateway-visible mock and syncs catalogs before routes", async () => {
  const source = await import("node:fs/promises").then(({ readFile }) =>
    readFile(new URL("../uat-browser-journey.mjs", import.meta.url), "utf8"),
  );
  assert.match(source, /LLMGW_UAT_MOCK_BASE_URL/);
  assert.match(source, /providers\/opencode-zen\/refresh/);
  assert.match(source, /textarea\[placeholder\^=\"Send a message\"\]/);
  assert.match(source, /Expected 404 non-disclosure for out-of-scope route/);
});

test("deterministic fixture declares its proven Chat surface", async () => {
	const source = await readFile(new URL("./fixture-server.mjs", import.meta.url), "utf8");
	assert.match(source, /supported_endpoints: \["\/v1\/chat\/completions"\]/);
});

test("live sweep excludes unpublished diagnostic catalog rows", async () => {
  const source = await readFile(new URL("./run.mjs", import.meta.url), "utf8");
  assert.match(source, /row\?\.disabled !== true && row\?\.published !== false/);
  assert.match(source, /const catalogResponse = page\.waitForResponse/);
  assert.match(source, /const catalog = await catalogResponse/);
  assert.match(source, /await option\.waitFor\(\{ state: "visible"/);
});

test("release policy requires execution but tolerates external outages", () => {
  const required = ["docker-health", "model-sweep-completeness", "model-sweep-evidence", "api-evidence", "route-evidence", "playground-evidence", "restart-persistence", "restart-api-evidence", "cleanup"];
  const base = { schema, mode: "deterministic", execution: { started: true, completed: true, commit: "abc" }, checks: required.map((name) => ({ name, required: true, status: "passed" })), model_sweep: [] };
	assert.equal(evaluatePolicy(base).verdict, "fail");
  const unavailable = {
    ...base,
    model_sweep: [{ classification: classifications.rateLimited }],
  };
	assert.equal(evaluatePolicy(unavailable).release_blocking, true);
  const passed = {
    ...base,
    model_sweep: [{ classification: classifications.pass }],
  };
  assert.equal(evaluatePolicy(passed).verdict, "pass");
  const regression = {
    ...base,
    model_sweep: [{ classification: classifications.productRegression }],
  };
  assert.equal(evaluatePolicy(regression).release_blocking, true);
  const failed = {
    ...base,
    checks: [{ name: "route", required: true, status: "failed", detail: "wrong served model" }],
  };
  assert.equal(evaluatePolicy(failed).release_blocking, true);
  const inconclusive = {
    ...passed,
    checks: [{ name: "route-evidence", required: true, status: "inconclusive", detail: "provider rate limited" }],
  };
  assert.equal(evaluatePolicy(inconclusive).verdict, "fail");
});

test("only external all-provider outages are override eligible", () => {
  const report = {
    schema,
    mode: "live",
    execution: { started: true, completed: true, commit: "abc" },
    model_sweep: [{ classification: classifications.rateLimited }],
    checks: [
      { name: "docker-health", required: true, status: "passed" },
	  { name: "anonymous-provider-automation", required: true, status: "passed" },
	  { name: "model-sweep-completeness", required: true, status: "passed" },
	  { name: "model-sweep-evidence", required: true, status: "inconclusive" },
      { name: "cleanup", required: true, status: "passed" },
    ],
  };
  const policy = evaluatePolicy(report);
  assert.equal(policy.verdict, "inconclusive");
  assert.equal(policy.override_eligible, true);
});

test("an empty live sweep is never override eligible", () => {
	const checks = requiredChecks.live.map((name) => ({
	  name, required: true, status: name === "model-sweep-evidence" ? "inconclusive" : "passed",
	}));
  const report = {
	  schema,
	  mode: "live",
	  execution: { started: true, completed: true, commit: "abc" },
	  model_sweep: [],
	  checks,
  };
  const policy = evaluatePolicy(report);
	assert.equal(policy.verdict, "fail");
	assert.equal(policy.release_blocking, true);
	assert.match(policy.reasons.join(" "), /no model observations/);
	assert.notEqual(policy.override_eligible, true);
});

test("unrelated inconclusive checks cannot authorize an outage override", () => {
	const checks = requiredChecks.live.map((name) => ({
	  name, required: true, status: name === "api-evidence" ? "inconclusive" : "passed",
	}));
	const policy = evaluatePolicy({
	  schema, mode: "live", execution: { started: true, completed: true, commit: "abc" },
	  model_sweep: [{ classification: classifications.pass }], checks,
	});
	assert.equal(policy.override_eligible, false);
});

test("candidate digest must match the digest-qualified image", () => {
	const checks = requiredChecks.deterministic.map((name) => ({ name, required: true, status: "passed" }));
	const policy = evaluatePolicy({
	  schema, mode: "deterministic",
	  execution: {
		started: true, completed: true, commit: "abc",
		candidate_image: `ghcr.io/example/repo@sha256:${"a".repeat(64)}`,
		candidate_digest: `sha256:${"b".repeat(64)}`, candidate_revision: "abc",
	  },
	  model_sweep: [{ classification: classifications.pass }], checks,
	});
	assert.equal(policy.release_blocking, true);
	assert.match(policy.reasons.join(" "), /digest does not match/);
});

test("malformed reports fail closed", () => {
  const result = evaluatePolicy({ mode: "unknown", execution: { started: true, completed: true }, checks: [{ name: "x", required: true, status: "skipped" }] });
  assert.equal(result.release_blocking, true);
  assert.match(result.reasons.join(" "), /schema|mode|status/);
});
