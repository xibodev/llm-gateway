# Repository backlog

This is the single active implementation tracker for `llm-gateway`.

## Product target

Ship a reliable single-node, self-hosted gateway for Claude Code, Codex, and
Copilot CLI, while preserving the same HTTP behavior for browsers, SDKs, backend
applications, and direct HTTP clients.

The shipped compatibility baseline covers stable model selection, core Anthropic
Messages and OpenAI Chat/Responses behavior, simple ordered failover, safe
credentials, cancellation, and real-provider smoke tests. The current release
adds single-node operability. It does not need hyperscale throughput, high
availability, dynamic account scheduling, quota marketplaces, or a universal
translation framework.

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

## Current release

| ID | State | Work and acceptance |
| --- | --- | --- |
| `OPS-001` | `queued` | Add atomic, restorable single-node backups of configuration and SQLite state, with explicit inspect and restore commands and tests that prove a restored gateway retains IAM and routing state. |
| `OPS-002` | `queued` | Add bounded retention for request logs, audit events, usage events, quota counters, outbox history, and obsolete backups without deleting active control-plane state. |
| `OPS-003` | `queued` | Inject semantic version, commit, and build time into binaries and images; expose them through `llmgw version`, `/health`, and the admin state without relying on manually edited constants. |
| `OPS-004` | `queued` | Consolidate tag validation, checksums, SBOM/provenance, binary artifacts, and multi-architecture image publication into one release workflow with a dry-run path. |

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
| Broad provider-by-provider cancellation cleanup | A synchronous public endpoint fails the central cancellation integration test or a real client leaves work running. Do not resume the preserved broad cancellation branch wholesale. |
| Manual installed-client interaction checks | A client upgrade changes behavior or the next client-compatibility release is prepared. Recheck Claude's interactive `/model` picker and physical Ctrl+C alongside the Codex and Copilot smoke. |
| Async video/background job cancellation | A supported provider exposes a stable cancellation API and users need it. |
| Optional sanitized body logging | Metadata and request IDs prove insufficient for an operational incident. Raw body logging remains off by default. |
| Full browser journey suite | Console regressions become a recurring support issue. |
| Advanced key-policy editing | Operators routinely require policy fields not adequately managed today. |
| Provider breadth, multi-account routing, provider quota, and alternate routing strategies | A concrete user requirement names the provider or routing behavior and a focused fixture can prove it. |
| Formal management OpenAPI | A third-party management client requires a versioned contract. |

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
