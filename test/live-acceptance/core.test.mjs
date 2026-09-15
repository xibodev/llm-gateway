import assert from "node:assert/strict";
import test from "node:test";
import {
  buildRoutePlans,
  classifications,
  classifyObservation,
  classifyPairedObservation,
  evaluatePolicy,
  directProviderObservation,
  isFreeModel,
  requiredChecks,
  schema,
  selectHealthyModels,
} from "./core.mjs";

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
