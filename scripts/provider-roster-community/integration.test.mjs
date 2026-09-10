import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { main as collect } from '../provider-roster/build.mjs';
import { main as reconcile } from './cli.mjs';
import { verifyEnvelope, MAX_ENVELOPE_BYTES } from './sign.mjs';
import { serializeJSON } from './io.mjs';
import { carryState } from '../provider-roster/normalize.mjs';
import { reconcile as reconcileState } from './reconcile.mjs';

test('offline collector → GitHub snapshot → signed staging artifact; failed next fetch retains quarantine', async () => {
  const root = await mkdtemp(join(tmpdir(), 'roster-community-'));
  try {
    const collected = join(root, 'collected'), reconciled = join(root, 'reconciled'), signed = join(root, 'signed');
    const fixtures = resolve('scripts/provider-roster/fixtures');
    const result = await collect(['--output', collected, '--fixtures', fixtures, '--offline', '--no-probe']);
    const id = result.payload.entries[0].id;
    const time = '2026-01-01T00:00:00Z';
    const user = id => ({ id, type: 'User', login: `person-${id}` });
    const api = { list: async path => {
      if (path.includes('?state=')) return [{ id: 1, number: 1, body: `### Roster entry ID\n\n${id}`, user: user(1), created_at: time }];
      if (path.endsWith('/reactions')) return Array.from({ length: 10 }, (_, i) => ({ id: i + 1, user: user(i + 1), content: '+1', created_at: time }));
      return [];
    } };
    const common = ['--repository', 'example/roster', '--payload', join(collected, 'payload.json')];
    await reconcile([...common, '--output', reconciled, '--bootstrap'], { api });
    const child = spawnSync(process.execPath, ['scripts/provider-roster-community/sign.mjs', '--input', reconciled, '--output', signed], { encoding: 'utf8' });
    assert.equal(child.status, 0, child.stderr);
    const json = async path => JSON.parse(await readFile(path, 'utf8'));
    const envelope = await json(join(signed, 'envelope.json'));
    const envelopeBytes = await readFile(join(signed, 'envelope.json'));
    assert.equal(envelopeBytes.toString('utf8'), serializeJSON(envelope));
    assert.ok(envelopeBytes.length <= MAX_ENVELOPE_BYTES);
    const trust = await json(join(signed, 'public-key.json'));
    assert.deepEqual(verifyEnvelope(envelope, trust), await readFile(join(signed, 'payload.json')));
    assert.equal(JSON.parse(verifyEnvelope(envelope, trust)).entries[0].state, 'quarantined');
    const signedPayload = JSON.parse(verifyEnvelope(envelope, trust));
    const signedState = await json(join(signed, 'report-state.json'));
    const removed = { ...signedPayload, entries: carryState(signedPayload.entries.slice(1), signedPayload, new Date().toISOString()) };
    assert.equal(removed.entries.find(entry => entry.id === id).state, 'withdrawn');
    const restored = reconcileState({ payload: removed, previous: signedState, repository: 'example/roster',
      restores: [{ entry_id: id, incident_since: time }] });
    assert.equal(restored.payload.entries.find(entry => entry.id === id).state, 'withdrawn');
    assert.match(await readFile(join(signed, 'TEST-TRUST.txt'), 'utf8'), /STAGING TEST/);
    const failed = { list: async () => { throw new Error('simulated outage'); } };
    const next = join(root, 'next');
    await reconcile([...common, '--output', next, '--previous', join(signed, 'report-state.json')], { api: failed });
    assert.equal((await json(join(next, 'payload.json'))).entries[0].state, 'quarantined');
    assert.equal((await json(join(next, 'report-state.json'))).fetch_status, 'stale');
    await assert.rejects(reconcile([...common, '--output', join(root, 'bootstrap-failure'), '--bootstrap'], { api: failed }), /Cannot bootstrap/);
    await assert.rejects(reconcile([...common, '--output', next, '--previous', join(root, 'missing.json')], { api }), /ENOENT/);
    await assert.rejects(reconcile([...common, '--output', next, '--apply'], { api }), /Unknown/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
