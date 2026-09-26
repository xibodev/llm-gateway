// Byte bounds shared with go/internal/roster's payload and URL validators.
export const MAX_NAME_BYTES = 256;
export const MAX_ID_BYTES = 128;
export const MAX_DESCRIPTION_BYTES = 8192;
export const MAX_URL_BYTES = 4096;

export function truncateUTF8(value, maxBytes) {
  let bytes = 0;
  let result = '';
  for (const codepoint of value) {
    const size = Buffer.byteLength(codepoint, 'utf8');
    if (bytes + size > maxBytes) break;
    result += codepoint;
    bytes += size;
  }
  return result;
}

export const withinUTF8 = (value, maxBytes) => typeof value === 'string' &&
  value.isWellFormed() && Buffer.byteLength(value, 'utf8') <= maxBytes;
