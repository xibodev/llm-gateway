# Contributing

llm-gateway is a public repository. Read [`AGENTS.md`](AGENTS.md) before making
changes; its secret, configuration, identity, and commit-history rules apply to
all contributors.

## Development

The Go module is under `go/`. The gateway composes three external modular
libraries (`llm-translate`, `llm-provider-auth`, and `llmgw-core`) as versioned
dependencies in `go.mod`, not as vendored directories. See
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for module boundaries and dependency rules.

The console source is under `go/internal/web/console/` and its generated `dist/` is
committed because the Go binary embeds it.

```bash
cd go
go build ./...
go vet ./...
go test ./...
govulncheck ./...

cd internal/web/console
npm ci
npm audit --audit-level=high
npm run lint
npm test
npm run check:dist
```

Documentation and website checks run with:

```bash
node scripts/check-docs.mjs
```

Two test suites guard the library extraction (llm-gateway#67):

- `go/internal/api/testdata/characterization` pins the gateway's observable HTTP
  behavior and the requests it sends upstream. A golden diff is a behavior
  change. Regenerate with
  `LLMGW_UPDATE_GOLDEN=1 go test ./internal/api -run TestHTTPCharacterization`
  only when the change is intended, and review the diff.
- `go/internal/architecture` enforces dependency direction. Code destined for the
  shared libraries may not gain gateway storage, configuration, or HTTP
  dependencies, and its coupling budgets can only shrink.

## Pull requests

- Keep one focused acceptance slice per branch and PR into `staging`.
- Add focused regression tests for demonstrated behavior.
- Avoid new dependencies when the standard library is sufficient.
- Do not stage or commit local configuration, real hosts, credentials, identity
  data, runtime state, or generated agent coordination files.
- Update the canonical guide for user-visible behavior; avoid duplicating
  technical recipes across pages.
- Rebuild console `dist/` after console source changes.

## Documentation ownership

- `README.md`: concise product entry point and tested quickstart.
- `docs/ARCHITECTURE.md`: modular architecture, module graph, boundaries, extension points.
- `docs/CONFIGURATION.md`: YAML/environment/state reference.
- `docs/CLIENTS.md`: coding-client setup and compatibility.
- `docs/PROVIDERS.md`: curated integration and credential boundary.
- `docs/ROUTING.md`: model addressing, retry, failover, adaptation.
- `docs/API.md`: public data-plane contract.
- `docs/MULTI_USER.md`: identity, keys, quotas, audit/outbox.
- `docs/OPERATIONS.md`: deployment, logging, retention, backup, release, rollback.
- `docs/UPGRADING.md`: migrations and compatibility aliases.
- `docs/LIMITATIONS.md`: explicit non-goals and known gaps.
- `SECURITY.md`: threat boundary and private reporting.
- `website/`: public GitHub Pages presentation of the same facts.

Provider names on the website/docs are checked against
`go/internal/providers/registry_manifest.json`; public API paths are checked
against `go/internal/api/server.go`.

## Security reports

Do not open a public issue for a vulnerability. Use the repository's private
vulnerability reporting flow described in [`SECURITY.md`](SECURITY.md).
