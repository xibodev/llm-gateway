#!/usr/bin/env node
import { parseArgs } from 'node:util';
import { readFile, mkdir, writeFile, rename, rm } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { buildRoster } from './pipeline.mjs';

export async function main(args = process.argv.slice(2)) {
  const { values } = parseArgs({ args, options: {
    output: { type: 'string' }, previous: { type: 'string' }, fixtures: { type: 'string' },
    offline: { type: 'boolean', default: false }, 'no-probe': { type: 'boolean', default: false },
    favicon: { type: 'boolean' }, 'no-favicon': { type: 'boolean', default: false }, help: { type: 'boolean', default: false },
  } });
  if (values.help) {
    console.log('node scripts/provider-roster/build.mjs --output DIR [--previous payload.json] [--fixtures DIR] [--offline] [--no-probe] [--no-favicon]');
    return;
  }
  if (!values.output) throw new Error('--output is required');
  const output = resolve(values.output);
  const previous = values.previous ? JSON.parse(await readFile(resolve(values.previous), 'utf8')) : undefined;
  await mkdir(output, { recursive: true });
  const atomicJSON = async (name, data) => {
    const target = join(output, name);
    const temporary = `${target}.${process.pid}.tmp`;
    try {
      await writeFile(temporary, `${JSON.stringify(data, null, 2)}\n`, { flag: 'wx' });
      await rename(temporary, target);
    } finally { await rm(temporary, { force: true }); }
  };
  try {
    const result = await buildRoster({ previous, fixtures: values.fixtures && resolve(values.fixtures),
      offline: values.offline, noProbe: values['no-probe'], favicon: !values['no-favicon'] });
    await atomicJSON('report.json', result.report);
    await atomicJSON('payload.json', result.payload);
    console.log(JSON.stringify(result.report.counts));
    return result;
  } catch (error) {
    await atomicJSON('report.json', error.report || { publishable: false, reason: 'collection_failed' });
    throw error;
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => { console.error('Roster build failed; inspect report.json in the output directory.'); process.exitCode = 1; });
}
