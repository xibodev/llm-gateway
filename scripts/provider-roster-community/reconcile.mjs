import { isHuman, numericID, repositoryPath } from './github.mjs';

export const emptyState = () => ({ schema_version: 1, entries: {} });

export function validateState(state) {
  if (state?.schema_version !== 1 || !state.entries || Array.isArray(state.entries) || typeof state.entries !== 'object') throw new Error('Invalid report state');
  for (const [id, item] of Object.entries(state.entries)) {
    if (!/^endpoint-[a-f0-9]{16,64}$/.test(id) || typeof item.quarantined !== 'boolean' ||
        !Array.isArray(item.confirmations) || !item.confirmations.every(numericID) ||
        !Array.isArray(item.watermark) || !item.watermark.every(x => typeof x === 'string') ||
        (item.issue_number !== null && !numericID(item.issue_number)) ||
        (item.incident_since !== null && !validDate(item.incident_since))) throw new Error('Invalid report state entry');
    if (item.issue_numbers !== undefined && (!Array.isArray(item.issue_numbers) || !item.issue_numbers.every(numericID))) throw new Error('Invalid legacy issue numbers');
  }
  if (state.repository !== undefined) repositoryPath(state.repository);
  if (state.issue_bindings !== undefined) {
    if (!state.repository || !state.issue_bindings || typeof state.issue_bindings !== 'object' || Array.isArray(state.issue_bindings)) throw new Error('Invalid issue bindings');
    for (const [number, id] of Object.entries(state.issue_bindings)) {
      if (!/^[1-9]\d*$/.test(number) || !numericID(Number(number)) || typeof id !== 'string' || !Object.hasOwn(state.entries, id)) throw new Error('Invalid issue binding');
    }
  }
  return state;
}

export function bindStateRepository(previous, repository) {
  validateState(previous);
  repositoryPath(repository);
  const canonical = repository.toLowerCase();
  if (previous.repository && previous.repository.toLowerCase() !== canonical) throw new Error('Report state belongs to another repository');
  const state = structuredClone(previous);
  state.repository = canonical;
  state.issue_bindings ??= {};
  // Older artifacts retained canonical issue numbers, and some retained duplicate arrays.
  // Recover only recorded attribution; issue bodies cannot override that history.
  for (const [id, item] of Object.entries(state.entries)) {
    for (const number of [item.issue_number, ...(item.issue_numbers ?? [])].filter(numericID)) {
      const existing = state.issue_bindings[number];
      if (existing && existing !== id) throw new Error('Conflicting historical issue attribution');
      state.issue_bindings[number] = id;
    }
  }
  return state;
}

function validDate(value) {
  return typeof value === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z$/.test(value) && Number.isFinite(Date.parse(value));
}

function votes(report) {
  const events = [];
  const add = (kind, object) => {
    if (!numericID(object.id) || !isHuman(object.user)) return;
    if (!validDate(object.created_at)) throw new Error('Invalid confirmation timestamp');
    events.push({ key: `${kind}:${object.id}`, user: object.user.id, at: object.created_at });
  };
  add('issue', report.issue);
  for (const reaction of report.rootReactions) if (reaction.content === '+1') add('reaction', reaction);
  for (const comment of report.confirmations) {
    add('comment', comment);
    for (const reaction of comment.reactions) if (reaction.content === '+1') add('reaction', reaction);
  }
  return events;
}

export function reconcile({ payload, previous = emptyState(), reports = [], complete = true, repository, restores = [], now = new Date().toISOString() }) {
  validateState(previous);
  if (!Array.isArray(payload?.entries) || !payload.entries.length) throw new Error('Cannot reconcile an empty roster');
  const state = bindStateRepository(previous, repository);
  const output = structuredClone(payload);
  const plans = [];
  const groups = new Map();
  const known = new Set([...payload.entries.map(e => e.id), ...Object.keys(previous.entries)]);
  for (const report of reports) {
    if (!complete) break;
    if (!numericID(report.issue.number)) throw new Error('Invalid issue number');
    const bound = state.issue_bindings[report.issue.number];
    if (bound && bound !== report.entryID) {
      plans.push({ action: 'reject_identity_edit', issue_number: report.issue.number, entry_id: bound,
        reason: 'Issue attribution is immutable; edited report excluded.' });
      continue;
    }
    if (!known.has(report.entryID)) continue;
    state.issue_bindings[report.issue.number] = report.entryID;
    if (!groups.has(report.entryID)) groups.set(report.entryID, []);
    groups.get(report.entryID).push(report);
  }
  const overrides = new Map();
  for (const restore of restores) {
    if (!known.has(restore.entry_id) || !validDate(restore.incident_since) || Date.parse(restore.incident_since) > Date.parse(now) || overrides.has(restore.entry_id)) throw new Error('Invalid restore override');
    const oldSince = previous.entries[restore.entry_id]?.incident_since;
    if (oldSince && Date.parse(restore.incident_since) < Date.parse(oldSince)) throw new Error('Restore epoch cannot move backwards');
    overrides.set(restore.entry_id, restore);
  }
  if (complete) {
    for (const id of known) {
      const group = groups.get(id) ?? [];
      const events = group.flatMap(votes);
      const item = state.entries[id] ?? { quarantined: payload.entries.some(e => e.id === id && e.state === 'quarantined'), confirmations: [], watermark: [], incident_since: null, issue_number: null };
      // Keep a canonical known issue stable across duplicate reports and temporary disappearance.
      item.issue_number ??= group.length ? Math.min(...group.map(r => r.issue.number)) : null;
      const restore = overrides.get(id);
      if (restore && restore.incident_since !== item.incident_since) {
        item.incident_since = restore.incident_since;
        item.watermark = [...new Set([...item.watermark, ...events.map(e => e.key)])].sort();
        item.quarantined = false;
        item.confirmations = [];
        plans.push({ action: 'restore', entry_id: id, issue_number: item.issue_number });
      }
      const baseline = new Set(item.watermark);
      const humans = new Set(events.filter(e => !baseline.has(e.key) && (!item.incident_since || Date.parse(e.at) > Date.parse(item.incident_since))).map(e => e.user));
      item.confirmations = [...humans].sort((a, b) => a - b);
      if (humans.size >= 10 && !item.quarantined) {
        item.quarantined = true;
        plans.push({ action: 'quarantine', entry_id: id, issue_number: item.issue_number });
      }
      for (const report of group) if (report.issue.number !== item.issue_number) {
        plans.push({ action: 'duplicate', entry_id: id, issue_number: report.issue.number, canonical_issue_number: item.issue_number });
      }
      state.entries[id] = item;
    }
  }
  for (const entry of output.entries) {
    const item = state.entries[entry.id];
    if (!item) continue;
    if (item.quarantined && entry.state !== 'withdrawn') entry.state = 'quarantined';
    // Only an explicit restored epoch can release a carried-forward community quarantine.
    if (!item.quarantined && item.incident_since && entry.state === 'quarantined') entry.state = 'active';
    entry.report = {
      issue_url: item.issue_number ? `https://github.com/${repository}/issues/${item.issue_number}` : '',
      confirmations: item.confirmations.length,
      reason: item.quarantined ? 'Community incident reached ten distinct human accounts.' : '',
    };
  }
  state.fetch_status = complete ? 'ok' : 'stale';
  if (complete) state.last_complete_at = now;
  return { payload: output, state, plan: { dry_run: true, fetch_status: state.fetch_status, actions: plans } };
}
