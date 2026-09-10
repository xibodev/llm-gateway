import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { deflateSync } from 'node:zlib';
import { crc32 } from '../provider-roster/logos.mjs';
import { GitHub, fetchReports, issueEntryID, repositoryPath } from './github.mjs';
import { reconcile, validateState } from './reconcile.mjs';
import { signStaging, verifyEnvelope, MAX_ENTRIES, MAX_ENVELOPE_BYTES } from './sign.mjs';
import { serializeJSON } from './io.mjs';
import { findBaseline } from './baseline.mjs';

const id = `endpoint-${createHash('sha256').update('openai\nhttps://api.example.com/v1').digest('hex').slice(0, 16)}`;
const before = '2026-01-01T00:00:00Z';
const since = '2026-01-02T00:00:00Z';
const after = '2026-01-03T00:00:00Z';
const human = id => ({ id, type: 'User', login: `person-${id}` });
const body = `### Roster entry ID\n\n${id}\n\n### Problem\n\nUnavailable`;
const object = (id, userID = id, at = before) => ({ id, user: human(userID), created_at: at });
const reaction = (id, userID = id, at = before) => ({ ...object(id, userID, at), content: '+1' });
const report = (number = 1, userID = 1) => ({ entryID: id, issue: { ...object(number, userID), number, body }, rootReactions: [], confirmations: [] });
const payload = () => ({ schema_version: 1, revision: 1, published_at: before, sources: [], entries: [{ id, name: 'Example', base_url: 'https://api.example.com/v1', protocol: 'openai', auth: 'api_key', offer: 'unknown', setup: 'candidate', state: 'active', sources: [{ repo: 'example/directory', commit: 'a'.repeat(40) }] }] });
const run = options => reconcile({ payload: payload(), repository: 'example/roster', now: after, ...options });

test('issue form requires exactly one canonical ID; never follows submitted URLs', () => {
  assert.equal(issueEntryID(body), id);
  assert.equal(issueEntryID(body.replaceAll('\n', '\r\n')), id);
  assert.equal(issueEntryID(`${body}\n${body}`), null);
  assert.equal(issueEntryID('### Roster entry ID\n\nhttps://localhost/private'), null);
  assert.throws(() => repositoryPath('example/roster/../../anything'));
});

test('duplicate issues and root/comment reactions count the union of human numeric accounts', () => {
  const first = report(1);
  first.rootReactions = [reaction(1, 1), ...Array.from({ length: 8 }, (_, i) => reaction(i + 2))];
  const duplicate = report(2);
  duplicate.confirmations = [{ ...object(100, 9), reactions: [reaction(200, 9)] }];
  let result = run({ reports: [first, duplicate] });
  assert.equal(result.payload.entries[0].report.confirmations, 9);
  assert.equal(result.payload.entries[0].state, 'active');
  duplicate.confirmations[0].reactions.push(reaction(201, 10));
  result = run({ reports: [duplicate, first] });
  assert.equal(result.payload.entries[0].report.confirmations, 10);
  assert.equal(result.payload.entries[0].state, 'quarantined');
  assert.equal(result.state.entries[id].issue_number, 1);
  assert.deepEqual(result.plan.actions.map(x => x.action).sort(), ['duplicate', 'quarantine']);
});

test('bots, string IDs, non-thumbs-up reactions, and arbitrary comments cannot vote', async () => {
  const issue = { ...report().issue, user: { id: 1, type: 'Bot', login: 'automation' } };
  const calls = [];
  const api = { list: async path => {
    calls.push(path);
    if (path.includes('?state=')) return [issue, { ...issue, number: 2, body: body.replace(id, 'endpoint-ffffffffffffffff') }, { ...issue, number: 3, pull_request: {} }];
    if (path.endsWith('/issues/1/reactions')) return [reaction(1, 2), { ...reaction(2, 3), content: 'heart' }, { ...reaction(3), user: { ...human(4), login: 'fake[bot]' } }, { ...reaction(4), user: human('5') }];
    if (path.endsWith('/issues/1/comments')) return [
      { ...object(20, 6), body: 'same here https://localhost/private' },
      { ...object(21, 7), body: `Roster confirmation: ${id}` },
      { ...object(22, 8), body: `Roster confirmation: ${id}`, user: { ...human(8), type: 'Bot' } },
    ];
    if (path.endsWith('/comments/21/reactions')) return [reaction(40, 9)];
    throw new Error('Unexpected path');
  } };
  const reports = await fetchReports(api, 'example/roster', new Set([id]));
  const result = run({ reports });
  assert.deepEqual(result.state.entries[id].confirmations, [2, 7, 9]);
  assert.equal(calls.length, 4);
  assert.ok(calls.every(path => path.startsWith('/repos/example/roster/')));
});

test('quarantine tombstones survive vote removal, missing entries and source reintroduction', () => {
  const incident = report();
  incident.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1));
  const first = run({ reports: [incident] });
  const absent = payload();
  absent.entries[0].id = 'endpoint-ffffffffffffffff';
  const second = run({ payload: absent, previous: first.state });
  assert.equal(second.state.entries[id].quarantined, true);
  const third = run({ previous: second.state });
  assert.equal(third.payload.entries[0].state, 'quarantined');
  assert.equal(third.state.entries[id].confirmations.length, 0);
});

test('incomplete report fetch preserves state and does not execute restore', () => {
  const incident = report();
  incident.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1));
  const first = run({ reports: [incident] });
  const second = run({ previous: first.state, complete: false, restores: [{ entry_id: id, incident_since: since }] });
  assert.deepEqual(second.state.entries, first.state.entries);
  assert.deepEqual(second.state.issue_bindings, first.state.issue_bindings);
  assert.equal(second.state.last_complete_at, first.state.last_complete_at);
  assert.equal(second.state.fetch_status, 'stale');
  assert.equal(second.payload.entries[0].state, 'quarantined');
  assert.deepEqual(second.plan.actions, []);
});

test('restore advances epoch and watermarks all observed votes including edited old comments', () => {
  const incident = report();
  incident.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1, i + 1, after));
  const first = run({ reports: [incident] });
  const restores = [{ entry_id: id, incident_since: since }];
  const second = run({ previous: first.state, reports: [incident], restores });
  assert.equal(second.payload.entries[0].state, 'active');
  assert.equal(second.payload.entries[0].report.confirmations, 0);
  incident.confirmations.push({ ...object(90, 20, before), updated_at: after, reactions: [] });
  const third = run({ previous: second.state, reports: [incident], restores });
  assert.equal(third.payload.entries[0].state, 'active');
  assert.equal(third.plan.actions.length, 0);
  incident.rootReactions.push(...Array.from({ length: 10 }, (_, i) => reaction(i + 100, i + 1, after)));
  const fourth = run({ previous: third.state, reports: [incident], restores });
  assert.equal(fourth.payload.entries[0].state, 'quarantined');
  assert.throws(() => run({ previous: third.state, restores: [{ entry_id: id, incident_since: before }] }), /backwards/);
});

test('invalid persisted state fails closed', () => {
  assert.throws(() => validateState({}));
  assert.throws(() => validateState({ schema_version: 1, entries: { [id]: { quarantined: false } } }));
});

test('issue attribution survives body edits, duplicates, deletion and reopening without transferring votes', () => {
  const secondID = 'endpoint-ffffffffffffffff';
  const value = payload();
  value.entries.push({ ...value.entries[0], id: secondID });
  const incident = report(1), duplicate = report(2);
  incident.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1));
  duplicate.rootReactions = structuredClone(incident.rootReactions);
  const first = run({ payload: value, reports: [incident, duplicate] });
  assert.equal(first.payload.entries[0].state, 'quarantined');
  assert.deepEqual(first.state.issue_bindings, { 1: id, 2: id });
  for (const item of [incident, duplicate]) {
    item.entryID = secondID;
    item.issue.body = body.replace(id, secondID);
  }
  const edited = run({ payload: value, previous: first.state, reports: [incident, duplicate] });
  assert.equal(edited.payload.entries[1].state, 'active');
  assert.equal(edited.payload.entries[1].report.confirmations, 0);
  assert.deepEqual(edited.state.issue_bindings, first.state.issue_bindings);
  assert.deepEqual(edited.plan.actions.map(x => x.action), ['reject_identity_edit', 'reject_identity_edit']);
  const deleted = run({ payload: value, previous: edited.state, reports: [] });
  duplicate.issue.state = 'open';
  const reopened = run({ payload: value, previous: deleted.state, reports: [duplicate] });
  assert.equal(reopened.payload.entries[1].state, 'active');
  assert.equal(reopened.plan.actions[0].action, 'reject_identity_edit');
  assert.deepEqual(reopened.state.issue_bindings, first.state.issue_bindings);
  assert.throws(() => run({ payload: value, previous: first.state, repository: 'another/roster' }), /another repository/);
  assert.equal(run({ payload: value, previous: first.state, repository: 'EXAMPLE/ROSTER' }).state.repository, 'example/roster');
  // Restore does not release an issue number for reuse by a different entry.
  const restored = run({ payload: value, previous: first.state, reports: [incident], restores: [{ entry_id: id, incident_since: since }] });
  assert.equal(restored.state.issue_bindings[1], id);
  assert.equal(restored.payload.entries[1].state, 'active');
});

test('legacy state recovers canonical and recorded duplicate bindings and rejects conflicts', () => {
  const first = run({ reports: [report()] });
  delete first.state.repository;
  delete first.state.issue_bindings;
  first.state.entries[id].issue_numbers = [1, 2];
  validateState(first.state);
  const secondID = 'endpoint-ffffffffffffffff';
  const value = payload();
  value.entries.push({ ...value.entries[0], id: secondID });
  const edited = report(2);
  edited.entryID = secondID;
  edited.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1));
  const migrated = run({ payload: value, previous: first.state, reports: [edited] });
  assert.deepEqual(migrated.state.issue_bindings, { 1: id, 2: id });
  assert.equal(migrated.payload.entries[1].state, 'active');
  const conflict = structuredClone(first.state);
  conflict.entries[secondID] = { ...conflict.entries[id], issue_number: 2, issue_numbers: [] };
  assert.throws(() => run({ payload: value, previous: conflict }), /Conflicting historical/);
  const invalid = structuredClone(migrated.state);
  invalid.issue_bindings[3] = 'endpoint-aaaaaaaaaaaaaaaa';
  assert.throws(() => validateState(invalid), /Invalid issue binding/);
});

test('fetch retains edited known reports for rejection without requesting their votes', async () => {
  let calls = 0;
  const api = { list: async () => {
    calls++;
    return [{ ...report().issue, body: '### Roster entry ID\n\nhttps://example.com/not-an-id' }];
  } };
  const reports = await fetchReports(api, 'example/roster', new Set([id]), { 1: id });
  assert.equal(calls, 1);
  assert.equal(reports.length, 1);
  assert.equal(reports[0].entryID, null);
  const first = run({ reports: [report()] });
  const result = run({ previous: first.state, reports });
  assert.equal(result.plan.actions[0].action, 'reject_identity_edit');
});

test('source withdrawal retains community incident but restore never reactivates withdrawn entry', () => {
  const incident = report();
  incident.rootReactions = Array.from({ length: 10 }, (_, i) => reaction(i + 1));
  const first = run({ reports: [incident] });
  const withdrawn = payload();
  withdrawn.entries[0].state = 'withdrawn';
  const second = run({ payload: withdrawn, previous: first.state, reports: [incident] });
  assert.equal(second.payload.entries[0].state, 'withdrawn');
  assert.match(second.payload.entries[0].report.reason, /ten distinct/);
  assert.equal(second.state.entries[id].quarantined, true);
  const restored = run({ payload: second.payload, previous: second.state, reports: [incident], restores: [{ entry_id: id, incident_since: since }] });
  assert.equal(restored.payload.entries[0].state, 'withdrawn');
  assert.equal(restored.state.entries[id].quarantined, false);
  const subsequent = run({ payload: restored.payload, previous: restored.state, reports: [incident] });
  assert.equal(subsequent.payload.entries[0].state, 'withdrawn');
});

test('baseline requires explicit manual bootstrap and never skips a lost latest artifact', async () => {
  const env = { GITHUB_REPOSITORY: 'example/roster', GITHUB_REF_NAME: 'staging', GITHUB_RUN_ID: '99', GITHUB_EVENT_NAME: 'schedule', ALLOW_BOOTSTRAP: 'true' };
  const empty = { list: async () => [] };
  await assert.rejects(findBaseline(empty, env), /bootstrap/);
  assert.equal(await findBaseline(empty, { ...env, GITHUB_EVENT_NAME: 'workflow_dispatch' }), 'found=false\n');
  const calls = [];
  const api = { list: async path => {
    calls.push(path);
    return path.endsWith('/artifacts') ? [{ name: 'provider-roster-staging-signed', expired: true }] : [
      { id: 50, run_number: 5, head_branch: 'staging', event: 'workflow_dispatch' },
      { id: 40, run_number: 4, head_branch: 'staging', event: 'schedule' },
      { id: 60, run_number: 6, head_branch: 'main', event: 'schedule' },
    ];
  } };
  await assert.rejects(findBaseline(api, env), /unavailable/);
  assert.ok(calls[1].endsWith('/runs/50/artifacts'));
  assert.equal(calls.length, 2);
});

test('GitHub pagination and retries are bounded and auth is not redirected', async () => {
  const calls = [], waits = [];
  const api = new GitHub({ maxPages: 2, sleep: async ms => waits.push(ms), fetcher: async (url, options) => {
    calls.push(url);
    assert.equal(options.redirect, 'error');
    if (calls.length === 1) return { ok: false, status: 429, headers: new Headers({ 'retry-after': '999999' }) };
    return { ok: true, json: async () => url.includes('page=2') ? [101] : Array.from({ length: 100 }, (_, i) => i) };
  } });
  assert.equal((await api.list('/repos/example/roster/issues')).length, 101);
  assert.deepEqual(waits, [30000]);
  const capped = new GitHub({ maxPages: 1, fetcher: async () => ({ ok: true, json: async () => Array(100).fill({}) }) });
  await assert.rejects(capped.list('/repos/example/roster/issues'), /pagination/);
  await assert.rejects(api.get('https://example.com'), /Invalid API path/);
  const denied = new GitHub({ sleep: async () => {}, fetcher: async () => ({ ok: false, status: 403, headers: new Headers() }) });
  await assert.rejects(denied.list('/repos/example/roster/issues'), /403/);
  assert.equal(denied.requests, 4);
});

test('ephemeral Ed25519 signs exact payload bytes and rejects tampering and wrong keys', () => {
  const bytes = Buffer.from(`${JSON.stringify(payload(), null, 2)}\n`);
  const { envelope, trust } = signStaging(bytes);
  assert.deepEqual(verifyEnvelope(envelope, trust), bytes);
  assert.equal(Buffer.from(trust.public_key, 'base64').length, 32);
  assert.match(trust.trust, /TEST ONLY/);
  const mutated = { ...envelope, payload: Buffer.from(JSON.stringify({ ...payload(), revision: 2 })).toString('base64') };
  assert.throws(() => verifyEnvelope(mutated, trust), /signature/);
  assert.throws(() => verifyEnvelope({ ...envelope, signature: Buffer.alloc(64).toString('base64') }, trust), /signature/);
  assert.throws(() => verifyEnvelope(envelope, signStaging(bytes).trust), /key/);
});

test('signer accepts 5000 entries and rejects 5001 generated entries', () => {
  assert.equal(MAX_ENTRIES, 5000);
  const value = payload();
  const template = value.entries[0];
  value.entries = Array.from({ length: MAX_ENTRIES }, (_, i) => {
    const base_url = `https://api.example.com/v1/${i}`;
    return { ...template, base_url, id: `endpoint-${createHash('sha256').update(`openai\n${base_url}`).digest('hex').slice(0, 16)}` };
  });
  signStaging(Buffer.from(JSON.stringify(value)));
  value.entries.push(template);
  assert.throws(() => signStaging(Buffer.from(JSON.stringify(value))), /entry limit/);
});

test('signer measures final base64 envelope at the transport boundary', () => {
  assert.equal(MAX_ENVELOPE_BYTES, 4 * 1024 * 1024);
  const bytes = Buffer.from(JSON.stringify(payload()));
  const small = signStaging(bytes);
  const overhead = Buffer.byteLength(serializeJSON(small.envelope)) - small.envelope.payload.length;
  const maxDecoded = Math.floor((MAX_ENVELOPE_BYTES - overhead) / 4) * 3;
  // Legal JSON whitespace generates the boundary in memory without a large fixture file.
  const near = Buffer.concat([bytes, Buffer.alloc(maxDecoded - bytes.length, 0x20)]);
  const accepted = signStaging(near);
  const size = Buffer.byteLength(serializeJSON(accepted.envelope));
  assert.ok(size <= MAX_ENVELOPE_BYTES && size > MAX_ENVELOPE_BYTES - 4);
  const oversized = Buffer.concat([near, Buffer.from(' ')]);
  assert.throws(() => signStaging(oversized), /Envelope exceeds/);
  assert.throws(() => verifyEnvelope({ ...accepted.envelope, payload: oversized.toString('base64') }, accepted.trust), /Envelope exceeds/);
});

test('inline raster logos stay signed; SVG and digest mismatches fail', () => {
  const chunk = (type, bytes) => {
    const result = Buffer.alloc(bytes.length + 12);
    result.writeUInt32BE(bytes.length);
    result.write(type, 4);
    bytes.copy(result, 8);
    result.writeUInt32BE(crc32(result.subarray(4, -4)), result.length - 4);
    return result;
  };
  const header = Buffer.alloc(13);
  header.writeUInt32BE(1, 0); header.writeUInt32BE(1, 4); header[8] = 8; header[9] = 6;
  const data = Buffer.concat([Buffer.from('89504e470d0a1a0a', 'hex'), chunk('IHDR', header), chunk('IDAT', deflateSync(Buffer.from([0, 0, 0, 0, 255]))), chunk('IEND', Buffer.alloc(0))]);
  const value = payload();
  value.entries[0].logo = { mime: 'image/png', data: data.toString('base64'), sha256: createHash('sha256').update(data).digest('hex'), license: 'Synthetic test image', source_url: 'https://example.com/logo.png' };
  const { envelope, trust } = signStaging(Buffer.from(JSON.stringify(value)));
  assert.deepEqual(JSON.parse(verifyEnvelope(envelope, trust)).entries[0].logo, value.entries[0].logo);
  value.entries[0].logo.sha256 = '0'.repeat(64);
  assert.throws(() => signStaging(Buffer.from(JSON.stringify(value))), /logo/);
  value.entries[0].logo.mime = 'image/svg+xml';
  assert.throws(() => signStaging(Buffer.from(JSON.stringify(value))), /logo/);
});
