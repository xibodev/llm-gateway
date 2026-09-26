#!/usr/bin/env node
import { mkdir, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { schema } from "./core.mjs";

const output = resolve(process.env.LLMGW_ACCEPTANCE_REPORT || "test/live-acceptance/report.json");
await mkdir(resolve(output, ".."), { recursive: true });
await writeFile(output, JSON.stringify({
  schema,
  mode: process.env.LLMGW_ACCEPTANCE_MODE || "live",
  execution: { started: false, completed: false, cleanup_completed: false },
  providers: [], model_sweep: [], routes: [], api: [], playground: [], claude: [], restart: {},
  checks: [{ name: "bootstrap", status: "failed", detail: "acceptance runner did not start", required: true }],
  policy: { verdict: "fail", release_blocking: true, override_eligible: false, reasons: ["acceptance runner did not start"], warnings: [] },
}, null, 2) + "\n");
