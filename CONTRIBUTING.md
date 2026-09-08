# Contributing

llm-gateway is a public repository. Read [`AGENTS.md`](AGENTS.md) before making
changes; its secret, configuration, identity, and commit-history rules apply to
all contributors.

## Development

The Go module is under `go/`. The console source is under
`go/internal/web/console/` and its generated `dist/` is committed because the Go
binary embeds it.

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
