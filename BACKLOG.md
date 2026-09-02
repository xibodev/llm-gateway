# Repository backlog

This is the single active implementation tracker for `llm-gateway`.

## Product target

Ship a reliable single-node, self-hosted gateway for Claude Code, Codex, and
Copilot CLI, while preserving the same HTTP behavior for browsers, SDKs, backend
applications, and direct HTTP clients.

The current release needs stable model selection, core Anthropic Messages and
OpenAI Chat/Responses compatibility, simple ordered failover, safe credentials,
correct cancellation, and a real-provider smoke test. It does not need
hyperscale throughput, high availability, dynamic account scheduling, quota
marketplaces, or a universal translation framework.

## Delivery policy

- One focused acceptance slice per feature branch and PR into `staging`.
- Run targeted tests while implementing. GitHub CI runs the complete Go and
  console gates for each PR.
- The `staging -> main` gate is Docker endpoint integration against a real
  provider plus simple host-side checks with the installed Claude, Codex, and
  Copilot CLIs.
- Do not build a test framework when direct endpoint calls or existing CLIs can
  establish the behavior.
- Fix only failures demonstrated by code inspection, focused tests, endpoint
  integration, or the real-client smoke.
- Real credentials, provider login, deployment, DNS, and infrastructure changes
  require explicit operator participation. Never commit credentials or local
  identity data.

## Completed on staging

- Dependency and project-policy correctness fixes.
- Provider, OAuth, audit, outbox, and proxy-error diagnostic redaction.
- Edge TTS handshake status, retry, and cleanup fixes.
- `gateway.db` remains authoritative; the legacy savings ledger is opt-in.
- Gateway-side Claude, Codex, and Copilot wire-contract fixtures.
- Deterministic, policy-aware Claude model discovery and selection.
- Native non-streaming Anthropic Messages pass-through and strict adapted core
  compatibility.
- Native and estimated Claude token counting, including the bare count route.
- Bounded SSE and NDJSON records.
- Request cancellation already reaches OpenAI-compatible Chat/Responses,
  Google Chat, and Claude requests adapted through those paths.

These are implementation and fixture results. They do not claim an installed
Claude CLI has selected a model successfully against Docker; that remains part
of the release gate below.

## Current release

| ID | State | Work and acceptance |
| --- | --- | --- |
| `CANCEL-001` | `partial` | Coding endpoints now stop OpenAI-compatible Chat/Responses and adapted Messages streams on request cancellation or downstream write/flush failure, close upstream work, suppress success terminals and retry/failover after abort, and record one `499/client_cancelled` row. Codex forwards the request context. Native Anthropic streaming, direct proxy endpoints, and the remaining provider breadth stay deferred; token-count cancellation remains non-billable and records no usage. |
| `SMOKE-001` | `ready` | Start the disposable Docker UAT gateway with normal internet access, connect a real provider through the console or device flow, sync its catalog, and run direct endpoint checks for models, Messages, token counting, Chat, Responses, and the stream forms used by the clients. Prefer Copilot because its real catalog exercises Claude-family model selection; Azure OpenAI is an optional second provider. Store no credential in the repository or test output. |
| `CLI-UAT-001` | `blocked` | After `SMOKE-001`, use the installed host CLIs with temporary shell environment/config pointing to the Docker gateway. Claude must display and select the intended model, complete one minimal prompt and one harmless tool-enabled flow, and stop on Ctrl+C. Codex must complete one Responses call; Copilot CLI must complete one BYOK call. Record only client version, selected public model ID, served provider/model, and pass/fail. Blocked only on operator participation and disposable real-provider credentials. |
| `RELEASE-001` | `queued` | When `CANCEL-001`, `SMOKE-001`, and `CLI-UAT-001` pass and GitHub CI is green, open one reviewed `staging -> main` PR. Do not add unrelated hardening to that promotion. |

## Cancellation contract

Cancellation is an HTTP lifecycle concern, not a provider dialect or CLI
feature. There is no translated abort payload for ordinary synchronous calls.

```text
client closes request / resets stream / aborts SDK call
                         |
                         v
                  request.Context done
                         |
       +-----------------+-----------------+
       |                 |                 |
       v                 v                 v
 stop retry/failover  stop translation  close upstream body/socket
                         |
                         v
                record client_cancelled
```

The contract applies equally to CLIs, browsers using `AbortController`, Go
clients canceling a context, .NET cancellation tokens, Python task cancellation,
and reverse-proxy disconnects. A client that merely stops reading while keeping
the request open has not canceled it.

Asynchronous operations are different. Once a video/background job has returned
an operation ID, canceling the initiating HTTP request cannot guarantee provider
job cancellation. A provider-specific job-cancel API is deferred until demanded.

## Release validation

### Automated per PR

```bash
cd go && go build ./... && go vet ./... && go test ./... && govulncheck ./...
cd internal/web/console && npm ci && npm audit --json && npm run lint && npm test && npm run check:dist
```

Local development may run only focused tests. The complete gate is required once
in GitHub CI before merge.

### Real-provider staging gate

Use `test/uat/docker-compose.uat.yml` with a fresh disposable state volume and a
real provider configured interactively or through local environment references.
Do not add provider secrets to `.env.example`, Compose files, fixtures, reports,
or chat transcripts.

The operator runs the host CLIs with process-scoped temporary environment or an
ephemeral config directory. The final gate instructions must include exact setup,
test, inspection, and cleanup commands; they must not persist or print secrets.

## Deferred efforts

| Effort | Re-entry trigger |
| --- | --- |
| Throughput and high availability | A real deployment needs high traffic or multiple gateway nodes. Existing direct-handler benchmarks remain diagnostic only; strict minted-key SQLite throughput is not a release target. |
| Broad provider-by-provider cancellation cleanup | A synchronous public endpoint fails the central `CANCEL-001` integration test or a real client leaves work running. Do not resume the preserved broad cancellation branch wholesale. |
| Async video/background job cancellation | A supported provider exposes a stable cancellation API and users need it. |
| Optional sanitized body logging | Metadata and request IDs prove insufficient for an operational incident. Raw body logging remains off by default. |
| Full browser journey suite | Console regressions become a recurring support issue. |
| Advanced key-policy editing | Operators routinely require policy fields not adequately managed today. |
| Provider breadth, multi-account routing, provider quota, and alternate routing strategies | A concrete user requirement names the provider or routing behavior and a focused fixture can prove it. |
| Formal management OpenAPI | A third-party management client requires a versioned contract. |
| Backup, retention, version injection, and broader deployment automation | Schedule as a separate operability/release milestone after this client-compatibility release. |

## Removed from the product plan

- Universal canonical translation IR or translator marketplace.
- A 100-provider target.
- Dynamic weighted, round-robin, least-used, cost-first, reset-aware, or
  headroom routing.
- Provider quota pools, borrowing, and virtual quota models.
- A trace console or streaming account/key selection in Playground.
- Marketing-site publication as an engineering milestone.

Existing shipped behavior is not removed merely because expansion is deferred.
Any removal requires a separate compatibility decision.
