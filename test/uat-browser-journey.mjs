#!/usr/bin/env node
import http from 'node:http';
import { mkdir, writeFile, readFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { execSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

// Resilient Playwright resolution
async function loadPlaywright() {
  try { return await import('playwright'); } catch {}
  try { return await import('playwright-core'); } catch {}
  try {
    const globalRoot = execSync('npm root -g', { encoding: 'utf8' }).trim();
    const candidate = join(globalRoot, '@playwright', 'cli', 'node_modules', 'playwright-core', 'index.js');
    const mod = await import(pathToFileURL(candidate).href);
    return mod.default || mod;
  } catch (err) {
    throw new Error('Playwright could not be loaded: ' + err.message);
  }
}

const BASE_URL = process.env.LLMGW_BASE_URL || 'http://127.0.0.1:8787';
const ADMIN_KEY = process.env.LLMGW_API_KEY || 'test-admin-key-llmgw-secret-2026';
const MOCK_PORT = 18080;
const OUTPUT_DIR = resolve('test/uat-output');
const SCREENSHOTS_DIR = join(OUTPUT_DIR, 'screenshots');

const report = {
  started_at: new Date().toISOString(),
  base_url: BASE_URL,
  steps: [],
  console_logs: [],
  network_failures: [],
  server_logs: '',
  summary: { total: 0, passed: 0, failed: 0, skipped: 0 },
};

async function logStep(name, fn) {
  report.summary.total++;
  const start = Date.now();
  const stepRecord = { name, status: 'in_progress', duration_ms: 0, error: null, screenshot: null };
  console.log(`[UAT] ─── Step: ${name} ───`);
  try {
    await fn(stepRecord);
    stepRecord.status = 'passed';
    report.summary.passed++;
    console.log(`[UAT] ✔ ${name} (${Date.now() - start}ms)`);
  } catch (err) {
    stepRecord.status = 'failed';
    stepRecord.error = err.message || String(err);
    report.summary.failed++;
    console.error(`[UAT] ✘ ${name} failed: ${stepRecord.error}`);
  } finally {
    stepRecord.duration_ms = Date.now() - start;
    report.steps.push(stepRecord);
  }
}

async function api(path, options = {}) {
  const url = `${BASE_URL}${path}`;
  const res = await fetch(url, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${ADMIN_KEY}`,
      ...(options.headers || {}),
    },
  });
  const text = await res.text();
  let json;
  try { json = JSON.parse(text); } catch { json = null; }
  return { status: res.status, ok: res.ok, json, text };
}

// Lightweight local mock upstream server to guarantee deterministic end-to-end inference
function startMockServer() {
  return new Promise((res) => {
    const srv = http.createServer((req, resp) => {
      let body = '';
      req.on('data', d => body += d);
      req.on('end', () => {
        resp.setHeader('Content-Type', 'application/json');
        if (req.url.includes('/models')) {
          resp.writeHead(200);
          resp.end(JSON.stringify({
            object: 'list',
            data: [
              { id: 'deepseek-v4-flash-free', object: 'model' },
              { id: 'muse-spark-1.3-contributor-free', object: 'model' },
            ],
          }));
          return;
        }
        resp.writeHead(200);
        resp.end(JSON.stringify({
          id: 'chatcmpl-mock-12345',
          object: 'chat.completion',
          created: Math.floor(Date.now() / 1000),
          model: 'deepseek-v4-flash-free',
          choices: [
            {
              index: 0,
              message: { role: 'assistant', content: 'Hello! 2 + 2 is 4.' },
              finish_reason: 'stop',
            },
          ],
          usage: { prompt_tokens: 10, completion_tokens: 8, total_tokens: 18 },
        }));
      });
    });
    srv.listen(MOCK_PORT, '127.0.0.1', () => res(srv));
  });
}

async function run() {
  await mkdir(SCREENSHOTS_DIR, { recursive: true });
  const mockServer = await startMockServer();
  console.log(`[UAT] Mock upstream active on http://127.0.0.1:${MOCK_PORT}`);

  const { chromium } = await loadPlaywright();

  console.log(`[UAT] Launching headless browser against ${BASE_URL}...`);
  const browser = await chromium.launch({
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-gpu'],
  });

  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    colorScheme: 'light',
  });

  const page = await context.newPage();

  // Instrument console and network
  page.on('console', msg => {
    const entry = { type: msg.type(), text: msg.text(), location: msg.location() };
    report.console_logs.push(entry);
    if (msg.type() === 'error') {
      console.warn(`[Browser Console Error] ${msg.text()}`);
    }
  });

  page.on('requestfailed', req => {
    const failure = req.failure();
    report.network_failures.push({
      url: req.url(),
      method: req.method(),
      error: failure?.errorText || 'Unknown failure',
    });
  });

  let mintedKeyToken = '';

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 1: Sign-in & Theme Toggle
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('01-auth-and-overview', async (step) => {
    await page.goto(`${BASE_URL}/console`, { waitUntil: 'domcontentloaded', timeout: 15000 });
    await page.waitForTimeout(1000);

    const adminInput = await page.$('input[type="password"], input[placeholder*="key" i], input[aria-label*="key" i]');
    if (adminInput) {
      await adminInput.fill(ADMIN_KEY);
      const submitBtn = await page.$('button:has-text("Sign in"), button[type="submit"]');
      if (submitBtn) await submitBtn.click();
      await page.waitForTimeout(1500);
    }

    const overviewShot = join(SCREENSHOTS_DIR, '01-overview.png');
    await page.screenshot({ path: overviewShot, fullPage: true });
    step.screenshot = '01-overview.png';

    // Theme toggle test
    const themeBtn = await page.$('button[aria-label*="dark" i], button[title*="dark" i], button:has-text("dark")');
    if (themeBtn) {
      await themeBtn.click();
      await page.waitForTimeout(500);
      const darkShot = join(SCREENSHOTS_DIR, '02-dark-overview.png');
      await page.screenshot({ path: darkShot, fullPage: true });
      // Toggle back to light
      await themeBtn.click();
      await page.waitForTimeout(300);
    }
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 2: Access & IAM (Humans, Services, Projects, Members)
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('02-access-and-iam', async (step) => {
    const accessNav = await page.$('button:has-text("Access"), a[href*="#access"]');
    if (accessNav) await accessNav.click();
    else await page.goto(`${BASE_URL}/console#access`);
    await page.waitForTimeout(1000);

    // Create Human Principal
    await api('/admin/api/principals', {
      method: 'POST',
      body: JSON.stringify({ display_name: 'Alice Engineer', kind: 'human', email: 'alice@example.com' }),
    });

    // Create Service Principal
    await api('/admin/api/principals', {
      method: 'POST',
      body: JSON.stringify({ display_name: 'Backend CI Service', kind: 'service' }),
    });

    // Create Project
    await api('/admin/api/projects', {
      method: 'POST',
      body: JSON.stringify({ slug: 'core-engineering', name: 'Core Engineering' }),
    });

    // Assign Alice as Owner
    const pList = await api('/admin/api/principals');
    const prjList = await api('/admin/api/projects');
    const alice = (pList.json?.principals || []).find(p => p.display_name === 'Alice Engineer');
    const project = (prjList.json?.projects || []).find(p => p.slug === 'core-engineering');

    if (alice && project) {
      await api('/admin/api/memberships', {
        method: 'POST',
        body: JSON.stringify({ principal_id: alice.id, project_id: project.id, role: 'Owner' }),
      });
    }

    await page.reload();
    await page.waitForTimeout(1500);

    const accessShot = join(SCREENSHOTS_DIR, '03-access-iam.png');
    await page.screenshot({ path: accessShot, fullPage: true });
    step.screenshot = '03-access-iam.png';
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 3: Provider Hub & Live Roster Verification
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('03-providers-and-roster', async (step) => {
    const provNav = await page.$('button:has-text("Providers"), a[href*="#providers"]');
    if (provNav) await provNav.click();
    else await page.goto(`${BASE_URL}/console#providers`);
    await page.waitForTimeout(1500);

    // Verify roster snapshot via API
    const rosterSnap = await api('/admin/api/provider-roster');
    console.log('[UAT] Live roster status:', rosterSnap.status, 'Entries:', rosterSnap.json?.entries?.length);

    // Screenshot 1: Full shelves view without search
    const shelvesShot = join(SCREENSHOTS_DIR, '04a-providers-shelves.png');
    await page.screenshot({ path: shelvesShot, fullPage: true });

    // Search for OpenCode in the search box
    const searchInput = await page.$('input[placeholder*="Search" i]');
    if (searchInput) {
      await searchInput.fill('OpenCode');
      await page.waitForTimeout(800);
    }

    // Click the OpenCode Zen row to expand its details drawer
    const opencodeRow = await page.$('button:has-text("OpenCode Zen"), [data-provider-trigger*="opencode" i]');
    if (opencodeRow) {
      await opencodeRow.click();
      await page.waitForTimeout(800);
    }

    const rosterShot = join(SCREENSHOTS_DIR, '04b-opencode-detail.png');
    await page.screenshot({ path: rosterShot, fullPage: true });
    step.screenshot = '04b-opencode-detail.png';

    if (searchInput) {
      await searchInput.fill('');
      await page.waitForTimeout(300);
    }
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 4: Provider Connection & Routing Setup
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('04-provider-and-route-setup', async (step) => {
    // 1. Configure OpenCode Zen provider instance pointing to local mock
    const provUpsert = await api('/admin/api/providers', {
      method: 'POST',
      body: JSON.stringify({
        id: 'opencode-zen',
        label: 'OpenCode Zen (Free Tier)',
        type: 'openai_compatible',
        base_url: `http://127.0.0.1:${MOCK_PORT}/v1`,
        api_key: 'free',
      }),
    });
    console.log('[UAT] Provider configure status:', provUpsert.status);

    // 2. Set up fallback route 'free-tier-chat'
    const routeUpsert = await api('/admin/api/endpoints', {
      method: 'POST',
      body: JSON.stringify({
        name: 'free-tier-chat',
        failover: [
          { provider: 'opencode-zen', model: 'deepseek-v4-flash-free' },
          { provider: 'opencode-zen', model: 'muse-spark-1.3-contributor-free' },
        ],
      }),
    });
    console.log('[UAT] Route 1 (free-tier-chat) status:', routeUpsert.status);

    // 3. Set up second route 'admin-only-route' for scope security testing
    const route2Upsert = await api('/admin/api/endpoints', {
      method: 'POST',
      body: JSON.stringify({
        name: 'admin-only-route',
        failover: [
          { provider: 'opencode-zen', model: 'deepseek-v4-flash-free' },
        ],
      }),
    });
    console.log('[UAT] Route 2 (admin-only-route) status:', route2Upsert.status);

    const routesNav = await page.$('button:has-text("Routes"), a[href*="#routes"]');
    if (routesNav) await routesNav.click();
    else await page.goto(`${BASE_URL}/console#routes`);
    await page.waitForTimeout(1500);

    const routesShot = join(SCREENSHOTS_DIR, '05-routes.png');
    await page.screenshot({ path: routesShot, fullPage: true });
    step.screenshot = '05-routes.png';
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 5: Mint API Key with Route Scopes (New Feature)
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('05-mint-key-with-scopes', async (step) => {
    const keysNav = await page.$('button:has-text("API keys"), a[href*="#keys"]');
    if (keysNav) await keysNav.click();
    else await page.goto(`${BASE_URL}/console#keys`);
    await page.waitForTimeout(1000);

    const prjList = await api('/admin/api/projects');
    const project = (prjList.json?.projects || []).find(p => p.slug === 'core-engineering') || prjList.json?.projects?.[0];
    if (!project) throw new Error('No project found to mint key in');

    const mintRes = await api('/admin/api/keys', {
      method: 'POST',
      body: JSON.stringify({
        project_id: project.id,
        name: 'cli-scoped-key',
        routes_only: true,
        allowed_routes: ['free-tier-chat'],
        admin_managed: true,
      }),
    });

    if (!mintRes.ok) throw new Error(`Mint key failed (${mintRes.status}): ${mintRes.text}`);
    mintedKeyToken = mintRes.json?.token || mintRes.json?.key || '';
    console.log('[UAT] Minted key prefix:', mintRes.json?.prefix, 'Token received:', Boolean(mintedKeyToken));

    await page.reload();
    await page.waitForTimeout(1500);

    const keysShot = join(SCREENSHOTS_DIR, '06-keys.png');
    await page.screenshot({ path: keysShot, fullPage: true });
    step.screenshot = '06-keys.png';
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 6: Playground Experience
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('06-playground-experience', async (step) => {
    const playNav = await page.$('button:has-text("Playground"), a[href*="#playground"]');
    if (playNav) await playNav.click();
    else await page.goto(`${BASE_URL}/console#playground`);
    await page.waitForTimeout(1500);

    // Select project if needed
    const selects = await page.$$('select');
    for (const sel of selects) {
      const options = await sel.$$eval('option', opts => opts.map(o => ({ value: o.value, text: o.textContent })));
      const prjOption = options.find(o => o.text.includes('core-engineering') || o.value.includes('core-engineering') || o.text.includes('Core Engineering'));
      if (prjOption) {
        await sel.selectOption(prjOption.value);
        await page.waitForTimeout(500);
      }
    }

    // Click on available route/model button if present
    const routeBtn = await page.$('button:has-text("free-tier-chat"), [data-model*="free-tier-chat"]');
    if (routeBtn) {
      await routeBtn.click();
      await page.waitForTimeout(500);
    }

    // Try sending prompt in playground
    const promptArea = await page.$('textarea, [contenteditable="true"]');
    if (promptArea) {
      await promptArea.fill('Say hello from Playground test');
      const sendBtn = await page.$('button:has-text("Send"), button:has-text("Run"), button[type="submit"]');
      if (sendBtn && !(await sendBtn.isDisabled())) {
        await sendBtn.click();
        await page.waitForTimeout(2000);
      }
    }

    const playShot = join(SCREENSHOTS_DIR, '07-playground.png');
    await page.screenshot({ path: playShot, fullPage: true });
    step.screenshot = '07-playground.png';
  });

  // ─────────────────────────────────────────────────────────────────────────────
  // Step 7: Gateway API Proxy & Key Scope Security Probe
  // ─────────────────────────────────────────────────────────────────────────────
  await logStep('07-gateway-inference-and-scope-enforcement', async () => {
    if (!mintedKeyToken) throw new Error('No minted key token available for inference probe');

    // 1. Allowed Route Request (Must succeed 200 OK)
    console.log('[UAT] 1. Calling allowed route "free-tier-chat"...');
    const chatRes = await fetch(`${BASE_URL}/v1/chat/completions`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${mintedKeyToken}`,
      },
      body: JSON.stringify({
        model: 'free-tier-chat',
        messages: [{ role: 'user', content: 'What is 2+2?' }],
      }),
    });

    console.log('[UAT] Allowed route status:', chatRes.status);
    const chatBody = await chatRes.text();
    console.log('[UAT] Response snippet:', chatBody.slice(0, 200));

    if (!chatRes.ok) {
      throw new Error(`Inference call to allowed route failed with status ${chatRes.status}: ${chatBody}`);
    }

    // 2. Out-of-Scope Existing Route (Must be blocked with 403 Forbidden)
    console.log('[UAT] 2. Calling forbidden existing route "admin-only-route"...');
    const forbiddenRes = await fetch(`${BASE_URL}/v1/chat/completions`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${mintedKeyToken}`,
      },
      body: JSON.stringify({
        model: 'admin-only-route',
        messages: [{ role: 'user', content: 'hello' }],
      }),
    });

    console.log('[UAT] Forbidden route status (expect 403):', forbiddenRes.status);
    if (forbiddenRes.status !== 403) {
      throw new Error(`Expected 403 Forbidden for out-of-scope model, got ${forbiddenRes.status}`);
    }
    console.log('[UAT] Key scope security boundary verified: out-of-scope route returned 403 Forbidden!');

    // 3. Non-Existent Model (Must return 404 Not Found)
    console.log('[UAT] 3. Calling non-existent route "does-not-exist"...');
    const notFoundRes = await fetch(`${BASE_URL}/v1/chat/completions`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${mintedKeyToken}`,
      },
      body: JSON.stringify({
        model: 'does-not-exist',
        messages: [{ role: 'user', content: 'hello' }],
      }),
    });

    console.log('[UAT] Non-existent route status (expect 404):', notFoundRes.status);
    if (notFoundRes.status !== 404) {
      throw new Error(`Expected 404 Not Found for missing route, got ${notFoundRes.status}`);
    }
    console.log('[UAT] All three routing and governance security boundaries verified!');
  });

  // Cleanup
  await browser.close();
  mockServer.close();

  // Correlate server logs
  try {
    report.server_logs = await readFile('E:/open-source-projects\llmgw/.gateway-state/gateway.log', 'utf8');
  } catch {}

  report.completed_at = new Date().toISOString();
  await writeFile(join(OUTPUT_DIR, 'report.json'), JSON.stringify(report, null, 2));

  console.log('\n[UAT] ══════════════════════════════════════════════════════════════');
  console.log(`[UAT] ALL DONE: ${report.summary.passed}/${report.summary.total} steps passed, ${report.summary.failed} failed.`);
  console.log(`[UAT] Report JSON: ${join(OUTPUT_DIR, 'report.json')}`);
  console.log(`[UAT] Screenshots: ${SCREENSHOTS_DIR}`);
  console.log('[UAT] ══════════════════════════════════════════════════════════════\n');
}

run().catch(err => {
  console.error('[UAT Fatal Error]', err);
  process.exit(1);
});
