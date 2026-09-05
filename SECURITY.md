# Security posture

`llm-gateway` is an **internal multi-user coding-CLI gateway**, not a public SaaS
relay. Human users authenticate through a trusted SSO reverse proxy; CLIs and
service principals authenticate with scoped API keys. Administrators, project
owners, members and services are distinct principals.

## Authentication & authorization
- **Admin auth**: Authentik/SSO admin-group assertions signed by a reverse-proxy
  shared secret, or the static `LLMGW_API_KEY` recovery credential. Project keys
  cannot call admin APIs.
- **User auth**: Authentik/SSO assertions provision human principals and power
  `/portal`; same-origin checks protect SSO-authenticated mutations.
- **API auth**: `llmgw_...` keys assigned to exactly one principal and
  project. Requests authenticate against SHA-256 hashes; encrypted token copies
  support explicit owner/admin reveal when credential encryption is configured.
- **Governance**: key + aggregate project policies enforce model/provider
  allowlists and persistent minute/day/month request, token, estimated-cost and
  model-credit limits through atomic SQLite counters.
- Constant-time comparison for admin keys.

## Secrets
- Provider API keys are AES-256-GCM encrypted in `gateway.db` for named system
  and per-human connections. `~/.llmgw/secrets.json` (**0600**) remains a
  compatibility/config seed, never a committed file. Secrets are never returned
  by list APIs (only connection metadata and `api_key_set`).
- IAM, hashed API keys, usage, quotas, audit and notification events live in
  `~/.llmgw/gateway.db` (**0600**, SQLite WAL).
- Provider connections, per-human BYOC credentials and gateway-managed shared
  provider credentials are encrypted with AES-256-GCM using
  `LLMGW_CREDENTIAL_ENCRYPTION_KEY`; the key itself is environment-only. Shared
  credentials require an explicit active project/provider/principal-kind
  binding and are never returned by an API.
- Upstream error bodies are **redacted** (emails + token-shaped strings) before
  they are surfaced or logged.
- Request logging is opt-in (`LLMGW_LOG_REQUESTS=1`) and records metadata only.
  Model-level telemetry remains in the usage ledger; the request log does not
  buffer request content merely to extract a model name.
  Full prompt/provider-response capture requires the separate unsafe opt-in
  `LLMGW_LOG_REQUEST_BODIES=1`; those bodies can contain credentials, PII, or
  proprietary content. Logs rotate at 100 MiB by default
  (`LLMGW_LOG_REQUESTS_MAX_BYTES`).

## Containers
- **Go**: `gcr.io/distroless/static-debian12:nonroot` — a static, stripped
  binary, no shell or package manager, non-root by default.
- **Console**: Node.js and Vite run only while building the embedded static
  assets; neither ships in the runtime image. Build dependencies are still part
  of the release supply chain and must be audited.
- **Dependency audit probes**: run `cd go && govulncheck ./...` and
  `cd internal/web/console && npm audit`. A healthy release has no unresolved
  applicable advisories. Current remediation work is tracked in
  [`BACKLOG.md`](BACKLOG.md) rather than recorded here as a claim that goes stale.
- **Release provenance**: tag releases produce per-binary SPDX SBOMs, verified
  SHA-256 checksums, build-provenance attestations, and a multi-architecture
  image with BuildKit SBOM/provenance. Tags must name the current `main` head,
  build inputs are pinned, and manual dispatch is a non-publishing dry run.

## SSRF — intentionally not guarded
The gateway connects to **operator-configured** provider base URLs, which
routinely include loopback/LAN addresses (Ollama, LocalAI, LM Studio, vLLM).
Blocking private/loopback targets would break the product's core local-provider
feature. Because the operator controls provider URLs and the service is meant for
loopback, request-forgery guards are deliberately omitted.

Do not expose the container port directly. Bind it to loopback and front it with
TLS + Authentik. Caddy must overwrite the SSO assertion secret/header; never
trust identity headers received directly from a client.

## GitHub Copilot as a provider
Using a Copilot subscription as a general gateway provider remains a grey area,
not a sanctioned public API. In multi-user mode, each human may connect their own
Copilot entitlement (BYOC). A service principal cannot own a human credential or
inherit a global credential implicitly. An administrator may import the configured
gateway credential into the encrypted store and bind it to one project, provider
and principal kind. Resolution is own human BYOC first, then that exact active
binding; absent, disabled or revoked credentials and bindings fail closed.

## Reporting
This is a personal project. If you find a security issue, open an issue in the
repository (omit any secrets/tokens from the report).

## Console and OAuth boundaries

- `/console` uses locally bundled assets only. The browser does not load a CDN dependency or runtime Node service.
- OAuth access, refresh, and ID tokens are kept together in the existing AES-GCM encrypted provider-connection boundary. List, poll, refresh, audit, and playground responses expose only safe connection metadata.
- GitHub Copilot and official OpenAI Codex OAuth connections are human-private. Codex persistence is owner-local and experimental; it is never a service/system credential or shared entitlement.
- Claude Code is supported as a documented Anthropic gateway client. The project does not implement reverse-engineered Claude personal OAuth, cookie providers, or MITM credential capture.
- The playground runs on the server with project policy and keyless project counters. Its response is scrubbed of credential-shaped fields before it reaches the browser.

## Provider expansion policy

Provider-platform ideas in `docs/PROVIDER_PARITY.md` are design research, not
committed scope. Any demand-driven expansion must preserve these boundaries:

- Official API-key, OAuth and local/no-auth integrations are preferred.
- Official CLI-token imports or subscription OAuth with uncertain proxy terms
  are human-owner only, disabled by default and require an explicit risk
  acknowledgement recorded in audit.
- Browser-cookie extraction, stealth session reuse and MITM credential capture
  remain out of scope.
- Provider/account/model/quota APIs expose operational metadata only; they never
  return credentials or token-shaped payloads.
- Multiple provider accounts must be isolated in cache keys and routing state.
- Automated tests use local fixtures, while human UAT uses real integrations
  with synthetic identity/project data and disposable encrypted state.
