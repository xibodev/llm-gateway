import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import {
  emptyState, validateState, loadState, saveState, acceptLogo, rejectLogo,
  markFailed, shouldSkipDiscovery, isRejectedHash,
} from './logo-state.mjs';

function tmpDir() { return join(tmpdir(), `logo-state-test-${Date.now()}-${Math.random().toString(36).slice(2)}`); }

describe('logo-state', () => {
  it('empty state has valid schema', () => {
    const s = emptyState();
    assert.equal(s.schema_version, 1);
    assert.deepEqual(s.entries, {});
    assert.ok(validateState(s));
  });

  it('validates well-formed state', () => {
    const s = emptyState();
    acceptLogo(s, 'openai', { method: 'bundled', asset_sha256: 'abc' });
    assert.ok(validateState(s));
  });

  it('rejects malformed schemas', () => {
    assert.equal(validateState(null), false);
    assert.equal(validateState({}), false);
    assert.equal(validateState({ schema_version: 1, entries: 'not-an-object' }), false);
    assert.equal(validateState({ schema_version: 99, entries: {} }), false);
  });

  it('accept and reject logos', () => {
    const s = emptyState();
    acceptLogo(s, 'groq', { method: 'favicon', asset_sha256: 'h1', source_url: 'https://groq.com/favicon.ico' });
    assert.equal(s.entries.groq.status, 'accepted');
    assert.equal(s.entries.groq.asset_sha256, 'h1');
    assert.ok(s.entries.groq.accepted_at);

    rejectLogo(s, 'groq', 'h1', 'blurry');
    assert.equal(s.entries.groq.status, 'rejected');
    assert.deepEqual(s.entries.groq.rejected_hashes, ['h1']);
    assert.ok(s.entries.groq.next_attempt_at);
  });

  it('shouldSkipDiscovery skips accepted entries', () => {
    const s = emptyState();
    acceptLogo(s, 'openai', { method: 'bundled' });
    assert.equal(shouldSkipDiscovery(s, 'openai'), true);
  });

  it('shouldSkipDiscovery skips rejected entries during cooldown', () => {
    const s = emptyState();
    rejectLogo(s, 'bad', 'hash', 'low_quality');
    assert.equal(shouldSkipDiscovery(s, 'bad'), true);
  });

  it('shouldSkipDiscovery allows retry after cooldown', () => {
    const s = emptyState();
    s.entries.test = {
      status: 'rejected',
      next_attempt_at: '2020-01-01T00:00:00.000Z',
      rejected_hashes: [],
    };
    assert.equal(shouldSkipDiscovery(s, 'test', { now: '2024-01-01T00:00:00.000Z' }), false);
  });

  it('shouldSkipDiscovery does not skip unknown entries', () => {
    const s = emptyState();
    assert.equal(shouldSkipDiscovery(s, 'unknown'), false);
  });

  it('isRejectedHash detects rejected hashes', () => {
    const s = emptyState();
    rejectLogo(s, 'a', 'hash1', 'bad');
    assert.equal(isRejectedHash(s, 'a', 'hash1'), true);
    assert.equal(isRejectedHash(s, 'a', 'hash2'), false);
    assert.equal(isRejectedHash(s, 'missing', 'hash1'), false);
  });

  it('markFailed records failure with cooldown', () => {
    const s = emptyState();
    markFailed(s, 'x', 'timeout');
    assert.equal(s.entries.x.status, 'failed');
    assert.equal(s.entries.x.failure_reason, 'timeout');
    assert.ok(s.entries.x.next_attempt_at);
  });

  it('persists and loads from disk', async () => {
    const dir = tmpDir();
    const file = join(dir, 'logo-state.json');
    try {
      await mkdir(dir, { recursive: true });
      const s = emptyState();
      acceptLogo(s, 'anthropic', { method: 'bundled' });
      await saveState(file, s);
      const loaded = await loadState(file);
      assert.equal(loaded.entries.anthropic.status, 'accepted');
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('loadState returns empty state on missing file', async () => {
    const s = await loadState('/nonexistent/path/logo-state.json');
    assert.equal(s.schema_version, 1);
    assert.deepEqual(s.entries, {});
  });

  it('loadState returns empty state on corrupted file', async () => {
    const dir = tmpDir();
    const file = join(dir, 'logo-state.json');
    try {
      await mkdir(dir, { recursive: true });
      await writeFile(file, '{invalid json', 'utf8');
      const s = await loadState(file);
      assert.equal(s.schema_version, 1);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('rejectLogo accumulates rejected hashes', () => {
    const s = emptyState();
    rejectLogo(s, 'x', 'h1', 'bad1');
    rejectLogo(s, 'x', 'h2', 'bad2');
    assert.deepEqual(s.entries.x.rejected_hashes, ['h1', 'h2']);
  });
});
