# Coding CLI compatibility

These profiles describe the gateway wire contract covered by credential-free
handler fixtures. They do not claim end-to-end testing of an installed client.
Use a gateway key placeholder such as `<LLMGW_API_KEY>`; provider and login
credentials are never part of these fixtures.

| Client | Base URL and credential | Requests and model selection | Fixture status |
| --- | --- | --- | --- |
| Claude Code | `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`; `ANTHROPIC_AUTH_TOKEN=<LLMGW_API_KEY>` supplies a bearer token and `ANTHROPIC_API_KEY=<LLMGW_API_KEY>` supplies `x-api-key`. Use API-key auth with `--bare` when a stored OAuth login or managed setting must not win. If a settings-level base URL exists, override it per invocation with `--settings '{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8787"}}'`; settings env takes precedence over the process env. | `GET /v1/models`; `POST /v1/messages` or bare `/messages`; `POST /v1/messages/count_tokens` or bare `/messages/count_tokens`. Exact `provider/model` and endpoint IDs are authoritative. Bare picker aliases are advertised only when policy leaves one canonical provider/model target. For a Chat-only target, disable client thinking and prompt caching for the core-profile smoke (`alwaysThinkingEnabled: false`, `showThinkingSummaries: false`, `DISABLE_PROMPT_CACHING=1`). | Non-stream native Anthropic targets receive the Messages payload directly, with only resolved model, `stream: false`, and configured gateway preamble changed. Native counting uses the first policy-eligible target; adapted targets return an approximate deterministic compact-JSON estimate marked by `X-LLMGW-Token-Count: estimate` without an upstream call. Counting does not consume inference quotas or write usage. Streaming preserves ordinary metadata and validated `output_config.effort` only on target families that carry them; explicit disabled thinking is preserved by native Anthropic and Copilot Chat targets. Adaptive thinking, structured output, cache controls, Responses-only thinking adaptation, and the extended profile fail closed. |
| Codex | In `config.toml`, select a custom `model_provider`; set that provider's `base_url = "http://127.0.0.1:8787/v1"`, `env_key = "LLMGW_API_KEY"`, `wire_api = "responses"`, and `requires_openai_auth = false`, then set `LLMGW_API_KEY=<LLMGW_API_KEY>`. For a Chat-only target, disable client features that add native Responses tools and output (`web_search`, reasoning summaries, and encrypted reasoning state); the gateway does not silently discard them. | `POST /v1/responses`; bare `/responses` is also accepted. Set the top-level `model` to an exact `provider/model` or endpoint name. | Responses envelope, selected wire-model identity, function-call output, terminal stream events, aliases, safe coding-client metadata/cache hints, and missing-model errors are fixture-covered. Non-empty `include`, reasoning summaries, built-in tools, and stateful Responses require a native Responses target. |
| GitHub Copilot CLI BYOK | `COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:8787/v1`; `COPILOT_PROVIDER_API_KEY=<LLMGW_API_KEY>` is accepted as a bearer token. Set `COPILOT_MODEL=<MODEL_SELECTOR>` and `COPILOT_PROVIDER_WIRE_MODEL=<PROVIDER_WIRE_MODEL>`. Use `COPILOT_PROVIDER_WIRE_API=completions` for Chat Completions or `COPILOT_PROVIDER_WIRE_API=responses` for Responses. | Chat uses `POST /v1/chat/completions` or bare `/chat/completions`; Responses uses `POST /v1/responses`. `<MODEL_SELECTOR>` is the client's selected model entry. `<PROVIDER_WIRE_MODEL>` is the exact `provider/model` or endpoint name sent in the request body, not a display label. For a Chat-only target, use `COPILOT_PROVIDER_WIRE_API=completions`. To send no tools in prompt mode, use an allowlist value that cannot match a tool, such as `--available-tools=__llmgw_no_tools__`; an empty `--available-tools=` value does not filter the installed client. | Chat envelope/tool calls, Responses envelope, selected wire-model identity, stream termination, aliases, safe coding-client metadata/cache hints, and missing-model errors are fixture-covered. Non-empty `include`, reasoning summaries, built-in tools, and stateful Responses require a native Responses target. |

If both headers are present, gateway authentication currently reads `x-api-key`
before `Authorization: Bearer`; an invalid `x-api-key` is not rescued by a valid
bearer token. Endpoint names select their pinned provider/model, while response
model fields identify the model actually served rather than repeating the
endpoint name.

Model discovery is sorted by public ID. Case and dot/dash variants share one
canonical alias key; aliases that collide across providers, with an endpoint,
or with a configured provider namespace are omitted rather than routed by sort
order. Exact `provider/model` and endpoint IDs remain available subject to the
same credential and policy checks. Responses continue to identify the native
model actually served, not the alias selected by the client.

Gateway fixture coverage does not prove an installed CLI works against a real
provider. The `staging -> main` gate in [`BACKLOG.md`](../BACKLOG.md) therefore
uses the installed host clients against the disposable Docker gateway after a
real provider is connected. Cancellation uses the same HTTP request lifecycle
for CLIs, SDKs, browsers, and backend applications.

Adaptation fails closed for adaptive/enabled `thinking`, `redacted_thinking`, `cache_control`, structured output, documents, unsupported image sources/media types, error tool results, native Responses tools, and unknown top-level or content fields. Native non-stream responses preserve those semantic JSON fields, including IDs, model, thinking/signatures, cache usage, and stop data. Token-count request bodies are limited to 32 MiB. Token-count forwarding never copies gateway authentication or arbitrary caller headers: it uses only the resolved provider credential, a valid nonempty caller `anthropic-version` (otherwise the provider default), and caller `anthropic-beta` values for native Anthropic targets.
