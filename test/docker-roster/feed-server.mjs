import http from 'node:http';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const PORT = parseInt(process.env.FEED_PORT || '9090', 10);
const SCENARIO = process.env.SCENARIO || 'valid';

const baseRoster = {
  schema_version: 1,
  published_at: '2026-09-09T00:00:00.000Z',
  revision: 1,
  entries: [
    {
      id: 'openai',
      name: 'OpenAI',
      state: 'active',
      base_url: 'https://api.openai.com/v1',
      inference: { completions: '/chat/completions' },
      auth: 'api_key',
      supports: { vision: true, temperature: true },
      signup_url: 'https://platform.openai.com',
      logo: { mime: 'image/png', data: '', license: 'proprietary' },
    },
    {
      id: 'groq',
      name: 'Groq',
      state: 'active',
      base_url: 'https://api.groq.com/openai/v1',
      inference: { completions: '/chat/completions' },
      auth: 'api_key',
      supports: {},
      signup_url: 'https://console.groq.com',
      logo: { mime: 'image/png', data: '', license: 'proprietary' },
    },
  ],
};

function getPayload() {
  switch (SCENARIO) {
    case 'valid':
      return JSON.stringify(baseRoster);
    case 'new_revision':
      return JSON.stringify({ ...baseRoster, revision: 2,
        entries: [...baseRoster.entries, {
          id: 'fireworks',
          name: 'Fireworks AI',
          state: 'active',
          base_url: 'https://api.fireworks.ai/inference/v1',
          inference: { completions: '/chat/completions' },
          auth: 'api_key',
          supports: {},
          signup_url: 'https://fireworks.ai',
          logo: { mime: 'image/png', data: '', license: 'proprietary' },
        }],
      });
    case 'invalid_json':
      return '{invalid json content';
    case 'oversized':
      return JSON.stringify({ ...baseRoster, padding: 'x'.repeat(5 * 1024 * 1024) });
    case 'empty':
      return JSON.stringify({ schema_version: 1, revision: 1, entries: [] });
    case 'unavailable':
      return null;
    default:
      return JSON.stringify(baseRoster);
  }
}

const server = http.createServer((req, res) => {
  console.log(`[${new Date().toISOString()}] ${req.method} ${req.url}`);

  if (req.url === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"status":"ok"}');
    return;
  }

  if (req.url === '/feed') {
    const payload = getPayload();
    if (payload === null) {
      res.writeHead(503, { 'content-type': 'text/plain' });
      res.end('Service Unavailable');
      return;
    }
    if (SCENARIO === 'invalid_json') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(payload);
      return;
    }
    if (SCENARIO === 'oversized') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(payload);
      return;
    }
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(payload);
    return;
  }

  res.writeHead(404);
  res.end('Not Found');
});

server.listen(PORT, '0.0.0.0', () => {
  console.log(`Fixture feed server listening on port ${PORT} (scenario: ${SCENARIO})`);
});
