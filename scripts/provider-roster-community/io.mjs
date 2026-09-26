import { readFile, mkdir, writeFile, stat } from 'node:fs/promises';
import { join } from 'node:path';

export async function readJSON(path, maxBytes = 16 * 1024 * 1024) {
  if ((await stat(path)).size > maxBytes) throw new Error('JSON input exceeds size limit');
  return JSON.parse(await readFile(path, 'utf8'));
}

export const serializeJSON = value => `${JSON.stringify(value, null, 2)}\n`;

export async function writeJSON(directory, name, value) {
  await mkdir(directory, { recursive: true });
  await writeFile(join(directory, name), serializeJSON(value));
}

export function args(argv, allowed, switches = []) {
  const result = {};
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i];
    if ((!allowed.includes(key) && !switches.includes(key)) || key in result) throw new Error('Unknown or repeated argument');
    if (switches.includes(key)) result[key] = true;
    else {
      if (!argv[i + 1] || argv[i + 1].startsWith('--')) throw new Error('Missing argument value');
      result[key] = argv[++i];
    }
  }
  return result;
}
