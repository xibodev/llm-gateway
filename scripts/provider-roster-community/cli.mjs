import { GitHub, fetchReports, repositoryPath } from './github.mjs';
import { emptyState, reconcile, validateState, bindStateRepository } from './reconcile.mjs';
import { args, readJSON, writeJSON } from './io.mjs';
import { pathToFileURL } from 'node:url';

export async function main(argv = process.argv.slice(2), { api = new GitHub({ token: process.env.GITHUB_TOKEN }) } = {}) {
  const options = args(argv, ['--payload', '--previous', '--repository', '--output', '--overrides'], ['--bootstrap']);
  if (!options['--payload'] || !options['--output']) throw new Error('Required: --payload and --output');
  const repository = options['--repository'] ?? process.env.GITHUB_REPOSITORY;
  repositoryPath(repository ?? '');
  if (Boolean(options['--previous']) === Boolean(options['--bootstrap'])) throw new Error('Choose --previous state.json or explicit --bootstrap');
  const previous = bindStateRepository(options['--previous'] ? validateState(await readJSON(options['--previous'])) : emptyState(), repository);
  const payload = await readJSON(options['--payload']);
  const restores = options['--overrides'] ? (await readJSON(options['--overrides'])).restores : [];
  if (!Array.isArray(restores)) throw new Error('Invalid restores file');
  if (process.env.ROSTER_RESTORE_ENTRY_ID || process.env.ROSTER_RESTORE_SINCE) {
    if (process.env.GITHUB_EVENT_NAME !== 'workflow_dispatch') throw new Error('Workflow restores require manual dispatch');
    restores.push({ entry_id: process.env.ROSTER_RESTORE_ENTRY_ID, incident_since: process.env.ROSTER_RESTORE_SINCE });
  }
  let reports = [], complete = true;
  try {
    reports = await fetchReports(api, repository,
      new Set([...payload.entries.map(e => e.id), ...Object.keys(previous.entries)]), previous.issue_bindings);
  } catch {
    // Never serialize GitHub content, URLs, or credentials from thrown errors.
    complete = false;
    console.error('Reports unavailable or incomplete; preserving previous community state.');
  }
  // A first run has no safe baseline to preserve.
  if (!complete && options['--bootstrap']) throw new Error('Cannot bootstrap without a complete report snapshot');
  const result = reconcile({ payload, previous, reports, complete, repository, restores });
  await writeJSON(options['--output'], 'payload.json', result.payload);
  await writeJSON(options['--output'], 'report-state.json', result.state);
  await writeJSON(options['--output'], 'actions-plan.json', result.plan);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => { console.error('Roster reconciliation failed; no replacement artifact should be signed.'); process.exitCode = 1; });
}
