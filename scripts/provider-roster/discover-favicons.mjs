import { lookup } from 'node:dns/promises';
import https from 'node:https';
import { isIP } from 'node:net';
import { createBrotliDecompress, createGunzip, createInflate } from 'node:zlib';
import { Transform, Writable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { resolve, join, isAbsolute } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createHash } from 'node:crypto';
import { isPublicAddress, Scheduler, FetchError, publicURL } from './network.mjs';
import { validatePNG } from './logos.mjs';

export const PAGE_LIMIT = 2 * 1024 * 1024;
export const ICON_LIMIT = 512 * 1024;
const MAX_CANDIDATES = 32;
const PNG = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);
const fail = code => { throw new FetchError(code); };

// Only public presentation/cache/referral queries are needed for this experiment.
// An allowlist also prevents unknown session/signature parameters reaching disk.
const publicQuery = /^(?:v|ver|version|rev|revision|hash|cache|cb|t|ts|w|h|width|height|size|s|dpr|quality|q|format|fm|fit|color|theme|lang|locale|ref|source|utm_(?:source|medium|campaign|term|content))$/i;
export function discoveryURL(input) {
  if (typeof input !== 'string' || Buffer.byteLength(input) > 4096 || /[\\\s<>\u0000-\u001f]/.test(input)) fail('blocked_url');
  let url;
  try { url = new URL(input); } catch { fail('blocked_url'); }
  if (url.username || url.password) fail('blocked_url');
  let path;
  try { path = decodeURIComponent(url.pathname); } catch { fail('blocked_url'); }
  if (/(?:^|\/)(?:token|access_token|secret|session|password|reset-password)(?:\/|=)|(?:sk|ghp|github_pat)[_-][a-z0-9_-]{12,}|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\./i.test(path)) fail('blocked_private_url');
  for (const [key, value] of url.searchParams) {
    if (!publicQuery.test(key) || value.length > 128 || !/^[\w.,~+ -]*$/u.test(value) ||
        /(?:sk|ghp|github_pat)[_-][a-z0-9_-]{12,}|bearer|eyJ[a-z0-9_-]+\./i.test(value)) fail('blocked_private_query');
  }
  const query = url.search;
  url.search = '';
  url.hash = '';
  const checked = publicURL(url.href);
  checked.search = query;
  return checked;
}

function byteLimiter(maxBytes) {
  let size = 0;
  return new Transform({ transform(chunk, _encoding, callback) {
    size += chunk.length;
    callback(size > maxBytes ? new FetchError('body_limit') : null, chunk);
  } });
}

// Both the wire body and decoded body are bounded, including streaming compression.
export async function readBoundedBody(stream, headers, maxBytes) {
  if (Number(headers['content-length']) > maxBytes) {
    stream.destroy();
    fail('body_limit');
  }
  const encoding = (headers['content-encoding'] || 'identity').trim().toLowerCase();
  const decoder = { gzip: createGunzip, br: createBrotliDecompress, deflate: createInflate }[encoding];
  if (!decoder && encoding !== 'identity') {
    stream.destroy();
    fail('unsupported_encoding');
  }
  const chunks = [];
  const sink = new Writable({ write(chunk, _encoding, callback) { chunks.push(chunk); callback(); } });
  await pipeline(stream, byteLimiter(maxBytes), ...(decoder ? [decoder(), byteLimiter(maxBytes)] : []), sink);
  return Buffer.concat(chunks);
}

// This deliberately does not widen the production collector's network contract.
export function createDiscoveryFetcher({ resolver = lookup, request = https.request,
  scheduler = new Scheduler(4), timeoutMs = 10000 } = {}) {
  if (!Number.isInteger(timeoutMs) || timeoutMs < 100 || timeoutMs > 30000) fail('invalid_timeout');
  return async function fetchPublic(input, { maxBytes = PAGE_LIMIT, sameOrigin = false } = {}) {
    if (!Number.isInteger(maxBytes) || maxBytes < 1 || maxBytes > PAGE_LIMIT) fail('invalid_limit');
    let url = discoveryURL(input);
    const originalOrigin = url.origin;
    const deadline = Date.now() + timeoutMs;
    for (let hop = 0; hop <= 3; hop++) {
      const response = await scheduler.run(url.hostname, async () => {
        let req;
        let res;
        let expired = false;
        let timer;
        const remaining = deadline - Date.now();
        if (remaining <= 0) fail('timeout');
        const timeout = new Promise((_, reject) => {
          timer = setTimeout(() => {
            expired = true;
            req?.destroy(new FetchError('timeout'));
            res?.destroy(new FetchError('timeout'));
            reject(new FetchError('timeout'));
          }, remaining);
        });
        const operation = async () => {
          const host = url.hostname.replace(/^\[|\]$/g, '');
          const addresses = isIP(host) ? [{ address: host, family: isIP(host) }]
            : await resolver(host, { all: true, verbatim: true });
          if (expired) fail('timeout');
          if (!addresses.length || addresses.some(a => !isPublicAddress(a.address) || isIP(a.address) !== a.family)) fail('blocked_address');
          const pinned = [...addresses].sort((a, b) => a.address.localeCompare(b.address))[0];
          return new Promise((resolveResponse, reject) => {
            req = request(url, {
              method: 'GET', agent: false, family: pinned.family,
              servername: isIP(host) ? undefined : host, rejectUnauthorized: true, maxHeaderSize: 16384,
              lookup: (_host, options, callback) => options.all
                ? callback(null, [pinned]) : callback(null, pinned.address, pinned.family),
              headers: { 'User-Agent': 'llm-gateway-favicon-experiment', Accept: '*/*', 'Accept-Encoding': 'gzip, br, deflate' },
            }, incoming => {
              res = incoming;
              if ([301, 302, 303, 307, 308].includes(res.statusCode)) {
                const location = res.headers.location;
                res.destroy();
                resolveResponse({ redirect: true, location });
                return;
              }
              readBoundedBody(res, res.headers, maxBytes).then(body => resolveResponse({
                status: res.statusCode, headers: res.headers, body, url: url.href,
              }), reject);
            });
            req.on('error', reject);
            req.end();
          });
        };
        try { return await Promise.race([operation(), timeout]); }
        catch (error) { throw error instanceof FetchError ? error : new FetchError('network_error'); }
        finally { clearTimeout(timer); }
      });
      if (!response.redirect) return response;
      if (hop === 3) fail('redirect_limit');
      if (!response.location) fail('invalid_redirect');
      try { url = discoveryURL(new URL(response.location, url).href); }
      catch (error) { throw error instanceof FetchError ? error : new FetchError('invalid_redirect'); }
      if (sameOrigin && url.origin !== originalOrigin) fail('blocked_cross_origin_redirect');
    }
  };
}

const entities = { amp: '&', AMP: '&', quot: '"', QUOT: '"', apos: "'", lt: '<', LT: '<', gt: '>', GT: '>',
  colon: ':', sol: '/', period: '.', quest: '?', equals: '=', num: '#', percnt: '%', lowbar: '_',
  hyphen: '‐', plus: '+', semi: ';', commat: '@', Tab: '\t', NewLine: '\n', nbsp: '\u00a0' };
export function decodeEntities(text) {
  return text.replace(/&(#x[\da-f]+;?|#\d+;?|[a-z][a-z\d]*;)/gi, (whole, entity) => {
    const key = entity.replace(/;$/, '');
    if (key[0] !== '#') return entities[key] ?? whole;
    const value = key[1]?.toLowerCase() === 'x' ? parseInt(key.slice(2), 16) : parseInt(key.slice(1), 10);
    return value > 0 && value <= 0x10ffff && !(value >= 0xd800 && value <= 0xdfff) ? String.fromCodePoint(value) : '\ufffd';
  }).replace(/&amp(?=[=&]|$)/g, '&');
}

function attributes(tag) {
  const result = {};
  const body = tag.replace(/^<\s*[\w:-]+/, '').replace(/\/?\s*>$/, '');
  for (const match of body.matchAll(/([^\s=/>]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g)) {
    const key = match[1].toLowerCase();
    if (!Object.hasOwn(result, key)) result[key] = decodeEntities(match[2] ?? match[3] ?? match[4] ?? '');
  }
  return result;
}

function rootCandidates(pageURL) {
  const page = discoveryURL(pageURL);
  return ['/favicon.ico', '/favicon.png', '/apple-touch-icon.png'].map(path => ({
    url: new URL(path, page).href, kind: 'root_fallback', rank: 4,
  }));
}

export function iconCandidates(html, pageURL) {
  const page = discoveryURL(pageURL);
  let base = page.href;
  let baseSeen = false;
  let inertDepth = 0;
  let raw = '';
  const links = [];
  // Tokenize complete tags (including quoted '>'); never interpret page scripts.
  for (const match of html.matchAll(/<!--[\s\S]*?(?:-->|$)|<![^>]*>|<\/?[a-z][a-z\d:-]*(?:\s+(?:[^'"<>]|"[^"]*"|'[^']*')*)?\s*\/?>/gi)) {
    const tag = match[0];
    const name = /^<\/?([\w:-]+)/.exec(tag)?.[1].toLowerCase();
    if (!name) continue;
    const closing = tag.startsWith('</');
    if (raw) { if (closing && name === raw) raw = ''; continue; }
    if (!closing && ['script', 'style', 'textarea', 'title', 'xmp', 'iframe', 'noembed', 'noframes'].includes(name)) { raw = name; continue; }
    if (name === 'template' || name === 'noscript') { inertDepth = Math.max(0, inertDepth + (closing ? -1 : 1)); continue; }
    if (inertDepth || closing || !['link', 'base'].includes(name)) continue;
    const attrs = attributes(tag);
    if (name === 'base' && !baseSeen && Object.hasOwn(attrs, 'href')) {
      baseSeen = true;
      try { base = new URL(attrs.href.trim(), page).href; } catch { /* Invalid base leaves document URL. */ }
    }
    if (name === 'link' && attrs.href) links.push(attrs);
  }
  const found = [];
  for (const attrs of links) {
    const rel = (attrs.rel || '').toLowerCase().split(/\s+/);
    const kind = rel.includes('icon') ? 'icon' : rel.includes('apple-touch-icon') ? 'apple-touch-icon'
      : rel.includes('apple-touch-icon-precomposed') ? 'apple-touch-icon-precomposed' : rel.includes('mask-icon') ? 'mask-icon' : '';
    if (!kind) continue;
    try {
      found.push({ url: discoveryURL(new URL(attrs.href.trim(), base).href).href, kind,
        rank: { icon: 0, 'apple-touch-icon': 1, 'apple-touch-icon-precomposed': 2, 'mask-icon': 3 }[kind] });
    } catch { /* Never report or request unsafe link contents. */ }
  }
  found.sort((a, b) => a.rank - b.rank);
  const seen = new Set();
  const unique = found.filter(item => {
    if (seen.has(item.url)) return false;
    seen.add(item.url);
    return true;
  }).slice(0, MAX_CANDIDATES - 3);
  for (const root of rootCandidates(page.href)) {
    if (!unique.some(item => item.url === root.url)) unique.push(root);
  }
  return unique;
}

export function imageType(bytes) {
  if (!Buffer.isBuffer(bytes) || bytes.length > ICON_LIMIT || bytes.length < 4) fail('unsupported_image');
  if (bytes.subarray(0, 8).equals(PNG)) {
    if (bytes.length < 24 || bytes.toString('ascii', 12, 16) !== 'IHDR') fail('invalid_image');
    if (!bytes.readUInt32BE(16) || !bytes.readUInt32BE(20) || bytes.readUInt32BE(16) > 4096 || bytes.readUInt32BE(20) > 4096) fail('image_dimensions');
    return 'image/png';
  }
  if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return 'image/jpeg';
  if (bytes.toString('ascii', 0, 4) === 'RIFF' && bytes.toString('ascii', 8, 12) === 'WEBP') return 'image/webp';
  if (bytes.readUInt32LE(0) === 0x00010000) {
    if (bytes.length < 22) fail('invalid_image');
    const count = bytes.readUInt16LE(4);
    if (!count || count > 256 || 6 + count * 16 > bytes.length) fail('invalid_image');
    for (let i = 0; i < count; i++) {
      const size = bytes.readUInt32LE(6 + i * 16 + 8);
      const start = bytes.readUInt32LE(6 + i * 16 + 12);
      if (start < 6 + count * 16 || !size || size > bytes.length - start) fail('invalid_image');
      if (bytes.subarray(start, start + 8).equals(PNG)) imageType(bytes.subarray(start, start + size));
    }
    return 'image/x-icon';
  }
  const text = bytes.toString('utf8').replace(/^\uFEFF/, '').trim();
  if (/^(?:<\?xml[^>]*>\s*)?(?:<!--[\s\S]*?-->\s*)*<svg[\s>]/i.test(text)) {
    if (/<\s*(?:[\w-]+:)?(?:script|foreignObject|iframe|object|embed)\b|<!DOCTYPE|<!ENTITY|\bon\w+\s*=|@import|\\/i.test(text) ||
        /(?:href|src)\s*=\s*(?:"(?!#)|'(?!#)|[^\s"'])/i.test(text) ||
        [...text.matchAll(/url\(([^)]*)\)/gi)].some(match => !/^(?:#[\w:.-]+|"#[\w:.-]+"|'#[\w:.-]+')$/.test(match[1].trim()))) fail('unsafe_svg');
    return 'image/svg+xml';
  }
  fail('unsupported_image');
}

export async function renderIcon(browser, bytes) {
  const mime = imageType(bytes);
  const context = await browser.newContext({ offline: true, serviceWorkers: 'block', acceptDownloads: false, permissions: [] });
  let timer;
  try {
    await context.route('**/*', route => route.abort());
    const page = await context.newPage();
    const operation = async () => {
      await page.setContent('<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; img-src data:; script-src \'none\'">');
      return page.evaluate(async ({ data, mime }) => {
        const image = new Image();
        image.src = `data:${mime};base64,${data}`;
        await image.decode();
        const width = image.naturalWidth;
        const height = image.naturalHeight;
        if (!width || !height || width > 4096 || height > 4096 || (width === 1 && height === 1)) return { error: 'image_dimensions' };
        const canvas = document.createElement('canvas');
        canvas.width = canvas.height = 128;
        const ctx = canvas.getContext('2d', { willReadFrequently: true });
        const scale = Math.min(128 / width, 128 / height);
        ctx.drawImage(image, (128 - width * scale) / 2, (128 - height * scale) / 2, width * scale, height * scale);
        const pixels = ctx.getImageData(0, 0, 128, 128).data;
        if (!pixels.some((value, index) => index % 4 === 3 && value > 0)) return { error: 'transparent_image' };
        return { data: canvas.toDataURL('image/png').split(',')[1], width, height };
      }, { data: bytes.toString('base64'), mime });
    };
    const result = await Promise.race([operation(), new Promise((_, reject) => {
      timer = setTimeout(() => reject(new FetchError('render_timeout')), 5000);
    })]);
    if (result.error) fail(result.error);
    const png = Buffer.from(result.data, 'base64');
    validatePNG(png);
    return { png, mime, width: result.width, height: result.height };
  } catch (error) { throw error instanceof FetchError ? error : new FetchError('image_decode_failed'); }
  finally { clearTimeout(timer); await context.close(); }
}

export function validateEntries(input) {
  if (!Array.isArray(input?.entries) || input.entries.length > 5000) fail('invalid_entries');
  const ids = new Set();
  return input.entries.map(entry => {
    if (!entry || typeof entry.id !== 'string' || !entry.id || entry.id.length > 128 || ids.has(entry.id) ||
        typeof entry.name !== 'string' || !entry.name || entry.name.length > 256 ||
        !['signup', 'documentation', 'none'].includes(entry.page_kind) || typeof entry.existing_logo !== 'boolean' ||
        (entry.page_url !== undefined && entry.page_url !== null && typeof entry.page_url !== 'string')) fail('invalid_entry');
    ids.add(entry.id);
    return { id: entry.id, name: entry.name, page_url: entry.page_url || '', page_kind: entry.page_kind, existing_logo: entry.existing_logo };
  });
}

const errorCode = error => error instanceof FetchError ? error.code : 'discovery_failed';
export async function discover(entries, { fetcher, render, concurrency = 4, savePNG = async () => {} }) {
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 8) fail('invalid_concurrency');
  const pages = new Map();
  const assets = new Map();
  const saved = new Map();
  const cached = (cache, key, job) => {
    if (!cache.has(key)) cache.set(key, Promise.resolve().then(job));
    return cache.get(key);
  };
  const inspectPage = async url => {
    let page;
    let pageFailure;
    try {
      page = await fetcher(url, { maxBytes: PAGE_LIMIT });
      if (page.status < 200 || page.status >= 300) fail(`page_http_${page.status}`);
      const type = (page.headers['content-type'] || '').toLowerCase();
      if (type && !/^(?:text\/html|application\/xhtml\+xml)(?:;|$)/.test(type)) fail('page_not_html');
    } catch (error) { pageFailure = errorCode(error); }
    // Failed navigation never authorizes using an auth provider's origin or markup.
    const finalURL = pageFailure ? url : discoveryURL(page.url || url).href;
    const pageMetadata = { final_page_url: finalURL, final_page_url_status: pageFailure ? 'unvisited' : 'fetched',
      ...(page ? { page_status: page.status } : {}), ...(pageFailure ? { page_failure: pageFailure } : {}) };
    const candidates = pageFailure ? rootCandidates(url) : iconCandidates(page.body.toString('utf8'), finalURL);
    const attempts = [];
    for (const candidate of candidates) {
      try {
        const sameOrigin = candidate.kind === 'root_fallback';
        const asset = await cached(assets, `${sameOrigin ? 'same-origin:' : ''}${candidate.url}`, async () => {
          const response = await fetcher(candidate.url, { maxBytes: ICON_LIMIT, sameOrigin });
          if (response.status !== 200) fail(`icon_http_${response.status}`);
          if (sameOrigin && discoveryURL(response.url || candidate.url).origin !== new URL(candidate.url).origin) fail('blocked_cross_origin_redirect');
          const rendered = await render(response.body);
          const header = validatePNG(rendered.png);
          if (header.width !== 128 || header.height !== 128) fail('invalid_normalization');
          const hash = createHash('sha256').update(rendered.png).digest('hex');
          const file = `logos/${hash}.png`;
          await cached(saved, hash, () => savePNG(file, rendered.png));
          return { file, sha256: hash, source_url: discoveryURL(response.url || candidate.url).href,
            source_mime: rendered.mime, source_width: rendered.width, source_height: rendered.height };
        });
        attempts.push({ url: candidate.url, kind: candidate.kind, status: 'collected' });
        return { status: 'collected', ...pageMetadata, ...asset, selected_kind: candidate.kind, attempts };
      } catch (error) { attempts.push({ url: candidate.url, kind: candidate.kind, status: 'failed', reason: errorCode(error) }); }
    }
    return { status: pageFailure ? 'page_inaccessible' : 'no_supported_icon',
      ...(pageFailure ? { reason: pageFailure } : {}), ...pageMetadata, attempts };
  };
  let next = 0;
  const results = new Array(entries.length);
  await Promise.all(Array.from({ length: concurrency }, async () => {
    while (next < entries.length) {
      const index = next++;
      const entry = entries[index];
      const metadata = { id: entry.id, name: entry.name, existing_logo: entry.existing_logo, page_kind: entry.page_kind };
      if (entry.page_kind === 'none' || !entry.page_url) {
        results[index] = { ...metadata, page_url: '', status: 'no_public_page', reason: 'no_page_url' };
        continue;
      }
      let url;
      try { url = discoveryURL(entry.page_url).href; }
      catch (error) {
        results[index] = { ...metadata, page_url: '', status: 'no_public_page', reason: errorCode(error) };
        continue;
      }
      const result = await cached(pages, url, () => inspectPage(url));
      results[index] = { ...metadata, page_url: url, ...result };
    }
  }));
  const counts = Object.fromEntries(['collected', 'page_inaccessible', 'no_supported_icon', 'no_public_page'].map(status => [status, results.filter(r => r.status === status).length]));
  const existing = results.filter(r => r.existing_logo).length;
  const added = results.filter(r => !r.existing_logo && r.status === 'collected').length;
  return { summary: { total: results.length, ...counts, existing_logos: existing, discovered_without_existing_logo: added,
    potential_coverage: existing + added, unique_pages: pages.size, unique_assets_attempted: assets.size, unique_pngs: saved.size }, results };
}

const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]);
const csvCell = value => `"${String(value ?? '').replace(/^[\s]*[=+@-]/, match => `'${match}`).replace(/"/g, '""')}"`;
export function reports(report) {
  const fields = ['id', 'name', 'page_kind', 'existing_logo', 'status', 'reason', 'page_failure', 'page_url', 'final_page_url', 'final_page_url_status', 'source_url', 'selected_kind', 'file', 'sha256'];
  const csv = [fields.join(','), ...report.results.map(row => fields.map(key => csvCell(row[key])).join(','))].join('\n') + '\n';
  const summary = report.summary;
  const markdown = `# Standalone favicon discovery\n\n` +
    `Inspected ${summary.total} entries; collected ${summary.collected} icons.\n\n` +
    `| Outcome | Count |\n| --- | ---: |\n${Object.entries(summary).map(([key, value]) => `| ${key} | ${value} |`).join('\n')}\n\n` +
    'Potential coverage is existing logos plus collected icons for entries without one; it is not an application change.\n\n' +
    'See results.json / results.csv for per-provider outcomes and source URLs; contact-sheet.html previews local PNGs. ' +
    'Static page markup only; JavaScript, authenticated pages and manifest discovery are not used. Rights are unreviewed.\n';
  const html = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">\n` +
    `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'">\n` +
    `<title>Favicon discovery contact sheet</title><style>body{font:16px system-ui;margin:24px;background:#eee;color:#222}main{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:16px}article{padding:16px;background:white;overflow-wrap:anywhere}img{width:128px;height:128px;object-fit:contain;background:repeating-conic-gradient(#eee 0% 25%,#fff 0% 50%) 0/16px 16px}small{display:block}</style>\n` +
    `<h1>Favicon discovery</h1><p>${summary.collected} / ${summary.total} collected. Standalone evidence; rights unreviewed.</p><main>` +
    report.results.map(row => `<article>${/^logos\/[a-f0-9]{64}\.png$/.test(row.file || '') ? `<img src="${row.file}" alt="">` : ''}<h2>${escapeHTML(row.name)}</h2><small>${escapeHTML(row.id)}</small><p>${escapeHTML(row.status)}${row.reason ? `: ${escapeHTML(row.reason)}` : ''}</p>${row.page_failure ? `<p>Page unavailable: ${escapeHTML(row.page_failure)}; original URL unvisited.</p>` : ''}<small>Existing logo: ${row.existing_logo ? 'yes' : 'no'}</small><small>${escapeHTML(row.source_url)}</small></article>`).join('\n') + '</main></html>\n';
  return { csv, markdown, html };
}

export function parseArgs(args) {
  const options = { concurrency: 4, timeoutMs: 10000 };
  const names = { '--input': 'input', '--output': 'output', '--playwright-module': 'playwrightModule', '--concurrency': 'concurrency', '--timeout-ms': 'timeoutMs' };
  const seen = new Set();
  for (let i = 0; i < args.length; i += 2) {
    const name = names[args[i]];
    if (!name || !args[i + 1] || args[i + 1].startsWith('--') || seen.has(name)) fail('invalid_arguments');
    seen.add(name);
    options[name] = ['concurrency', 'timeoutMs'].includes(name) ? Number(args[i + 1]) : args[i + 1];
  }
  if (!options.input || !options.output || !Number.isInteger(options.concurrency) || options.concurrency < 1 || options.concurrency > 8 ||
      !Number.isInteger(options.timeoutMs) || options.timeoutMs < 100 || options.timeoutMs > 30000 ||
      (options.playwrightModule && !isAbsolute(options.playwrightModule))) fail('invalid_arguments');
  return options;
}

export async function main(args = process.argv.slice(2)) {
  const options = parseArgs(args);
  const raw = await readFile(options.input);
  if (raw.length > 8 * 1024 * 1024) fail('input_limit');
  const entries = validateEntries(JSON.parse(raw.toString('utf8')));
  let playwright;
  try { playwright = await import(options.playwrightModule ? pathToFileURL(options.playwrightModule).href : 'playwright'); }
  catch { fail('playwright_unavailable_pass_absolute_module_path'); }
  const chromium = playwright.chromium || playwright.default?.chromium;
  if (!chromium) fail('playwright_chromium_unavailable');
  const output = resolve(options.output);
  await mkdir(join(output, 'logos'), { recursive: true });
  const browser = await chromium.launch({ headless: true, timeout: 30000 });
  try {
    const report = await discover(entries, {
      concurrency: options.concurrency,
      fetcher: createDiscoveryFetcher({ timeoutMs: options.timeoutMs, scheduler: new Scheduler(options.concurrency, 2) }),
      render: bytes => renderIcon(browser, bytes),
      savePNG: (file, png) => writeFile(join(output, file), png),
    });
    const { csv, markdown, html } = reports(report);
    await Promise.all([
      writeFile(join(output, 'results.json'), JSON.stringify(report, null, 2) + '\n'),
      writeFile(join(output, 'results.csv'), csv),
      writeFile(join(output, 'report.md'), markdown),
      writeFile(join(output, 'contact-sheet.html'), html),
    ]);
    console.log(JSON.stringify(report.summary, null, 2));
    return report;
  } finally { await browser.close(); }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => { console.error(errorCode(error)); process.exitCode = 1; });
}
