#!/usr/bin/env node
import { existsSync } from "node:fs";
import { resolve } from "node:path";

const platform = process.platform;
const arch = process.arch;
const suffix = platform === "win32" ? ".exe" : "";
const packageName = `claude-code-${platform}-${arch}`;
const candidate = resolve(import.meta.dirname, "node_modules", "@anthropic-ai", packageName, `claude${suffix}`);
if (!existsSync(candidate)) {
  console.error(`pinned Claude binary is unavailable for ${platform}-${arch}: ${candidate}`);
  process.exit(1);
}
console.log(candidate);
