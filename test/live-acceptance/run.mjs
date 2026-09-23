#!/usr/bin/env node
import { execFile, spawn } from "node:child_process";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { promisify } from "node:util";
import { randomBytes, randomUUID } from "node:crypto";
import {
  buildRoutePlans,
  chatCompletionPassed,
  classifications,
  classifyObservation,
  classifyPairedObservation,
  directProviderObservation,
  evaluatePolicy,
  isFreeModel,
  routeResultPassed,
  safeExcerpt,
  schema,
  selectHealthyModels,
} from "./core.mjs";

const exec = promisify(execFile);
const repo = resolve(import.meta.dirname, "..", "..");
const mode = process.env.LLMGW_ACCEPTANCE_MODE || "live";
const output = resolve(process.env.LLMGW_ACCEPTANCE_REPORT || `${repo}/test/live-acceptance/report.json`);
const port = Number(process.env.LLMGW_ACCEPTANCE_PORT || 18810);
const sweepConcurrency = Math.max(1, Number(process.env.LLMGW_ACCEPTANCE_CONCURRENCY || 1));
const modelTimeout = Math.max(5_000, Number(process.env.LLMGW_ACCEPTANCE_MODEL_TIMEOUT_MS || 180_000));
const completionMaxTokens = 512;
const baseURL = `http://127.0.0.1:${port}`;
const runID = `${Date.now()}-${process.pid}`;
const project = `llmgw-live-${runID}`;
const image = `llmgw-live-acceptance:${runID}`;
const container = `${project}-gateway`;
const volume = `${project}-state`;
const network = `${project}-network`;
const accessNetwork = `${project}-access`;
const edge = `${project}-edge`;
const adminKey = `uat_${randomBytes(24).toString("base64url")}`;
const encryptionKey = randomBytes(32).toString("base64");
const report = {
  schema,
  mode,
  execution: { started: true, completed: false, cleanup_completed: false, started_at: new Date().toISOString(), commit: "", image },
  providers: [],
  model_sweep: [],
  routes: [],
  api: [],
  playground: [],
  claude: [],
  restart: {},
  checks: [],
  policy: null,
};

function progress(message) {
  console.log(`[${new Date().toISOString()}] ${message}`);
}

function check(name, status, detail, required = true, evidence = {}) {
  const redact = (value) => String(value || "").replaceAll(adminKey, "[REDACTED]").replaceAll(encryptionKey, "[REDACTED]");
  report.checks.push({ name, status, detail: safeExcerpt(redact(detail), 500), required, ...evidence });
}

async function writeReport() {
  report.execution.completed_at = new Date().toISOString();
  report.policy = evaluatePolicy(report);
  await mkdir(resolve(output, ".."), { recursive: true });
  await writeFile(output, JSON.stringify(report, null, 2) + "\n");
}

async function command(file, args, options = {}) {
  const result = await exec(file, args, { cwd: repo, maxBuffer: 16 << 20, ...options });
  return { stdout: result.stdout.trim(), stderr: result.stderr.trim() };
}

async function commandWithInput(file, args, input, options = {}) {
  return new Promise((resolveCommand, rejectCommand) => {
    const child = spawn(file, args, { cwd: repo, ...options, stdio: ["pipe", "pipe", "pipe"] });
    const stdout = [];
    const stderr = [];
    const timer = options.timeout ? setTimeout(() => child.kill(), options.timeout) : null;
    child.stdout.on("data", (chunk) => stdout.push(chunk));
    child.stderr.on("data", (chunk) => stderr.push(chunk));
    child.on("error", rejectCommand);
    child.on("close", (code, signal) => {
      if (timer) clearTimeout(timer);
      const result = {
        stdout: Buffer.concat(stdout).toString().trim(),
        stderr: Buffer.concat(stderr).toString().trim(),
      };
      if (code === 0) resolveCommand(result);
      else rejectCommand(Object.assign(new Error(`command exited with ${signal || code}`), result, { code, signal }));
    });
    child.stdin.end(input);
  });
}

async function docker(...args) {
  let options = {};
  if (args.length && typeof args.at(-1) === "object") options = args.pop();
  return command("docker", args, options);
}

async function request(path, { method = "GET", key = adminKey, body, timeout = 30_000 } = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeout);
  const started = Date.now();
  try {
    const response = await fetch(baseURL + path, {
      method,
      signal: controller.signal,
      headers: {
        Accept: "application/json",
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
        ...(key ? { Authorization: `Bearer ${key}` } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await response.text();
    let json = null;
    try { json = JSON.parse(text); } catch {}
    return { status: response.status, text, json, duration_ms: Date.now() - started };
  } catch (error) {
    return { status: 0, text: "", json: null, error: error.name === "AbortError" ? "timeout" : error.message, timedOut: error.name === "AbortError", duration_ms: Date.now() - started };
  } finally {
    clearTimeout(timer);
  }
}

function chatText(json) {
  const msg = json?.choices?.[0]?.message;
  return msg?.content || msg?.reasoning || msg?.reasoning_content || "";
}

function messagesText(json) {
  return Array.isArray(json?.content) ? json.content.filter((part) => part?.type === "text").map((part) => part.text).join("") : "";
}

function responsesText(json) {
  return Array.isArray(json?.output)
    ? json.output.flatMap((item) => Array.isArray(item?.content) ? item.content : [])
      .filter((part) => part?.type === "output_text").map((part) => part.text).join("")
    : "";
}

const anonymousProfiles = [
  { provider: "opencode-zen", registry_id: "opencode_zen", catalog: "https://opencode.ai/zen/v1/models", chat: "https://opencode.ai/zen/v1/chat/completions" },
  { provider: "kilo-code", registry_id: "kilo_code", catalog: "https://api.kilo.ai/api/gateway/models", chat: "https://api.kilo.ai/api/gateway/chat/completions" },
  { provider: "llm7", registry_id: "llm7", catalog: "https://api.llm7.io/v1/models", chat: "https://api.llm7.io/v1/chat/completions" },
  { provider: "ovh-ai", registry_id: "ovh_ai_endpoints", catalog: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/models", chat: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/chat/completions" },
  { provider: "pollinations", registry_id: "pollinations", catalog: "https://text.pollinations.ai/models", chat: "https://text.pollinations.ai/v1/chat/completions" },
];

function anonymousProfile(provider) {
  return anonymousProfiles.find((profile) => profile.provider === provider);
}

function errorText(result) {
  if (result.status >= 200 && result.status < 300) return "";
  return result.error || result.json?.error?.message || result.json?.error || result.text;
}

function diagnosticExcerpt(value, limit = 500) {
  const text = String(value || "").trim();
  if (text.length <= limit) return text;
  const half = Math.floor((limit - 5) / 2);
  return `${text.slice(0, half)} ... ${text.slice(-half)}`;
}

function responseEvidence(result, expectedModel) {
  const choices = Array.isArray(result.json?.choices) ? result.json.choices : [];
  const first = choices[0] && typeof choices[0] === "object" ? choices[0] : null;
  const message = first?.message && typeof first.message === "object" ? first.message : null;
  const content = message?.content;
  const text = chatText(result.json) || messagesText(result.json) || responsesText(result.json);
  const error = result.error || result.json?.error?.message || result.json?.error || (result.status >= 300 ? result.text : "");
  return {
    status: result.status,
    expected_model: expectedModel,
    actual_model: safeExcerpt(result.json?.model, 100),
    model_match: result.json?.model === expectedModel,
    choices: choices.length,
    finish_reason: safeExcerpt(first?.finish_reason, 40),
    content_type: content === null ? "null" : Array.isArray(content) ? "array" : typeof content,
    text_length: text.length,
    error: safeExcerpt(typeof error === "string" ? error : JSON.stringify(error), 120),
  };
}

async function waitHealth() {
  for (let attempt = 0; attempt < 60; attempt++) {
    const health = await request("/health", { key: "", timeout: 2_000 });
    if (health.status === 200) return health;
    await new Promise((done) => setTimeout(done, 500));
  }
  throw new Error("gateway did not become healthy");
}

async function configureProvider(id, registry_id, base_url) {
  const result = await request("/admin/api/providers", {
    method: "POST",
    body: { id, registry_id, base_url, force_api_support: true },
  });
  if (result.status !== 200) throw new Error(`configure ${id}: ${result.status} ${errorText(result)}`);
  const refresh = await request(`/admin/api/providers/${encodeURIComponent(id)}/refresh`, { method: "POST", body: {} , timeout: 60_000 });
  const catalog = await request(`/admin/api/providers/${encodeURIComponent(id)}/catalog`);
  const models = catalog.json?.models || [];
  const diagnostics = catalog.json?.catalog || {};
  report.providers.push({
    id, registry_id, configure_status: result.status, refresh_status: refresh.status,
    refresh_success: refresh.json?.success ?? null, catalog_status: catalog.status,
    catalog_state: diagnostics.status || "", failure_code: diagnostics.failure_code || "",
    model_count: models.length,
  });
  if (models.length) {
    check(`provider-catalog:${id}`, "passed", `${models.length} model(s) discovered`, false);
  } else if (mode === "deterministic") {
    check(`provider-catalog:${id}`, "failed", `catalog unavailable: ${diagnostics.failure_code || "empty"}`, true);
  } else {
    const direct = await directCatalogProbe(id);
    const candidateRegression = direct.status === 200 && direct.model_count > 0;
    check(`provider-catalog:${id}`, candidateRegression ? "failed" : "warning", `${models.length} gateway model(s); direct status=${direct.status}, models=${direct.model_count}; ${diagnostics.failure_code || "empty"}`, candidateRegression);
  }
  return models;
}

async function directCatalogProbe(provider) {
  const endpoint = anonymousProfile(provider)?.catalog;
  if (!endpoint) return { status: 0, model_count: 0 };
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), modelTimeout);
  try {
    const response = await fetch(endpoint, { signal: controller.signal });
    const payload = await response.json().catch(() => null);
    const models = Array.isArray(payload) ? payload : payload?.data;
    return { status: response.status, model_count: Array.isArray(models) ? models.length : 0 };
  } catch {
    return { status: 0, model_count: 0 };
  } finally {
    clearTimeout(timer);
  }
}

async function sweepProvider(provider, models) {
  const candidates = models
    .filter((row) => row?.disabled !== true && row?.published !== false)
    .filter((row) => row?.free === true || isFreeModel(provider, row?.id))
    .map((row) => row.id);
  progress(`sweep ${provider}: ${candidates.length} published model(s)`);
  let cursor = 0;
  const worker = async () => {
    while (cursor < candidates.length) {
      const model = candidates[cursor++];
    const result = await request("/v1/chat/completions", {
      method: "POST",
       body: { model: `${provider}/${model}`, messages: [{ role: "user", content: "hi" }], max_tokens: completionMaxTokens },
      timeout: modelTimeout,
    });
    const text = chatText(result.json);
    const gatewayObservation = { ...result, error: errorText(result), validEnvelope: Boolean(text) && result.json?.model === model };
    let direct = null;
    if (mode === "live" && classifyObservation(gatewayObservation) !== classifications.pass) {
      direct = await directProviderProbe(provider, model);
    }
    const classification = direct ? classifyPairedObservation(gatewayObservation, direct) : classifyObservation(gatewayObservation);
    report.model_sweep.push({
      provider, model, status: result.status, duration_ms: result.duration_ms, classification,
      text: safeExcerpt(text), error: safeExcerpt(errorText(result)),
      direct: direct ? { status: direct.status, classification: classifyObservation(direct), error: safeExcerpt(direct.error), duration_ms: direct.duration_ms } : undefined,
    });
    }
  }
  await Promise.all(Array.from({ length: Math.min(sweepConcurrency, candidates.length || 1) }, worker));
  report.model_sweep.sort((left, right) => `${left.provider}/${left.model}`.localeCompare(`${right.provider}/${right.model}`));
  const passed = report.model_sweep.filter((row) => row.provider === provider && row.classification === classifications.pass).length;
  progress(`sweep ${provider}: complete, ${passed}/${candidates.length} passed`);
}

async function directProviderProbe(provider, model) {
  const endpoint = anonymousProfile(provider)?.chat;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), modelTimeout);
  const started = Date.now();
  try {
    if (!endpoint) return { status: 0, validEnvelope: false, error: "no direct provider probe", duration_ms: 0 };
    const zenResponses = provider === "opencode-zen" && model.startsWith("muse-spark-");
    const sessionID = `ses_${randomBytes(16).toString("hex")}`;
    const response = await fetch(zenResponses ? "https://opencode.ai/zen/v1/responses" : endpoint, {
      method: "POST",
      signal: controller.signal,
      headers: {
        "Content-Type": "application/json",
        ...(provider === "opencode-zen" ? {
          Authorization: "Bearer public",
          "x-opencode-project": `prj_${randomBytes(16).toString("hex")}`,
          "x-opencode-session": sessionID,
          "x-opencode-request": `msg_${randomBytes(16).toString("hex")}`,
          "x-opencode-client": "llmgw-acceptance",
          "User-Agent": "llm-gateway-acceptance",
        } : {}),
      },
      body: JSON.stringify(zenResponses
        ? { model, input: "hi", max_output_tokens: 512, stream: false }
        : { model, messages: [{ role: "user", content: "hi" }], max_tokens: completionMaxTokens, stream: false }),
    });
    const text = await response.text();
    let json = null;
    try { json = JSON.parse(text); } catch {}
    const answer = zenResponses ? responsesText(json) : chatText(json);
    return directProviderObservation({ status: response.status, json, text, duration_ms: Date.now() - started }, answer, model);
  } catch (error) {
    return { status: 0, validEnvelope: false, timedOut: error.name === "AbortError", error: error.name === "AbortError" ? "timeout" : error.message, duration_ms: Date.now() - started };
  } finally {
    clearTimeout(timer);
  }
}

async function waitAnonymousProviderAutomation() {
  for (let attempt = 0; attempt < 120; attempt++) {
    const state = await request("/admin/api/state", { timeout: 15_000 });
    const configured = state.json?.providers || [];
    const statuses = state.json?.provider_statuses || [];
    const complete = anonymousProfiles.every((profile) => {
      const instance = configured.find((row) => row.id === profile.provider && row.registry_id === profile.registry_id);
      const status = statuses.find((row) => row.id === profile.registry_id);
	  return instance && status?.instances?.some((row) => row.id === profile.provider &&
		(row.last_check_operation === "verify" || (row.last_check_operation === "catalog_sync" && row.last_check_success === false)));
    });
    if (complete) {
      const entries = [];
	  const failures = [];
	  let verified = 0;
      for (const profile of anonymousProfiles) {
        const catalog = await request(`/admin/api/providers/${encodeURIComponent(profile.provider)}/catalog`);
        const models = catalog.json?.models || [];
        const diagnostics = catalog.json?.catalog || {};
        const status = statuses.find((row) => row.id === profile.registry_id)?.instances?.find((row) => row.id === profile.provider);
		const verificationFinished = status?.last_check_operation === "verify";
		const verificationPassed = verificationFinished && status?.last_check_success === true;
		if (verificationPassed) verified++;
		let directCatalog = null;
		if (!models.length) {
		  directCatalog = await directCatalogProbe(profile.provider);
		  if (directCatalog.status === 200 && directCatalog.model_count > 0) failures.push(`${profile.provider}: gateway catalog empty while direct catalog has ${directCatalog.model_count} rows`);
		}
		report.providers.push({
          id: profile.provider, registry_id: profile.registry_id, automated: true,
          catalog_status: catalog.status, catalog_state: diagnostics.status || "",
          failure_code: diagnostics.failure_code || "", model_count: models.length,
		  verify_status: verificationPassed ? "passed" : verificationFinished ? "failed" : "not_run",
          verified_model: status?.verified_model || "",
		  direct_catalog: directCatalog,
        });
        entries.push({ provider: profile.provider, models });
      }
      const usable = entries.filter((entry) => entry.models.some((row) => row?.free === true)).length;
	  const passed = failures.length === 0 && configured.filter((row) => anonymousProfile(row.id)).length === anonymousProfiles.length;
	  check("anonymous-provider-automation", passed ? "passed" : "failed", `${anonymousProfiles.length}/${anonymousProfiles.length} provider(s) auto-connected and checked; ${usable} filtered free catalog(s); ${verified} verification(s) passed${failures.length ? `; ${failures.join("; ")}` : ""}`, true);
      return entries;
    }
    await new Promise((done) => setTimeout(done, 5_000));
  }
  throw new Error("anonymous provider automation did not complete within 10 minutes");
}

async function createIdentityAndKey() {
  const principal = await request("/admin/api/principals", { method: "POST", body: { kind: "human", display_name: "Live acceptance owner" } });
  const projectResult = await request("/admin/api/projects", { method: "POST", body: { slug: `live-${runID}`, name: "Live acceptance" } });
  const principalID = principal.json?.id;
  const projectID = projectResult.json?.id;
  if (!principalID || !projectID) throw new Error("failed to create acceptance identity");
  const membership = await request("/admin/api/memberships", { method: "POST", body: { principal_id: principalID, project_id: projectID, role: "owner" } });
  if (membership.status !== 200) throw new Error(`membership: ${membership.status}`);
  const key = await request("/admin/api/keys", { method: "POST", body: { principal_id: principalID, project_id: projectID, name: "live acceptance" } });
  const token = key.json?.token || key.json?.key;
  if (!token) throw new Error(`mint key: ${key.status}`);
  return { principalID, projectID, token };
}

async function createRoutes(healthy) {
  const plans = buildRoutePlans(healthy);
  for (const plan of plans) {
    const result = await request("/admin/api/endpoints", { method: "POST", body: { name: plan.name, failover: plan.members } });
    report.routes.push({ ...plan, create_status: result.status });
    if (result.status !== 200) throw new Error(`route ${plan.name}: ${result.status} ${errorText(result)}`);
  }
  return plans;
}

async function testAPIs(key, plans, healthy) {
  let routePasses = 0;
  const targets = [{ name: `${healthy[0].provider}/${healthy[0].model}`, kind: "exact", expectedAttempts: 1 }, ...plans.map((item) => ({ name: item.name, kind: item.kind, expectedAttempts: item.expectedAttempts }))];
  for (const target of targets) {
    const plan = plans.find((item) => item.name === target.name);
    const validModels = plan ? plan.members.filter((m) => !m.provider.startsWith("fault-")).map((item) => item.model) : [healthy[0].model];
    const targetFinal = plan ? (plan.members.find((m) => !m.provider.startsWith("fault-")) || healthy[0]) : healthy[0];
    const chat = await request("/v1/chat/completions", { method: "POST", key, body: { model: target.name, messages: [{ role: "user", content: "hi" }], max_tokens: completionMaxTokens }, timeout: 90_000 });
    const chatBody = chatText(chat.json);
    report.api.push({ surface: "chat", target: target.name, status: chat.status, duration_ms: chat.duration_ms, text: safeExcerpt(chatBody), error: safeExcerpt(errorText(chat)) });
    const chatModel = chat.json?.model || "";
    await recordLiveCheck(`chat:${target.name}`, chatCompletionPassed(chat, validModels), chat, targetFinal);
    if (target.expectedAttempts > 1 && chat.status === 200 && Boolean(chatBody)) {
      const telemetry = await request("/admin/api/telemetry");
      const event = (telemetry.json?.recent || []).find((item) => item.requested === target.name);
      const attempts = event?.attempts?.length || 0;
      const served = event?.served || "";
      const expectedServed = `${healthy[0].provider}/${healthy[0].model}`;
      const order = (event?.attempts || []).map((attempt) => `${attempt.provider}/${attempt.model}`);
      const expectedOrder = (plan?.members || []).map((member) => `${member.provider}/${member.model}`);
      const valid = attempts === target.expectedAttempts && served === expectedServed && JSON.stringify(order) === JSON.stringify(expectedOrder) && (target.expectedAttempts < 2 || event?.attempts?.[0]?.throttled === true);
      check(`trace:${target.name}`, valid ? "passed" : "failed", `attempts=${attempts}, served=${served}, order=${order.join(" -> ")}`, true);
    }
    const messages = await request("/v1/messages", { method: "POST", key, body: { model: target.name, messages: [{ role: "user", content: "hi" }], max_tokens: completionMaxTokens }, timeout: 90_000 });
    const messageBody = messagesText(messages.json);
    report.api.push({ surface: "messages", target: target.name, status: messages.status, duration_ms: messages.duration_ms, text: safeExcerpt(messageBody), error: safeExcerpt(errorText(messages)) });
    const messagesPassed = messages.status === 200 && Boolean(messageBody) && validModels.includes(messages.json?.model);
    await recordLiveCheck(`messages:${target.name}`, messagesPassed, messages, targetFinal);
    if (target.kind !== "exact" && chat.status === 200 && Boolean(chatBody) && messagesPassed) routePasses++;
  }
  check("api-evidence", report.api.some((item) => item.status === 200) ? "passed" : "inconclusive", `${report.api.filter((item) => item.status === 200).length}/${report.api.length} API call(s) passed`, true);
  check("route-evidence", routePasses > 0 ? "passed" : "inconclusive", `${routePasses}/${plans.length} route(s) passed Chat and Messages`, true);
}

async function recordLiveCheck(name, passed, result, finalModel) {
  if (passed) {
    check(name, "passed", chatText(result.json) || messagesText(result.json) || "ok", true);
    return;
  }
  if (mode !== "live") {
    check(name, "failed", errorText(result), true);
    return;
  }
  const direct = await directProviderProbe(finalModel.provider, finalModel.model);
  const classification = classifyPairedObservation(
    { ...result, error: errorText(result), validEnvelope: false },
    direct,
  );
  const product = classification === classifications.productRegression;
  const unknown = classification === classifications.attributionInconclusive;
  const gateway = result.firstEvidence
    ? { first: result.firstEvidence, replay: responseEvidence(result, finalModel.model) }
    : responseEvidence(result, finalModel.model);
  const detail = `${classification}: gateway=${JSON.stringify(gateway)} direct=${JSON.stringify(responseEvidence(direct, finalModel.model))}`;
  check(name, product ? "failed" : unknown ? "inconclusive" : "warning", detail, product || unknown);
}

async function testPlayground(identity, healthy, plans) {
  let chromium;
  try { ({ chromium } = await import("playwright")); }
  catch (error) { check("playwright", "failed", error.message, true); return; }
  const browser = await chromium.launch({ headless: true });
  let passes = 0;
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const targets = [
      ...healthy.map((item) => ({ target: `${item.provider}/${item.model}`, members: [item], minimumAttempts: 1 })),
      ...plans.map((item) => ({ target: item.name, members: item.members, minimumAttempts: item.expectedAttempts })),
    ];
    for (const [index, targetCase] of targets.entries()) {
      const { target, members, minimumAttempts } = targetCase;
      const page = await context.newPage();
      const catalogResponse = page.waitForResponse((candidate) => {
        if (candidate.request().method() !== "GET") return false;
        const url = new URL(candidate.url());
        return url.pathname.endsWith("/admin/api/models") &&
          url.searchParams.get("principal_id") === identity.principalID &&
          url.searchParams.get("project_id") === identity.projectID;
      }, { timeout: 30_000 });
      await page.goto(`${baseURL}/console#/playground`, { waitUntil: "domcontentloaded" });
      await page.waitForFunction(() => document.querySelector(".playground-page") || document.querySelector('input[type="password"]'), null, { timeout: 15_000 });
      const password = page.locator('input[type="password"]');
      if (await password.isVisible().catch(() => false)) {
        await password.fill(adminKey);
        await page.getByRole("button", { name: /sign in/i }).click();
      }
      await page.waitForSelector(".playground-page", { timeout: 15_000 });
      await page.waitForFunction(
        ({ principalID, projectID }) => {
          const selects = [...document.querySelectorAll(".playground-settings__scope select")];
          return selects.some((select) => select.value === principalID) && selects.some((select) => select.value === projectID);
        },
        { principalID: identity.principalID, projectID: identity.projectID },
        { timeout: 15_000 },
      );
      const catalog = await catalogResponse;
      if (!catalog.ok()) throw new Error(`Playground catalog returned HTTP ${catalog.status()}`);
      const catalogPayload = await catalog.json().catch(() => null);
      const combo = page.locator(".model-combo input");
      await combo.focus();
      await combo.fill(target);
      const option = page.locator('.model-combo [role="option"]', { hasText: target }).first();
      try {
        await option.waitFor({ state: "visible", timeout: 10_000 });
      } catch (error) {
        const row = (catalogPayload?.data || []).find((item) => item?.id === target);
        const catalogIDs = (catalogPayload?.data || []).map((item) => item?.id).filter(Boolean);
        const rendered = await page.locator('.model-combo [role="option"] strong').allTextContents();
        throw new Error(`Playground did not offer ${target}; catalog_row=${JSON.stringify(row || null)} catalog_ids=${JSON.stringify(catalogIDs.slice(0, 40))} input=${JSON.stringify(await combo.inputValue().catch(() => ""))} rendered=${JSON.stringify(rendered.slice(0, 20))}; ${error.message}`);
      }
      await option.dispatchEvent("mousedown");
      await page.waitForFunction((value) => document.querySelector(".model-combo input")?.value === value, target, { timeout: 10_000 });
      const composer = page.locator('.chat-composer textarea[placeholder^="Send a message"]');
      await composer.fill("hi");
      const [response] = await Promise.all([
        page.waitForResponse((candidate) => candidate.request().method() === "POST" && candidate.url().endsWith("/admin/api/playground/v1/chat/completions"), { timeout: 70_000 }),
        page.getByRole("button", { name: /^Send$/ }).click(),
      ]);
      const payload = await response.json().catch(() => null);
      await page.waitForFunction(() => document.querySelectorAll(".chat-turn--assistant").length === 1 || document.querySelector(".state-panel--error"), null, { timeout: 10_000 });
      const routed = await page.locator(".playground-outcome").innerText();
      const assistantText = await page.locator(".chat-turn--assistant:not(.chat-turn--pending) p").textContent().catch(() => "");
      const passed = response.status() === 200 && Boolean(assistantText?.trim()) && routed.toLowerCase().includes("routed result") && payload?.project_id === identity.projectID && payload?.principal_id === identity.principalID && routeResultPassed(payload, members, minimumAttempts);
      if (passed) passes++;
      report.playground.push({ target, status: passed ? "passed" : "failed", http_status: response.status(), excerpt: safeExcerpt(routed) });
      if (passed || mode !== "live") {
        check(`playground:${target}`, passed ? "passed" : "failed", routed, true);
      } else {
        const final = healthy.find((item) => item.provider === payload?.served?.provider && item.model === payload?.served?.model) ||
          members.find((item) => !item.provider.startsWith("fault-")) || healthy[0];
        const direct = await directProviderProbe(final.provider, final.model);
        const classification = classifyPairedObservation({ status: response.status(), error: routed, validEnvelope: false }, direct);
        const product = classification === classifications.productRegression;
        const unknown = classification === classifications.attributionInconclusive;
        check(`playground:${target}`, product ? "failed" : unknown ? "inconclusive" : "warning", `${classification}: ${routed}`, product || unknown);
      }
      if (index === 0) await page.locator(".chat-thread__clear").click().catch(() => {});
      await page.close();
    }
  } finally {
    await browser.close();
  }
  check("playground-evidence", passes > 0 ? "passed" : "inconclusive", `${passes}/${healthy.length + plans.length} Playground target(s) passed`, true);
}

async function testClaude(identity, healthy, plans) {
  if (process.env.LLMGW_ACCEPTANCE_CLAUDE === "0") {
    check("claude", "warning", "Claude CLI disabled", false);
    return;
  }
  const claudeCommand = process.env.LLMGW_CLAUDE_BIN || "claude";
  const target = `${healthy[0].provider}/${healthy[0].model}`;
  const claudeConfig = resolve(repo, ".acceptance-claude", runID);
  const claudeWorkspace = resolve(tmpdir(), `llmgw-claude-${runID}`);
  await mkdir(claudeConfig, { recursive: true });
  await mkdir(claudeWorkspace, { recursive: true });
  await writeFile(resolve(claudeWorkspace, "fixture.txt"), "llmgw-live-acceptance\n");
  const env = {
    ...process.env,
    CLAUDE_CONFIG_DIR: claudeConfig,
    ANTHROPIC_BASE_URL: baseURL,
    ANTHROPIC_API_KEY: identity.token,
    ANTHROPIC_AUTH_TOKEN: "",
    DISABLE_PROMPT_CACHING: "1",
    CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT: "1",
  };
  const invoke = async (name, model, prompt, extra = [], persist = false) => {
    try {
      const persistence = persist ? [] : ["--no-session-persistence"];
      const settings = {
        modelOverrides: { "claude-sonnet-4-6": model },
        alwaysThinkingEnabled: false,
        showThinkingSummaries: false,
        autoCompactEnabled: false,
        env: { DISABLE_PROMPT_CACHING: "1", CLAUDE_CODE_DISABLE_THINKING: "1" },
      };
      const { stdout, stderr } = await commandWithInput(claudeCommand, ["-p", "--bare", "--settings", JSON.stringify(settings), "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--disable-slash-commands", "--no-chrome", "--model", "claude-sonnet-4-6", ...persistence, "--permission-prompts", "none", ...extra], prompt, { cwd: claudeWorkspace, env, timeout: 120_000 });
      const text = stdout.trim();
      report.claude.push({ name, model, status: text ? "passed" : "failed", text: safeExcerpt(text), warning: safeExcerpt(stderr) });
      return { passed: Boolean(text), text, stderr };
    } catch (error) {
      const detail = [error.stderr, error.stdout, error.message].filter(Boolean).join("\n");
      report.claude.push({ name, model, status: "failed", error: diagnosticExcerpt(detail) });
      return { passed: false, text: "", stderr: detail };
    }
  };
  const required = [];
  required.push(["exact-hi", await invoke("exact-hi", target, "Say hi in one short sentence.", ["--tools="])]);
  const sessionID = randomUUID();
  const memoryA = `alpha-${randomBytes(4).toString("hex")}`;
  const memoryB = `beta-${randomBytes(4).toString("hex")}`;
  const turns = [
    `Remember this token for our conversation: ${memoryA}. Acknowledge briefly.`,
    "Return the token I asked you to remember.",
    `Also remember this second token: ${memoryB}. Acknowledge briefly.`,
    "Return both remembered tokens, separated by one space.",
  ];
  for (let index = 0; index < turns.length; index++) {
    const resume = index === 0 ? ["--session-id", sessionID] : ["--resume", sessionID];
    const result = await invoke(`four-turn-${index + 1}`, target, turns[index], ["--tools=", ...resume], true);
    if (index === 1) result.passed = result.passed && result.text.includes(memoryA);
    if (index === 3) result.passed = result.passed && result.text.includes(memoryA) && result.text.includes(memoryB);
    required.push([`four-turn-${index + 1}`, result]);
  }
  for (const plan of plans) required.push([plan.name, await invoke(plan.name, plan.name, "Reply with exactly: ok", ["--tools="])]);
  const toolResult = await invoke("read-tool", plans.at(-1).name, "Use Read on fixture.txt and return its first line exactly.", ["--tools=Read", "--allowedTools=Read", "--permission-mode", "dontAsk"]);
  toolResult.passed = toolResult.passed && toolResult.text.includes("llmgw-live-acceptance");
  required.push(["read-tool", toolResult]);
  const passed = required.filter(([, result]) => result.passed).length;
  for (const [name, result] of required) {
    if (result.passed) check(`claude:${name}`, "passed", result.text, false);
    else check(`claude:${name}`, "warning", result.stderr || "empty output", false);
  }
  const byName = Object.fromEntries(required);
  const conversationPassed = turns.every((_, index) => byName[`four-turn-${index + 1}`]?.passed === true);
  const exactPassed = byName["exact-hi"]?.passed === true;
  const routesPassed = plans.every((plan) => byName[plan.name]?.passed === true);
  const toolPassed = byName["read-tool"]?.passed === true;
  for (const [name, lanePassed] of [
    ["claude-exact-evidence", exactPassed],
    ["claude-four-turn-evidence", conversationPassed],
    ["claude-route-evidence", routesPassed],
    ["claude-tool-evidence", toolPassed],
  ]) {
    check(name, lanePassed ? "passed" : "inconclusive", lanePassed ? "capability passed" : "capability did not produce live evidence", true);
  }
  const capabilityPasses = [exactPassed, conversationPassed, routesPassed, toolPassed].filter(Boolean).length;
  check("claude-evidence", capabilityPasses === 4 ? "passed" : "inconclusive", `${capabilityPasses}/4 Claude capabilities and ${passed}/${required.length} individual lane(s) passed`, true);
  await rm(claudeConfig, { recursive: true, force: true });
  await rm(claudeWorkspace, { recursive: true, force: true });
}

async function setupDocker() {
  progress(`docker: preparing ${mode} gateway`);
  report.execution.commit = (await command("git", ["rev-parse", "HEAD"])).stdout;
  const candidateImage = process.env.LLMGW_ACCEPTANCE_IMAGE;
  if (candidateImage) {
    progress("docker: pulling candidate image");
    await docker("pull", candidateImage);
    const inspection = JSON.parse((await docker("image", "inspect", candidateImage)).stdout)[0];
    const expectedCommit = process.env.LLMGW_ACCEPTANCE_EXPECT_COMMIT || "";
    const expectedDigest = candidateImage.includes("@") ? candidateImage.split("@").at(-1) : "";
    const repoDigests = inspection?.RepoDigests || [];
    const revision = inspection?.Config?.Labels?.["org.opencontainers.image.revision"] || "";
    if (expectedCommit && revision !== expectedCommit) throw new Error(`candidate image revision ${revision} does not match ${expectedCommit}`);
    if (expectedDigest && !repoDigests.some((value) => value.endsWith(`@${expectedDigest}`))) throw new Error(`candidate image digest does not match ${expectedDigest}`);
    await docker("tag", candidateImage, image);
    report.execution.candidate_image = candidateImage;
    report.execution.candidate_digest = expectedDigest;
    report.execution.candidate_revision = revision;
  } else {
    progress("docker: building local image");
    await docker("build", "-f", "go/Dockerfile", "--build-arg", `VERSION=acceptance-${report.execution.commit.slice(0, 8)}`, "--build-arg", `COMMIT=${report.execution.commit}`, "-t", image, "go");
  }
  await docker("network", "create", ...(mode === "deterministic" ? ["--internal"] : []), network);
  if (mode === "deterministic") await docker("network", "create", accessNetwork);
  await docker("volume", "create", volume);
  const publishedPort = mode === "deterministic" ? [] : ["-p", `127.0.0.1:${port}:8787`];
  await docker("run", "-d", "--name", container, "--network", network, "--network-alias", "gateway", ...publishedPort, "-v", `${volume}:/state`, "-e", "LLMGW_HOST=0.0.0.0", "-e", "LLMGW_PORT=8787", "-e", "LLMGW_STATE_DIR=/state", "-e", "LLMGW_API_KEY", "-e", "LLMGW_CREDENTIAL_ENCRYPTION_KEY", "-e", "LLMGW_ALLOW_UNAUTHENTICATED_API=0", ...(mode === "live" ? ["-e", "LLMGW_ANONYMOUS_PROVIDER_AUTOMATION=true"] : []), image, "serve", { env: { ...process.env, LLMGW_API_KEY: adminKey, LLMGW_CREDENTIAL_ENCRYPTION_KEY: encryptionKey } });
  progress("docker: gateway container started, waiting for health");
  if (mode === "deterministic") {
    await docker("create", "--name", edge, "--network", accessNetwork, "-p", `127.0.0.1:${port}:8080`, "-v", `${resolve(import.meta.dirname, "edge-nginx.conf")}:/etc/nginx/nginx.conf:ro`, "nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10");
    await docker("network", "connect", network, edge);
    await docker("start", edge);
  }
  const health = await waitHealth();
  check("docker-health", health.status === 200 ? "passed" : "failed", health.text, true);
  progress("docker: gateway healthy");
}

async function restartAndCheck(identity, healthy, plans) {
  const before = await request("/admin/api/state");
  await docker("restart", container);
  await waitHealth();
  const after = await request("/admin/api/state");
  const size = (value) => Array.isArray(value) ? value.length : value && typeof value === "object" ? Object.keys(value).length : 0;
  const count = (state, key) => key === "endpoints"
    ? size(state?.endpoints || state?.categories)
    : size(state?.[key]);
  const keys = ["providers", "endpoints", "principals", "projects"];
  const counts = Object.fromEntries(keys.map((key) => [key, [count(before.json, key), count(after.json, key)]]));
  const fingerprint = (state) => JSON.stringify({
    providers: (state?.providers || []).map((item) => [item.id, item.type, item.registry_id]).sort(),
    endpoints: Object.entries(state?.endpoints || state?.categories || {}).sort(),
    principals: (state?.principals || []).map((item) => [item.id, item.kind, item.status]).sort(),
    projects: (state?.projects || []).map((item) => [item.id, item.slug, item.status]).sort(),
  });
  const passed = Object.values(counts).every(([left, right]) => left === right) && fingerprint(before.json) === fingerprint(after.json);
  report.restart = { status: passed ? "passed" : "failed", counts, content_equal: fingerprint(before.json) === fingerprint(after.json) };
  check("restart-persistence", passed ? "passed" : "failed", JSON.stringify(counts), true);
  if (!passed) return;
  let restartPasses = 0;
  for (const target of [`${healthy[0].provider}/${healthy[0].model}`, plans.at(-1).name]) {
    const requestOptions = { method: "POST", key: identity.token, body: { model: target, messages: [{ role: "user", content: "hi" }], max_tokens: completionMaxTokens }, timeout: 90_000 };
    let result = await request("/v1/chat/completions", requestOptions);
    let ok = chatCompletionPassed(result, healthy[0].model);
    if (!ok && result.status >= 200 && result.status < 300) {
      const first = responseEvidence(result, healthy[0].model);
      result = await request("/v1/chat/completions", requestOptions);
      result.firstEvidence = first;
      ok = chatCompletionPassed(result, healthy[0].model);
      if (ok) check(`restart-replay:${target}`, "warning", `equivalent replay passed after unusable 2xx response: ${JSON.stringify(first)}`, false);
    }
    if (ok) restartPasses++;
    if (ok || mode !== "live") check(`restart-chat:${target}`, ok ? "passed" : "failed", errorText(result) || chatText(result.json), true);
    else await recordLiveCheck(`restart-chat:${target}`, false, result, healthy[0]);
  }
  const telemetry = await request("/admin/api/telemetry");
  const event = (telemetry.json?.recent || []).find((item) => item.requested === plans.at(-1).name);
  const routePersisted = event?.attempts?.length === plans.at(-1).expectedAttempts && event?.served === `${healthy[0].provider}/${healthy[0].model}`;
  check("restart-api-evidence", restartPasses === 2 && routePersisted ? "passed" : "inconclusive", `${restartPasses}/2 post-restart API call(s) passed; route_trace=${routePersisted}`, true);
}

async function cleanup(servers = []) {
  progress("cleanup: removing temporary resources");
  const errors = [];
  for (const name of servers) await docker("rm", "-f", name).catch((error) => errors.push(error.message));
  if (mode === "deterministic") await docker("rm", "-f", edge).catch((error) => errors.push(error.message));
  await docker("rm", "-f", container).catch((error) => errors.push(error.message));
  await docker("volume", "rm", "-f", volume).catch((error) => errors.push(error.message));
  await docker("network", "rm", network).catch((error) => errors.push(error.message));
  if (mode === "deterministic") await docker("network", "rm", accessNetwork).catch((error) => errors.push(error.message));
  await docker("image", "rm", "-f", image).catch((error) => errors.push(error.message));
  report.execution.cleanup_completed = errors.length === 0;
  check("cleanup", errors.length === 0 ? "passed" : "failed", errors.join("; ") || "temporary resources removed", true);
  progress(`cleanup: ${errors.length ? `completed with ${errors.length} error(s)` : "complete"}`);
}

async function runAcceptance() {
  if (mode === "policy") {
    const input = JSON.parse(await readFile(process.env.LLMGW_ACCEPTANCE_INPUT, "utf8"));
    const policy = evaluatePolicy(input);
    console.log(JSON.stringify(policy, null, 2));
    process.exitCode = policy.release_blocking ? 1 : 0;
    return;
  }
  const servers = [];
  try {
    progress(`acceptance: start mode=${mode}`);
    await writeReport();
    await setupDocker();
    progress("identity: creating scoped owner and project");
    const identity = await createIdentityAndKey();
    let modelsByProvider = [];
    if (mode === "deterministic") {
      const createFixture = async (id, status, models) => {
        const name = `${project}-${id}`;
        await docker("run", "-d", "--name", name, "--network", network, "-e", `STATUS=${status}`, "-e", `MODELS=${models.join(",")}`, "-v", `${resolve(import.meta.dirname)}:/harness:ro`, "node:22.23.2-alpine", "node", "/harness/fixture-server.mjs");
        servers.push(name);
        for (let attempt = 0; attempt < 20; attempt++) {
          const probe = await docker("exec", name, "wget", "-qO-", "http://127.0.0.1:8080/models").catch(() => null);
          if (probe) break;
          await new Promise((done) => setTimeout(done, 100));
        }
        await configureProvider(id, "custom_openai", `http://${name}:8080`);
        return models.map((model) => ({ id: model }));
      };
      const fault429 = await createFixture("fault-429", 429, ["fault-model"]);
      const fault503 = await createFixture("fault-503", 503, ["fault-model"]);
      const healthy = await createFixture("kilo-code", 200, ["fixture-a-free", "fixture-b-free", "fixture-c-free"]);
      modelsByProvider = [{ provider: "kilo-code", models: healthy }];
      report.providers.push({ id: "fault-429", fixture_models: fault429.length }, { id: "fault-503", fixture_models: fault503.length });
    } else {
	  progress("providers: waiting for anonymous automation");
	  modelsByProvider = await waitAnonymousProviderAutomation();
	  progress(`providers: automation complete for ${modelsByProvider.length} provider(s)`);
      for (const [id, status] of [["fault-429", 429], ["fault-503", 503]]) {
        const name = `${project}-${id}`;
        await docker("run", "-d", "--name", name, "--network", network, "-e", `STATUS=${status}`, "-e", "MODELS=fault-model", "-v", `${resolve(import.meta.dirname)}:/harness:ro`, "node:22.23.2-alpine", "node", "/harness/fixture-server.mjs");
        servers.push(name);
        for (let attempt = 0; attempt < 20; attempt++) {
          const probe = await docker("exec", name, "wget", "-qO-", "http://127.0.0.1:8080/models").catch(() => null);
          if (probe) break;
          await new Promise((done) => setTimeout(done, 100));
        }
        await configureProvider(id, "custom_openai", `http://${name}:8080`);
      }
    }
    progress("models: starting published-model sweep");
    for (const entry of modelsByProvider) await sweepProvider(entry.provider, entry.models);
    const candidates = modelsByProvider.reduce((total, entry) => total + entry.models.filter((row) =>
      row?.disabled !== true && row?.published !== false && (row?.free === true || isFreeModel(entry.provider, row?.id)),
    ).length, 0);
    check("model-sweep-completeness", candidates > 0 && report.model_sweep.length === candidates ? "passed" : candidates === 0 ? "inconclusive" : "failed", `${report.model_sweep.length}/${candidates} free model(s) observed`, true);
    const healthy = selectHealthyModels(report.model_sweep);
    if (!healthy.length) {
      const deterministicFailure = mode === "deterministic";
      check("model-sweep-evidence", deterministicFailure ? "failed" : "inconclusive", `0/${candidates} free model(s) passed`, true);
    } else {
      check("model-sweep-evidence", "passed", `${healthy.length}/${candidates} free model(s) supplied usable evidence`, true);
      check("healthy-cohort", "passed", `${healthy.length} model(s) selected`, true);
      progress(`routes: creating plans from ${healthy.length} healthy model(s)`);
      const plans = await createRoutes(healthy);
      progress("api: testing chat, messages, and failover routes");
      await testAPIs(identity.token, plans, healthy);
      progress("playground: testing browser targets");
      await testPlayground(identity, healthy, plans);
      if (mode === "live") {
        progress("claude: testing CLI capabilities");
        await testClaude(identity, healthy, plans);
      }
      progress("restart: testing persisted state and routes");
      await restartAndCheck(identity, healthy, plans);
    }
    report.execution.completed = true;
  } catch (error) {
    progress(`acceptance: failed: ${error.message}`);
    check("harness", "failed", error.stack || error.message, true);
  } finally {
    await cleanup(servers);
    await writeReport();
  }
  console.log(JSON.stringify({ report: output, policy: report.policy, model_counts: report.model_sweep.reduce((counts, item) => ({ ...counts, [item.classification]: (counts[item.classification] || 0) + 1 }), {}) }, null, 2));
  if (report.policy.release_blocking) process.exitCode = 1;
}

await runAcceptance();
