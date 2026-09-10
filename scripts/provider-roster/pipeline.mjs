import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { SOURCES, extractSource } from './extractors.mjs';
import { normalizeEntry, mergeEntries, carryState, unchecked } from './normalize.mjs';
import { createSafeFetcher, probeEndpoint } from './network.mjs';
import { collectLogos } from './logos.mjs';
import { validatePayload } from './contract.mjs';
import { API_EVIDENCE } from './catalog.mjs';
import { sourceNotice } from './notices.mjs';

export async function loadSnapshot(source, fetcher, { fixtures, offline = false } = {}) {
  if (fixtures) {
    const manifest = JSON.parse(await readFile(join(fixtures, 'snapshots.json'), 'utf8'));
    const commit = manifest[source.repo]?.commit;
    if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('fixture_commit');
    return { commit, text: await readFile(join(fixtures, source.fixture), 'utf8') };
  }
  if (offline) throw new Error('offline_without_snapshot');
  const response = await fetcher(`https://api.github.com/repos/${source.repo}/commits/HEAD`, { maxBytes: 1024 * 1024 });
  if (response.status !== 200) throw new Error('source_commit_http');
  const commit = JSON.parse(response.body.toString('utf8')).sha;
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('source_commit_schema');
  const content = await fetcher(`https://raw.githubusercontent.com/${source.repo}/${commit}/${source.file}`, { maxBytes: 2 * 1024 * 1024 });
  if (content.status !== 200) throw new Error('source_content_http');
  return { commit, text: content.body.toString('utf8') };
}

export async function buildRoster({ previous, fixtures, offline = false, noProbe = false,
  favicon = true, now = new Date().toISOString(), fetcher = createSafeFetcher(), snapshotLoader = loadSnapshot,
  logoState = null } = {}) {
  if (previous) validatePayload(previous);
  const sourceResults = await Promise.all(SOURCES.map(async source => {
    let snapshot;
    try {
      snapshot = await snapshotLoader(source, fetcher, { fixtures, offline });
      if (!/^[a-f0-9]{40}$/.test(snapshot.commit)) throw new Error('source_commit_schema');
      const provenance = { repo: source.repo, commit: snapshot.commit,
        url: `https://github.com/${source.repo}/blob/${snapshot.commit}/${source.file}`,
        license: source.license, notice: sourceNotice(source),
        license_url: `https://github.com/${source.repo}/blob/${snapshot.commit}/${source.license === 'MIT' ? 'LICENSE' : source.file}` };
      const extracted = extractSource(source.repo, snapshot.text);
      const skipped = extracted.filter(row => row.skipped).map(row => ({ name: row.name, reason: row.skipped }));
      const entries = extracted.filter(row => !row.skipped).map(row => normalizeEntry(row, provenance));
      if (!entries.length) throw new Error('source_empty');
      return { source: { repo: source.repo, commit: snapshot.commit, status: 'ok', license: source.license },
        entries, diagnostic: { repo: source.repo, status: 'ok', count: entries.length, skipped } };
    } catch (error) {
      const priorSource = previous?.sources.find(item => item.repo === source.repo);
      // Keep only the failed source's provenance, so successful removals do not remain falsely attributed.
      const entries = (previous?.entries || []).filter(entry => entry.sources.some(item => item.repo === source.repo))
        .map(entry => ({ ...structuredClone(entry), sources: entry.sources.filter(item => item.repo === source.repo) }));
      return { source: { repo: source.repo, commit: priorSource?.commit || '', status: 'stale', license: source.license },
        entries, diagnostic: { repo: source.repo, status: 'stale', retained: entries.length,
          attempted_commit: snapshot?.commit || '', reason: /^schema_|^source_|^fixture_|^offline_/.test(error.message)
            ? error.message : 'fetch_or_parse_failed' } };
    }
  }));
  let entries = carryState(mergeEntries(sourceResults.flatMap(result => result.entries)), previous, now);
  if (!entries.length) {
    const error = new Error('no_entries_refusing_empty_payload');
    error.report = { sources: sourceResults.map(result => result.diagnostic), counts: { entries: 0 }, publishable: false };
    throw error;
  }
  const allStale = sourceResults.every(result => result.source.status === 'stale');
  if (allStale && previous) entries = carryState(structuredClone(previous.entries), previous, now);
  await Promise.all(entries.map(async entry => {
    entry.probe = offline || noProbe ? unchecked() : await probeEndpoint(entry, fetcher, now);
  }));
  const logos = await collectLogos(entries, fetcher, { offline, favicon, logoState, now });
  const payload = validatePayload({ schema_version: 1, revision: (previous?.revision || 0) + 1,
    published_at: now, entries, sources: sourceResults.map(result => result.source) });
  const countBy = field => Object.fromEntries([...new Set(entries.map(entry => entry[field]))].sort()
    .map(value => [value, entries.filter(entry => entry[field] === value).length]));
  const report = { publishable: true, all_sources_stale: allStale,
    sources: sourceResults.map(result => result.diagnostic),
    counts: { entries: entries.length, setup: countBy('setup'), state: countBy('state'),
      conflicts: entries.filter(entry => entry.conflicts?.length).length,
      logos: entries.filter(entry => entry.logo?.data).length,
      probes: Object.fromEntries([...new Set(entries.map(entry => entry.probe.status))].sort()
        .map(status => [status, entries.filter(entry => entry.probe.status === status).length])) },
    logos, compatibility_evidence: Object.fromEntries(entries.filter(entry => API_EVIDENCE[entry.base_url])
      .map(entry => [entry.id, API_EVIDENCE[entry.base_url].evidence])),
    notices: SOURCES.map(source => ({ repo: source.repo, license: source.license,
      notice: sourceNotice(source),
      attribution: `Derived factual provider metadata from ${source.repo}; see commit-pinned sources for original notices.` })) };
  return { payload, report };
}
