# Limitations and non-goals

This page is part of the product contract. It prevents broad gateway language
from implying behavior the implementation does not provide.

## Scale and availability

- Single-node SQLite control plane with one deterministic database connection.
- No clustered state, distributed locks, horizontal scaling, or HA failover.
- No hyperscale throughput target.

## Protocol scope

- Core OpenAI Chat/Responses/models/embeddings/audio/image/video surfaces, not
  every OpenAI API or field.
- Core Anthropic Messages and token counting, not every Anthropic beta feature.
- Unsupported or lossy translated fields fail closed.
- No formal, versioned management OpenAPI yet.

## Routing

- Ordered endpoint failover, not weighted, round-robin, least-used, cost-first,
  quota-aware, reset-aware, or headroom routing.
- Streaming can fail over only before the first response byte.
- Embeddings, audio, image, and video select one eligible target.
- Provider circuit state is process-local.
- The released loader does not apply YAML retry/circuit `policies`; do not rely
  on serialized overrides surviving restart. See [configuration](CONFIGURATION.md#provider-resilience).

## Provider behavior

- Registry availability does not guarantee an account entitlement, deployment,
  model, region, or installed-client result.
- Google AI Studio and Vertex native chat streaming is not implemented.
- Image/video generation is implemented only by capable Google providers today.
- Upstream quota storage/advisory foundations exist, but no production quota
  adapter drives routing; unknown remains unknown.
- Estimated cost is not provider invoice reconciliation. Unknown models can
  record zero unless price overrides are configured.

## Client confidence

Claude Code, Codex, and Copilot CLI have fixture-backed wire profiles plus a
documented real-client UAT gate. This is not blanket certification of every
future installed client release.

## Identity and credentials

- Generic system API-key fallback is not project-binding-gated. The exact binding
  requirement applies to the supported Copilot shared-service path.
- Codex OAuth and personal Copilot OAuth are human-private.
- The gateway does not implement Claude personal-subscription OAuth.
- `secrets.json` can retain plaintext system-provider credentials for
  compatibility; not every secret is encrypted at rest.
- No browser-cookie extraction, MITM interception, or stealth session reuse.

## Console gaps

- The primary key editor exposes common RPM/daily fields; the project policy
  editor and APIs cover the full policy model.
- Shared Copilot binding APIs exist without a complete primary-console editor.
- Provider `force_api_support` is configurable through YAML/API but not every UI
  form exposes it.
- Portal chat works, while capability-specific portal playground actions are not
  all registered as separate user API routes.
- The API supports embeddings; the primary console does not have an embeddings
  playground.

## Operations

- Backups are operator-triggered and local; there is no built-in schedule or
  remote backup destination.
- Request logs are excluded from built-in backups.
- Checksums detect corruption, not archive authorship.
- `/health` proves process liveness, not provider readiness.
- Alert delivery requires an external worker/webhook.
- The included Caddy/Compose files are not a turnkey Authentik deployment.

## Security boundary

- SSRF blocking is intentionally absent because local/LAN provider URLs are a
  core feature.
- Do not expose the raw listener publicly. Terminate TLS and enforce trusted
  identity at a reverse proxy.
- Full request-body logging can persist secrets, personal data, and proprietary
  source.

## Not on the product plan

- Universal canonical translation IR or translator marketplace.
- A 100-provider target.
- Provider quota pools, borrowing, or virtual quotas.
- Trace-console expansion or streaming account selection in the playground.
- Public hosted gateway service.
