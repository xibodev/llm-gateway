# Repository backlog

This is the single active implementation tracker for `llm-gateway`. It covers
the whole repository: gateway correctness, security, operations, console,
testing, and provider expansion. Provider parity is one workstream, not a
separate backlog.

[`docs/PROVIDER_PARITY.md`](docs/PROVIDER_PARITY.md) remains a target product
contract. It describes the intended provider, translation, account, quota, and
routing model, but it does not establish implementation status. Current source
and passing tests are the authority for the facts below.

## Tracking rules

| Field | Meaning |
| --- | --- |
| `ready` | Scoped and not waiting on another backlog item. |
| `queued` | Scoped, but waiting on one or more listed dependencies. |
| `in_progress` | An active change is implementing the item. |
| `partial` | A usable foundation exists, but the acceptance boundary is not met. |
| `blocked` | A named decision or dependency prevents implementation. |
| `done` | Acceptance evidence passed and documentation reflects the result. |

- Priorities are `P0` for current correctness/security, `P1` for the next
  architectural or product capability, and `P2` for later expansion.
- A status changes to `done` only with code references and passing automated
  evidence. A commit subject, branch name, or document alone is not evidence.
- Record durable behavior and the command that proves it. Do not record branch
  snapshots, commit hashes, dates, live counts, or deployment observations.
- Keep credentials, identity-bearing values, live infrastructure details, and
  UAT secrets out of this public repository.
- A backlog item authorizes local implementation and validation only. It does
  not authorize a deployment, DNS change, provider login, or remote mutation.
- An item with unmet `Depends on` entries is not pullable even when its state is
  `partial`; `partial` records existing implementation, not dependency readiness.

## Reconciled baseline

The following behavior is present in the current source. The boundary column is
part of the fact: it prevents an implemented foundation from being mistaken for
the larger product capability built on top of it.

| Area | Verified implementation | Evidence | Current boundary |
| --- | --- | --- | --- |
| Runtime | One Go process serves inference, management APIs, and an embedded Preact console. The release image is static, distroless, and non-root. | `go/cmd/llmgw/main.go`, `go/internal/web/web.go`, `go/Dockerfile` | The supported topology is a single writable state directory on one node. |
| Public APIs | OpenAI Chat, Responses, embeddings, audio, image, video, models, and Anthropic Messages/count-tokens routes are registered. Selected bare CLI paths are aliased to `/v1`. | `go/internal/api/server.go` | `/messages/count_tokens` has no bare-path alias; request orchestration is repeated across handlers; there is no generated API contract. |
| Identity and governance | Principals, projects, memberships, hashed one-time API keys, project/key allowlists, persistent request/token/cost/credit quotas, audit, alerts, and an outbox are in SQLite. | `go/internal/iam/schema.go`, `go/internal/iam/keys.go`, `go/internal/iam/quota.go` | The project-policy console omits model-credit fields and can overwrite them with zero on save; the key console exposes only a subset of backend policy fields. |
| SSO and admin access | A trusted Authentik proxy assertion or static recovery key can administer the gateway. Portal mutations require a same-origin browser request. | `go/internal/api/sso.go`, `go/internal/api/auth.go` | The reverse proxy and IdP remain deployment dependencies, not bundled services. |
| Provider credentials | Human connections are named, private, AES-256-GCM encrypted, and cache-isolated. Gateway-owned credentials and exact service bindings have persistent schemas and APIs. | `go/internal/iam/connections.go`, `go/internal/iam/credentials.go`, `go/internal/iam/credential_bindings.go` | The generic provider factory still falls back to the system/config credential for service principals; exact binding authorization is enforced explicitly only by some provider paths. This differs from the documented fail-closed service contract. |
| Provider integrations | A validated manifest drives curated provider metadata. Runtime transports cover OpenAI-compatible providers, native Anthropic, Google AI Studio/Vertex, Azure OpenAI, Bedrock, GitHub Copilot, Ollama, and Edge TTS. Multiple instances of one integration are supported. | `go/internal/providers/registry_manifest.json`, `go/internal/providers/registry_manifest.go`, `go/internal/providers/factory.go` | Providers are compiled into the binary. Manifest entries do not yet prove a registered auth, quota, and translation path as one admission gate. |
| OAuth | General auth-adapter interfaces are used by GitHub Copilot and OpenAI Codex device flows. Tokens are stored in encrypted owner-private connections. | `go/internal/providers/auth_adapter.go`, `go/internal/api/oauth.go` | No broader approved adapter tranche or audited risk-acknowledgement gate exists. |
| Catalogs | Provider/principal/project-scoped catalogs are persisted, schema-versioned, lazily refreshed, and invalidated on relevant credential/configuration changes. `fresh`, `stale`, `unknown`, and `not_discoverable` are distinct states. | `go/internal/providers/catalog.go`, `go/internal/api/provider_status.go` | Model visibility, custom aliases, and compatibility overrides are not persisted separately from fetched catalogs. |
| Routing | Exact `provider/model`, endpoint failover chains, and catalog-backed native aliases resolve deterministically. Providers can retry and use an in-memory circuit breaker. | `go/internal/router/failover.go`, `go/internal/providers/policy.go` | Selection is ordered provider/model failover. It is not account-, quota-, health-, weight-, or cost-aware. |
| Translation | Basic Anthropic text/function-tool cases and OpenAI Chat translate in both directions. Chat and Responses have explicit compatibility checks and native-Responses preservation where supported. Stateful Responses are pinned to an exact private human target. | `go/internal/translate/anthropic.go`, `go/internal/translate/responses.go`, `go/internal/api/responses_api.go` | Anthropic images/documents, thinking signatures, cache controls, rich tool results, some tool-choice controls, usage detail, and stream events can be dropped or flattened. Native Anthropic traffic is translated through OpenAI-shaped maps rather than passed through losslessly. |
| Claude Code compatibility | The gateway exposes `/v1/models`, `/v1/messages`, and `/v1/messages/count_tokens`; it generates Claude-prefixed discovery aliases and normalizes dot/dash forms plus bracketed context tags. | `go/internal/api/models.go`, `go/internal/router/failover.go`, `go/internal/api/anthropic.go` | Model selection is not reliable when providers share a native ID: alias listing and resolution use different ordering and the selected alias loses provider identity. Metadata-light native Anthropic rows can be omitted, endpoint visibility is unproven, `[1m]` is only stripped, Messages translation/streaming is lossy, and token counting is a model-independent byte heuristic. |
| Non-chat modalities | Embeddings and audio use single-target proxy paths; Edge TTS can synthesize natively; Google providers expose image/video interfaces; video uses start-and-poll jobs. | `go/internal/api/embeddings.go`, `go/internal/api/audio.go`, `go/internal/api/media.go` | These paths intentionally do not share all chat failover semantics, and their limits/cancellation behavior is not unified. |
| Provider lifecycle | The console supports provider instances, credential/file onboarding, catalog sync, cache reset, real test completion, model lists, model-to-Playground handoff, and private connection metadata. | `go/internal/web/console/src/pages/ProviderDetail.tsx`, `go/internal/api/provider_status.go` | Account priority, health, cooldown, default selection, and quota controls are not a complete management surface. |
| Provider account state | Connection-scoped priority, credential revision, health, cooldown, expiry, tier, and last-event fields are persisted. Some human-scoped catalog lifecycle probes apply revision-guarded health updates. | `go/internal/iam/provider_accounts.go`, `go/internal/api/provider_status.go` | System credentials and several provider types do not produce account observations; test completions and normal routed inference do not feed health; runtime selection still chooses the default connection rather than an exact account candidate. |
| Upstream quota | Credential-revision-guarded quota snapshots, freshness semantics, secret-shaped metadata rejection, API projections, and an honest `unknown` console view exist. | `go/internal/iam/provider_quota.go`, `go/internal/api/usage_api.go`, `go/internal/web/console/src/pages/UsageQuotas.tsx` | No production quota adapter is registered or invoked. Snapshot data has no routing impact. |
| Playground | Admin users can run project-attributed, non-streaming chat, speech, transcription, image, and video requests; portal users have a registered chat route. Chat returns latency, usage, raw response, and a safe failover trace. | `go/internal/api/playground.go`, `go/internal/api/server.go`, `go/internal/web/console/src/pages/Playground.tsx` | The portal UI exposes media actions whose `/user/api/playground/*` routes are not registered. The Playground cannot stream or select a gateway key/provider account, and it has no correlated log view. |
| Persistence | `gateway.db` is the IAM/control-plane authority. Compatibility/savings usage, failover telemetry, catalogs, configuration, and provider sessions use additional DB/files, some of which may be configured outside the state directory. | `go/internal/iam/store.go`, `go/internal/router/savings.go`, `go/internal/router/telemetry.go`, `go/internal/config/config.go` | A consistent backup must discover every configured durable path, and best-effort duplicate usage/telemetry writes can diverge. |
| Validation | Go build/vet/tests and console typecheck/source tests/dist parity run in CI. A sealed Docker stack models 429, 5xx, timeout, and ordered failover; real-provider UAT is documented separately. | `.github/workflows/ci.yml`, `test/integration/`, `test/uat/` | The sealed stack has no contract-mapped runner and is not a CI job. Console tests inspect source contracts rather than driving a browser. |

## Corrections from the retired tracker

The former provider-only tracker mixed an old branch snapshot with future
requirements. These entries were reconciled before it was removed:

| Former tracker statement | Current fact and disposition |
| --- | --- |
| The provider hub had no provider detail route. | Implemented: provider details, per-instance lifecycle, catalogs, private connections, and Playground handoff are present. No active item repeats this work. |
| Models had no Play action. | Implemented: a provider model opens the Playground with that model selected. Broader model management remains under `MODEL-001`. |
| Provider-account persistence had no production health writer. | Partially superseded: catalog lifecycle probes now write revision-guarded health. Test-completion and normal inference feedback remain under `ACCOUNT-002`. |
| Upstream quota had only storage/API foundations. | The advisory console read path also exists. Production adapters, refresh, thresholds, and routing remain under `QUOTA-001` and `QUOTA-002`. |
| The validated manifest had a fixed historical entry count. | Removed as a status mechanism. The manifest and its validation test are the count/probe; breadth is tracked by acceptance behavior under `PROVIDER-002`. |
| GitHub Actions were deployment-only. | False for this repository: `.github/workflows/ci.yml` runs on pushes and pull requests. The unified backlog preserves the actual CI model. |
| All implementation status belonged to one provider-parity branch. | Removed. This file tracks the repository independent of branch names or historical commit hashes. |

## P0: integrity and security

| ID | State | Work | Acceptance boundary |
| --- | --- | --- | --- |
| `GOV-001` | `ready` | Reconcile provider credential authorization with the documented principal/project contract. The generic API-key factory currently permits service traffic to fall back to a system/config credential without consulting an exact binding. | Define one explicit contract for human, service, admin, and local callers; enforce it before catalog exposure and dispatch for every provider type; test unbound, cross-project, disabled, revoked, personal, and bound cases; include a migration note if existing service access narrows. |
| `CFG-001` | `ready` | Make runtime configuration updates atomic and failure-aware. `config.Get` exposes a live pointer after releasing its lock, while admin `persist` ignores `config.Save` errors. | Readers receive immutable snapshots or remain under a safe accessor; mutation plus persistence has a defined commit/rollback boundary; management APIs report write failures; provider/catalog invalidation happens only for the accepted state; concurrency tests pass, including `go test -race` for affected packages. |
| `HTTP-001` | `ready` | Bound every public request body by surface. Credential routes are bounded, but general JSON and transcription multipart paths are not consistently protected by `http.MaxBytesReader`. | Document per-surface limits; enforce them before decoding/parsing; return stable `413` errors; cover padded JSON, multipart spill-to-disk, oversized media, and normal streaming requests in tests. |
| `HTTP-002` | `ready` | Propagate client cancellation and deadlines through routing, provider interfaces, OAuth/token exchange where applicable, and outbound HTTP requests. | Provider methods accept a context; outbound requests use it; retries/backoff stop promptly; disconnects do not continue billable calls when cancellation is possible; cancellation and timeout errors remain distinguishable and tested. |
| `DEPS-001` | `done` | The console lockfile resolves the Vite toolchain to patched, peer-compatible releases without a major upgrade, and a clean build reproduces the committed embedded bundle. | `cd go/internal/web/console && npm ci && npm audit --json && npm run lint && npm test && npm run build && npm run check:dist`; `cd go && go build ./... && go vet ./... && go test ./...`; `cd go && docker build --pull=false --tag llmgw:deps-001-local --file Dockerfile .` |
| `DATA-001` | `partial` | Define one authoritative persistence and backup model for usage, savings, failover telemetry, catalogs, and control-plane data. | Either migrate active telemetry into `gateway.db` or explicitly version and transactionally back up every sidecar; remove ambiguous duplicate ledgers; supply a tested online backup/restore probe; document retention and failure semantics. |
| `IAM-002` | `done` | The project-policy editor covers every writable backend field, including daily/monthly model credits, and its replacement request preserves the complete policy contract. A source-contract test fails when a future writable field lacks editor coverage. | `cd go/internal/web/console && npm ci && npm audit --json && npm run lint && npm test && npm run check:dist`; `cd go && go build ./... && go vet ./... && go test ./...` |
| `ERROR-001A` | `done` | Protect the core non-streaming inference error path. Native Anthropic, Ollama, and Google failures do not consistently redact upstream detail before it reaches clients, provider verification, or failover telemetry. | Use one idempotent text sanitizer at provider error construction plus API/telemetry defense; preserve upstream HTTP status; cover OpenAI-compatible, Anthropic, Ollama, Google, all-target failure, verification, and raw telemetry with synthetic token/email fixtures. Do not change successful payloads or streaming in this slice. |
| `ERROR-001B` | `done` | After `ERROR-001A`, normalize non-chat proxy failures. Audio and embeddings currently forward non-2xx upstream bodies directly. | Implemented bounded non-2xx response handling for audio and embeddings through sanitized provider invocation errors and the standard gateway envelope. Effective response status is derived after body reading, redirects are not followed, and usage records the final status/error exactly once. Successful bodies and content types remain unchanged. Evidence: `go test ./internal/api ./internal/providers`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `ERROR-001C1` | `done` | Extract the diagnostic sanitizer into a dependency-neutral package so authentication and persistence packages can reuse it without an import cycle. | Preserve existing provider sanitizer behavior and wrappers; sanitize nested maps/slices without mutating input; keep sanitation-before-rune-limit and idempotence; pass existing A/B tests plus nested and boundary tests. |
| `ERROR-001C2` | `done` | GCP token exchange and Copilot/Codex device, token, refresh, and revoke error producers sanitize and bound provider-controlled and transport diagnostics at construction. Package-local typed errors retain safe HTTP status and provider codes without changing successful token, identity, device, cache, or revocation behavior. | Synthetic fixtures cover llmgw keys, bearer values, email addresses, query tokens, semantic poll states, typed status/code, successful device/token flows, and bounded final error text. Evidence: `go test -count=1 ./internal/diagnostics ./internal/gcpauth ./internal/copilotauth ./internal/codexauth`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `ERROR-001C3` | `done` | Generic user/admin OAuth and legacy Copilot browser APIs sanitize every projected error before rune-limiting it, with an adapter-boundary defense for future unsafe poll producers. | Semantic poll statuses and successful device/verification/connection values remain intact; legacy global-admin authorization still persists its token to the isolated global cache, while access, refresh, and ID tokens never enter response envelopes. Synthetic generic and legacy user/admin fixtures cover malicious llmgw keys, bearer values, emails, query tokens, rune bounds, successful values, and authorized cache persistence. Evidence: `go test -count=1 ./internal/providers ./internal/api`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `ERROR-001C4` | `done` | Provider-check detail/model, nested audit detail, and outbox delivery errors are sanitized at persistence and read boundaries. Field-aware sanitation preserves functional model, voice, provider, route, endpoint, operation, and ID values while redacting explicit credential shapes. | Free-form diagnostics retain broad opaque-value protection; sanitization precedes rune-safe bounds; historical unsafe and malformed rows are nonfatal and safe; status, scope, generation, latency, timestamps, payload, retry, and lease semantics remain intact. Evidence: `go test -count=1 ./internal/diagnostics ./internal/iam`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `ERROR-001C5` | `done` | Edge TTS websocket handshake failures expose only a generic transport diagnostic or the final HTTP status through a typed invocation error; credential-bearing URLs, subscription keys, signatures, and connection IDs are discarded. Failed HTTP response bodies close, including both attempts of the single 403 clock-skew retry, while successful synthesis and voice discovery remain unchanged. | Synthetic 401, 403-to-success, 403-to-403, 429, and non-HTTP transport fixtures prove response closure, final status, one retry, and leak-free diagnostics. Evidence: `go test -count=1 ./internal/providers`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `ERROR-001C6` | `done` | Optional body logging persists bounded sanitized diagnostic snapshots instead of raw body prefixes. Complete JSON is recursively sanitized, plain text is sanitized before bounding, and incomplete or capture-limit content becomes a safe byte-count summary. Metadata-only logging, downstream bytes/status, SSE flushing, and complete byte counts remain unchanged. | Exact known Content-Length consumption, conservative unknown/chunked completion, short response writes, and flush-committed status are covered. Evidence: `go test -count=1 ./internal/api`; `go build ./...`; `go vet ./...`; `go test -count=1 ./...`; `govulncheck ./...`; `git diff --check`. |
| `DEPLOY-001` | `ready` | Make the supplied production deployment satisfy the documented SSO and encrypted-credential trust boundary. | Production Compose passes required SSO/encryption settings by reference, Caddy/Authentik deployment guidance overwrites trusted identity headers and keeps the gateway private, startup fails clearly for an incomplete secure mode, and synthetic configuration tests prove the boundary without real secrets. |
| `PLAY-002` | `ready` | Repair portal Playground modality parity. The shared UI calls speech, transcription, image, and video under `/user/api`, but only the admin routes are registered. | Either register owner-scoped media routes with the same project membership/policy/quota checks as chat or hide unsupported portal controls; add handler and browser tests for every displayed modality. |

## P0: Claude Code compatibility

Claude Code is a primary client, so broken discovery or Messages behavior is a
current product defect rather than future provider-parity work. These slices fix
the current architecture first. The later `TRANS-*`, `MODEL-001`, and `API-001`
work must preserve their conformance tests when the internals are replaced.

### Current Claude defect map

| Defect | Current evidence | Owning slice |
| --- | --- | --- |
| A displayed bare alias can name one provider but dispatch to another because model listing iterates an unsorted map while native resolution sorts providers and takes the first canonical match. Policy is applied only after that choice. | `go/internal/api/models.go`, `go/internal/router/failover.go`, `go/internal/api/keypolicy.go` | `CLAUDE-002` |
| Dot/dash/case normalization can collapse distinct native IDs. Endpoint names, slash-containing native IDs, and synthetic `claude-` prefixes share an unchecked namespace. | `go/internal/router/failover.go`, `go/internal/api/admin.go` | `CLAUDE-002` |
| Native Anthropic catalog rows carry too little metadata for `isChatModel`, aliases omit capabilities/surfaces, and endpoint names are not proven visible in Claude's picker. | `go/internal/providers/anthropic.go`, `go/internal/api/models.go` | `CLAUDE-003` |
| Catalog failure/staleness affects bare aliases differently from exact `provider/model`; an empty lazy refresh can retain old rows, and aggregate discovery has no latency contract. | `go/internal/providers/catalog.go`, `go/internal/router/failover.go` | `CLAUDE-003` |
| Messages translation can drop images/documents, thinking and signatures, cache controls, tool-result error state, tool-choice controls, usage detail, and provider-native response semantics. Native Anthropic traffic is double-translated through the OpenAI pivot. | `go/internal/api/anthropic.go`, `go/internal/providers/anthropic.go`, `go/internal/translate/anthropic.go` | `CLAUDE-004` |
| Messages streaming commits `200` before provider setup, does not reliably surface iterator errors, loses native event types, and can present buffered Responses adaptation as streaming. | `go/internal/api/anthropic.go`, `go/internal/providers/anthropic.go`, `go/internal/translate/stream.go` | `CLAUDE-005` |
| Count-tokens divides retained string bytes by four without model resolution or structured content/tools. The bare count route is absent; caller Anthropic version/beta headers and `[1m]` semantics are not preserved. | `go/internal/api/server.go`, `go/internal/api/anthropic.go`, `go/internal/router/failover.go` | `CLAUDE-006` |
| Existing tests cover helper conversions and alias normalization, not an actual Claude-compatible model-list-to-Messages journey with collisions, policy, streaming, thinking, or counting. | `go/internal/router/failover_test.go`, `go/internal/translate/translate_test.go`, `test/integration/` | `CLAUDE-001`, `CLAUDE-007` |

| ID | State | Depends on | Work and acceptance boundary |
| --- | --- | --- | --- |
| `CLAUDE-001` | `ready` | none | Capture a deterministic Claude Code compatibility contract before changing behavior. Add fixture-backed handler tests for model-list decoding, model selection, `/v1/messages`, `/messages`, count-tokens paths, non-streaming/streaming text, tools, images/documents, thinking, cache controls, errors, and usage. Record each feature as exact, lossy, unsupported, or broken; tests for known broken cases may initially assert the current failure and are flipped by the slices below. |
| `CLAUDE-002` | `queued` | `CLAUDE-001`, `GOV-001` | Make model discovery and selection identity-safe. Sort model output; define one canonical public ID for provider models, endpoint routes, and Claude-compatible picker aliases; detect dot/dash/case, slash-native, endpoint, and synthetic-prefix collisions; never advertise an alias whose displayed owner differs from its dispatched target; apply credential and key/project policy before selecting among equivalent targets; preserve a round-trippable model ID in responses. |
| `CLAUDE-003` | `queued` | `CLAUDE-002`, `CLAUDE-004`, `CLAUDE-005` | Make Claude model availability honest after identity, Messages, and streaming behavior are known. Classify native Anthropic and other metadata-light chat rows correctly; expose configured endpoint routes through collision-free IDs proven selectable by the Claude picker; carry capabilities, supported surfaces, source target, and compatibility state onto every picker row; normalize `/v1/...` and unversioned surface spellings consistently; do not advertise Messages streaming/tools/vision/thinking unless the complete path supports them; define stale/empty/undiscoverable catalog behavior and aggregate discovery latency bounds. |
| `CLAUDE-004` | `queued` | `CLAUDE-001` | Close Anthropic Messages request/response fidelity gaps on the current architecture. Preserve or explicitly reject system blocks, images, documents, tool definitions/choice, rich tool results and `is_error`, thinking/redacted-thinking signatures, cache controls/usage, stop reasons, usage details, metadata, and output configuration. Add a direct native Anthropic path or prove the intermediate round-trip is lossless for each advertised feature; mark adapted Messages responses with safe compatibility provenance. |
| `CLAUDE-005` | `queued` | `CLAUDE-001`, `HTTP-002`, `ERROR-001A` | Correct Anthropic streaming and error semantics. Resolve a stream before committing HTTP 200; parse complete SSE records; propagate in-band and transport failures as Anthropic error events without a false `message_stop` or success accounting; preserve native thinking/signature/tool/usage events; retain upstream HTTP status in safe Anthropic-shaped errors; replace buffered Responses emulation with real event translation or mark the path non-streaming for `CLAUDE-003` to advertise honestly. This is the Claude-specific completion slice of `STREAM-001`. |
| `CLAUDE-006` | `queued` | `CLAUDE-001`, `CLAUDE-004` | Make token counting and Anthropic headers explicit and model-aware. Add the bare `/messages/count_tokens` route, validate model/policy/credential context, account for system/content/tool/document/image blocks, proxy native counting when supported, and label or reject estimation where exact counting is impossible. Define and test `anthropic-version`, approved `anthropic-beta`, custom upstream Anthropic auth modes, `[1m]` semantics, one documented downstream credential variable, and deterministic behavior if both `x-api-key` and Bearer authentication are presented. |
| `CLAUDE-007` | `queued` | `TEST-001`, `CLAUDE-003`, `CLAUDE-006` | Run the Claude compatibility acceptance gate through the unified local runner. The sealed stack must prove provider models and configured endpoints appear under picker-selectable IDs and every displayed row routes to its intended provider/model or chain under duplicate catalogs and policy restrictions; then exercise text, tools, multimodal, thinking/cache behavior, streaming failure, token counting, and response-model round-trip. A separately approved disposable real Claude Code run validates the actual CLI; no real login or credential belongs in automated tests. |

## P1: architecture and product completion

| ID | State | Depends on | Work and acceptance boundary |
| --- | --- | --- | --- |
| `TEST-001` | `partial` | `HTTP-001`, `CLAUDE-001` | Add one contract-mapped local acceptance command covering Go build/vet/test, console lint/test/dist parity, vulnerability probes, fresh and upgraded migrations, container build, the sealed failover stack, and Claude compatibility fixtures. It must use synthetic state, require no credential, assert expected outcomes rather than only start services, and leave no containers or volumes behind. |
| `TEST-002` | `queued` | `TEST-001` | Add browser-level admin and portal tests for authentication, first-run setup, provider onboarding, route editing, key lifecycle, and Playground handoff. Keep source-contract tests for cheap invariants, but do not treat them as user-journey proof. |
| `STREAM-001` | `partial` | `HTTP-002`, `CLAUDE-005` | Unify pre-first-byte and mid-stream failure behavior across Chat, Responses, and Messages after the Claude-specific Messages path is corrected. No facade may commit `200` before routing can still return a normal HTTP error; terminal stream errors and usage accounting need dialect-specific conformance tests. |
| `OBS-001` | `partial` | `DATA-001` | Introduce one request/correlation ID and typed, secret-free trace across ingress, policy, translation, target attempts, account selection, usage, audit, and logs. Expose scoped traces through APIs/console without direct DB access. |
| `IAM-001` | `partial` | `GOV-001` | Complete per-key policy management in the console. Every backend-supported allowlist and request/token/cost/credit limit must be viewable and editable without exposing key material; revoked keys remain irreversible. |
| `DEPR-001` | `ready` | none | Finish the `categories` to `endpoints` and `supported_endpoints` to `supported_surfaces` deprecations using an explicit release boundary. Remove dual routes/fields only after compatibility tests and upgrade notes identify affected clients. |
| `TRANS-001` | `queued` | `TEST-001`, `CLAUDE-007` | Generalize the executable Claude contract into a translation capability matrix for Chat, Responses, Messages, Ollama, and each admitted native dialect. Classify request, response, tools, multimodal, reasoning, structured output, cache controls, usage, errors, model identity, and streams as exact, explicitly lossy, or unsupported. |
| `TRANS-002` | `queued` | `TRANS-001` | Introduce typed canonical request, response, tool, content, usage, error, and stream-event representations, then migrate current map-based paths incrementally behind compatibility tests. Provider-specific payload maps must stop at adapter boundaries. |
| `TRANS-003` | `queued` | `TRANS-002` | Register ingress/egress translators and a compatibility planner. Reject unsupported capability combinations before provider dispatch and expose the chosen path/loss state to model listing, Playground, and traces. |
| `AUTH-001` | `partial` | none | Complete the auth-adapter admission and risk policy. Every available OAuth/import integration must resolve a registered adapter; yellow flows must be disabled by default, human-only, warning-gated, and audited; red flows cannot register. Add further official adapters only with fixture tests and explicit operator approval for real login UAT. |
| `PROVIDER-001` | `partial` | `AUTH-001`, `TRANS-003` | Strengthen provider admission. An `available` manifest entry must have official provenance plus registered runtime, auth, discovery, quota-if-declared, and tested translation paths. Dangling adapter IDs must fail validation. |
| `ONBOARD-001` | `partial` | `AUTH-001`, `PROVIDER-001` | Complete manifest-driven onboarding from provider selection through credential/base URL/region/owner fields to a validated result. Yellow-risk flows require explicit acknowledgement and audit; red interception/scraping flows remain impossible to register. |
| `ACCOUNT-001` | `partial` | `GOV-001`, `PROVIDER-001` | Build providers for an exact connection ID and credential revision. Cache keys and invalidation must include both. Add scoped APIs/UI for default, priority, status, expiry, tier, health, cooldown, and approved proxy references without returning credential-derived data. |
| `ACCOUNT-002` | `partial` | `ACCOUNT-001`, `OBS-001` | Feed test-completion and normal request success/failure into monotonic, revision-guarded account health. Define cooldown/recovery behavior and ensure stale completions cannot overwrite newer state or rotated credentials. |
| `MODEL-001` | `queued` | `CLAUDE-002`, `CLAUDE-003`, `TRANS-003`, `ACCOUNT-001` | Store operator-defined model overrides separately from fetched catalogs: visibility, display label, aliases/custom IDs, source/target dialect, compatibility/loss policy, and allowed/blocked parameters. Preserve the canonical identity and collision rules established by the Claude slices; enforce visibility in models, native resolution, endpoints, Playground, and future virtual models. |
| `PLAY-001` | `partial` | `PLAY-002`, `ACCOUNT-001`, `MODEL-001`, `OBS-001`, `STREAM-001` | Preserve the implemented model handoff and add provider/account context into a streaming Playground with selectable server-side execution context by active key ID and exact provider account. Return correlation ID, translation plan/loss, latency, usage, served account/provider/model, fallback trace, and scoped logs without retrieving a key secret. |
| `QUOTA-001` | `partial` | `ACCOUNT-001`, `PROVIDER-001` | Wire a bounded quota refresh lifecycle to the adapter registry and implement verified GitHub Copilot and OpenAI Codex adapters where official responses support it. Persist only normalized secret-free snapshots guarded by credential revision; unknown remains unknown. |
| `QUOTA-002` | `queued` | `QUOTA-001`, `MODEL-001` | Add per-model/account minimum-remaining thresholds with explicit stale/unknown treatment. Expose source, confidence, freshness, reset, and exclusion reason; feed eligibility without replacing downstream project/key quotas. |
| `OPS-001` | `partial` | `DATA-001`, `OBS-001` | Add readiness and operational probes that distinguish process health, writable state, migration health, and optional provider reachability. Provide tested backup/restore and graceful-shutdown procedures for the supported single-node topology. |

## P2: routing and expansion

| ID | State | Depends on | Work and acceptance boundary |
| --- | --- | --- | --- |
| `ROUTE-001` | `queued` | `ACCOUNT-002`, `QUOTA-002`, `MODEL-001`, `TRANS-003` | Define a typed routing candidate and safe decision trace containing provider/model/account, policy, translation, health, cooldown, quota, cost, and recent-use facts. Ineligible candidates must be excluded before dispatch. |
| `ROUTE-002` | `queued` | `ROUTE-001` | Add fixed-account and dynamic strategies: priority, weighted, round-robin, power-of-two choices, least-used, last-known-good, cost-first, reset-aware, and headroom. Preserve ordered endpoint failover as the compatibility default and test deterministic concurrency. |
| `SHARE-001` | `queued` | `ROUTE-002` | Add provider-account quota pools with weighted key allocations, caps, borrowing, supported reset windows, and `quota/<group>/<provider>/<model>` virtual IDs. Pool enforcement must compose with stricter project/key policy and emit secret-free utilization evidence. |
| `COMBO-001` | `queued` | `ROUTE-002`, `SHARE-001` | Derive optional coding, reasoning, fast, chat, vision, free, and premium route templates from connected capabilities and compatibility plans. Never materialize credentials or advertise an unavailable template as usable. |
| `PROVIDER-002` | `queued` | `PROVIDER-001`, `TRANS-003`, `TEST-001` | Expand curated text/coding integrations from official documentation toward the provider-contract breadth gate. An entry counts as available only with fixture-backed auth, transport, discovery, and translation behavior; catalog-only entries remain planned. |
| `PROVIDER-003` | `queued` | `PROVIDER-001`, `ACCOUNT-001`, `QUOTA-001`, `TRANS-003` | Prove one additional native non-OpenAI dialect through the shared translator and account/quota path. Attempt an approved official Google flow first; if unavailable, record the reason and use another operator-approved native provider without cookie extraction or MITM behavior. |
| `UAT-001` | `blocked` | `CLAUDE-007`, `ONBOARD-001`, `PROVIDER-002`, `PROVIDER-003`, `PLAY-001`, `ROUTE-002`, `SHARE-001`, `TEST-001`, `TEST-002` | Run disposable real-provider and real-client UAT with synthetic identities and explicit operator-supplied credentials. Prove Claude Code model selection plus Models, Chat, Responses, Messages, translation, OAuth, account routing, quota, sharing, audit, backup, and teardown. Blocked until dependencies pass locally and an operator approves each real login. |
| `API-001` | `queued` | `TRANS-003`, `DEPR-001` | Publish a machine-readable public API contract for stable inference and management surfaces, including authentication, limits, error envelopes, streaming events, deprecations, and compatibility guarantees. Validate examples against handlers in CI. |
| `RELEASE-001` | `partial` | `TEST-001`, `DATA-001` | Make release identity come from one source and verify binaries, images, embedded console assets, checksums, migration compatibility, and public documentation from the tagged tree. Eliminate conflicting hard-coded version claims. |
| `WEB-001` | `blocked` | `RELEASE-001` | Publish the dependency-free website only after the repository URL, host, and domain are explicitly chosen. Keep the marketing surface separate from protected gateway administration paths. |

## Deferred and excluded

| Scope | Disposition |
| --- | --- |
| Fusion, pipeline, context-relay, strict-random, and other advanced routing strategies | Defer until `ROUTE-002` is complete and measured. |
| Live WebSocket cascade visualization and queue simulation | Defer until typed traces from `OBS-001` exist. |
| Provider breadth beyond the product-contract gate | Continue after `PROVIDER-002`; it must not weaken admission evidence. |
| New image, video, search, web-fetch, agent, or plugin families | Require a separate product and security decision; current modality support is not blanket approval to expand. |
| Browser-cookie extraction, MITM interception, stealth session reuse, and credential scraping | Prohibited. Do not implement or register these providers. |
| Remote UAT deployment, DNS, and live infrastructure mutation | Outside this backlog's authority and requires explicit operator approval. |

## Recommended execution order

1. Close the finite current defects: `DEPS-001`, `IAM-002`, `ERROR-001A`,
   `ERROR-001B`, `ERROR-001C1` through `ERROR-001C6`, `PLAY-002`, `DEPLOY-001`,
   `GOV-001`, `CFG-001`, `HTTP-001`, and `HTTP-002`.
2. Build `CLAUDE-001`, then complete `TEST-001` so every following compatibility
   slice runs through one repeatable local gate.
3. Implement `CLAUDE-002` and `CLAUDE-004` in parallel. Follow `CLAUDE-004` with
   `CLAUDE-006`; build `CLAUDE-005` after `HTTP-002` and `ERROR-001A`; then close
   `CLAUDE-003` against the resulting identity, Messages, and streaming behavior.
4. Complete `CLAUDE-007`. The gateway must have a reliable primary-client
   contract before its translation internals are replaced.
5. Finish `DATA-001`, then `OBS-001`. Establish `TRANS-001` through `TRANS-003`,
   preserving the Claude gate; complete `AUTH-001`, `PROVIDER-001`, and then
   `ONBOARD-001`.
6. Complete `ACCOUNT-001`. Then build `MODEL-001`, `QUOTA-001`, `ACCOUNT-002`,
   and `STREAM-001` as their dependencies permit; follow with `QUOTA-002`,
   `PLAY-001`, and `OPS-001`.
7. Add `ROUTE-001`, `ROUTE-002`, quota sharing, combos, and provider breadth
   only after eligibility and translation decisions are testable before dispatch.
8. Run `TEST-002`, then `UAT-001` last. Local fixtures and unit tests do not
   establish a real CLI, provider login, quota report, or deployment result.

## Definition of done

Every implementation item must satisfy its row's acceptance boundary and the
repository gates relevant to its change. The complete local gate remains:

```bash
cd go && go build ./... && go vet ./... && go test ./...
cd internal/web/console && npm ci && npm run lint && npm test && npm run check:dist
```

Changes to persistence also require fresh and upgraded migration tests. Changes
to provider, routing, translation, or streaming behavior require deterministic
fixture coverage. Changes to a user journey require browser-level evidence once
`TEST-002` exists. Update this backlog and the affected current-state or target
contract documentation in the same change.
