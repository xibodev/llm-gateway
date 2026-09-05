# llm-gateway (Go)

A ground-up Go port of the Python `llm-gateway` — a standalone, multi-provider
LLM proxy for coding CLIs (Claude Code, Copilot CLI, Codex). Same behaviour,
same `/admin` UI, same wire APIs — but a **single static ~16 MB binary**,
instant start, and ~12–15 MB idle RAM.

## Why Go

| | Python image | Go |
|---|---|---|
| Artifact | 235 MB Docker image | **~16 MB static binary** / ~2 MB distroless image |
| Idle RAM | ~47 MiB | **~12–15 MiB** |
| Start | ~1 s | **instant** |
| Deploy | Python + venv or Docker | `scp` the binary + a systemd unit |

Concurrency is goroutine-based (no GIL), so many simultaneous streams are cheap.

## Layout

```
cmd/llmgw            entrypoint (serve)
internal/config      settings, providers, endpoints, keys, secrets, local discovery, bedrock
internal/providers   provider stack on two axes — an OpenAI-standard TRANSPORT
                     (openai.go) + native anthropic/ollama, and a pluggable AUTH
                     strategy (openai_auth.go: bearer key | Copilot OAuth). One
                     OpenAI transport backs openai_compatible / bedrock / litellm /
                     github_copilot. Factory + retry/circuit-breaker. Lazy model
                     catalog (modelcache.go) drives API adaptation.
internal/copilotauth GitHub Copilot OAuth device flow + session cache
internal/translate   dialect matrix: Anthropic ⇄ OpenAI Chat, and OpenAI Chat ⇄
                     Responses (responses.go) — pure, provider-agnostic transforms
internal/router      failover executor, usage ledger + throttle/fallback telemetry (sqlite)
internal/api         HTTP server, bearer auth, /v1 facades, /admin control panel
internal/web         embedded admin dashboard (admin.html)
```

## API adaptation (`force_api_support`, experimental)

Some models speak only one OpenAI-family API — Copilot's `gpt-5.5` /
`gpt-5.4-mini` / `gpt-5.3-codex` are **Responses-API-only** and reject
`/chat/completions`. Opt in and the gateway translates a Chat Completions
request to the model's native endpoint (from the catalog's `supported_surfaces`,
formerly `supported_endpoints`) and translates the reply back, so a CLI keeps
speaking one API.

- **Off by default** — unadapted, the provider's real error (status + detail) is
  passed straight through, never masked.
- Enable per provider (`force_api_support: true` in `config.yaml`) or per request
  (`"force_api_support": true` in the body, which overrides the provider setting
  and is stripped before forwarding).
- Deterministic (catalog-driven, not trial-and-error). Adapted responses carry an
  `X-LLMGW-Adapted` header; a request-level flag also echoes a no-op
  `forced_support` object in the body.

## Build & run

```bash
go build -o llmgw ./cmd/llmgw
LLMGW_ALLOW_UNAUTHENTICATED_API=1 ./llmgw serve   # http://127.0.0.1:8787/admin
```

`llmgw version` prints semantic version, source commit, and RFC3339 build time.
Local builds honestly report development/unknown defaults unless those fields
are injected with Go linker `-X` flags; release automation injects all three.

State lives under `~/.llmgw` (override with `LLMGW_STATE_DIR`):
`gateway.db` (IAM, hashed keys, encrypted provider connections, usage, quotas,
audit, alerts), `config.yaml`, the legacy/config-seed `secrets.json`, and provider
session caches.

Explicit project/provider/service-principal bindings protect encrypted
gateway-managed provider credentials. Services receive no provider credential
unless that exact active binding exists. Human BYOC remains first in resolution,
and its provider/catalog cache remains principal-scoped across unrelated service
credential mutations.

## Endpoints

`GET /v1/models` · `POST /v1/chat/completions` · `POST /v1/responses` ·
`POST /v1/messages` (Anthropic) · `GET /health` · `/admin` (+ `/admin/api/*`) ·
`/portal` (+ `/user/api/*`). Provider-connection APIs support named, private
per-human API keys while the existing Copilot routes remain backward compatible.

## Docker (tiny)

```bash
docker build -t llm-gateway-go .   # ~2 MB distroless/static image
docker run -p 8787:8787 -v llmgw:/state llm-gateway-go
```

Offline state maintenance is built into the same binary:

```bash
llmgw backup create /secure/path/llmgw-state.tar.gz
llmgw backup inspect /secure/path/llmgw-state.tar.gz
llmgw backup restore /secure/path/llmgw-state.tar.gz --force
```

Stop `serve` first. The process lock prevents backup or restore from racing a
running gateway. Archives are checksum-verified and should be protected as
secrets; the credential-encryption key remains external.

## Deploy to Ubuntu (systemd)

```bash
sudo install -m0755 llmgw /usr/local/bin/llmgw
sudo useradd --system --no-create-home --shell /usr/sbin/nologin llmgw
sudo mkdir -p /var/lib/llmgw && sudo chown llmgw:llmgw /var/lib/llmgw
# /etc/systemd/system/llmgw.service → ExecStart=/usr/local/bin/llmgw serve
#   User=llmgw  EnvironmentFile=/etc/llmgw.env  (LLMGW_STATE_DIR=/var/lib/llmgw …)
sudo systemctl enable --now llmgw
```

Put Caddy/nginx in front for TLS (disable proxy buffering so SSE streams).

## Python compatibility build

The Python implementation remains a transport/API compatibility reference. The
Go build is canonical for IAM, SSO, BYOC, persistent quotas, audit, alerts and
management UI. System provider secrets can be seeded from `secrets.json` into
encrypted DB connections; human provider connections are always encrypted and
owner-scoped. Gateway API keys authenticate by hash; when credential encryption is
configured, their owner or an administrator can reveal the encrypted token.

## vNext console and OAuth

`/console` is the primary locally embedded Preact console. `/admin` redirects to it; `/admin-legacy` and `/portal-legacy` preserve the former documents. Build frontend source with `cd internal/web/console && npm ci && npm run build`; committed `dist/` assets are embedded into the static binary, so Node.js is a build-time dependency only.

The supported subscription boundary is intentionally narrow: GitHub Copilot uses official device authorization, and OpenAI Codex uses device authorization plus PKCE at the documented token endpoint. Codex requires an `openai_codex_client_id` the operator is authorized to use and is owner-private/experimental. OpenAI does not currently document third-party Codex client registration, so the gateway does not embed the official CLI's first-party client ID. Claude Code is an Anthropic gateway client, not an OAuth provider; use supported Anthropic API or gateway credentials. No Claude personal OAuth is implemented.

The owner playground is a server-side, non-streaming route exercise. It requires an owner/admin project membership, records keyless project-attributed usage, and never mints or exposes a browser key.
