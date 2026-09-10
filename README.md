# llm-gateway

Self-hosted, single-node LLM gateway for Claude Code, Codex, GitHub Copilot CLI
BYOK, SDKs, and direct HTTP clients.

Point clients at one gateway, address an exact configured model as
`provider/model`, or create a named endpoint with an ordered failover chain.
The gateway exposes core OpenAI and Anthropic protocol surfaces, enforces
project and key policy, and ships an embedded administration console in one Go
binary.

## What it does

- Routes OpenAI Chat Completions, Responses, embeddings, audio, image, and video
  requests plus Anthropic Messages and token counting.
- Selects exact models, deterministic unambiguous aliases, or named ordered
  endpoint chains.
- Advances eligible chat requests after upstream throttling, server errors, or
  timeouts before output starts.
- Connects curated hosted, OAuth, local, and custom compatible providers.
- Enforces model/provider allowlists and persistent request, token, estimated
  cost, and model-credit limits at key and project scope.
- Stores IAM, encrypted connections, usage, quotas, alerts, and retained audit
  history in local SQLite.
- Provides `/console` for administrators and `/portal` for SSO-authenticated
  human owners.
- Creates inspectable offline backups, bounds operational history, and reports
  immutable release build identity.

This is an internal gateway, not a public relay, provider marketplace, quota
scheduler, or high-availability control plane. See
[`docs/LIMITATIONS.md`](docs/LIMITATIONS.md) for explicit boundaries.

## Quickstart

### Docker Compose

```bash
git clone https://github.com/xibodev/llm-gateway.git
cd llm-gateway
cp llmgw.config.example.yaml config.local.yaml

export LLMGW_API_KEY="$(openssl rand -base64 32)"
export LLMGW_CREDENTIAL_ENCRYPTION_KEY="$(openssl rand -base64 32)"
docker compose up -d --build
```

Open `http://127.0.0.1:8787/console` and enter the administrator key from
`LLMGW_API_KEY`. The source configuration is mounted read-only and seeds a
writable copy in the state volume on first start.

Check the process and build identity:

```bash
curl -fsS http://127.0.0.1:8787/health
curl -fsS -H "Authorization: Bearer ${LLMGW_API_KEY}" \
  http://127.0.0.1:8787/admin/api/state
```

The full first-run journey and Windows commands are in
[`docs/QUICKSTART.md`](docs/QUICKSTART.md).

## Client setup

| Client | Gateway configuration |
| --- | --- |
| Claude Code | `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` and a gateway key in `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` |
| Codex | Custom provider with `base_url = "http://127.0.0.1:8787/v1"`, `env_key = "LLMGW_API_KEY"`, and `wire_api = "responses"` |
| Copilot CLI BYOK | `COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:8787/v1`, gateway key, explicit wire API, and exact wire model |
| OpenAI SDKs | `OPENAI_BASE_URL=http://127.0.0.1:8787/v1` and `OPENAI_API_KEY=<gateway key>` |

Use an ID returned by `GET /v1/models`. Complete, copyable profiles and their
known limits are in [`docs/CLIENTS.md`](docs/CLIENTS.md).

## Public data plane

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Process health and build identity; no authentication |
| `GET` | `/v1/models` | Policy- and credential-eligible models, endpoint rows, and safe aliases |
| `POST` | `/v1/chat/completions` | OpenAI-shaped chat, tools, streaming, and vision filtering |
| `POST` | `/v1/responses` | Native Responses or strict stateless Chat fallback |
| `POST` | `/v1/messages` | Native or strictly adapted Anthropic Messages |
| `POST` | `/v1/messages/count_tokens` | Native count or marked deterministic estimate |
| `POST` | `/v1/embeddings` | Single-target OpenAI-shaped embedding proxy |
| `POST` | `/v1/audio/transcriptions` | Single-target multipart speech-to-text proxy |
| `POST` | `/v1/audio/speech` | OpenAI-shaped or native Edge TTS speech synthesis |
| `POST` | `/v1/images/generations` | Inline base64 image generation through a capable provider |
| `POST` | `/v1/videos/generations` | Start or poll a long-running video operation |

Except for `/health`, data-plane requests require a static gateway key or a
gateway-issued project key unless unauthenticated local mode was explicitly
enabled. See [`docs/API.md`](docs/API.md) for aliases, request limits, streaming,
adaptation, cancellation, and per-surface failover semantics.

## Model addressing

- `provider/model`: one exact target. Provider retry policy may retry it, but
  there is no cross-target route failover.
- Endpoint name: an ordered chain of pinned provider/model members.
- Bare provider-native name: accepted only when normalization produces one
  unambiguous policy-eligible target.
- Unknown selector: `404`; choose an ID from `GET /v1/models`.

Streaming can move to another target only before the first response byte.
Embeddings, audio, image, and video use the first eligible target rather than
walking a cross-model chain. See [`docs/ROUTING.md`](docs/ROUTING.md).

## Providers and credentials

The curated registry includes API-key, OAuth, local/no-auth, Google, Azure,
Bedrock, and custom compatible integrations. Registry availability means the
configuration/runtime path exists; it does not guarantee that a particular
account, model, region, or installed client has passed live verification.

Generic API-key providers resolve a human's private default connection first,
then a system connection, then a legacy config seed. Copilot has a stricter
shared-service path: a service requires an exact active project/provider/kind
binding. Codex OAuth is human-private. See [`docs/PROVIDERS.md`](docs/PROVIDERS.md)
and [`docs/MULTI_USER.md`](docs/MULTI_USER.md).

## Operations

State defaults to `~/.llmgw`. Stop the gateway before backup or restore:

```bash
llmgw backup create
llmgw backup inspect /secure/path/llmgw-state.tar.gz
llmgw backup restore /secure/path/llmgw-state.tar.gz --force
```

The default create command writes under `<state>/backups`, where
`LLMGW_BACKUP_KEEP` applies. Explicit external archive paths are not pruned.
Archives can contain credentials; checksums detect corruption, not authorship,
and `LLMGW_CREDENTIAL_ENCRYPTION_KEY` remains external.

`llmgw version`, `/health`, and `/admin/api/state` report the same semantic
version, source commit, and RFC3339 build time. See
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) for deployment, retention, logging,
backup, recovery, release verification, and rollback.

## Documentation

- [Quickstart](docs/QUICKSTART.md)
- [Configuration reference](docs/CONFIGURATION.md)
- [Client profiles](docs/CLIENTS.md)
- [Provider integrations](docs/PROVIDERS.md)
- [Routing and failover](docs/ROUTING.md)
- [Data-plane API](docs/API.md)
- [Multi-user governance](docs/MULTI_USER.md)
- [Operations](docs/OPERATIONS.md)
- [Compatibility and upgrades](docs/UPGRADING.md)
- [Limitations](docs/LIMITATIONS.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)

The public website is published from `website/` through GitHub Pages.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
