export const SOURCES = Object.freeze([
  { repo: 'mnfst/awesome-free-llm-apis', file: 'data.json', fixture: 'mnfst.json', license: 'CC0-1.0' },
  { repo: 'nejib1/Free-LLM', file: 'README.md', fixture: 'nejib.md', license: 'MIT' },
  { repo: 'open-free-llm-api/awesome-freellm-apis', file: 'README.md', fixture: 'open-free.md', license: 'MIT' },
  { repo: 'oniondas/Awesome-Hidden-AI-Credits-Free', file: 'README.md', fixture: 'onion.md', license: 'CC0-1.0' },
]);

export const plainText = text => String(text ?? '').replace(/<[^>]*>/g, '')
  .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1').replace(/[*`]/g, '')
  .replace(/&amp;/g, '&').replace(/[\u0000-\u001f]/g, ' ').replace(/\s+/g, ' ').trim();
const link = text => text.match(/\[[^\]]*\]\((https:\/\/[^\s)]+)\)/)?.[1]
  || text.match(/href=["'](https:\/\/[^"']+)["']/)?.[1] || '';
const endpoint = text => text.match(/`(https:\/\/[^`]+)`/)?.[1] || '';
// Directory cells may append a warning after the linked name (for example a deposit condition).
const providerName = text => plainText(text.match(/^\[([^\]]+)\]\(/)?.[1] || text);
const key = text => providerName(text).toLowerCase();
const requireText = (value, field) => {
  if (typeof value !== 'string' || !value.trim()) throw new Error(`schema_${field}`);
  return value;
};

export function classifyAuth(text) {
  const value = plainText(text).toLowerCase();
  const none = /\bno (?:api )?key(?: required)?\b|\bwithout (?:an? )?(?:api )?key\b|\bunauthenticated access\b/.test(value);
  const positive = value.replace(/\bno (?:api )?key(?: required)?\b|\bwithout (?:an? )?(?:api )?key\b/g, '');
  const required = /\b(?:api key|token) (?:is )?required\b|\brequires (?:an? )?(?:api key|token)\b/.test(positive);
  return none === required ? 'unknown' : none ? 'none' : 'api_key';
}

export function classifyOffer(text) {
  const value = plainText(text).toLowerCase();
  const offers = [];
  if (/\bfree tier\b|\bpermanent(?:ly)? free\b|\bfree models\b|\bfree access\b/.test(value)) offers.push('free_tier');
  if (/(?:credit|allowance).{0,35}(?:daily|monthly|per month|every day|renew)|(?:daily|monthly|\/month).{0,35}(?:credit|allowance)/.test(value)) offers.push('recurring_credit');
  if (/\btrial\b|\bone[- ]time\b|\b(?:starting|signup|sign.up|instant) bonus\b|\bstarting credits\b/.test(value)) offers.push('trial');
  if (/\bpaid only\b|\bno free tier\b/.test(value)) return 'paid';
  return offers.length === 1 ? offers[0] : 'unknown';
}

function marked(text, start, end) {
  if (text.split(start).length !== 2 || text.split(end).length !== 2) throw new Error('schema_markers');
  const a = text.indexOf(start) + start.length;
  const b = text.indexOf(end);
  if (b <= a) throw new Error('schema_markers');
  return text.slice(a, b);
}

function table(text, expected) {
  const lines = text.split(/\r?\n/).map(line => line.trim()).filter(line => line.startsWith('|'));
  const cells = line => line.slice(1, line.endsWith('|') ? -1 : undefined).split(/(?<!\\)\|/).map(cell => cell.trim());
  if (lines.length < 3) throw new Error('schema_empty_table');
  const headers = cells(lines[0]).map(plainText);
  if (expected.some(header => !headers.includes(header)) || !cells(lines[1]).every(cell => /^:?-+:?$/.test(cell))) {
    throw new Error('schema_table_header');
  }
  return lines.slice(2).map(line => {
    const row = cells(line);
    if (row.length !== headers.length || !plainText(row[0])) throw new Error('schema_table_row');
    return Object.fromEntries(headers.map((header, i) => [header, row[i]]));
  });
}

function jsonSource(text) {
  const data = JSON.parse(text);
  if (!Array.isArray(data.providers) || !data.providers.length) throw new Error('schema_providers');
  return data.providers.map(row => {
    requireText(row.name, 'name');
    requireText(row.baseUrl, 'base_url');
    requireText(row.url, 'url');
    requireText(row.description, 'description');
    if (!['provider_api', 'inference_provider'].includes(row.category) || !Array.isArray(row.models)) {
      throw new Error('schema_provider');
    }
    return { name: row.name, base_url: row.baseUrl, signup_url: row.url,
      auth: classifyAuth(row.description), offer: classifyOffer(row.description),
      requirements: [plainText(row.description)], endpointEvidence: true };
  });
}

function readmeTables(text, variant) {
  const isNejib = variant === 'nejib';
  const block = name => isNejib
    ? marked(text, `<!--TABLE:${name}:START-->`, `<!--TABLE:${name}:END-->`)
    : marked(text, `<!-- BEGIN_${name} -->`, `<!-- END_${name} -->`);
  const groups = isNejib ? [['PERMANENT', 'free_tier'], ['RENEWABLE', 'recurring_credit'], ['TRIAL', 'trial']]
    : [['PERMANENT_FREE', 'free_tier'], ['RENEWABLE', 'recurring_credit']];
  const offers = new Map();
  for (const [group, offer] of groups) {
    for (const row of table(block(group), ['Provider'])) {
      const name = key(row.Provider);
      if (offers.has(name)) throw new Error('schema_duplicate_provider');
      const rowOffer = classifyOffer(Object.entries(row).filter(([k]) => !['Provider', 'Get API Key', 'Key Models'].includes(k)).map(([, v]) => v).join(' '));
      const conflict = rowOffer !== 'unknown' && rowOffer !== offer;
      offers.set(name, { offer: conflict ? 'unknown' : offer,
        conflicts: conflict ? [`offer: ${[offer, rowOffer].sort().join(' | ')}`] : [],
        requirements: Object.entries(row).filter(([k]) => k !== 'Get API Key')
          .map(([k, v]) => `${k}: ${plainText(v)}`) });
    }
  }
  const rows = table(block(isNejib ? 'QUICKREF' : 'QUICK_REF'), ['Provider', 'Base URL', 'Get API Key']);
  return rows.map(row => {
    const details = offers.get(key(row.Provider));
    if (!details) throw new Error('schema_unmatched_provider');
    const base = endpoint(row['Base URL']);
    const signup = link(row['Get API Key']) || link(row.Provider);
    // Cline currently has neither URL; preserve a diagnostic rather than invent an endpoint.
    if (!base && !signup) return { skipped: 'missing_endpoint', name: plainText(row.Provider) };
    return { name: providerName(row.Provider), base_url: base || signup, signup_url: signup,
      auth: 'unknown', ...details, endpointEvidence: Boolean(base) };
  });
}

function proseSource(text) {
  const sections = ['Large Language Models (LLMs)', 'Image Generation', 'Audio & Speech', 'Embeddings', 'Multi-Modal & All-in-One'];
  const output = [];
  for (const section of sections) {
    const heading = `## ${section}`;
    const lines = text.split(/\r?\n/);
    const index = lines.indexOf(heading);
    if (index < 0) throw new Error('schema_prose_section');
    const next = lines.findIndex((line, i) => i > index && line.startsWith('## '));
    const body = lines.slice(index + 1, next < 0 ? undefined : next);
    let count = 0;
    for (const line of body) {
      if (!/^\* /.test(line)) continue;
      const match = line.match(/^\* \[([^\]]+)\]\((https:\/\/[^\s)]+)\)\s+(.+)$/);
      if (!match) throw new Error('schema_prose_item');
      count++;
      output.push({ name: match[1], base_url: match[2], signup_url: match[2],
        auth: classifyAuth(match[3]), offer: classifyOffer(match[3]),
        requirements: [plainText(match[3])], endpointEvidence: false });
    }
    if (!count) throw new Error('schema_prose_empty');
  }
  return output;
}

export function extractSource(repo, text) {
  if (typeof text !== 'string' || text.length > 2 * 1024 * 1024) throw new Error('schema_size');
  switch (repo) {
    case SOURCES[0].repo: return jsonSource(text);
    case SOURCES[1].repo: return readmeTables(text, 'nejib');
    case SOURCES[2].repo: return readmeTables(text, 'open-free');
    case SOURCES[3].repo: return proseSource(text);
    default: throw new Error('untrusted_source');
  }
}
