# Go implementation

`go/` is the canonical product application implementation of llm-gateway. It builds
a static Go binary with the Preact console embedded at compile time; Node.js is a
build-time dependency only.

The application composes three standalone, modular libraries:
- `github.com/xibodev/llm-translate`: protocol translation for Anthropic, OpenAI Chat, and Responses APIs.
- `github.com/xibodev/llm-provider-auth`: provider authentication flows, OAuth device codes, and key parsing.
- `github.com/xibodev/llmgw-core`: headless proxy engine, target resolution, and circuit breaking.

These external modules are versioned Go dependencies in `go.mod`, not vendored directories.
See [`docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md) for module boundaries and dependency rules.

## Package map

```text
cmd/llmgw             CLI: serve, health, version, backup/inspect/restore
internal/api          OpenAI/Anthropic facades and admin/user APIs
internal/buildinfo    linker-injected version, commit, and build time
internal/config       settings, provider instances, endpoints, local detection
internal/diagnostics  secret-redaction, text-limiting utilities
internal/iam          SQLite IAM, keys, quotas, usage, audit, alerts, retention
internal/operations   locked backup, inspection, restore, and recovery journal
internal/providers    transports, auth adapters, core adapters, catalogs, retries, circuit state
internal/roster       provider catalog synchronization and metadata
internal/router       target resolution, ordered failover, telemetry, usage bridge
internal/web          embedded Preact bundle and legacy compatibility documents
```

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
govulncheck ./...
```

Console source changes also require:

```bash
cd internal/web/console
npm ci
npm audit --audit-level=high
npm run lint
npm test
npm run check:dist
```

Run a local development gateway with explicit local-only credentials:

```bash
export LLMGW_API_KEY="$(openssl rand -base64 32)"
export LLMGW_CREDENTIAL_ENCRYPTION_KEY="$(openssl rand -base64 32)"
go run ./cmd/llmgw serve
```

Local builds report `0.0.0-dev`, `unknown` commit, and `unknown` build time.
Release automation injects immutable values through linker flags. Probe with:

```bash
go run ./cmd/llmgw version
```

User and operator documentation starts at the repository
[`README.md`](../README.md). Detailed configuration, APIs, provider behavior,
operations, and upgrade guidance live under [`docs/`](../docs/).
