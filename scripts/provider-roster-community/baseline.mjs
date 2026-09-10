import { appendFile } from 'node:fs/promises';
import { GitHub, repositoryPath } from './github.mjs';
import { pathToFileURL } from 'node:url';

export async function findBaseline(api, env = process.env) {
  const repository = repositoryPath(env.GITHUB_REPOSITORY ?? '');
  const branch = env.GITHUB_REF_NAME;
  if (!branch) throw new Error('Missing workflow context');
  const runs = await api.list(`${repository}/actions/workflows/provider-roster.yml/runs?branch=${encodeURIComponent(branch)}&status=success`, 'workflow_runs');
  const previous = runs.filter(run => String(run.id) !== env.GITHUB_RUN_ID &&
    run.head_branch === branch && ['schedule', 'workflow_dispatch'].includes(run.event)).sort((a, b) => b.run_number - a.run_number)[0];
  if (!previous) {
    if (env.ALLOW_BOOTSTRAP !== 'true' || env.GITHUB_EVENT_NAME !== 'workflow_dispatch') throw new Error('No baseline; explicit manual bootstrap required');
    return 'found=false\n';
  }
  if (!Number.isSafeInteger(previous.id) || previous.id <= 0) throw new Error('Invalid run ID');
  const artifacts = await api.list(`${repository}/actions/runs/${previous.id}/artifacts`, 'artifacts');
  const artifact = artifacts.find(item => item.name === 'provider-roster-staging');
  // Never silently fall back to older state: that could discard a restore or a tombstone.
  if (!artifact || artifact.expired) throw new Error('Latest baseline artifact unavailable; recover state manually');
  return `found=true\nrun_id=${previous.id}\n`;
}

async function main() {
  if (!process.env.GITHUB_OUTPUT) throw new Error('Missing workflow output path');
  const result = await findBaseline(new GitHub({ token: process.env.GITHUB_TOKEN }));
  await appendFile(process.env.GITHUB_OUTPUT, result);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => { console.error('Previous roster state unavailable; refusing implicit state reset.'); process.exitCode = 1; });
}
