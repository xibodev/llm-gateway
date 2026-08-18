# llm-gateway

Standalone, multi-provider LLM proxy for coding CLIs (Claude Code, GitHub Copilot CLI,
Codex). A standalone service with no external coordination-plane dependency.

**Purpose:** connect your providers, then juggle subscription limits and survive
outages. Address any real model as `provider/model`, or define **categories** —
named, ordered failover chains of pinned real models that cascade on
429/5xx/timeout, within a provider and/or across providers.

## Surfaces
- `POST /v1/messages`, `POST /v1/messages/count_tokens` — Anthropic Messages (Claude Code)
- `POST /v1/chat/completions`, `POST /v1/responses` — OpenAI (Codex, Copilot BYOK)
- `POST /v1/audio/transcriptions`, `POST /v1/audio/speech` — audio STT/TTS (proxied to an OpenAI-compatible provider such as LocalAI, or synthesized natively by the built-in `edge_tts` provider — Microsoft Edge read-aloud voices, no credential; its endpoint/token/voice defaults are baked in and overridable via `base_url`, `api_key`, `default_voice`)
- `POST /v1/images/generations` — image generation (OpenAI-shaped, returns `b64_json`)
- `POST /v1/videos/generations` — video generation; starts a long-running job, or polls one when the body carries `operation`
- `GET /v1/models` — every provider's real models, namespaced `provider/model`, plus your category names
- `GET /admin` — SSO/admin control plane: providers, users/services, projects,
  memberships, private provider connections, policies, keys, usage, alerts and audit
- `GET /portal` — SSO self-service: personal keys, usage, provider API-key
  connections and per-human Copilot BYOC
- `GET /health`

Multimodal requests (images) route to vision-capable models automatically. API
keys are shown once and stored only as hashes. Key and project governance supports
expiry, model/provider allowlists, persistent RPM, daily/monthly request, token,
estimated-cost and model-credit budgets. Opt-in **API adaptation** lets a
`/chat/completions` request reach a Responses-only model (e.g. `gpt-5.5`). See
`docs/MULTI_USER.md`, `docs/PROVIDER_PARITY.md`,
`docs/PROVIDER_PARITY_BACKLOG.md`, `SECURITY.md` and `deploy/`.

Provider credentials are resolved explicitly. Humans use their own encrypted
BYOC credential first. A service can use a gateway-managed encrypted provider
credential only when its active project has an active binding for that exact
provider and the `service` principal kind. Provider, project and key model
allowlists still intersect after credential resolution; no provider token is
returned to the workload.

## Model addressing (no tiers, no classifier, no aliases)
- `provider/model` — that exact model on that provider (e.g. `copilot/gpt-4o-mini`). No failover.
- a **category** name — cascades through its pinned models in order (failover).
- a bare **provider-native** name — normalized onto the matching catalog model, so a coding CLI can pass names straight through. Claude Code's `/model` picker sends Anthropic-native rows like `claude-opus-4-8[1m]`; the gateway maps that to `copilot/claude-opus-4.8` (version dots/dashes unified, a `[…]` context tag dropped). A name with no catalog match (e.g. a retired `claude-opus-4`) still `404`s.
- anything else → `404` (pick from `GET /v1/models`).

## Implementation

Go (`go/`) is the only implementation. The console is a Preact bundle embedded
into the binary at build time, so the runtime needs no Node.js. An earlier
Python reference build lived under `src/llmgw/`; it was removed once the Go
build reached parity, and remains in history if you need it.

## Run (Go)
```powershell
cd go
go run ./cmd/llmgw serve
```
Open the panel at `http://127.0.0.1:8787/admin`. Providers/categories you build there persist to `~/.llmgw/config.yaml`. The IAM,
hashed keys, encrypted provider connections, usage, quotas, audit and notification
outbox live in `~/.llmgw/gateway.db` (SQLite WAL). The legacy system
`secrets.json` remains a compatibility/config seed; once a system connection is
seeded, the database is authoritative and later config reloads do not overwrite it.

## Config
`providers` (curated registry integration or advanced type + base URL) and
`categories` (failover chains). Fully editable from `/admin`, or hand-edit — see
`llmgw.config.example.yaml`. Curated registry metadata supplies known endpoints,
authentication methods and planned OAuth integrations without embedding a Node
runtime or a plugin marketplace.

Human-owned provider connections are named, encrypted and private to that
principal. A personal connection overrides the system credential only for that
principal; other callers continue using the encrypted system connection or legacy
config secret. OAuth subscription connections cannot be assigned to service or
system principals.

## API adaptation (`force_api_support`, experimental)
Some OpenAI-family models speak only one API (for example, Copilot GPT-5.x models that require `/responses`). Adaptation is **off by default**: without opt-in, the gateway passes the provider's real HTTP status and error detail through.

Opt in per provider with `force_api_support: true` or per request with `"force_api_support": true`. The gateway uses the persisted model catalog's `supported_endpoints` to decide deterministically, translates Chat Completions to Responses when needed, and marks adapted replies with `X-LLMGW-Adapted` (request-level opt-in also echoes `forced_support` in the body).

## Wire up a CLI
- **Claude Code:** `ANTHROPIC_BASE_URL=http://127.0.0.1:8787` + `ANTHROPIC_AUTH_TOKEN=<LLMGW_API_KEY>`
- **Codex:** provider `base_url=http://127.0.0.1:8787/v1`
- **Copilot CLI (BYOK):** `COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:8787/v1` + `COPILOT_PROVIDER_API_KEY=<LLMGW_API_KEY>`

Pick a `provider/model` or a category name from `GET /v1/models`.

## Deploy (container)
```bash
LLMGW_API_KEY=... docker compose up -d --build
```
Builds `go/Dockerfile` (distroless, nonroot), mounts your config read-only and
persists gateway state in a named volume at `/state`. For a real box with TLS,
use `deploy/docker-compose.prod.yml` + `Caddyfile` — see `deploy/DEPLOY.md`.

## Local console

Open `http://127.0.0.1:8787/console` for the locally embedded operational console. The playground follows the selected model's capability: chat models get a conversation thread, speech models a text-to-audio surface with playback, and transcription models an audio upload. `/admin` redirects there; `/admin-legacy` and `/portal-legacy` remain available while teams transition. `/portal` serves the same bundle in owner mode and calls only `/user/api/*`; console administration calls only `/admin/api/*`. The bundle has no CDN, external font, or runtime Node dependency.

For console source work:
```powershell
cd go/internal/web/console
npm ci
npm run lint
npm test
npm run build
npm run check:dist
```

The generated `dist/` assets are committed because Go embeds them. The Go runtime does not need Node.js.

## Google providers

Google is reachable through two surfaces that share a request grammar and little
else, so they are separate providers:

| | `ai_studio` | `vertex_ai` |
|---|---|---|
| Host | `generativelanguage.googleapis.com` | `{region-}aiplatform.googleapis.com` |
| Auth | API key | service-account key (JSON) or a service-account-bound API key |
| Config | `api_key` | `api_key` or an uploaded key, plus `project`, `location` |
| Billing | Gemini API prepay credits | Cloud billing account |

Model ids differ between them for the same underlying model, and availability is
per model *and* per location: `gemini-3.5-flash` answers at `location: global`
while Veo answers only in a region such as `us-central1`. Configure one provider
per location you need — the model id you use must match the surface.

## Supported private OAuth boundary

- **GitHub Copilot:** official GitHub device authorization, private to one human principal.
- **OpenAI Codex:** device authorization and PKCE code exchange, stored as an encrypted owner-local experimental connection and used through the Codex Responses transport. Set an `openai_codex_client_id` you are authorized to operate before starting this flow. OpenAI does not currently document third-party Codex client registration, so the gateway intentionally does not embed the official CLI's first-party client ID.
- **Claude Code:** a documented Anthropic gateway client. Configure an Anthropic API key or supported gateway credential upstream; the gateway does not implement Claude personal-subscription OAuth.

Automated OAuth tests use only local mocked endpoints and fake tokens. No real provider login is required for development or validation.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

Contributing and agent instructions: [AGENTS.md](AGENTS.md).
