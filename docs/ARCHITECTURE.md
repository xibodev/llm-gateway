# Architecture and Modular Design

This document details the modular architecture of `llm-gateway`, the module dependency graph, ownership boundaries, extension points, testing responsibilities, and release processes.

---

## 1. Module Graph and Dependency Direction

The gateway ecosystem is split into three standalone, reusable open-source libraries and one canonical product application:

```text
       +-----------------------+       +------------------------+
       |  llm-provider-auth    |       |     llm-translate      |
       |  (auth & OAuth flows) |       | (wire transformations) |
       +-----------+-----------+       +-----------+------------+
                   |                               ^
                   |                               |
                   |                   +-----------+------------+
                   |                   |       llmgw-core       |
                   |                   |    (headless engine)   |
                   |                   +-----------+------------+
                   |                               ^
                   +---------------+---------------+
                                   |
                       +-----------+------------+
                       |      llm-gateway       |
                       |  (product application) |
                       +------------------------+
```

### Dependency Rules

1. **Acyclic Hierarchy**:
   - `github.com/xibodev/llm-translate` is a pure leaf module with zero external dependencies.
   - `github.com/xibodev/llm-provider-auth` is a leaf module with zero external dependencies, providing provider authentication flows and safe diagnostics.
   - `github.com/xibodev/llmgw-core` depends only on `llm-translate` for wire translation. It does not depend on `llm-provider-auth` or `llm-gateway`.
   - `llm-gateway` (in `go/`) is the top-level product application composing all three libraries via adapters.
2. **No Reverse Dependencies**:
   - Extracted libraries never import `llm-gateway` application packages or assume gateway SQLite/config structures.
3. **Versioned Semantic Dependencies**:
   - Libraries are imported as semantic version tags in `go.mod` (e.g. `v0.1.x`).
   - Local directory `replace` directives are never committed to the production repository; production builds run with `GOWORK=off`.

---

## 2. Ownership Boundaries

| Module | Canonical Repository | Primary Responsibilities |
| --- | --- | --- |
| `llm-translate` | `github.com/xibodev/llm-translate` | Bidirectional protocol transformations: Anthropic Messages, OpenAI Chat Completions, Responses API, tool definitions, tool results, reasoning/thinking blocks, multi-part vision, and SSE stream conversion. Zero storage, zero persistence, zero policy. |
| `llm-provider-auth` | `github.com/xibodev/llm-provider-auth` | Upstream provider authentication: OAuth device-code flows, GCP service account key parsing and JWT token minting, token caching, session exchange, and bounded, secret-sanitized auth diagnostics. Zero routing, zero IAM database. |
| `llmgw-core` | `github.com/xibodev/llmgw-core` | Headless, embeddable proxy engine: provider abstractions, reusable HTTP transports, target resolution, ordered failover, circuit breaking, retry behavior, and pluggable extension hooks (`Authenticator`, `PolicyGate`, `CredentialResolver`, `UsageHook`). Zero SQLite, zero console, zero operational backups. |
| `llm-gateway` | `github.com/xibodev/llm-gateway` | The complete self-hosted gateway product: CLI (`serve`, `health`, `version`, `backup`), YAML settings, SQLite persistence, key/project IAM, rate limits, quotas, retained audit logging, `/console` administration and `/portal` user surfaces, roster auto-refresh, and adapters connecting product state to the core. |

---

## 3. Extension Points and Adapters

The gateway integrates with `llmgw-core` and `llm-provider-auth` through clean adapter interfaces located in `internal/providers`:

1. **Provider Authentication (`ProviderAuthAdapter`)**:
   - Implemented in `internal/providers/auth_adapter.go`.
   - Maps gateway provider instances to device flows (`DeviceProviderAuthAdapter`), session refresh (`RefreshableProviderAuthAdapter`), and revocation (`RevocableProviderAuthAdapter`).
   - Connects dynamic gateway configuration (e.g. `config.Get().GithubCopilotCacheDir`) to `copilotauth` via resolver hooks.
2. **Core Engine Adapter (`CoreProviderAdapter`)**:
   - Implemented in `internal/providers/core_adapter.go`.
   - Adapts internal gateway `Provider` implementations to `coreproviders.Provider`.
   - Converts gateway endpoint definitions to `core.RouteConfig`.
   - Adapts gateway IAM credential resolution to `core.CredentialResolver`.
   - Adapts project/key allowlists to `core.PolicyGate`.
   - Bridges telemetry to `core.UsageHook`.
   - Builds live `core.Engine` instances via `BuildCoreEngine()`.
3. **Routing Targets**:
   - `internal/router/failover.go` aliases `core.Target` and `core.Resolution`, ensuring target and failover representation matches the core engine contract.

---

## 4. Testing Responsibilities

Each layer maintains dedicated test boundaries:

1. **Module Unit Tests**:
   - `llm-translate`: Bidirectional wire mapping fidelity, streaming SSE deltas, tool translation parity, and synthetic response tests.
   - `llm-provider-auth`: Token exchange, device polling state machines, PEM parsing, secret leak prevention, and diagnostic text bounds.
   - `llmgw-core`: Headless HTTP handler routing, circuit breaker trips/cooldowns, policy gates, and failover mechanics.
2. **Gateway Unit and Integration Tests**:
   - Run via `go test ./...` in `go/`.
   - Tests CLI commands, SQLite IAM migrations, encrypted credential storage, quota enforcement, catalog synchronization, and API surface routing.
3. **Console Verification**:
   - Run in `go/internal/web/console`.
   - Typechecking (`npm run lint`), component tests (`npm test`), dependency security audits (`npm audit --audit-level=high`), and distribution drift check (`npm run check:dist`).
4. **Deterministic Acceptance Testing**:
   - Automated via `test/live-acceptance/run.mjs` in deterministic mode.
   - Spins up clean isolated containers against mocked backends to verify Docker health, catalog sweeps, chat/streaming evidence, route failover, and restart persistence.
5. **Live Acceptance Testing**:
   - Release gating harness verifying live provider connectivity, token usage, and error attribution.

---

## 5. Release and Distribution Process

1. **Library Versioning**:
   - Standalone libraries publish semantic version tags independently (`vMAJOR.MINOR.PATCH`).
   - Library updates are pushed to their respective public repositories and verified with `go vet`, `go test`, and `govulncheck`.
2. **Gateway Dependency Pinning**:
   - `go/go.mod` consumes published library tags directly via standard Go module resolution.
   - `go mod tidy -diff` must produce zero differences.
3. **Release Binaries and Container**:
   - Release workflow (`.github/workflows/release.yml`) builds five cross-compiled OS/architecture binaries (`windows/amd64`, `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`) with `CGO_ENABLED=0`.
   - Multi-architecture container images (`linux/amd64`, `linux/arm64`) are built from `go/Dockerfile` with SBOM generation and SLSA provenance attestation.
   - Release gate requires validation, CI, acceptance, and dry-run confirmation before immutable release publication.
