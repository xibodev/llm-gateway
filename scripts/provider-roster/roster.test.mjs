import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateSync } from 'node:zlib';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import { SOURCES, extractSource, classifyAuth, canonicalURL, stableID, normalizeEntry,
  mergeEntries, carryState, isPublicAddress, publicURL, createSafeFetcher, Scheduler, probeEndpoint,
  collectLogos, validatePNG, validatePayload, buildRoster, loadSnapshot, sha256, MAX_ENTRIES } from './index.mjs';
import { crc32, extractLogoPNG, validateLogo } from './logos.mjs';
import { LOGO_SOURCES } from './catalog.mjs';
import { main } from './build.mjs';

const fixtures = fileURLToPath(new URL('./fixtures/', import.meta.url));
const now = '2026-01-01T00:00:00Z';
const offline = options => buildRoster({ fixtures, offline: true, now, ...options });
const provenance = { repo: SOURCES[0].repo, commit: '1'.repeat(40), url: 'https://github.com/mnfst/awesome-free-llm-apis' };
const row = options => normalizeEntry({ name: 'Example', base_url: 'https://api.groq.com/openai/v1',
  auth: 'unknown', offer: 'free_tier', endpointEvidence: true, ...options }, provenance);

function png(width = 1) {
  const chunk = (type, data) => {
    const buffer = Buffer.alloc(data.length + 12);
    buffer.writeUInt32BE(data.length);
    buffer.write(type, 4);
    data.copy(buffer, 8);
    buffer.writeUInt32BE(crc32(buffer.subarray(4, -4)), buffer.length - 4);
    return buffer;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width);
  header.writeUInt32BE(1, 4);
  header[8] = 8;
  header[9] = 6;
  return Buffer.concat([Buffer.from('89504e470d0a1a0a', 'hex'), chunk('IHDR', header),
    chunk('IDAT', deflateSync(Buffer.alloc(1 + width * 4))), chunk('IEND', Buffer.alloc(0))]);
}

function ico(images = [{ bytes: png(), width: 1, height: 1 }]) {
  const directory = Buffer.alloc(6 + 16 * images.length);
  directory.writeUInt16LE(1, 2);
  directory.writeUInt16LE(images.length, 4);
  let start = directory.length;
  for (const [i, image] of images.entries()) {
    const offset = 6 + i * 16;
    directory[offset] = image.width === 256 ? 0 : image.width;
    directory[offset + 1] = image.height === 256 ? 0 : image.height;
    directory.writeUInt16LE(1, offset + 4);
    directory.writeUInt16LE(32, offset + 6);
    directory.writeUInt32LE(image.bytes.length, offset + 8);
    directory.writeUInt32LE(start, offset + 12);
    start += image.bytes.length;
  }
  return Buffer.concat([directory, ...images.map(image => image.bytes)]);
}

test('four actual source structures parse synthetic snapshots and preserve provenance', async () => {
  const { payload, report } = await offline({ fetcher: () => assert.fail('offline network') });
  assert.equal(payload.schema_version, 1);
  assert.equal(payload.sources.length, 4);
  assert.ok(payload.sources.every(source => source.status === 'ok'));
  assert.equal(report.sources[2].skipped[0].name, 'Cline');
  const groq = payload.entries.find(entry => entry.base_url === 'https://api.groq.com/openai/v1');
  assert.equal(groq.sources.length, 3);
  assert.equal(groq.setup, 'compatible');
  assert.equal(groq.auth, 'api_key');
  assert.ok(groq.sources.every(source => source.url.includes(source.commit) && source.license));
  const prose = payload.entries.find(entry => entry.name === 'Example Proxy');
  assert.equal(prose.base_url, 'https://proxy.example.com/register');
  assert.equal(prose.protocol, 'unknown');
  assert.equal(prose.offer, 'unknown');
  assert.ok(prose.conflicts.includes('offer: free_tier | trial'));
  assert.equal(payload.entries.find(entry => entry.name === 'Native API').setup, 'candidate');
  assert.ok(!payload.entries.some(entry => /Local|Featured|Cline/.test(entry.name)));
  assert.ok(!payload.entries.find(entry => entry.name === 'Example Audio').offer_expires_at);
  assert.deepEqual(payload, (await offline()).payload);
  validatePayload(JSON.parse(JSON.stringify(payload)));
});

test('unknown auth never means no key; no card and key links do not prove anonymous access', () => {
  assert.equal(classifyAuth('No credit card, registration optional'), 'unknown');
  assert.equal(classifyAuth('No API key required'), 'none');
  assert.equal(classifyAuth('API key required'), 'api_key');
  assert.equal(classifyAuth('No API key required. Token required.'), 'unknown');
  const unknown = row({ base_url: 'https://api.example.com/v1', auth: 'none' });
  assert.equal(unknown.setup, 'candidate');
  assert.equal(unknown.protocol, 'unknown');
  assert.equal(row({ base_url: 'https://console.groq.com/keys', endpointEvidence: false }).setup, 'candidate');
});

test('canonical identity preserves protocol, region, gateway and meaningful paths', () => {
  assert.equal(canonicalURL('https://API.EXAMPLE.com:443/v1/'), 'https://api.example.com/v1');
  assert.equal(stableID('openai', 'https://api.example.com/v1/'), stableID('openai', 'https://API.example.com/v1'));
  for (const [protocol, url] of [['anthropic', 'https://api.example.com/v1'], ['openai', 'https://api.example.com/V1'],
    ['openai', 'https://eu.example.com/v1'], ['openai', 'https://api.example.com/v1/openai']]) {
    assert.notEqual(stableID(protocol, url), stableID('openai', 'https://api.example.com/v1'));
  }
  assert.throws(() => canonicalURL('https://user:secret@example.com/v1'));
  assert.throws(() => canonicalURL('https://api.example.com/v1?api_key=secret'));
});

test('merge order is deterministic and contradictions stay conservative', () => {
  const a = row({ auth: 'none', offer: 'free_tier' });
  const b = row({ auth: 'api_key', offer: 'trial', name: 'Alias' });
  assert.deepEqual(mergeEntries([a, b]), mergeEntries([b, a]));
  const [merged] = mergeEntries([a, b]);
  assert.equal(merged.auth, 'unknown');
  assert.equal(merged.offer, 'unknown');
  assert.equal(merged.setup, 'candidate');
  assert.ok(merged.conflicts.includes('auth: api_key | none'));
  assert.equal(mergeEntries([merged, a])[0].auth, 'unknown');
  assert.equal(mergeEntries([merged, a])[0].offer, 'unknown');
});

test('schema drift, malformed rows, empty sources and unknown extractors fail closed', async () => {
  assert.throws(() => extractSource(SOURCES[0].repo, '{"providers":[]}'));
  assert.throws(() => extractSource(SOURCES[0].repo, '{"providers":[{}]}'));
  assert.throws(() => extractSource('example/other', 'anything'));
  const text = await readFile(join(fixtures, 'nejib.md'), 'utf8');
  assert.throws(() => extractSource(SOURCES[1].repo, text.replace('<!--TABLE:QUICKREF:END-->', '')));
  assert.throws(() => extractSource(SOURCES[1].repo, text.replace('Base URL', 'Endpoint')));
  assert.throws(() => extractSource(SOURCES[1].repo, text.replace('| No | Free tier |', '| No |')));
});

test('partial failure retains old provenance; total failure cannot publish an empty roster', async () => {
  const { payload: previous } = await offline();
  const loader = async (source, fetcher, options) => {
    if (source === SOURCES[0]) throw new Error('failed');
    return loadSnapshot(source, fetcher, options);
  };
  const partial = await offline({ previous, snapshotLoader: loader });
  assert.equal(partial.payload.sources[0].status, 'stale');
  assert.equal(partial.report.sources[0].retained, 4);
  assert.ok(partial.payload.entries.some(entry => entry.name === 'Example API' && entry.state === 'active'));
  const fail = () => { throw new Error('failed'); };
  const full = await offline({ previous, snapshotLoader: fail });
  assert.equal(full.report.all_sources_stale, true);
  assert.equal(full.payload.entries.length, previous.entries.length);
  assert.deepEqual(full.payload.entries, previous.entries);
  await assert.rejects(offline({ snapshotLoader: fail }), /refusing_empty/);
});

test('quarantine, report, withdrawal, expiry and disappearance tombstones survive refresh', async () => {
  const { payload: previous } = await offline();
  const [quarantine, withdrawn, expired, confirmed] = previous.entries;
  quarantine.state = 'quarantined';
  quarantine.report = { issue_url: 'https://github.com/example/reports/issues/1', confirmations: 2, reason: 'incident' };
  withdrawn.state = 'withdrawn';
  expired.offer_expires_at = '2025-01-01T00:00:00Z';
  confirmed.report.confirmations = 10;
  const result = await offline({ previous });
  const entries = new Map(result.payload.entries.map(entry => [entry.id, entry]));
  assert.equal(entries.get(quarantine.id).state, 'quarantined');
  assert.deepEqual(entries.get(quarantine.id).report, quarantine.report);
  assert.equal(entries.get(withdrawn.id).state, 'withdrawn');
  assert.equal(entries.get(expired.id).state, 'active');
  assert.equal(entries.get(expired.id).offer, 'unknown');
  assert.ok(entries.get(expired.id).requirements.some(value => value.includes('offer has expired')));
  assert.equal(entries.get(expired.id).offer_expires_at, expired.offer_expires_at);
  assert.equal(entries.get(confirmed.id).state, 'quarantined');
  const extra = row({ base_url: 'https://removed.example.com/v1' });
  previous.entries.push(extra);
  const removed = await offline({ previous });
  assert.equal(removed.payload.entries.find(entry => entry.id === extra.id).state, 'withdrawn');
});

test('public address policy blocks IPv4/IPv6 special ranges and URL tricks', () => {
  for (const address of ['0.1.2.3', '10.1.2.3', '100.64.1.2', '127.0.0.1', '169.254.169.254', '172.31.2.3',
    '192.168.1.2', '192.0.0.8', '192.0.2.1', '198.18.0.1', '198.51.100.2', '203.0.113.3', '224.0.0.1', '255.255.255.255',
    '::', '::1', '::ffff:8.8.8.8', '::ffff:0808:0808', '64:ff9b::808:808', 'fc00::1', 'fe80::1', 'ff02::1',
    '2001:db8::1', '2001::1', '2002:0808:0808::1', '3fff::1']) assert.equal(isPublicAddress(address), false, address);
  for (const address of ['8.8.8.8', '1.1.1.1', '2606:4700:4700::1111']) assert.equal(isPublicAddress(address), true, address);
  for (const url of ['http://example.com', 'https://user:pass@example.com', 'https://127.1', 'https://0x7f000001',
    'https://2130706433', 'https://[::1]', 'https://example.com:444', 'https://localhost', 'https://foo.local',
    'https://example.com/?token=secret', 'https://example.com/#fragment']) assert.throws(() => publicURL(url), url);
});

test('UTF-8 producer limits preserve codepoints and the shared fixture exercises long CJK names', async () => {
  const { payload } = await offline();
  const cjk = payload.entries.find(entry => entry.base_url === 'https://cjk.example.com/v1');
  assert.ok(cjk);
  const fixture = JSON.parse(await readFile(join(fixtures, 'mnfst.json'), 'utf8'));
  const raw = fixture.providers.find(entry => entry.baseUrl === cjk.base_url).name;
  assert.ok(Buffer.byteLength(raw) > 256);
  assert.equal(cjk.name, [...raw].slice(0, 85).join(''));
  assert.equal(Buffer.byteLength(cjk.name), 255);
  assert.equal(row({ name: 'a'.repeat(253) + '😀' }).name, 'a'.repeat(253));
  assert.equal(row({ name: '😀'.repeat(100) }).name, '😀'.repeat(64));
  assert.ok(row({ name: 'a'.repeat(254) + '😀' }).name.isWellFormed());
  for (const [field, value] of [['name', '界'.repeat(86)], ['name', 'a'.repeat(257)],
    ['name', '\ud800'], ['id', 'a'.repeat(129)], ['description', '界'.repeat(2731)],
    ['description', 'a'.repeat(8193)]]) {
    const invalid = structuredClone(payload);
    invalid.entries[0][field] = value;
    assert.throws(() => validatePayload(invalid), /invalid_payload_contract/, field);
  }
  const valid = structuredClone(payload);
  valid.entries[0].name = '😀'.repeat(64);
  valid.entries[0].description = '😀'.repeat(2048);
  validatePayload(valid);
  for (const set of [entry => { entry.signup_url = 'https://example.com/' + 'x'.repeat(4096); },
    entry => { entry.sources[0].license_url = 'https://example.com/' + 'x'.repeat(4096); },
    entry => { entry.report.issue_url = 'https://example.com/' + 'x'.repeat(4096); },
    entry => { entry.docs_url = 'https://example.com/' + 'x'.repeat(4096); }]) {
    const invalid = structuredClone(payload);
    set(invalid.entries[0]);
    assert.throws(() => validatePayload(invalid), /invalid_payload_url/);
  }
  assert.throws(() => publicURL('https://example.com/' + 'x'.repeat(4096)), /blocked_url/);
});

test('source withdrawal overrides quarantine and survives incident reset and source return', async () => {
  const { payload: previous } = await offline();
  const missing = row({ base_url: 'https://removed.example.com/v1' });
  missing.state = 'quarantined';
  missing.report = { issue_url: 'https://github.com/example/reports/issues/1', confirmations: 10, reason: 'incident' };
  previous.entries.push(missing);
  const { payload: removed } = await offline({ previous });
  const tombstone = removed.entries.find(entry => entry.id === missing.id);
  assert.equal(tombstone.state, 'withdrawn');
  assert.deepEqual(tombstone.report, missing.report);
  // Community restoration clears the incident, not an independent source withdrawal.
  tombstone.report = { issue_url: '', confirmations: 0, reason: '' };
  const { payload: reset } = await offline({ previous: removed });
  assert.equal(reset.entries.find(entry => entry.id === missing.id).state, 'withdrawn');
  const returned = carryState([row({ base_url: missing.base_url })], reset, now);
  assert.equal(returned.find(entry => entry.id === missing.id).state, 'withdrawn');
  assert.equal(returned.find(entry => entry.id === missing.id).report.confirmations, 0);
});

function transport({ status = 200, body = Buffer.from('ok'), headers = {}, inspect = () => {} } = {}) {
  return (url, options, callback) => {
    inspect(url, options);
    const req = new EventEmitter();
    req.destroy = error => req.emit('error', error);
    req.end = () => queueMicrotask(() => {
      const response = new PassThrough();
      response.statusCode = status;
      response.headers = headers;
      callback(response);
      response.end(body);
    });
    return req;
  };
}

test('DNS is resolved once, every answer checked, connection pinned and TLS checks hostname', async () => {
  let lookups = 0;
  let requests = 0;
  const fetcher = createSafeFetcher({ resolver: async () => {
    lookups++;
    return [{ address: '8.8.8.8', family: 4 }];
  }, request: transport({ inspect: (url, options) => {
    requests++;
    assert.equal(url.hostname, 'public.example.com');
    assert.equal(options.servername, 'public.example.com');
    assert.equal(options.rejectUnauthorized, true);
    assert.equal(options.agent, false);
    assert.equal(options.method, 'GET');
    assert.equal(options.headers.Authorization, undefined);
    options.lookup(url.hostname, {}, (error, address, family) => {
      assert.equal(error, null); assert.equal(address, '8.8.8.8'); assert.equal(family, 4);
    });
    options.lookup(url.hostname, { all: true }, (_, records) => assert.equal(records[0].address, '8.8.8.8'));
  } }) });
  assert.equal((await fetcher('https://public.example.com')).status, 200);
  assert.equal(lookups, 1);
  assert.equal(requests, 1);
  const blocked = createSafeFetcher({ resolver: async () => [{ address: '8.8.8.8', family: 4 }, { address: '127.0.0.1', family: 4 }],
    request: () => assert.fail('must not connect') });
  await assert.rejects(blocked('https://mixed.example.com'), /blocked_address/);
});

test('safe fetch rejects redirects, compressed bodies, oversized bodies and stalled DNS', async () => {
  for (const [config, expected] of [[{ status: 302, headers: { location: 'https://127.0.0.1' } }, /blocked_redirect/],
    [{ headers: { 'content-encoding': 'gzip' } }, /blocked_encoding/],
    [{ headers: { 'content-length': '200' } }, /body_limit/], [{ body: Buffer.alloc(101) }, /body_limit/]]) {
    const fetcher = createSafeFetcher({ resolver: async () => [{ address: '8.8.8.8', family: 4 }], request: transport(config) });
    await assert.rejects(fetcher('https://public.example.com', { maxBytes: 100 }), expected);
  }
  let resume;
  const fetcher = createSafeFetcher({ resolver: () => new Promise(resolve => { resume = resolve; }), request: () => assert.fail('late DNS connected') });
  await assert.rejects(fetcher('https://slow.example.com', { timeoutMs: 10 }), /timeout/);
  resume([{ address: '8.8.8.8', family: 4 }]);
  await new Promise(resolve => setImmediate(resolve));
});

test('total deadline destroys a stalled request, including a response that never ends', async () => {
  let destroyed = 0;
  const fetcher = createSafeFetcher({ resolver: async () => [{ address: '8.8.8.8', family: 4 }],
    request: (_url, _options, callback) => {
      const request = new EventEmitter();
      const response = new PassThrough();
      response.statusCode = 200;
      response.headers = {};
      request.end = () => { callback(response); response.write('partial'); };
      request.destroy = error => { destroyed++; response.destroy(); request.emit('error', error); };
      return request;
    } });
  await assert.rejects(fetcher('https://stall.example.com', { timeoutMs: 10 }), /timeout/);
  assert.equal(destroyed, 1);
});

test('scheduler limits to eight global and two per host without head-of-line starvation', async () => {
  const scheduler = new Scheduler();
  let active = 0;
  let peak = 0;
  const hosts = new Map();
  const tasks = Array.from({ length: 32 }, (_, i) => i < 12 ? 'busy' : `host${i % 5}`);
  await Promise.all(tasks.map(host => scheduler.run(host, async () => {
    active++;
    peak = Math.max(peak, active);
    hosts.set(host, (hosts.get(host) || 0) + 1);
    assert.ok(active <= 8);
    assert.ok(hosts.get(host) <= 2);
    await new Promise(resolve => setTimeout(resolve, 2));
    active--;
    hosts.set(host, hosts.get(host) - 1);
  })));
  assert.equal(peak, 8);
});

test('probes measure HTTP only, never mutate auth/offer or request inference', async () => {
  for (const [status, expected] of [[200, 'reachable'], [401, 'auth_required'], [403, 'auth_required'],
    [429, 'rate_limited'], [404, 'failed'], [500, 'failed']]) {
    const entry = row();
    const before = structuredClone(entry);
    const probe = await probeEndpoint(entry, async url => { assert.equal(url, entry.base_url); return { status }; }, now);
    assert.equal(probe.status, expected);
    assert.deepEqual(entry, before);
  }
  assert.equal((await probeEndpoint(row({ endpointEvidence: false }), () => assert.fail('homepage probe'), now)).status, 'not_checked');
});

test('PNG validation checks signature, CRC, size, dimensions, compressed data and no trailing/SVG content', () => {
  validatePNG(png());
  const corrupt = png();
  corrupt[40] ^= 1;
  for (const bytes of [Buffer.from('<svg/>'), Buffer.from('<html/>'), Buffer.alloc(65537), corrupt,
    Buffer.concat([png(), Buffer.from('<script/>')]), png(513), png().subarray(0, 45)]) assert.throws(() => validatePNG(bytes));
});

test('ICO extraction selects the largest validated PNG without converting non-PNG frames', () => {
  const bytes = png(256);
  const images = [{ bytes: png(), width: 1, height: 1 },
    { bytes: Buffer.alloc(40), width: 32, height: 32 }, { bytes, width: 256, height: 1 }];
  assert.deepEqual(extractLogoPNG(ico(images)), bytes);
  assert.deepEqual(extractLogoPNG(ico([...images].reverse())), bytes);
  assert.deepEqual(extractLogoPNG(bytes), bytes);
  for (const other of [Buffer.alloc(40), Buffer.from('<svg><script/></svg>'), Buffer.from('<html/>')]) {
    assert.throws(() => extractLogoPNG(ico([{ bytes: other, width: 1, height: 1 }])), /ico_png_missing/);
    assert.throws(() => extractLogoPNG(other));
  }
});

test('ICO validation rejects malformed headers, directory entries, bounds, overlaps and hidden tails', () => {
  const changes = [
    b => b.writeUInt16LE(1, 0), b => b.writeUInt16LE(2, 2), // CUR is not ICO.
    b => b.writeUInt16LE(0, 4), b => b.writeUInt16LE(65, 4),
    b => b.writeUInt16LE(2, 4), b => { b[9] = 1; },
    b => b.writeUInt16LE(2, 10), b => b.writeUInt16LE(7, 12),
    b => b.writeUInt32LE(0, 14), b => b.writeUInt32LE(0xffffffff, 14),
    b => b.writeUInt32LE(6, 18), b => b.writeUInt32LE(0xffffffff, 18),
    b => { b[6] = 2; }, b => { b[7] = 0; },
  ];
  for (const change of changes) { const bytes = ico(); change(bytes); assert.throws(() => extractLogoPNG(bytes)); }
  for (let length = 0; length < 22; length++) assert.throws(() => extractLogoPNG(ico().subarray(0, length)));
  assert.throws(() => extractLogoPNG(ico().subarray(0, -1)));
  assert.throws(() => extractLogoPNG(Buffer.concat([ico(), Buffer.from('<svg/>')])));
  assert.throws(() => extractLogoPNG(Buffer.alloc(65537)));
  const pair = ico([{ bytes: png(), width: 1, height: 1 }, { bytes: png(), width: 1, height: 1 }]);
  pair.writeUInt32LE(pair.readUInt32LE(18), 34);
  assert.throws(() => extractLogoPNG(pair));
  // A skipped DIB frame cannot smuggle an out-of-bounds directory entry past a valid PNG.
  const skipped = ico([{ bytes: png(), width: 1, height: 1 }, { bytes: Buffer.alloc(40), width: 1, height: 1 }]);
  skipped.writeUInt32LE(0xffffffff, 34);
  assert.throws(() => extractLogoPNG(skipped));
});

test('embedded PNG must satisfy the same CRC, dimension and decompression checks as standalone PNG', () => {
  const corrupt = png(); corrupt[40] ^= 1;
  // Valid chunk CRCs do not authorize illegal filters, broken zlib or a decompression bomb.
  const withIDAT = data => {
    const chunk = Buffer.alloc(data.length + 12);
    chunk.writeUInt32BE(data.length); chunk.write('IDAT', 4); data.copy(chunk, 8);
    chunk.writeUInt32BE(crc32(chunk.subarray(4, -4)), chunk.length - 4);
    return Buffer.concat([png().subarray(0, 33), chunk, png().subarray(-12)]);
  };
  const filterPNG = withIDAT(deflateSync(Buffer.from([5, 0, 0, 0, 0])));
  const bomb = withIDAT(deflateSync(Buffer.alloc(1024 * 1024)));
  const brokenZlib = withIDAT(Buffer.from('not zlib'));
  for (const bytes of [corrupt, filterPNG, bomb, brokenZlib, png(513), png().subarray(0, -1), Buffer.concat([png(), Buffer.from('<svg/>')])]) {
    assert.throws(() => extractLogoPNG(ico([{ bytes, width: 1, height: 1 }])));
  }
  // Validate all PNG members; do not silently choose a good member beside a corrupt one.
  assert.throws(() => extractLogoPNG(ico([{ bytes: png(32), width: 32, height: 1 }, { bytes: corrupt, width: 1, height: 1 }])));
});

test('favicon ICO embeds only PNG bytes with original URL and rights; invalid ICO preserves prior logo', async () => {
  const entries = [row({ base_url: 'https://api.example.com/v1', signup_url: 'https://huggingface.co/join' }),
    row({ base_url: 'https://api.example.com/v2', signup_url: 'https://huggingface.co/settings/tokens' })];
  const calls = [];
  const bytes = png(32);
  await collectLogos(entries, async (url, options) => {
    calls.push(url);
    assert.deepEqual(options, { maxBytes: 65536, timeoutMs: 6000 });
    return { status: 200, body: ico([{ bytes, width: 32, height: 1 }]) };
  });
  assert.deepEqual(calls, ['https://huggingface.co/favicon.ico']);
  for (const entry of entries) {
    assert.deepEqual(Buffer.from(entry.logo.data, 'base64'), bytes);
    assert.equal(entry.logo.sha256, sha256(bytes));
    assert.equal(entry.logo.source_url, calls[0]);
    assert.equal(entry.logo.license, 'unknown');
    validateLogo(entry.logo);
  }
  const previous = structuredClone(entries[0].logo);
  await collectLogos(entries, async () => ({ status: 200, body: ico().subarray(0, -1) }));
  assert.deepEqual(entries[0].logo, previous);
});

test('reviewed raster mappings cover exact API and site aliases, never names or host suffixes', async () => {
  const hosts = new Set();
  for (const asset of LOGO_SOURCES) {
    const entries = asset.hosts.map(host => {
      assert.ok(!hosts.has(host), `duplicate mapping: ${host}`); hosts.add(host);
      return row({ name: 'Unrelated display name', base_url: `https://${host}/v1` });
    });
    const calls = [];
    await collectLogos(entries, async url => { calls.push(url); return { status: 200, body: png() }; }, { favicon: false });
    assert.deepEqual(calls, [asset.url]);
    assert.ok(entries.every(entry => entry.logo?.source_url === asset.url && entry.logo.license === asset.license));
    const unapproved = [row({ name: 'Groq', base_url: `https://${asset.hosts[0]}.example.com/v1` }),
      row({ name: 'Cohere', base_url: `https://unreviewed.${asset.hosts[0]}/v1` })];
    await collectLogos(unapproved, () => assert.fail('hostname or name heuristic'), { favicon: false });
    assert.ok(unapproved.every(entry => !entry.logo));
  }
});

test('automatic logos download approved raster, cache requests, hash bytes and preserve fallback', async () => {
  const entries = [row(), row({ name: 'Alias' }), row({ base_url: 'https://new.example.com/v1' })];
  let requests = 0;
  const bytes = png();
  const diagnostics = await collectLogos(entries, async url => {
    requests++;
    assert.equal(url, 'https://groq.com/apple-touch-icon.png');
    return { status: 200, body: bytes };
  });
  assert.equal(requests, 1);
  assert.equal(entries[0].logo.sha256, sha256(bytes));
  assert.equal(entries[0].logo.mime, 'image/png');
  assert.ok(entries[0].logo.license);
  assert.equal(diagnostics.find(item => item.id === entries[2].id).status, 'initials');
  const previousLogo = structuredClone(entries[0].logo);
  await collectLogos(entries, async () => ({ status: 200, body: Buffer.from('<svg/>') }));
  assert.deepEqual(entries[0].logo, previousLogo);
  entries[2].signup_url = 'https://huggingface.co/settings/tokens';
  await collectLogos([entries[2]], async url => {
    assert.equal(url, 'https://huggingface.co/favicon.ico');
    return { status: 200, body: bytes };
  }, { favicon: true });
  assert.equal(entries[2].logo.mime, 'image/png');
  assert.equal(entries[2].logo.license, 'unknown');
});

test('automatic favicon discovery is approved-origin-only, cached per host and falls back to PNG', async () => {
  const entries = [row({ base_url: 'https://api.example.com/v1', signup_url: 'https://huggingface.co/settings/tokens' }),
    row({ base_url: 'https://api.example.com/v2', signup_url: 'https://huggingface.co/join' })];
  const urls = [];
  await collectLogos(entries, async (url, options) => {
    urls.push(url);
    assert.equal(options.maxBytes, 65536);
    assert.equal(options.timeoutMs, 6000);
    return { status: 200, body: url.endsWith('.ico') ? Buffer.from('<svg/>') : png() };
  });
  assert.deepEqual(urls, ['https://huggingface.co/favicon.ico', 'https://huggingface.co/favicon.png']);
  assert.ok(entries.every(entry => entry.logo.license === 'unknown' && entry.logo.source_url === urls[1]));
  for (const signup of ['', 'https://community.example.com', 'https://huggingface.co.evil.example.com', 'https://huggingface.co:444', 'https://user:secret@huggingface.co']) {
    const entry = row({ base_url: 'https://api.example.com/v1' });
    entry.signup_url = signup;
    await collectLogos([entry], () => assert.fail('unapproved signup origin'));
    assert.equal(entry.logo, undefined);
  }
});

test('favicon request budget is deterministic, bounded and reports exhaustion', async () => {
  const entries = [row({ base_url: 'https://api.example.com/v1', signup_url: 'https://huggingface.co/join' }),
    row({ base_url: 'https://api.example.com/v2', signup_url: 'https://console.mistral.ai/keys' })];
  async function run(rows) {
    const urls = [];
    const diagnostics = await collectLogos(rows, async url => { urls.push(url); return { status: 404, body: Buffer.alloc(0) }; }, { maxRequests: 2 });
    assert.equal(urls.length, 2);
    assert.equal(new Set(urls.map(url => new URL(url).host)).size, 1);
    assert.ok(diagnostics.some(item => item.reason === 'logo_budget_exhausted'));
    return urls;
  }
  assert.deepEqual(await run(entries), await run([...entries].reverse()));
  await collectLogos(entries, () => assert.fail('offline network'), { offline: true });
  await collectLogos(entries, () => assert.fail('disabled discovery'), { favicon: false });
});

test('expired offers leave incident states intact through later refreshes and outages', async () => {
  const { payload: previous } = await offline();
  for (const [i, state] of ['active', 'quarantined', 'withdrawn'].entries()) {
    previous.entries[i].state = state;
    previous.entries[i].offer = 'free_tier';
    previous.entries[i].offer_expires_at = '2025-01-01T00:00:00Z';
  }
  for (const options of [{}, { snapshotLoader: () => { throw new Error('outage'); } }]) {
    const { payload } = await offline({ previous, ...options });
    for (const [i, state] of ['active', 'quarantined', 'withdrawn'].entries()) {
      assert.equal(payload.entries[i].state, state);
      assert.equal(payload.entries[i].offer, 'unknown');
    }
    if (options.snapshotLoader) assert.ok(payload.sources.every(source => source.status === 'stale'));
  }
});

test('collector accepts 5000 entries and rejects 5001, matching the Go contract', () => {
  assert.equal(MAX_ENTRIES, 5000);
  const entries = Array.from({ length: MAX_ENTRIES }, (_, i) => row({ base_url: `https://api.example.com/v${i}` }));
  const payload = { schema_version: 1, revision: 1, published_at: now, entries,
    sources: [{ ...provenance, status: 'ok' }] };
  validatePayload(payload);
  payload.entries.push(row({ base_url: 'https://api.example.com/extra' }));
  assert.throws(() => validatePayload(payload), /invalid_payload_contract/);
});

test('snapshot fetch pins content to the resolved commit and uses no credentials', async () => {
  const urls = [];
  const commit = 'a'.repeat(40);
  const snapshot = await loadSnapshot(SOURCES[0], async url => {
    urls.push(url);
    return { status: 200, body: Buffer.from(urls.length === 1 ? JSON.stringify({ sha: commit }) : '{}') };
  });
  assert.equal(snapshot.commit, commit);
  assert.equal(urls[1], `https://raw.githubusercontent.com/${SOURCES[0].repo}/${commit}/data.json`);
});

test('no-probe still collects logos; output validation rejects tampering and invalid vocabularies', async () => {
  let downloads = 0;
  const { payload } = await buildRoster({ fixtures, now, noProbe: true, fetcher: async url => {
    assert.equal(url, 'https://groq.com/apple-touch-icon.png');
    downloads++;
    return { status: 200, body: png() };
  } });
  assert.equal(downloads, 1);
  assert.ok(payload.entries.every(entry => entry.probe.status === 'not_checked'));
  const entry = payload.entries.find(entry => entry.logo?.data);
  assert.ok(entry.sources.some(source => source.notice.includes('MIT License')));
  for (const mutate of [copy => { copy.entries[0].protocol = 'oauth'; },
    copy => { copy.entries[0].id = 'changed'; },
    copy => { copy.entries[0].offer_expires_at = '30 days'; },
    copy => { copy.entries.find(row => row.logo?.data).logo.sha256 = '0'.repeat(64); }]) {
    const copy = structuredClone(payload);
    mutate(copy);
    assert.throws(() => validatePayload(copy));
  }
  const retained = await offline({ previous: payload });
  assert.deepEqual(retained.payload.entries.find(row => row.id === entry.id).logo, entry.logo);
});

test('CLI writes contract payload/report and failed refresh leaves existing payload intact', async () => {
  const output = await mkdtemp(join(tmpdir(), 'provider-roster-'));
  try {
    await main(['--output', output, '--fixtures', fixtures, '--offline', '--no-probe']);
    const bytes = await readFile(join(output, 'payload.json'), 'utf8');
    validatePayload(JSON.parse(bytes));
    assert.ok(JSON.parse(await readFile(join(output, 'report.json'), 'utf8')).publishable);
    await assert.rejects(main(['--output', output, '--offline']), /refusing_empty/);
    assert.equal(await readFile(join(output, 'payload.json'), 'utf8'), bytes);
    assert.equal(JSON.parse(await readFile(join(output, 'report.json'), 'utf8')).publishable, false);
  } finally { await rm(output, { recursive: true, force: true }); }
});
