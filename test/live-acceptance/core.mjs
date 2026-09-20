export const schema = "llmgw.live-acceptance/v1";

export const classifications = Object.freeze({
  pass: "PASS",
  productRegression: "PRODUCT_REGRESSION",
  dependencyOutage: "DEPENDENCY_OUTAGE",
  providerContractDrift: "PROVIDER_CONTRACT_DRIFT",
  rateLimited: "RATE_LIMITED",
  modelDrift: "MODEL_DRIFT",
  authConfig: "AUTH_CONFIG",
  attributionInconclusive: "ATTRIBUTION_INCONCLUSIVE",
  noEvidence: "NO_EVIDENCE",
});

export const requiredChecks = Object.freeze({
  deterministic: ["docker-health", "model-sweep-completeness", "model-sweep-evidence", "api-evidence", "route-evidence", "playground-evidence", "restart-persistence", "restart-api-evidence", "cleanup"],
  live: ["docker-health", "anonymous-provider-automation", "model-sweep-completeness", "model-sweep-evidence", "api-evidence", "route-evidence", "playground-evidence", "claude-exact-evidence", "claude-four-turn-evidence", "claude-route-evidence", "claude-tool-evidence", "claude-evidence", "restart-persistence", "restart-api-evidence", "cleanup"],
});

export function isFreeModel(provider, model) {
  const id = String(model || "").trim().toLowerCase();
  if (!id) return false;
  if (provider === "opencode-zen") return id === "big-pickle" || id.endsWith("-free");
  if (provider === "kilo-code") {
    return id.endsWith(":free") || id.endsWith("/free") || id.endsWith("-free");
  }
  return false;
}

export function classifyObservation({ status = 0, error = "", validEnvelope = false, timedOut = false }) {
  const message = String(error || "").toLowerCase();
  if (status >= 200 && status < 300 && validEnvelope) return classifications.pass;
  if (status >= 200 && status < 300) return classifications.productRegression;
  if (timedOut || status === 0 || status === 408 || status >= 500) return classifications.dependencyOutage;
  if (status === 429) return classifications.rateLimited;
  if (status === 401 || status === 403) return classifications.authConfig;
  if (status === 404 || ((status === 400 || status === 422) && /model|unavailable|not found|does not exist/.test(message))) {
    return classifications.modelDrift;
  }
  if (status >= 400 && status < 500) return classifications.productRegression;
  return classifications.noEvidence;
}

export function classifyPairedObservation(gateway, direct) {
  const gatewayClass = classifyObservation(gateway);
  if (gatewayClass === classifications.pass) return gatewayClass;
  const directClass = classifyObservation(direct);
  if (directClass === classifications.pass) {
	if (gatewayClass === classifications.dependencyOutage || gatewayClass === classifications.rateLimited) {
	  return gatewayClass;
	}
	return classifications.productRegression;
  }
  if (gatewayClass === directClass) {
    return directClass === classifications.productRegression ? classifications.providerContractDrift : directClass;
  }
  if (directClass === classifications.noEvidence) return classifications.attributionInconclusive;
  return classifications.attributionInconclusive;
}

export function directProviderObservation(result, text, model) {
	const transportStatus = result?.status || 0;
	const embeddedStatus = Number(result?.json?.error?.code ?? result?.json?.error?.status ?? 0);
	const status = transportStatus >= 200 && transportStatus < 300 &&
	  Number.isInteger(embeddedStatus) && embeddedStatus >= 400 && embeddedStatus <= 599
	  ? embeddedStatus : transportStatus;
  const error = result?.error || result?.json?.error?.message || result?.json?.error || result?.text || "";
  return { ...result, status, error, validEnvelope: status >= 200 && status < 300 && Boolean(text) && result?.json?.model === model };
}

export function chatCompletionPassed(result, model) {
  const text = result?.json?.choices?.[0]?.message?.content || "";
  const models = Array.isArray(model) ? model : [model];
  return result?.status >= 200 && result.status < 300 && Boolean(text) && models.includes(result?.json?.model);
}

export function selectHealthyModels(observations, limit = 3) {
  const passed = observations.filter((item) => item.classification === classifications.pass && item.text);
  const selected = [];
  const providers = [...new Set(passed.map((item) => item.provider))];
  for (const provider of providers) {
    const match = passed.find((item) => item.provider === provider && !selected.includes(item));
    if (match) selected.push(match);
    if (selected.length === limit) return selected;
  }
  for (const item of passed) {
    if (!selected.includes(item)) selected.push(item);
    if (selected.length === limit) break;
  }
  return selected;
}

export function buildRoutePlans(healthyModels, faultProviders = [
  { provider: "fault-429", model: "fault-model" },
  { provider: "fault-503", model: "fault-model" },
]) {
  if (!healthyModels.length) return [];
  const healthy = healthyModels.slice(0, 3).map(({ provider, model }) => ({ provider, model }));
  const final = healthy[0];
  return [
    { name: "live-healthy", kind: "healthy", members: healthy, expectedAttempts: 1 },
    { name: "live-one-broken", kind: "one-broken", members: [faultProviders[0], final], expectedAttempts: 2 },
    { name: "live-two-broken", kind: "two-broken", members: [...faultProviders, final], expectedAttempts: 3 },
  ];
}

export function evaluatePolicy(report) {
  const hardFailures = [];
  const warnings = [];
  if (report?.schema !== schema) hardFailures.push("invalid or missing report schema");
  if (!Object.hasOwn(requiredChecks, report?.mode)) hardFailures.push(`invalid acceptance mode: ${report?.mode ?? "missing"}`);
  if (report?.execution?.started !== true || report?.execution?.completed !== true) hardFailures.push("acceptance execution did not complete");
  const checks = Array.isArray(report?.checks) ? report.checks : [];
  if (!Array.isArray(report?.checks)) hardFailures.push("checks must be an array");
  const sweep = Array.isArray(report?.model_sweep) ? report.model_sweep : [];
  if (!Array.isArray(report?.model_sweep)) hardFailures.push("model_sweep must be an array");
  const names = new Set();
  for (const check of checks) {
    if (!check?.name || names.has(check.name)) hardFailures.push(`duplicate or missing check name: ${check?.name ?? "missing"}`);
    names.add(check?.name);
    if (!["passed", "warning", "failed", "inconclusive"].includes(check?.status)) hardFailures.push(`${check?.name ?? "check"}: invalid status ${check?.status}`);
	if (typeof check?.required !== "boolean") hardFailures.push(`${check?.name ?? "check"}: required must be boolean`);
	if (check.required && check.status === "warning") hardFailures.push(`${check.name}: required checks cannot be warnings`);
    if (check.required && check.status === "failed") hardFailures.push(`${check.name}: ${check.detail || "failed"}`);
    if (check.status === "warning") warnings.push(`${check.name}: ${check.detail || "warning"}`);
  }
  const knownClassifications = new Set(Object.values(classifications));
  for (const item of sweep) {
	if (!item || typeof item !== "object" || !knownClassifications.has(item.classification)) {
	  hardFailures.push(`invalid model observation classification: ${item?.classification ?? "missing"}`);
	}
  }
  if (report?.mode === "live" && sweep.length === 0) hardFailures.push("live acceptance produced no model observations");
  const productRegressions = sweep.filter((item) => item?.classification === classifications.productRegression);
  if (productRegressions.length) hardFailures.push(`${productRegressions.length} candidate-only live model regression(s)`);
  const healthy = sweep.filter((item) => item?.classification === classifications.pass);
  const externalFailures = sweep.filter((item) => [
    classifications.dependencyOutage,
    classifications.rateLimited,
    classifications.modelDrift,
    classifications.authConfig,
    classifications.providerContractDrift,
  ].includes(item?.classification));
  const overrideableExternalFailures = sweep.filter((item) => [
	classifications.dependencyOutage,
	classifications.rateLimited,
	classifications.modelDrift,
	classifications.providerContractDrift,
  ].includes(item?.classification));
  const attributionUnknown = sweep.filter((item) => item?.classification === classifications.attributionInconclusive);
  const modelEvidence = checks.find((check) => check.name === "model-sweep-evidence");
  const requiredPassed = (name) => checks.some((check) => check.name === name && check.required === true && check.status === "passed");
	const noHealthyExternalEvidence = report?.mode === "live" && sweep.length > 0 && healthy.length === 0 &&
	  overrideableExternalFailures.length === sweep.length && modelEvidence?.required === true && modelEvidence?.status === "inconclusive" &&
	  ["docker-health", "anonymous-provider-automation", "model-sweep-completeness", "cleanup"].every(requiredPassed);
  const required = noHealthyExternalEvidence
	? ["docker-health", "anonymous-provider-automation", "model-sweep-completeness", "model-sweep-evidence", "cleanup"]
    : (requiredChecks[report?.mode] ?? []);
  for (const name of required) {
    if (!checks.some((check) => check.name === name && check.required === true)) hardFailures.push(`required check missing: ${name}`);
  }
  if (!report?.execution?.commit) hardFailures.push("report is not bound to a source commit");
  const candidateImage = String(report?.execution?.candidate_image || "");
  const candidateDigest = String(report?.execution?.candidate_digest || "");
  const candidateRevision = String(report?.execution?.candidate_revision || "");
  if (candidateImage) {
	const match = candidateImage.match(/@(sha256:[0-9a-f]{64})$/);
	if (!match) hardFailures.push("candidate image is not pinned by a canonical sha256 digest");
	else if (candidateDigest !== match[1]) hardFailures.push("candidate digest does not match candidate image");
	if (!candidateRevision) hardFailures.push("candidate report is missing revision evidence");
	else if (candidateRevision !== report.execution.commit) hardFailures.push("candidate revision does not match report commit");
	} else if (candidateDigest || candidateRevision) {
	hardFailures.push("candidate evidence is present without a candidate image");
  }
  if (hardFailures.length) return { verdict: "fail", release_blocking: true, reasons: hardFailures, warnings };
  if (attributionUnknown.length) {
    return {
      verdict: "inconclusive",
      release_blocking: false,
      override_eligible: false,
      reasons: [`${attributionUnknown.length} candidate/control result(s) could not be attributed`],
      warnings,
    };
  }
  const inconclusiveChecks = checks.filter((check) => check.required === true && check.status === "inconclusive");
  if (inconclusiveChecks.length) {
    if (report?.mode === "deterministic") {
      return { verdict: "fail", release_blocking: true, reasons: inconclusiveChecks.map((check) => `${check.name}: ${check.detail || "no evidence"}`), warnings };
    }
    const overrideEligible = noHealthyExternalEvidence && inconclusiveChecks.length === 1 && inconclusiveChecks[0].name === "model-sweep-evidence";
    return {
      verdict: "inconclusive",
      release_blocking: false,
      override_eligible: overrideEligible,
      reasons: inconclusiveChecks.map((check) => `${check.name}: ${check.detail || "no evidence"}`),
      warnings,
    };
  }
  if (!healthy.length) {
	if (report?.mode === "deterministic") {
	  return { verdict: "fail", release_blocking: true, override_eligible: false, reasons: ["deterministic acceptance produced no passing model evidence"], warnings };
	}
    return {
      verdict: "inconclusive",
      release_blocking: false,
      override_eligible: noHealthyExternalEvidence,
      reasons: ["no anonymous live model produced usable release evidence"],
      warnings: [...warnings, `${externalFailures.length} external observation(s) were unavailable`],
    };
  }
  if (warnings.length || externalFailures.length) {
    return {
      verdict: "pass_with_warnings",
      release_blocking: false,
      override_eligible: false,
      reasons: [`${healthy.length} live model observation(s) passed`],
      warnings: [...warnings, `${externalFailures.length} external observation(s) were unavailable`],
    };
  }
  return { verdict: "pass", release_blocking: false, override_eligible: false, reasons: [`${healthy.length} live model observation(s) passed`], warnings: [] };
}

export function safeExcerpt(value, limit = 240) {
  return String(value || "").replace(/\s+/g, " ").trim().slice(0, limit);
}
