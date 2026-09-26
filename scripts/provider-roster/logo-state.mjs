import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { dirname } from 'node:path';
import { sha256 } from './normalize.mjs';

const STATE_SCHEMA = 1;

export function emptyState() {
  return { schema_version: STATE_SCHEMA, entries: {} };
}

export function validateState(state) {
  if (!state || typeof state !== 'object') return false;
  if (state.schema_version !== STATE_SCHEMA) return false;
  if (!state.entries || typeof state.entries !== 'object') return false;
  for (const [id, entry] of Object.entries(state.entries)) {
    if (!id || typeof id !== 'string') return false;
    if (!entry || typeof entry !== 'object') return false;
    if (!['accepted', 'rejected', 'pending', 'failed'].includes(entry.status)) return false;
    if (entry.asset_sha256 !== undefined && typeof entry.asset_sha256 !== 'string') return false;
    if (entry.rejected_hashes !== undefined && !Array.isArray(entry.rejected_hashes)) return false;
    if (entry.override_revision !== undefined && typeof entry.override_revision !== 'number') return false;
  }
  return true;
}

export async function loadState(path) {
  try {
    const raw = await readFile(path, 'utf8');
    const state = JSON.parse(raw);
    if (!validateState(state)) return emptyState();
    return state;
  } catch {
    return emptyState();
  }
}

export async function saveState(path, state) {
  if (!validateState(state)) throw new Error('invalid_logo_state');
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, JSON.stringify(state, null, 2) + '\n', 'utf8');
}

export function getEntry(state, id) {
  return state.entries[id] || null;
}

export function acceptLogo(state, id, { method, asset_sha256, source_url, page_url }) {
  state.entries[id] = {
    status: 'accepted',
    method: method || 'unknown',
    asset_sha256: asset_sha256 || '',
    source_url: source_url || '',
    page_url: page_url || '',
    accepted_at: new Date().toISOString(),
    last_attempt_at: new Date().toISOString(),
    next_attempt_at: '',
    failure_reason: '',
    rejected_hashes: state.entries[id]?.rejected_hashes || [],
    override_revision: state.entries[id]?.override_revision || 0,
  };
}

export function rejectLogo(state, id, hash, reason) {
  const existing = state.entries[id] || {};
  const rejected = new Set(existing.rejected_hashes || []);
  if (hash) rejected.add(hash);
  state.entries[id] = {
    ...existing,
    status: 'rejected',
    last_attempt_at: new Date().toISOString(),
    next_attempt_at: retryCooldown(existing),
    failure_reason: reason || '',
    rejected_hashes: [...rejected],
  };
}

export function markFailed(state, id, reason) {
  const existing = state.entries[id] || {};
  state.entries[id] = {
    ...existing,
    status: 'failed',
    last_attempt_at: new Date().toISOString(),
    next_attempt_at: retryCooldown(existing),
    failure_reason: reason || '',
  };
}

export function shouldSkipDiscovery(state, id, { now = new Date().toISOString() } = {}) {
  const entry = state.entries[id];
  if (!entry) return false;
  if (entry.status === 'accepted') return true;
  if (entry.status === 'rejected') {
    if (!entry.next_attempt_at) return true;
    return new Date(now) < new Date(entry.next_attempt_at);
  }
  if (entry.status === 'failed') {
    if (!entry.next_attempt_at) return false;
    return new Date(now) < new Date(entry.next_attempt_at);
  }
  return false;
}

export function isRejectedHash(state, id, hash) {
  const entry = state.entries[id];
  if (!entry?.rejected_hashes) return false;
  return entry.rejected_hashes.includes(hash);
}

function retryCooldown(existing) {
  const attempts = (existing.retry_count || 0) + 1;
  const base = 24 * 60 * 60 * 1000;
  const maxDelay = 7 * 24 * 60 * 60 * 1000;
  const delay = Math.min(base * Math.pow(2, attempts - 1), maxDelay);
  return new Date(Date.now() + delay).toISOString();
}
