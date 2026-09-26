import { createHash } from 'node:crypto';
import { publicURL } from './network.mjs';
import { API_EVIDENCE } from './catalog.mjs';
import { plainText } from './extractors.mjs';
import { truncateUTF8, MAX_NAME_BYTES } from './limits.mjs';

export const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
export const compare = (a, b) => a < b ? -1 : a > b ? 1 : 0;
export const sortedUnique = values => [...new Set(values)].sort(compare);
export const unchecked = () => ({ status: 'not_checked', checked_at: '', http_status: 0 });

export function canonicalURL(input, { site = false } = {}) {
  let url;
  try { url = new URL(input); } catch { throw new Error('invalid_url'); }
  if (url.username || url.password) throw new Error('url_credentials');
  // Directory signup links contain referrals. They are never part of endpoint identity.
  if (site) { url.search = ''; url.hash = ''; }
  publicURL(url.href);
  url.pathname = url.pathname.replace(/%[0-9a-f]{2}/gi, encoded => {
    const character = String.fromCharCode(parseInt(encoded.slice(1), 16));
    return /[a-z0-9_~.-]/i.test(character) ? character : encoded.toUpperCase();
  });
  // Keep doubled slashes: they can select distinct routes. Strip only an ordinary trailing slash.
  return url.href.endsWith('//') ? url.href : url.href.replace(/\/$/, '');
}

export function stableID(protocol, baseURL) {
  return `endpoint-${sha256(`${protocol}\n${canonicalURL(baseURL)}`).slice(0, 16)}`;
}

export function normalizeEntry(raw, source) {
  let hasEndpoint = raw.endpointEvidence;
  let base = raw.base_url;
  const requirements = [...(raw.requirements || [])];
  if (/[{}]|%7[b-d]/i.test(base)) {
    base = raw.signup_url;
    hasEndpoint = false;
    requirements.push('Source endpoint requires account-specific substitution; review setup.');
  }
  base = canonicalURL(base, { site: !hasEndpoint });
  const known = hasEndpoint ? API_EVIDENCE[base] : undefined;
  const protocol = known?.protocol || 'unknown';
  const auth = raw.auth && raw.auth !== 'unknown' ? raw.auth : known?.auth || 'unknown';
  const signup = raw.signup_url ? canonicalURL(raw.signup_url, { site: true }) : '';
  return {
    id: stableID(protocol, base), name: truncateUTF8(plainText(raw.name), MAX_NAME_BYTES), protocol,
    base_url: base, signup_url: signup, auth, offer: raw.offer || 'unknown',
    description: 'Source-reported offer; confirm provider terms.',
    setup: known && auth !== 'unknown' ? 'compatible' : 'candidate', state: 'active',
    sources: [source], probe: unchecked(),
    report: { issue_url: '', confirmations: 0, reason: '' },
    requirements: sortedUnique(requirements.map(value => plainText(value).slice(0, 1200))),
    conflicts: sortedUnique(raw.conflicts || []),
  };
}

export function mergeEntries(entries) {
  const groups = new Map();
  for (const entry of entries) {
    const list = groups.get(entry.id) || [];
    list.push(entry);
    groups.set(entry.id, list);
  }
  return [...groups.values()].map(rows => {
    rows.sort((a, b) => compare(JSON.stringify(a), JSON.stringify(b)));
    const result = structuredClone(rows[0]);
    const conflicts = rows.flatMap(row => row.conflicts || []);
    for (const field of ['auth', 'offer']) {
      const values = sortedUnique(rows.map(row => row[field]).filter(value => value !== 'unknown'));
      result[field] = values.length === 1 && !conflicts.some(conflict => conflict.startsWith(`${field}:`)) ? values[0] : 'unknown';
      if (values.length > 1) conflicts.push(`${field}: ${values.join(' | ')}`);
    }
    for (const field of ['name', 'signup_url']) {
      const values = sortedUnique(rows.map(row => row[field]).filter(Boolean));
      result[field] = values[0] || '';
      if (values.length > 1) conflicts.push(`${field}: ${values.join(' | ')}`);
    }
    result.sources = [...new Map(rows.flatMap(row => row.sources).map(source =>
      [JSON.stringify(source), source])).values()].sort((a, b) => compare(JSON.stringify(a), JSON.stringify(b)));
    result.requirements = sortedUnique(rows.flatMap(row => row.requirements || []));
    result.conflicts = sortedUnique(conflicts);
    result.setup = result.protocol !== 'unknown' && result.auth !== 'unknown' &&
      !result.conflicts.some(value => /^(?:auth|protocol):/.test(value)) ? 'compatible' : 'candidate';
    return result;
  }).sort((a, b) => compare(a.id, b.id));
}

export function carryState(entries, previous, now) {
  const old = new Map((previous?.entries || []).map(entry => [entry.id, entry]));
  const result = new Map(entries.map(entry => [entry.id, entry]));
  // Disappearance is not an operator restore. Keep all old rows as withdrawn tombstones.
  for (const [id, entry] of old) {
    if (!result.has(id)) result.set(id, { ...structuredClone(entry), state: 'withdrawn' });
  }
  for (const entry of result.values()) {
    const prior = old.get(entry.id);
    if (prior) {
      if (entry.state !== 'withdrawn' && prior.state !== 'active') entry.state = prior.state;
      if (prior.report) entry.report = structuredClone(prior.report);
      if (prior.offer_expires_at) entry.offer_expires_at = prior.offer_expires_at;
      if (prior.logo && !entry.logo) entry.logo = structuredClone(prior.logo);
    }
    if ((entry.report?.confirmations || 0) >= 10 && entry.state !== 'withdrawn') entry.state = 'quarantined';
    if (entry.offer_expires_at && Date.parse(entry.offer_expires_at) <= Date.parse(now)) {
      entry.offer = 'unknown';
      entry.requirements = sortedUnique([...(entry.requirements || []), 'Previously recorded offer has expired; maintainer review required.']);
    }
  }
  return [...result.values()].sort((a, b) => compare(a.id, b.id));
}
