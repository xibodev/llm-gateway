import { lookup } from 'node:dns/promises';
import https from 'node:https';
import { isIP } from 'node:net';
import { withinUTF8, MAX_URL_BYTES } from './limits.mjs';

export class FetchError extends Error {
  constructor(code) { super(code); this.code = code; }
}

const ipv4Ranges = [
  ['0.0.0.0', 8], ['10.0.0.0', 8], ['100.64.0.0', 10], ['127.0.0.0', 8],
  ['169.254.0.0', 16], ['172.16.0.0', 12], ['192.0.0.0', 24], ['192.0.2.0', 24],
  ['192.88.99.0', 24], ['192.168.0.0', 16], ['198.18.0.0', 15],
  ['192.31.196.0', 24], ['192.52.193.0', 24], ['192.175.48.0', 24],
  ['198.51.100.0', 24], ['203.0.113.0', 24], ['224.0.0.0', 3],
];
const v4 = address => address.split('.').reduce((n, part) => (n << 8n) | BigInt(part), 0n);
function v6(address) {
  const [left, right] = address.split('::');
  const a = left ? left.split(':') : [];
  const b = right ? right.split(':') : [];
  return [...a, ...Array(8 - a.length - b.length).fill('0'), ...b]
    .reduce((n, part) => (n << 16n) | BigInt(`0x${part}`), 0n);
}
const prefix = (value, base, bits, width) => value >> BigInt(width - bits) === base >> BigInt(width - bits);

export function isPublicAddress(address) {
  if (isIP(address) === 4) {
    return !ipv4Ranges.some(([base, bits]) => prefix(v4(address), v4(base), bits, 32));
  }
  if (isIP(address) !== 6 || address.includes('%') || address.includes('.')) return false;
  const value = v6(address);
  // Only global unicast; reject transition, protocol-assignment and documentation space.
  return prefix(value, v6('2000::'), 3, 128) && ![
    ['2001::', 23], ['2001:db8::', 32], ['2002::', 16], ['3fff::', 20], ['2620:4f:8000::', 48],
  ].some(([base, bits]) => prefix(value, v6(base), bits, 128));
}

export function publicURL(input) {
  if (!withinUTF8(input, MAX_URL_BYTES)) throw new FetchError('blocked_url');
  let url;
  try { url = new URL(input); } catch { throw new FetchError('blocked_url'); }
  if (url.protocol !== 'https:' || url.username || url.password || url.port ||
      url.hash || url.search || /[\\\s{}<>]/.test(input) || /%7[b-d]/i.test(url.pathname)) {
    throw new FetchError('blocked_url');
  }
  const host = url.hostname.replace(/^\[|\]$/g, '');
  if (!isIP(host) && (host.length > 253 || host.split('.').some(label =>
    !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label) || label.length > 63))) {
    throw new FetchError('blocked_address');
  }
  if (host.endsWith('.') || (!isIP(host) && (!host.includes('.') ||
      /(?:^|\.)(?:localhost|local|internal|test|invalid|onion)$/.test(host))) ||
      (isIP(host) && !isPublicAddress(host))) throw new FetchError('blocked_address');
  return url;
}

export class Scheduler {
  constructor(concurrency = 8, perHost = 2) {
    if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 8 ||
        !Number.isInteger(perHost) || perHost < 1 || perHost > 2) throw new Error('invalid concurrency');
    this.limit = concurrency;
    this.perHost = perHost;
    this.active = 0;
    this.hosts = new Map();
    this.queue = [];
  }
  run(host, job) {
    return new Promise((resolve, reject) => {
      this.queue.push({ host, job, resolve, reject });
      this.drain();
    });
  }
  drain() {
    while (this.active < this.limit) {
      const index = this.queue.findIndex(item => (this.hosts.get(item.host) || 0) < this.perHost);
      if (index < 0) return;
      const item = this.queue.splice(index, 1)[0];
      this.active++;
      this.hosts.set(item.host, (this.hosts.get(item.host) || 0) + 1);
      Promise.resolve().then(item.job).then(item.resolve, item.reject).finally(() => {
        this.active--;
        this.hosts.set(item.host, this.hosts.get(item.host) - 1);
        this.drain();
      });
    }
  }
}

// Injection points are for tests. Production never uses fetch(), proxies, cookies or ambient tokens.
export function createSafeFetcher({ resolver = lookup, request = https.request, scheduler = new Scheduler() } = {}) {
  return async function safeFetch(input, { maxBytes = 256 * 1024, timeoutMs = 8000 } = {}) {
    const url = publicURL(input);
    return scheduler.run(url.hostname, async () => {
      let req;
      let expired = false;
      let timer;
      const deadline = new Promise((_, reject) => {
        timer = setTimeout(() => {
          expired = true;
          req?.destroy(new FetchError('timeout'));
          reject(new FetchError('timeout'));
        }, timeoutMs);
      });
      const operation = async () => {
        const hostname = url.hostname.replace(/^\[|\]$/g, '');
        const addresses = isIP(hostname) ? [{ address: hostname, family: isIP(hostname) }]
          : await resolver(hostname, { all: true, verbatim: true });
        if (expired) throw new FetchError('timeout');
        if (!addresses.length || addresses.some(a => !isPublicAddress(a.address) || isIP(a.address) !== a.family)) {
          throw new FetchError('blocked_address');
        }
        const pinned = [...addresses].sort((a, b) => a.address < b.address ? -1 : a.address > b.address ? 1 : 0)[0];
        return new Promise((resolve, reject) => {
          req = request(url, {
            method: 'GET', agent: false, family: pinned.family,
            servername: isIP(hostname) ? undefined : hostname,
            rejectUnauthorized: true, maxHeaderSize: 16384,
            lookup: (_host, options, callback) => options.all
              ? callback(null, [pinned]) : callback(null, pinned.address, pinned.family),
            headers: { 'User-Agent': 'llm-gateway-provider-roster', Accept: '*/*', 'Accept-Encoding': 'identity' },
          }, res => {
            const fail = code => { res.destroy(); reject(new FetchError(code)); };
            if (res.statusCode >= 300 && res.statusCode < 400) return fail('blocked_redirect');
            if (res.headers['content-encoding'] && res.headers['content-encoding'] !== 'identity') return fail('blocked_encoding');
            if (Number(res.headers['content-length']) > maxBytes) return fail('body_limit');
            let size = 0;
            const chunks = [];
            res.on('data', chunk => {
              size += chunk.length;
              if (size > maxBytes) return fail('body_limit');
              chunks.push(chunk);
            });
            res.on('error', reject);
            res.on('aborted', () => reject(new FetchError('aborted')));
            res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
          });
          req.on('error', reject);
          req.end();
        });
      };
      try { return await Promise.race([operation(), deadline]); }
      catch (error) { throw error instanceof FetchError ? error : new FetchError('network_error'); }
      finally { clearTimeout(timer); }
    });
  };
}

export async function probeEndpoint(entry, fetcher, now) {
  if (entry.state !== 'active' || entry.protocol === 'unknown') {
    return { status: 'not_checked', checked_at: '', http_status: 0 };
  }
  try {
    const response = await fetcher(entry.base_url, { maxBytes: 64 * 1024, timeoutMs: 6000 });
    const status = response.status === 401 || response.status === 403 ? 'auth_required'
      : response.status === 429 ? 'rate_limited'
        : response.status >= 200 && response.status < 300 ? 'reachable' : 'failed';
    return { status, checked_at: now, http_status: response.status };
  } catch (error) {
    return { status: error.code?.startsWith('blocked_') ? 'blocked' : 'failed', checked_at: now, http_status: 0 };
  }
}
