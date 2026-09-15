#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { evaluatePolicy } from "./core.mjs";

const reportPath = process.argv[2];
if (!reportPath) {
  console.error("usage: node policy.mjs <report.json>");
  process.exit(2);
}
const report = JSON.parse(await readFile(reportPath, "utf8"));
const policy = evaluatePolicy(report);
console.log(JSON.stringify(policy, null, 2));
if (policy.release_blocking) process.exitCode = 1;
