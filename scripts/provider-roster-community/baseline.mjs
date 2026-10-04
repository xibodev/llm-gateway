import { appendFile } from 'node:fs/promises';
import { GitHub, repositoryPath } from './github.mjs';
import { pathToFileURL } from 'node:url';

// The newest successful scheduled or manual roster run on this branch, if any,
// and whether its staging artifact can still be downloaded.
async function latestStaging(api, env) {
  const repository = repositoryPath(env.GITHUB_REPOSITORY ?? '');
  const branch = env.GITHUB_REF_NAME;
  if (!branch) throw new Error('Missing workflow context');
  const runs = await api.list(`${repository}/actions/workflows/provider-roster.yml/runs?branch=${encodeURIComponent(branch)}&status=success`, 'workflow_runs');
  const latest = runs.filter(run => String(run.id) !== env.GITHUB_RUN_ID &&
    run.head_branch === branch && ['schedule', 'workflow_dispatch'].includes(run.event)).sort((a, b) => b.run_number - a.run_number)[0];
  if (!latest) return null;
  if (!Number.isSafeInteger(latest.id) || latest.id <= 0) throw new Error('Invalid run ID');
  const artifacts = await api.list(`${repository}/actions/runs/${latest.id}/artifacts`, 'artifacts');
  const artifact = artifacts.find(item => item.name === 'provider-roster-staging');
  return { id: latest.id, available: Boolean(artifact && !artifact.expired) };
}

export async function findBaseline(api, env = process.env) {
  const latest = await latestStaging(api, env);
  if (!latest) {
    if (env.ALLOW_BOOTSTRAP !== 'true' || env.GITHUB_EVENT_NAME !== 'workflow_dispatch') throw new Error('No baseline; explicit manual bootstrap required');
    return 'found=false\n';
  }
  // Never silently fall back to older state: that could discard a restore or a tombstone.
  if (!latest.available) throw new Error('Latest baseline artifact unavailable; recover state manually');
  return `found=true\nrun_id=${latest.id}\n`;
}

// Website deployments republish the newest roster, so a documentation change
// never removes the feed installations fetch. An older artifact could
// resurrect a withdrawn entry, so without the newest one the site ships
// without a feed and the caller is warned instead.
export async function findPublished(api, env = process.env, warn = () => {}) {
  const latest = await latestStaging(api, env);
  if (latest?.available) return `found=true\nrun_id=${latest.id}\n`;
  warn(latest ? `roster run ${latest.id} has no unexpired provider-roster-staging artifact` : 'no successful provider roster run exists');
  return 'found=false\n';
}

async function main(published) {
  if (!process.env.GITHUB_OUTPUT) throw new Error('Missing workflow output path');
  const api = new GitHub({ token: process.env.GITHUB_TOKEN });
  const result = published
    ? await findPublished(api, process.env, reason => console.log(`::warning title=Provider roster omitted::Deploying without roster/payload.json: ${reason}.`))
    : await findBaseline(api);
  await appendFile(process.env.GITHUB_OUTPUT, result);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const published = process.argv.slice(2).includes('--published');
  main(published).catch(() => {
    console.error(published
      ? 'Published roster lookup failed; refusing to deploy without the feed.'
      : 'Previous roster state unavailable; refusing implicit state reset.');
    process.exitCode = 1;
  });
}
