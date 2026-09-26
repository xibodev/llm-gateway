import { createHash, generateKeyPairSync, sign, verify } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { args, readJSON, writeJSON, serializeJSON } from './io.mjs';
import { validateState } from './reconcile.mjs';
import { validatePayload as validateCollectorPayload, MAX_ENTRIES } from '../provider-roster/contract.mjs';

export { MAX_ENTRIES };
export const MAX_ENVELOPE_BYTES = 4 * 1024 * 1024;

export function validateEnvelopeSize(envelope) {
  // Measure the exact on-disk representation, including base64, formatting and newline.
  if (Buffer.byteLength(serializeJSON(envelope)) > MAX_ENVELOPE_BYTES) throw new Error('Envelope exceeds size limit');
}

function base64(value) {
  if (typeof value !== 'string' || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) throw new Error('Invalid base64');
  return Buffer.from(value, 'base64');
}

export function validatePayload(payload) {
  if (payload?.entries?.length > MAX_ENTRIES) throw new Error('Roster exceeds entry limit');
  validateCollectorPayload(payload);
  if (!payload.entries.length) throw new Error('Cannot sign an empty roster');
  return payload;
}

export function signStaging(bytes) {
  if (bytes.length > MAX_ENVELOPE_BYTES) throw new Error('Payload exceeds size limit');
  validatePayload(JSON.parse(bytes.toString('utf8')));
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  const raw = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  const keyID = `staging-test-${createHash('sha256').update(raw).digest('hex').slice(0, 16)}`;
  const envelope = { key_id: keyID, payload: bytes.toString('base64'), signature: sign(null, bytes, privateKey).toString('base64') };
  const trust = { key_id: keyID, algorithm: 'Ed25519', public_key: raw.toString('base64'), public_key_pem: publicKey.export({ format: 'pem', type: 'spki' }), trust: 'TEST ONLY: ephemeral artifact key, not production trust' };
  verifyEnvelope(envelope, trust);
  return { envelope, trust };
}

export function verifyEnvelope(envelope, trust) {
  validateEnvelopeSize(envelope);
  if (envelope.key_id !== trust.key_id) throw new Error('Unknown signing key');
  const bytes = base64(envelope.payload);
  const signature = base64(envelope.signature);
  if (signature.length !== 64 || !verify(null, bytes, trust.public_key_pem, signature)) throw new Error('Invalid signature');
  validatePayload(JSON.parse(bytes.toString('utf8')));
  return bytes;
}

async function main() {
  const options = args(process.argv.slice(2), ['--input', '--output']);
  if (!options['--input'] || !options['--output']) throw new Error('Required: --input and --output');
  const input = options['--input'], output = options['--output'];
  // Bound JSON before loading the exact bytes that are signed.
  await readJSON(join(input, 'payload.json'), MAX_ENVELOPE_BYTES);
  const bytes = await readFile(join(input, 'payload.json'));
  const state = validateState(await readJSON(join(input, 'report-state.json')));
  const plan = await readJSON(join(input, 'actions-plan.json'));
  if (plan.dry_run !== true || !Array.isArray(plan.actions)) throw new Error('Expected dry-run plan');
  const { envelope, trust } = signStaging(bytes);
  await mkdir(output, { recursive: true });
  await writeFile(join(output, 'payload.json'), bytes);
  await writeJSON(output, 'envelope.json', envelope);
  await writeJSON(output, 'public-key.json', trust);
  await writeJSON(output, 'report-state.json', state);
  await writeJSON(output, 'actions-plan.json', plan);
  await writeFile(join(output, 'TEST-TRUST.txt'), 'STAGING TEST ARTIFACT ONLY\nThe included ephemeral public key verifies this artifact, not a production publisher.\nNever import this key as production trust. No private key is retained. Logos are inline in the signed payload.\n');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(() => { console.error('Staging artifact signing failed.'); process.exitCode = 1; });
}
