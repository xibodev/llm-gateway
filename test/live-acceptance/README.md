# Live acceptance harness

This harness builds or pulls an exact llm-gateway candidate, starts it with an
empty Docker volume, configures anonymous providers, sweeps their free models,
creates representative routes, exercises Chat and Messages, drives the
Playground with Playwright, runs Claude Code, restarts the container, and writes
a structured report.

## Modes

- `deterministic`: local fixture providers prove the harness, route order,
  Playground, and restart behavior without external dependencies. CI requires
  this mode.
- `live`: the gateway's anonymous provider automation connects Kilo Code,
  LLM7, OVH AI Endpoints and Pollinations without credentials. Every free model
  is called through the gateway; failed observations get a direct-provider
  comparison before classification.
- `policy`: evaluate an existing report with `node policy.mjs <report.json>`.

Each model call waits `LLMGW_ACCEPTANCE_MODEL_TIMEOUT_MS`, 180 seconds by
default. The candidate gateway is started with a provider timeout of two thirds
of that, so a slow upstream is recorded as the gateway's own outcome rather
than as the harness giving up first.

Run locally:

```powershell
cd test/live-acceptance
npm ci --ignore-scripts --no-audit --no-fund
npx playwright install chromium
$env:LLMGW_ACCEPTANCE_MODE = "deterministic" # or live
$env:LLMGW_ACCEPTANCE_REPORT = "$PWD/report.json"
node run.mjs
```

Live mode uses the installed `claude` command by default. CI resolves the pinned
platform binary from `@anthropic-ai/claude-code` with `claude-bin.mjs`.

## Release policy

The job must execute and produce a report. Candidate-only regressions and
required deterministic failures block. Provider outages, rate limits, catalog
drift, and provider-side invalid envelopes remain visible but do not become
product passes. If every live provider is unavailable, the result is
`inconclusive`; release promotion requires an explicit repository override.

Reports and local Claude state are ignored. The workflows upload reports as
artifacts. No provider credential is required for the anonymous matrix.
