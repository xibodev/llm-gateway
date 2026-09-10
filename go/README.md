# Go implementation

`go/` is the only runtime implementation of llm-gateway. It builds a static Go
binary with the Preact console embedded at compile time; Node.js is a build-time
dependency only.

## Package map

```text
cmd/llmgw             CLI: serve, health, version, backup/inspect/restore
internal/api          OpenAI/Anthropic facades and admin/user APIs
internal/buildinfo    linker-injected version, commit, and build time
internal/config       settings, provider instances, endpoints, local detection
internal/iam          SQLite IAM, keys, quotas, usage, audit, alerts, retention
internal/operations   locked backup, inspection, restore, and recovery journal
internal/providers    transports, auth adapters, catalogs, retries, circuit state
internal/router       target resolution, ordered failover, telemetry, usage bridge
internal/translate    strict Anthropic/Chat/Responses transformations
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
