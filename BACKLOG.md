# Repository backlog

This is the single active implementation tracker for `llm-gateway`.

## Product target

Build a single-node, self-hosted gateway that reliably serves Claude Code,
Codex, and Copilot CLI through Anthropic Messages and OpenAI Chat/Responses.
It must provide stable model selection, simple ordered failover, secure
credentials, recoverable state, bounded resource use, and measured capacity of
at least 1,000 completed local synthetic requests per second on suitable
single-node infrastructure.

The goal is not a hyperscale provider platform. SQLite, one Go process, exact
`provider/model` routes, and ordered endpoint chains remain the default design.
New architecture is justified only by a failing compatibility, correctness,
security, operability, or measured-capacity test.

[`docs/PROVIDER_PARITY.md`](docs/PROVIDER_PARITY.md) is retained as historical
design research. It is not a target contract and does not authorize provider
breadth, dynamic account routing, quota sharing, or a universal translation
framework.

## Tracking rules

| State | Meaning |
| --- | --- |
| `ready` | Committed, scoped, and pullable now. |
| `partial` | Useful implementation exists; the stated acceptance boundary remains open. |
| `queued` | Committed work waiting on listed dependencies. |
| `blocked` | External approval or input prevents otherwise-ready work. |
| `deferred` | Not committed now; includes a concrete re-entry trigger. |
| `done` | Acceptance evidence passed and documentation reflects the result. |

- Implement one acceptance slice per feature branch and PR into `staging`.
- Promote a finite milestone from `staging` to `main`; do not wait for every
  deferred idea.
- Keep a slice reviewable. Split it before editing when it spans unrelated
  subsystems or is likely to exceed roughly 10 production/test paths.
- A commit, branch, or document is not completion evidence. Record the command
  or test that establishes the durable behavior.
- This backlog authorizes local implementation and validation only. Real logins,
  deployment, DNS, and infrastructure changes require explicit approval.

## Completed foundations

| ID | Result | Evidence |
| --- | --- | --- |
| `DEPS-001` | Console build dependencies resolve to patched compatible releases; the embedded bundle remains reproducible. | `npm ci && npm audit --json && npm run lint && npm test && npm run check:dist` |
| `IAM-002` | Project-policy edits preserve every backend field, including model-credit limits, with a source-contract guard for future fields. | `go test ./internal/api`; console test and dist gate |
| `ERROR-001A` | Core provider, API, verification, router, and telemetry diagnostics redact credentials before limiting while retaining upstream status. | Provider/router/API tests and `govulncheck ./...` |
| `ERROR-001B` | Audio and embedding non-2xx proxy responses use safe gateway errors; status and usage remain accurate; successful bytes remain unchanged. | Audio/embedding success, redirect, malformed, oversized, and read-failure tests |
| `ERROR-001C1` | Authentication and persistence code can reuse one dependency-neutral, bounded diagnostic sanitizer. | Diagnostics and provider tests |
| `ERROR-001C2` | GCP, Copilot, and Codex error producers emit bounded safe diagnostics while preserving successful auth flows. | Auth-package tests |
| `ERROR-001C3` | OAuth browser projections sanitize errors and never expose stored tokens; legacy authorized polling still persists its credential. | Provider/API OAuth tests |
| `ERROR-001C4` | Provider-check, audit, and outbox diagnostics are safe on write and historical read without corrupting model/voice/provider identifiers. | Diagnostics/IAM raw-row and history tests |
| `ERROR-001C5` | Edge TTS handshake failures retain status without leaking signed URLs and close each failed response exactly once. | Edge TTS retry/status/closure tests |

## Milestone 1: safety and measured capacity

This milestone is promoted to `main` when every item below is `done`, the full
repository gate passes, and the local capacity gate meets the product target.

### Capacity profile

`LOAD-001A` is a conventional Go benchmark baseline for exact
`echo/echo-default` traffic through `NewServer`: unauthenticated, static-key,
minted-key, and immediate static-key SSE. Run it with
`go test -run '^$' -bench 'BenchmarkGateway' -benchmem ./internal/api`; standard
Go output is the report and requests/sec is `1e9 / ns/op`. It uses direct handler
calls, so it measures gateway hot paths without loopback networking, connection
management, or production-provider capacity.

`LOAD-001B` remains the milestone capacity gate. Build a simple external or
loopback gate only after the measured hot-path fixes land, if release evidence
still needs network-level throughput validation.

| ID | State | Depends on | Work and acceptance |
| --- | --- | --- | --- |
| `LOAD-001A` | `done` | none | The standard-library benchmark runs all four local direct-handler scenarios without external providers. A deterministic correctness test reconciles minted-key usage events with key and project request/token counters. Evidence: `go test -run '^$' -bench 'BenchmarkGateway' -benchmem ./internal/api` and `go test -run TestGatewayMintedUsageCountersReconcile ./internal/api`. |
| `LOAD-001B` | `queued` | `DATA-001A`, `DATA-001B`, `PERF-001` | Add the real milestone/release capacity gate after hot-path fixes. It may be a simple external or loopback test; record reference-host evidence and require at least 1,000 completed local synthetic requests/sec without unexpected statuses or counter drift. |
| `DATA-001A` | `done` | `LOAD-001A` | `gateway.db` remains authoritative and the synchronous legacy compatibility `usage.db` savings ledger is opt-in/off by default. Explicit `savings.enabled: true` users retain custom path, baseline, labels, and read behavior; migration import remains unchanged and idempotent. Default static-key and unauthenticated benchmarks exceed 1,000 requests/sec. |
| `DATA-001B` | `queued` | `LOAD-001A`, `DATA-001A` | Make optional failover telemetry unable to backpressure inference. Failure-heavy load remains bounded and inference completion does not wait on `telemetry.db`; events are bounded, dropped/coalesced with an explicit counter, or stored asynchronously; operator reads remain safe. |
| `PERF-001` | `queued` | `LOAD-001A`, `DATA-001A` | Optimize only measured minted-key contention in key lookup, `last_used_at`, project policy, quota admission, and usage reconciliation. The minted-key scenario meets the capacity profile while strict quotas, revocation, reconciliation, restart behavior, and attribution remain correct. No Redis/Postgres requirement is introduced. |
| `GOV-001` | `ready` | none | Use one fail-closed provider-credential decision for catalog and dispatch. Services require an exact active project/provider binding; human, static-admin, and local behavior remains explicit. Stop presenting the recovery admin key as the routine CLI credential. Unbound, cross-project, disabled, revoked, personal, bound, static-admin, and local cases pass across provider types and model listing. |
| `CFG-001` | `ready` | none | Replace the live mutable settings pointer with atomic immutable snapshots and make management persistence failure-aware. A failed config save leaves runtime state and provider/catalog caches unchanged; concurrent reads and successful updates pass race-capable tests. |
| `HTTP-001A` | `ready` | none | Bound buffered inbound JSON request bodies by surface. Stable 413 behavior covers inference and management JSON while documented normal limits remain accepted. |
| `HTTP-001B` | `ready` | none | Bound multipart request bodies and file handling. Stable 413 behavior covers credentials, transcription, and playground media without unbounded disk spill or whole-file duplication beyond the documented limit. |
| `HTTP-001C1` | `ready` | none | Bound non-streaming OpenAI-compatible, Azure, and Codex upstream response/error bodies. Oversized responses fail with stable safe 502 behavior while normal configured model responses remain accepted. |
| `HTTP-001C2` | `ready` | none | Bound non-streaming Anthropic, Ollama, and Google upstream response/error bodies. Oversized responses fail with stable safe 502 behavior while normal text and supported media responses remain accepted. |
| `HTTP-001D` | `queued` | `CLI-001` | Bound individual SSE/NDJSON records without limiting valid stream duration. Oversized records fail safely and normal Chat, Responses, and Messages fixtures remain compatible. |
| `HTTP-002A` | `ready` | none | Add context-aware provider/router method variants and migrate OpenAI-compatible plus Google non-streaming calls first. Cancelling ingress cancels those billable calls; compatibility wrappers keep other providers green. |
| `HTTP-002B` | `queued` | `HTTP-002A` | Migrate Anthropic, Ollama, Azure, and Edge TTS inference calls to contextual outbound requests. Each implementation has focused cancellation tests. |
| `HTTP-002C` | `queued` | `HTTP-002A` | Migrate GCP, Copilot, and Codex auth/token calls to contextual outbound requests without changing successful device or refresh behavior. |
| `HTTP-002D` | `queued` | `HTTP-002A`, `HTTP-002B` | Make retry backoff cancellation-aware. Disconnects stop waits promptly; retry/failover semantics remain covered. |
| `HTTP-002E` | `queued` | `HTTP-001D`, `HTTP-002A`, `HTTP-002B` | Make active stream reads cancellation-aware, close response bodies promptly, and avoid a global write timeout that breaks valid long streams. |
| `RETRY-001` | `ready` | none | Align retry and failover with the documented contract. Retry/fail over only transport failures, timeouts, 429, and selected 5xx responses; ordinary 400/401/403/404 are not duplicated; stateful requests remain single-attempt. |
| `DEPLOY-001` | `ready` | none | Make the supplied production Compose/Caddy path satisfy the documented trust boundary. SSO/encryption settings are passed by secret reference; trusted identity headers are stripped/overwritten; gateway/state remain private and restrictive; incomplete secure mode fails clearly; synthetic deployment tests use no real secret. |
| `AUTH-001` | `partial` | none | Finish only the existing GitHub Copilot and OpenAI Codex official auth flows. Explicit risk acknowledgement, refresh serialization, revocation, owner isolation, and fixture tests pass. No open-ended auth-adapter expansion. |
| `PLAY-002` | `ready` | none | Hide portal media controls whose owner routes do not exist. Every control visible in portal mode has a registered policy-enforced route; admin media behavior remains unchanged. |
| `KEY-001` | `ready` | none | Make the key list accurately summarize all existing restrictions. A key restricted by expiry, allowlists, request, token, cost, or credit policy is never labelled `Unrestricted`; advanced editing remains deferred. |
| `CLI-001` | `ready` | none | Pin fixture contracts for supported Claude Code, Codex, and Copilot CLI versions: authentication/base URL, paths, model selection, one tool turn, streaming, errors, cancellation, and response model identity. Optional features are explicitly supported, rejected, or unavailable rather than silently dropped. |

## Milestone 2: coding-client compatibility

| ID | State | Depends on | Work and acceptance |
| --- | --- | --- | --- |
| `CLAUDE-001` | `partial` | `CLI-001`, `GOV-001` | Make Claude model discovery deterministic and round-trippable. Sort rows, preserve exact provider/endpoint identity, hide ambiguous aliases, apply policy before advertising, and classify native Anthropic rows correctly. Every displayed picker ID must route to exactly what it names. |
| `CLAUDE-002` | `partial` | `CLI-001` | Use direct native Anthropic pass-through where possible. For adapted providers, preserve the observed Claude core profile (system/text, tools/results, supported images, stop/usage) and reject unsupported thinking/cache/document fields before dispatch instead of silently losing them. |
| `CLAUDE-003` | `partial` | `CLAUDE-002` | Add bare `/messages/count_tokens`; define downstream auth-header precedence and supported Anthropic version/beta forwarding; proxy native counting when available and return a documented bounded estimate otherwise. |
| `OPENAI-CLI-001` | `partial` | `CLI-001` | Prove Codex against native `/v1/responses` events and Copilot BYOK against the exact Chat/Responses paths it uses, including tools, stream termination, safe errors, cancellation, and model identity. This is downstream client compatibility, separate from subscription-provider OAuth. |
| `STREAM-001A` | `partial` | `HTTP-001D`, `HTTP-002E`, `CLI-001` | Correct Chat and Responses stream setup and terminal failure accounting. Resolve before HTTP 200, parse bounded complete records, emit one dialect-correct failure, close/cancel promptly, and never record failed streams as success credits. |
| `STREAM-001B` | `queued` | `STREAM-001A`, `CLAUDE-002` | Correct Anthropic Messages and native Anthropic stream translation. Preserve supported text/tool/usage events, surface transport/in-band failures without false `message_stop`, and reject unsupported event types explicitly. |
| `TEST-001` | `partial` | `CLI-001` | Add one credential-free fixture command for the three CLI contracts and ordered failover. Existing CI remains responsible for build, vet, unit tests, console tests, and dist parity. |
| `CLI-002` | `blocked` | `CLAUDE-001`, `CLAUDE-002`, `CLAUDE-003`, `OPENAI-CLI-001`, `STREAM-001B`, `TEST-001` | Run the local fixture gate, then one explicitly approved disposable smoke for Claude Code, Codex, and Copilot CLI using normal coding flows and ordered failover. Record tested client versions; store no credential or personal identity. |

## Milestone 3: dependable single-node operation

| ID | State | Depends on | Work and acceptance |
| --- | --- | --- | --- |
| `OBS-001` | `partial` | `DATA-001A` | Generate or accept one safe request ID, return it to clients, propagate it upstream where appropriate, and include it in structured logs, usage, and fallback records. No distributed tracing platform or trace console. |
| `OPS-001A` | `ready` | none | Separate process liveness from local readiness and make graceful drain configurable. Readiness checks parsed configuration and writable state, never external providers; shutdown reports timeout/failure and drains within the configured window. |
| `OPS-001B` | `queued` | `DATA-001A` | Add one tested online backup/restore command and bounded retention procedure. A disposable state directory restores IAM/config/catalog/session authority and passes readiness; retention cannot delete current quota/audit state accidentally. |
| `PROVIDER-001` | `partial` | `TEST-001` | Define fixture-backed availability for the existing provider runtimes used by the three coding clients. A provider is labelled available only when its configured auth, discovery, and relevant inference path pass fixtures. Add other providers only through a new demand-specific item. |
| `RELEASE-001A` | `queued` | `TEST-001` | Inject one version identity from tagged builds into binaries and images. `version`, health output, user-agent metadata, artifact names, and docs derive from the release tag or one source without conflicting constants. |
| `RELEASE-001B` | `queued` | `OPS-001B`, `RELEASE-001A` | Guard publication and migration compatibility. Release workflows reject tags not reachable from `main`, retain checksums/image gates, and test fresh plus current-state startup before publishing. |

## Deferred ideas

These items are not dependencies of active work.

| Scope | Re-entry trigger |
| --- | --- |
| Optional sanitized request/response body snapshots (`ERROR-001C6`) | Operators need body capture after metadata plus request IDs prove insufficient. Raw body logging remains explicitly unsafe and off by default. |
| Full browser journey suite | Console regressions become a recurring support problem; begin with one setup smoke. |
| Advanced per-key policy editing | Operators routinely need fields that cannot be managed adequately through current admin surfaces. Accurate display is active under `KEY-001`. |
| Removing deprecated route/field names | A release boundary and client evidence show compatibility aliases can be removed safely. |
| Multi-account selection, health, and provider quota adapters | A real deployment needs simultaneous accounts or a stable official quota API. |
| Formal management OpenAPI contract | Third-party consumers require a versioned management API. |
| Additional provider onboarding or catalog breadth | A concrete user/provider request exists with official documentation and fixtures. |

## Removed from the product plan

- A universal canonical translation IR and translator marketplace.
- A 100-provider catalog target.
- Dynamic weighted, round-robin, power-of-two, least-used, cost-first,
  reset-aware, headroom, or last-known-good routing.
- Provider quota pools, borrowing, virtual quota model IDs, and automatic route
  combos.
- Model override/alias policy subsystems beyond deterministic client-compatible
  IDs and existing allowlists.
- Streaming account/key selection and a trace console in Playground.
- A marketing-site publication milestone.

Existing shipped code is not removed merely because related expansion is no
longer planned. Any removal requires a separate compatibility decision.

## Repository gate

```bash
cd go && go build ./... && go vet ./... && go test ./... && govulncheck ./...
cd internal/web/console && npm ci && npm audit --json && npm run lint && npm test && npm run check:dist
```

Persistence changes also run backup/restart/migration tests. Streaming and client
compatibility changes run their fixture profiles. Every milestone promotion runs
the `LOAD-001A` benchmarks, and capacity completion requires `LOAD-001B`. Update
this backlog in the same PR as the behavior and evidence.
