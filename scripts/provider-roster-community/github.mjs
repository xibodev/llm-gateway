const API = 'https://api.github.com';

export function repositoryPath(repository) {
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository)) throw new Error('Invalid GitHub repository');
  return `/repos/${repository}`;
}

export class GitHub {
  constructor({ token = '', fetcher = fetch, sleep = ms => new Promise(resolve => setTimeout(resolve, ms)), maxPages = 20, maxRequests = 1000 } = {}) {
    Object.assign(this, { token, fetcher, sleep, maxPages, maxRequests });
    this.requests = 0;
  }

  async get(path) {
    if (!path.startsWith('/repos/') || path.includes('://')) throw new Error('Invalid API path');
    for (let attempt = 0; attempt < 4; attempt++) {
      if (++this.requests > this.maxRequests) throw new Error('GitHub request budget exceeded');
      const response = await this.fetcher(`${API}${path}`, {
        headers: { Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28',
          ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}) },
        signal: AbortSignal.timeout(20000), redirect: 'error',
      });
      if (response.ok) return response.json();
      if (![403, 429, 500, 502, 503, 504].includes(response.status) || attempt === 3) {
        throw new Error(`GitHub request failed (${response.status})`);
      }
      // Bound even server-supplied delays; exhaustion is a stale snapshot, never an empty one.
      const retry = Number(response.headers.get('retry-after'));
      await this.sleep(Math.min(30000, Math.max(1000 * 2 ** attempt, Number.isFinite(retry) ? retry * 1000 : 0)));
    }
  }

  async list(path, field) {
    const items = [];
    for (let page = 1; page <= this.maxPages; page++) {
      const result = await this.get(`${path}${path.includes('?') ? '&' : '?'}per_page=100&page=${page}`);
      const batch = field ? result[field] : result;
      if (!Array.isArray(batch)) throw new Error('Invalid GitHub list response');
      items.push(...batch);
      if (batch.length < 100) return items;
    }
    throw new Error('GitHub pagination limit exceeded');
  }
}

export function issueEntryID(body) {
  const matches = [...String(body ?? '').matchAll(/^### Roster entry ID\s*\r?\n\s*\r?\n([^\r\n]+)(?=\r?\n|$)/gm)];
  if (matches.length !== 1) return null;
  const value = matches[0][1].trim();
  return /^endpoint-[a-f0-9]{16,64}$/.test(value) ? value : null;
}

export const isConfirmation = (body, id) => String(body ?? '').trim() === `Roster confirmation: ${id}`;
export const numericID = value => Number.isSafeInteger(value) && value > 0;
export const isHuman = user => user?.type === 'User' && numericID(user.id) && !/\[bot\]$/i.test(user.login ?? '');

export async function fetchReports(api, repository, knownIDs, issueBindings = {}) {
  const root = repositoryPath(repository);
  // Do not depend on a label being pre-created, and do not let closing an issue erase a report.
  const issues = await api.list(`${root}/issues?state=all&sort=created&direction=asc`);
  const reports = [];
  for (const issue of issues) {
    const entryID = issueEntryID(issue.body);
    if (issue.pull_request || (!knownIDs.has(entryID) && !Object.hasOwn(issueBindings, issue.number))) continue;
    if (!numericID(issue.number) || !numericID(issue.id)) throw new Error('Invalid issue identifier');
    if (Object.hasOwn(issueBindings, issue.number) && issueBindings[issue.number] !== entryID) {
      reports.push({ entryID, issue, rootReactions: [], confirmations: [] });
      continue;
    }
    const path = `${root}/issues/${issue.number}`;
    const rootReactions = await api.list(`${path}/reactions`);
    const comments = await api.list(`${path}/comments`);
    const confirmations = [];
    for (const comment of comments) {
      if (!isConfirmation(comment.body, entryID) || !isHuman(comment.user)) continue;
      if (!numericID(comment.id)) throw new Error('Invalid comment identifier');
      confirmations.push({ ...comment, reactions: await api.list(`${root}/issues/comments/${comment.id}/reactions`) });
    }
    reports.push({ entryID, issue, rootReactions, confirmations });
  }
  return reports;
}
