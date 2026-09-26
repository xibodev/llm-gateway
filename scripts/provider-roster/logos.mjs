import { inflateSync } from 'node:zlib';
import { LOGO_SOURCES, APPROVED_SIGNUP_ORIGINS } from './catalog.mjs';
import { sha256 } from './normalize.mjs';
import { publicURL } from './network.mjs';
import { shouldSkipDiscovery, isRejectedHash } from './logo-state.mjs';

const SIGNATURE = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);
export function crc32(bytes) {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

export function validatePNG(bytes) {
  if (!Buffer.isBuffer(bytes) || bytes.length > 65536 || bytes.length < 57 || !bytes.subarray(0, 8).equals(SIGNATURE)) {
    throw new Error('invalid_png');
  }
  let offset = 8;
  let header;
  let ended = false;
  let idatEnded = false;
  let palette = false;
  const compressed = [];
  while (offset < bytes.length) {
    if (offset + 12 > bytes.length) throw new Error('invalid_png_chunk');
    const length = bytes.readUInt32BE(offset);
    if (offset + 12 + length > bytes.length) throw new Error('invalid_png_chunk');
    const type = bytes.toString('ascii', offset + 4, offset + 8);
    const data = bytes.subarray(offset + 8, offset + 8 + length);
    if (!/^[A-Za-z]{4}$/.test(type) || crc32(bytes.subarray(offset + 4, offset + 8 + length)) !== bytes.readUInt32BE(offset + 8 + length)) {
      throw new Error('invalid_png_crc');
    }
    if (!header && type !== 'IHDR') throw new Error('invalid_png_header');
    if (type === 'IHDR') {
      if (header || length !== 13) throw new Error('invalid_png_header');
      header = { width: data.readUInt32BE(0), height: data.readUInt32BE(4), depth: data[8], color: data[9] };
      const depths = { 0: [1, 2, 4, 8, 16], 2: [8, 16], 3: [1, 2, 4, 8], 4: [8, 16], 6: [8, 16] };
      if (!header.width || !header.height || header.width > 512 || header.height > 512 ||
          !depths[header.color]?.includes(header.depth) || data[10] || data[11] || data[12]) throw new Error('unsupported_png');
    } else if (type === 'PLTE') {
      if (palette || compressed.length || !length || length % 3 || length > 768) throw new Error('invalid_png_palette');
      palette = true;
    } else if (type === 'IDAT') {
      if (idatEnded) throw new Error('invalid_png_data');
      compressed.push(data);
    } else if (type === 'IEND') {
      if (length || !compressed.length || offset + 12 !== bytes.length) throw new Error('invalid_png_end');
      ended = true;
    } else {
      if (compressed.length) idatEnded = true;
      // Animated PNG and unrecognized critical chunks are outside this raster contract.
      if (/^[A-Z]/.test(type) || ['acTL', 'fcTL', 'fdAT'].includes(type)) throw new Error('unsupported_png_chunk');
    }
    offset += length + 12;
  }
  if (!ended || (header.color === 3 && !palette)) throw new Error('invalid_png_end');
  const channels = { 0: 1, 2: 3, 3: 1, 4: 2, 6: 4 }[header.color];
  const stride = Math.ceil(header.width * channels * header.depth / 8) + 1;
  const raw = inflateSync(Buffer.concat(compressed), { maxOutputLength: stride * header.height });
  if (raw.length !== stride * header.height) throw new Error('invalid_png_data');
  for (let offset = 0; offset < raw.length; offset += stride) {
    if (raw[offset] > 4) throw new Error('invalid_png_filter');
  }
  return header;
}

export function validateLogo(logo) {
  if (!logo?.data) return;
  if (logo.mime !== 'image/png' || typeof logo.data !== 'string' || logo.data.length > 87384 ||
      !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(logo.data)) throw new Error('invalid_logo');
  const bytes = Buffer.from(logo.data, 'base64');
  validatePNG(bytes);
  if (sha256(bytes) !== logo.sha256 || !logo.license || !logo.source_url) throw new Error('invalid_logo_metadata');
}

// ICO is only a directory here: never decode DIB/BMP or rasterize remote vectors.
// Validate every directory range before looking for a PNG, including skipped frames.
export function extractLogoPNG(bytes) {
  if (!Buffer.isBuffer(bytes) || bytes.length > 65536) throw new Error('invalid_logo_bytes');
  if (bytes.subarray(0, 8).equals(SIGNATURE)) {
    validatePNG(bytes);
    return bytes;
  }
  if (bytes.length < 22 || bytes.readUInt16LE(0) !== 0 || bytes.readUInt16LE(2) !== 1) throw new Error('invalid_ico');
  const count = bytes.readUInt16LE(4);
  const directoryEnd = 6 + count * 16;
  if (!count || count > 64 || directoryEnd > bytes.length) throw new Error('invalid_ico_directory');
  const frames = [];
  for (let i = 0; i < count; i++) {
    const offset = 6 + i * 16;
    const size = bytes.readUInt32LE(offset + 8);
    const start = bytes.readUInt32LE(offset + 12);
    const planes = bytes.readUInt16LE(offset + 4);
    const depth = bytes.readUInt16LE(offset + 6);
    if (bytes[offset + 3] || planes > 1 || ![0, 1, 4, 8, 16, 24, 32].includes(depth) ||
        !size || start < directoryEnd || start > bytes.length || size > bytes.length - start) throw new Error('invalid_ico_entry');
    frames.push({ start, end: start + size, width: bytes[offset] || 256, height: bytes[offset + 1] || 256 });
  }
  const ranges = [...frames].sort((a, b) => a.start - b.start);
  if (ranges[0].start !== directoryEnd || ranges.at(-1).end !== bytes.length) throw new Error('invalid_ico_extent');
  for (let i = 1; i < ranges.length; i++) {
    if (ranges[i].start !== ranges[i - 1].end) throw new Error('invalid_ico_overlap_or_gap');
  }
  let selected;
  let area = 0;
  for (const frame of frames) {
    const png = bytes.subarray(frame.start, frame.end);
    if (!png.subarray(0, 8).equals(SIGNATURE)) continue;
    const header = validatePNG(png);
    if (header.width !== frame.width || header.height !== frame.height) throw new Error('invalid_ico_dimensions');
    if (header.width * header.height > area) {
      selected = png;
      area = header.width * header.height;
    }
  }
  if (!selected) throw new Error('ico_png_missing');
  return selected;
}

export const LOGO_REQUEST_BUDGET = 128;

export async function collectLogos(entries, fetcher, { offline = false, favicon = true,
  maxRequests = LOGO_REQUEST_BUDGET, logoState = null, now = new Date().toISOString() } = {}) {
  if (!Number.isInteger(maxRequests) || maxRequests < 0 || maxRequests > LOGO_REQUEST_BUDGET) throw new Error('invalid_logo_budget');
  const cache = new Map();
  const reserved = new Set();
  const hosts = new Map();
  const diagnostics = [];
  // Reserve a deterministic candidate budget before network completion order can influence it.
  const plans = [...entries].sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0).map(entry => {
    const candidates = [];
    let reason = 'signup_origin_not_approved';
    // Gap-only: skip discovery if an accepted logo already exists in the state.
    if (logoState && shouldSkipDiscovery(logoState, entry.id, { now })) {
      return { entry, assets: [], reason: 'accepted_or_cooldown', skipped: true };
    }
    if (!offline && entry.state === 'active') {
      const approved = LOGO_SOURCES.find(asset => asset.hosts.includes(new URL(entry.base_url).hostname));
      if (approved) candidates.push(approved);
      if (favicon && entry.signup_url) {
        let signup;
        try { signup = publicURL(entry.signup_url); } catch { /* Unsafe site links never authorize discovery. */ }
        if (signup && APPROVED_SIGNUP_ORIGINS.includes(signup.origin)) {
          for (const path of ['/favicon.ico', '/favicon.png']) {
            candidates.push({ url: `${signup.origin}${path}`, license: 'unknown' });
          }
        }
      }
    } else reason = offline ? 'offline' : 'inactive';
    const assets = candidates.filter(asset => {
      if (reserved.has(asset.url)) return true;
      const host = new URL(asset.url).hostname;
      if (reserved.size >= maxRequests || (hosts.get(host) || 0) >= 2) {
        reason = 'logo_budget_exhausted';
        return false;
      }
      reserved.add(asset.url);
      hosts.set(host, (hosts.get(host) || 0) + 1);
      return true;
    });
    return { entry, assets, reason };
  });
  for (const plan of plans) {
    const { entry, assets, skipped } = plan;
    let reason = plan.reason;
    if (skipped) {
      diagnostics.push({ id: entry.id, status: 'retained', reason });
      continue;
    }
    let collected = false;
    for (const asset of assets) {
      if (!cache.has(asset.url)) cache.set(asset.url, (async () => {
        const response = await fetcher(asset.url, { maxBytes: 65536, timeoutMs: 6000 });
        if (response.status !== 200) throw new Error('logo_http');
        const bytes = extractLogoPNG(response.body);
        return { mime: 'image/png', data: bytes.toString('base64'), sha256: sha256(bytes),
          license: asset.license, source_url: asset.url };
      })());
      try {
        const logo = await cache.get(asset.url);
        // Check if this hash was previously rejected.
        if (logoState && isRejectedHash(logoState, entry.id, logo.sha256)) {
          reason = 'previously_rejected';
          continue;
        }
        entry.logo = logo;
        collected = true;
        diagnostics.push({ id: entry.id, status: 'collected', source_url: asset.url });
        break;
      } catch { reason = 'logo_unavailable_or_invalid'; }
    }
    if (!collected) {
      diagnostics.push({ id: entry.id, status: entry.logo?.data ? 'retained' : 'initials', reason });
    }
  }
  return diagnostics.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
}
