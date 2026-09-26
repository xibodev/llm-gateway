import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { Readable } from 'node:stream';
import { gzipSync, brotliCompressSync, deflateSync } from 'node:zlib';
import { pathToFileURL } from 'node:url';
import { crc32, validatePNG } from './logos.mjs';
import { discoveryURL, iconCandidates, createDiscoveryFetcher, readBoundedBody, imageType,
  discover, reports, parseArgs, validateEntries, renderIcon, PAGE_LIMIT, ICON_LIMIT } from './discover-favicons.mjs';

function png(width = 128, height = 128, alpha = 255) {
  const chunk = (type, data) => {
    const out = Buffer.alloc(data.length + 12);
    out.writeUInt32BE(data.length);
    out.write(type, 4);
    data.copy(out, 8);
    out.writeUInt32BE(crc32(out.subarray(4, -4)), out.length - 4);
    return out;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width); header.writeUInt32BE(height, 4); header[8] = 8; header[9] = 6;
  const data = Buffer.alloc((width * 4 + 1) * height);
  for (let y = 0; y < height; y++) for (let x = 0; x < width; x++) {
    const offset = y * (width * 4 + 1) + 1 + x * 4;
    data[offset] = 255; data[offset + 3] = alpha;
  }
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', header), chunk('IDAT', deflateSync(data)), chunk('IEND', Buffer.alloc(0))]);
}

function transport(routes, seen = []) {
  return (url, options, callback) => {
    const req = new EventEmitter();
    req.destroy = error => { if (error) req.emit('error', error); };
    req.end = () => queueMicrotask(() => {
      seen.push({ url: url.href, options });
      const route = routes[url.href];
      if (!route) return req.emit('error', new Error('missing fixture'));
      const res = Readable.from(route.body ? [route.body] : []);
      res.statusCode = route.status ?? 200;
      res.headers = route.headers ?? {};
      callback(res);
    });
    return req;
  };
}
const publicDNS = async () => [{ address: '8.8.8.8', family: 4 }];

test('case, quoted/unquoted attributes, entities, base URL and stable icon priority', () => {
  const icons = iconCandidates(`<!-- <link rel=icon href=bad.png> -->
    <script>const fake = '<link rel=icon href="script.png">';</script>
    <template><link rel=icon href=template.png></template>
    <div title="<link rel=icon href=attribute.png>"></div>
    <BASE HREF='../assets/'><base href='https://ignored.example/'>
    <link rel='mask-icon' href='mask.svg'><link rel=apple-touch-icon-precomposed href=old.png>
    <LiNk REL='APPLE-TOUCH-ICON' href='apple.png'>
    <LINK data-note='a > b' REL="shortcut ICON" HREF="icon&#46;png?v=1&amp;size=64">
    <link rel=icon href=//cdn.example/favicon.png><link rel=stylesheet href=x.css>`, 'https://example.com/account/signup');
  assert.deepEqual(icons.map(i => [i.kind, i.url]), [
    ['icon', 'https://example.com/assets/icon.png?v=1&size=64'], ['icon', 'https://cdn.example/favicon.png'],
    ['apple-touch-icon', 'https://example.com/assets/apple.png'], ['apple-touch-icon-precomposed', 'https://example.com/assets/old.png'],
    ['mask-icon', 'https://example.com/assets/mask.svg'], ['root_fallback', 'https://example.com/favicon.ico'],
    ['root_fallback', 'https://example.com/favicon.png'], ['root_fallback', 'https://example.com/apple-touch-icon.png'],
  ]);
});

test('root fallback uses final page origin, skips unsafe links and bounds candidates', () => {
  assert.deepEqual(iconCandidates('<base href="https://cdn.example/a/"><link rel=icon href="/logo.svg">', 'https://final.example/signup').map(c => c.url),
    ['https://cdn.example/logo.svg', 'https://final.example/favicon.ico', 'https://final.example/favicon.png', 'https://final.example/apple-touch-icon.png']);
  const icons = iconCandidates('<link rel=icon href="http://127.0.0.1/x"><link rel=icon href="/x?token=secret">' +
    Array.from({ length: 50 }, (_, i) => `<link rel=icon href="/${i}.png">`).join(''), 'https://example.com/');
  assert.equal(icons.length, 32);
  assert.equal(icons.at(-1).kind, 'root_fallback');
  assert.equal(iconCandidates('<link rel=mask-icon href=/same.svg><link rel=icon href=/same.svg>', 'https://example.com/')[0].kind, 'icon');
});

test('public cache queries allowed; credentials, private queries and addresses rejected', () => {
  assert.equal(discoveryURL('https://example.com/icon.svg?v=abc123&size=64#symbol').href, 'https://example.com/icon.svg?v=abc123&size=64');
  for (const url of ['http://example.com/', 'https://user:pass@example.com/', 'https://localhost/', 'https://127.0.0.1/',
    'https://[::1]/', 'https://[::ffff:127.0.0.1]/', 'https://169.254.169.254/', 'https://10.0.0.1/',
    'https://example.com:8443/', 'https://example.com/?%74oken=secret', 'https://example.com/?api_key=secret',
    'https://example.com/?email=person', 'https://example.com/?v=https%3A%2F%2Fprivate.example',
    'https://example.com/reset-password/secret', 'https://example.com/a?signature=abc']) assert.throws(() => discoveryURL(url));
});

test('redirects resolve relative paths, pin DNS anew each hop and never forward cookies', async () => {
  const seen = [];
  const hosts = [];
  const fetcher = createDiscoveryFetcher({ resolver: async host => { hosts.push(host); return publicDNS(); }, request: transport({
    'https://example.com/signup': { status: 302, headers: { location: '/account?v=1', 'set-cookie': 'fixture=unused' } },
    'https://example.com/account?v=1': { status: 301, headers: { location: 'https://other.example/join' } },
    'https://other.example/join': { body: Buffer.from('ok') },
  }, seen) });
  const result = await fetcher('https://example.com/signup');
  assert.equal(result.url, 'https://other.example/join');
  assert.deepEqual(hosts, ['example.com', 'example.com', 'other.example']);
  for (const { options } of seen) {
    assert.equal(options.agent, false);
    assert.equal(options.rejectUnauthorized, true);
    assert.equal(options.headers.Cookie, undefined);
    assert.equal(options.headers.Authorization, undefined);
    options.lookup('example.com', {}, (error, address, family) => { assert.equal(error, null); assert.equal(address, '8.8.8.8'); assert.equal(family, 4); });
    options.lookup('example.com', { all: true }, (_error, addresses) => assert.deepEqual(addresses, [{ address: '8.8.8.8', family: 4 }]));
  }
});

test('SSRF redirects, mixed DNS, DNS rebinding, secret redirects and redirect loops fail closed', async () => {
  for (const location of ['https://127.0.0.1/', 'https://example.com/?token=secret', 'https://private.example/']) {
    const seen = [];
    const fetcher = createDiscoveryFetcher({ resolver: async host => host === 'private.example'
      ? [{ address: '8.8.8.8', family: 4 }, { address: '10.0.0.1', family: 4 }] : publicDNS(),
    request: transport({ 'https://example.com/': { status: 302, headers: { location } } }, seen) });
    await assert.rejects(fetcher('https://example.com/'), /blocked_/);
    assert.equal(seen.length, 1);
  }
  let calls = 0;
  const rebinding = createDiscoveryFetcher({ resolver: async () => [{ address: ++calls === 1 ? '8.8.8.8' : '127.0.0.1', family: 4 }],
    request: transport({ 'https://example.com/': { status: 302, headers: { location: '/next' } } }) });
  await assert.rejects(rebinding('https://example.com/'), /blocked_address/);
  const seen = [];
  const loop = createDiscoveryFetcher({ resolver: publicDNS, request: transport({ 'https://example.com/': { status: 302, headers: { location: '/' } } }, seen) });
  await assert.rejects(loop('https://example.com/'), /redirect_limit/);
  assert.equal(seen.length, 4);
});

test('compressed bodies have wire and decoded bounds', async () => {
  const body = Buffer.from('hello'.repeat(100));
  for (const [encoding, compress] of [['gzip', gzipSync], ['br', brotliCompressSync], ['deflate', deflateSync]]) {
    assert.deepEqual(await readBoundedBody(Readable.from([compress(body)]), { 'content-encoding': encoding }, 1000), body);
    await assert.rejects(readBoundedBody(Readable.from([compress(body)]), { 'content-encoding': encoding }, 100), /body_limit/);
  }
  await assert.rejects(readBoundedBody(Readable.from([Buffer.alloc(101)]), {}, 100), /body_limit/);
  await assert.rejects(readBoundedBody(Readable.from([]), { 'content-length': '101' }, 100), /body_limit/);
  await assert.rejects(readBoundedBody(Readable.from([]), { 'content-encoding': 'unsupported' }, 100), /unsupported_encoding/);
});

test('DNS deadline stops a slow resolution before any request', async () => {
  let requested = false;
  const fetcher = createDiscoveryFetcher({ timeoutMs: 100, resolver: () => new Promise(resolve => setTimeout(() => resolve(publicDNS()), 150)),
    request: () => { requested = true; throw new Error('unexpected'); } });
  await assert.rejects(fetcher('https://example.com/'), /timeout/);
  await new Promise(resolve => setTimeout(resolve, 75));
  assert.equal(requested, false);
});

test('request deadline destroys stalled response streams', async () => {
  let response;
  const fetcher = createDiscoveryFetcher({ timeoutMs: 100, resolver: publicDNS, request: (_url, _options, callback) => {
    const req = new EventEmitter();
    req.destroy = error => req.emit('error', error);
    req.end = () => {
      response = new Readable({ read() {} });
      response.statusCode = 200; response.headers = {};
      callback(response);
    };
    return req;
  } });
  await assert.rejects(fetcher('https://example.com/'), /timeout/);
  assert.equal(response.destroyed, true);
});

test('image sniffing rejects oversized PNG and active or externally referencing SVG', () => {
  assert.equal(imageType(png()), 'image/png');
  const oversized = png(); oversized.writeUInt32BE(4097, 16);
  assert.throws(() => imageType(oversized), /image_dimensions/);
  assert.equal(imageType(Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"/></svg>')), 'image/svg+xml');
  assert.equal(imageType(Buffer.from('<svg><path fill="url(\'#gradient\')"/><use href="#shape"/></svg>')), 'image/svg+xml');
  for (const content of ['<svg><script>alert(1)</script></svg>', '<svg><foreignObject/></svg>',
    '<svg><image href="https://example.com/a"/></svg>', '<svg><use href="data:image/svg+xml,anything"/></svg>',
    '<svg><style>@import "https://example.com/a"</style></svg>', '<svg onload="alert(1)"/>',
    '<svg><path fill="url(https://example.com/a)"/></svg>', '<html>not an image</html>']) assert.throws(() => imageType(Buffer.from(content)));
});

test('discovery caches pages/assets, records distinct failures, first usable icon and potential coverage', async () => {
  const entries = validateEntries({ entries: [
    { id: 'a', name: 'A', page_kind: 'signup', page_url: 'https://example.com/signup', existing_logo: true },
    { id: 'b', name: 'B', page_kind: 'documentation', page_url: 'https://example.com/signup', existing_logo: false },
    { id: 'c', name: 'C', page_kind: 'signup', page_url: 'https://other.example/', existing_logo: false },
    { id: 'd', name: 'D', page_kind: 'signup', page_url: 'https://failed.example/', existing_logo: false },
    { id: 'e', name: 'E', page_kind: 'none', existing_logo: false },
    { id: 'f', name: 'F', page_kind: 'signup', page_url: 'https://empty.example/', existing_logo: false },
    { id: 'g', name: 'G', page_kind: 'signup', page_url: 'https://example.com/?token=do-not-record', existing_logo: false },
  ] });
  const calls = new Map();
  let renders = 0;
  let saves = 0;
  const report = await discover(entries, { concurrency: 4, fetcher: async (url, { maxBytes }) => {
    calls.set(url, (calls.get(url) || 0) + 1);
    assert.ok([PAGE_LIMIT, ICON_LIMIT].includes(maxBytes));
    if (url.includes('failed.example')) return { status: 403, headers: {}, body: Buffer.alloc(0), url };
    if (url.endsWith('bad.png') || /\/(?:favicon\.(?:ico|png)|apple-touch-icon\.png)$/.test(url)) return { status: 404, headers: {}, body: Buffer.alloc(0), url };
    if (url.endsWith('good.png')) return { status: 200, headers: {}, body: png(), url };
    return { status: 200, headers: { 'content-type': 'text/html' }, url, body: Buffer.from(url.includes('empty.example') ? '' :
      '<link rel=icon href="https://cdn.example/bad.png"><link rel=apple-touch-icon href="https://cdn.example/good.png">') };
  }, render: async () => { renders++; return { png: png(), mime: 'image/png', width: 128, height: 128 }; }, savePNG: async () => { saves++; } });
  assert.deepEqual(report.results.map(r => r.status), ['collected', 'collected', 'collected', 'page_inaccessible', 'no_public_page', 'no_supported_icon', 'no_public_page']);
  assert.equal(report.summary.potential_coverage, 3);
  assert.equal(report.summary.discovered_without_existing_logo, 2);
  assert.ok([...calls.values()].every(count => count === 1));
  assert.equal(renders, 1); assert.equal(saves, 1);
  assert.equal(report.results[0].selected_kind, 'apple-touch-icon');
  assert.equal(report.results[0].attempts[0].reason, 'icon_http_404');
  assert.ok(!JSON.stringify(report).includes('do-not-record'));
});

test('blocked auth navigation recovers original-origin PNG without following auth or asset redirects', async () => {
  const seen = [];
  const fetcher = createDiscoveryFetcher({ resolver: publicDNS, request: transport({
    'https://example.com/signup': { status: 302, headers: { location: 'https://auth.example/login?token=never-follow' } },
    'https://example.com/favicon.ico': { status: 302, headers: { location: 'https://auth.example/favicon.ico' } },
    'https://example.com/favicon.png': { body: png() },
  }, seen) });
  const report = await discover([{ id: 'x', name: 'X', page_kind: 'signup', page_url: 'https://example.com/signup', existing_logo: false }], {
    fetcher, render: async bytes => ({ png: bytes, mime: 'image/png', width: 128, height: 128 }),
  });
  const row = report.results[0];
  assert.equal(row.status, 'collected');
  assert.equal(row.page_failure, 'blocked_private_query');
  assert.equal(row.final_page_url, 'https://example.com/signup');
  assert.equal(row.final_page_url_status, 'unvisited');
  assert.equal(row.page_status, undefined);
  assert.equal(row.selected_kind, 'root_fallback');
  assert.equal(row.source_url, 'https://example.com/favicon.png');
  assert.equal(row.attempts[0].reason, 'blocked_cross_origin_redirect');
  assert.deepEqual(seen.map(r => r.url), ['https://example.com/signup', 'https://example.com/favicon.ico', 'https://example.com/favicon.png']);
  assert.ok(!JSON.stringify(report).includes('never-follow'));
});

test('HTTP page failures retain provenance when Apple root fallback succeeds', async () => {
  const fetcher = createDiscoveryFetcher({ resolver: publicDNS, request: transport({
    'https://example.com/signup': { status: 403 },
    'https://example.com/favicon.ico': { status: 404 },
    'https://example.com/favicon.png': { status: 404 },
    'https://example.com/apple-touch-icon.png': { body: png() },
  }) });
  const report = await discover([{ id: 'x', name: 'X', page_kind: 'signup', page_url: 'https://example.com/signup', existing_logo: false }], {
    fetcher, render: async bytes => ({ png: bytes, mime: 'image/png', width: 128, height: 128 }),
  });
  const row = report.results[0];
  assert.equal(row.status, 'collected');
  assert.equal(row.page_failure, 'page_http_403');
  assert.equal(row.page_status, 403);
  assert.equal(row.final_page_url_status, 'unvisited');
  assert.equal(row.selected_kind, 'root_fallback');
  assert.equal(row.source_url, 'https://example.com/apple-touch-icon.png');
});

test('blocked original URLs never authorize root fallback requests', async () => {
  const report = await discover(['https://example.com/?token=private', 'https://127.0.0.1/signup'].map((page_url, i) => ({
    id: String(i), name: 'X', page_kind: 'signup', page_url, existing_logo: false,
  })), { fetcher: async () => assert.fail('must not fetch'), render: async () => assert.fail('must not render') });
  assert.deepEqual(report.results.map(row => row.status), ['no_public_page', 'no_public_page']);
  assert.equal(report.summary.unique_assets_attempted, 0);
});

test('offline reports escape untrusted labels, formula CSV and only embed hashed local PNG paths', () => {
  const report = { summary: { total: 1, collected: 0 }, results: [{ id: 'id', name: '=formula <script>boom</script>', status: 'no_public_page',
    file: 'https://remote.example/image.png', source_url: 'https://example.com/?v=a&size=64' }] };
  const output = reports(report);
  assert.match(output.csv, /"'=formula/);
  assert.ok(!output.html.includes('<script>'));
  assert.ok(!output.html.includes('<img'));
  assert.match(output.html, /default-src 'none'/);
  assert.match(output.html, /&lt;script&gt;/);
});

test('CLI bounds and input validation', () => {
  assert.equal(parseArgs(['--input', 'entries.json', '--output', 'out', '--concurrency', '8', '--timeout-ms', '5000']).concurrency, 8);
  for (const args of [[], ['--input', 'x'], ['--input', 'x', '--output', 'out', '--concurrency', '9'],
    ['--input', 'x', '--output', 'out', '--timeout-ms', '30001'], ['--input', 'x', '--output', 'out', '--playwright-module', './relative.mjs']]) assert.throws(() => parseArgs(args));
  assert.throws(() => validateEntries({ entries: [{ id: 'x', name: 'X', page_kind: 'unknown', existing_logo: false }] }));
});

// Optional real browser verification uses only synthesized in-memory images, never HTTP.
test('Chromium renders supported formats, preserves aspect, rejects transparent and 1x1 images', { skip: !process.env.FAVICON_TEST_PLAYWRIGHT_MODULE }, async () => {
  const playwright = await import(pathToFileURL(process.env.FAVICON_TEST_PLAYWRIGHT_MODULE).href);
  const browser = await playwright.chromium.launch({ headless: true });
  try {
    const result = await renderIcon(browser, Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="80" height="40"><rect width="80" height="40" fill="red"/></svg>'));
    assert.equal(validatePNG(result.png).width, 128);
    assert.equal(result.width, 80); assert.equal(result.height, 40);
    await assert.rejects(renderIcon(browser, png(1, 1)), /image_dimensions/);
    await assert.rejects(renderIcon(browser, png(16, 16, 0)), /transparent_image/);
    const page = await browser.newPage();
    const samples = await page.evaluate(() => {
      const c = document.createElement('canvas'); c.width = 32; c.height = 16;
      c.getContext('2d').fillRect(0, 0, 32, 16);
      return ['image/png', 'image/jpeg', 'image/webp'].map(type => c.toDataURL(type).split(',')[1]);
    });
    for (const sample of samples) assert.equal(validatePNG((await renderIcon(browser, Buffer.from(sample, 'base64'))).png).height, 128);
    const image = png(32, 32);
    const directory = Buffer.alloc(22); directory.writeUInt16LE(1, 2); directory.writeUInt16LE(1, 4);
    directory[6] = directory[7] = 32; directory.writeUInt16LE(1, 10); directory.writeUInt16LE(32, 12);
    directory.writeUInt32LE(image.length, 14); directory.writeUInt32LE(22, 18);
    assert.equal(validatePNG((await renderIcon(browser, Buffer.concat([directory, image]))).png).width, 128);
    // A classic BMP-backed ICO exercises the format excluded by the production collector.
    const dib = Buffer.alloc(40 + 32 * 32 * 4 + 32 * 4);
    dib.writeUInt32LE(40); dib.writeInt32LE(32, 4); dib.writeInt32LE(64, 8);
    dib.writeUInt16LE(1, 12); dib.writeUInt16LE(32, 14);
    for (let offset = 40; offset < 40 + 32 * 32 * 4; offset += 4) { dib[offset + 2] = 255; dib[offset + 3] = 255; }
    directory.writeUInt32LE(dib.length, 14);
    assert.equal(validatePNG((await renderIcon(browser, Buffer.concat([directory, dib]))).png).width, 128);
    const pixels = await page.evaluate(async data => {
      const img = new Image(); img.src = `data:image/png;base64,${data}`; await img.decode();
      const c = document.createElement('canvas'); c.width = c.height = 128;
      const ctx = c.getContext('2d'); ctx.drawImage(img, 0, 0);
      return [ctx.getImageData(64, 0, 1, 1).data[3], ctx.getImageData(64, 64, 1, 1).data[3]];
    }, result.png.toString('base64'));
    assert.deepEqual(pixels, [0, 255]);
  } finally { await browser.close(); }
});
