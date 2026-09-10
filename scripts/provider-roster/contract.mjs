import { canonicalURL, stableID } from './normalize.mjs';
import { validateLogo } from './logos.mjs';
import { withinUTF8, MAX_NAME_BYTES, MAX_ID_BYTES, MAX_DESCRIPTION_BYTES, MAX_URL_BYTES } from './limits.mjs';

export const MAX_ENTRIES = 5000;

const enums = {
  protocol: ['openai', 'anthropic', 'unknown'], auth: ['api_key', 'none', 'unknown'],
  offer: ['free_tier', 'recurring_credit', 'trial', 'paid', 'unknown'],
  setup: ['compatible', 'candidate'], state: ['active', 'quarantined', 'withdrawn'],
};
const date = value => typeof value === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(value) && Number.isFinite(Date.parse(value));
const sourceValid = source => source && /^[\w.-]+\/[\w.-]+$/.test(source.repo) && /^[a-f0-9]{40}$/.test(source.commit);
const optionalURL = value => {
  if (value === undefined || value === '') return;
  if (!withinUTF8(value, MAX_URL_BYTES)) throw new Error('invalid_payload_url');
  canonicalURL(value);
};
export function validatePayload(payload) {
  const fail = () => { throw new Error('invalid_payload_contract'); };
  if (payload?.schema_version !== 1 || !Number.isSafeInteger(payload.revision) || payload.revision < 1 ||
      !date(payload.published_at) || !Array.isArray(payload.entries) || payload.entries.length > MAX_ENTRIES ||
      !Array.isArray(payload.sources)) fail();
  const ids = new Set();
  for (const source of payload.sources) {
    if (!sourceValid(source) && !(source?.status === 'stale' && source.commit === '' && /^[\w.-]+\/[\w.-]+$/.test(source.repo))) fail();
    if (!['ok', 'stale'].includes(source.status)) fail();
    optionalURL(source.url);
    optionalURL(source.license_url);
  }
  for (const entry of payload.entries) {
    if (!entry || !withinUTF8(entry.name, MAX_NAME_BYTES) || !entry.name.trim() ||
        !withinUTF8(entry.id, MAX_ID_BYTES) || !entry.id ||
        (entry.description !== undefined && !withinUTF8(entry.description, MAX_DESCRIPTION_BYTES))) fail();
    for (const [field, values] of Object.entries(enums)) if (!values.includes(entry[field])) fail();
    if (canonicalURL(entry.base_url) !== entry.base_url || entry.id !== stableID(entry.protocol, entry.base_url) || ids.has(entry.id)) fail();
    ids.add(entry.id);
    if (entry.setup === 'compatible' && (entry.protocol === 'unknown' || entry.auth === 'unknown')) fail();
    optionalURL(entry.base_url);
    optionalURL(entry.signup_url);
    optionalURL(entry.docs_url);
    if (!Array.isArray(entry.sources) || !entry.sources.length || !entry.sources.every(sourceValid)) fail();
    for (const source of entry.sources) {
      optionalURL(source.url);
      optionalURL(source.license_url);
    }
    if (entry.offer_expires_at && !date(entry.offer_expires_at)) fail();
    for (const field of ['requirements', 'conflicts']) {
      if (entry[field] !== undefined && (!Array.isArray(entry[field]) || entry[field].some(value => typeof value !== 'string'))) fail();
    }
    if (entry.probe) {
      if (!['not_checked', 'reachable', 'auth_required', 'rate_limited', 'failed', 'blocked'].includes(entry.probe.status) ||
          !Number.isInteger(entry.probe.http_status) || entry.probe.http_status < 0 || entry.probe.http_status > 599 ||
          (entry.probe.checked_at && !date(entry.probe.checked_at))) fail();
    }
    if (entry.report && (!Number.isSafeInteger(entry.report.confirmations) || entry.report.confirmations < 0)) fail();
    optionalURL(entry.report?.issue_url);
    optionalURL(entry.logo?.source_url);
    validateLogo(entry.logo);
  }
  return payload;
}
