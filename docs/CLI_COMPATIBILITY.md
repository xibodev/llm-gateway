# Coding CLI compatibility

These profiles describe the gateway wire contract covered by credential-free
handler fixtures. They do not claim end-to-end testing of an installed client.
Use a gateway key placeholder such as `<LLMGW_API_KEY>`; provider and login
credentials are never part of these fixtures.

| Client | Base URL and credential | Requests and model selection | Fixture status |
| --- | --- | --- | --- |
| Claude Code | `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`; `ANTHROPIC_AUTH_TOKEN=<LLMGW_API_KEY>` supplies a bearer token. The gateway also accepts `x-api-key`. | `GET /v1/models`; `POST /v1/messages` or bare `/messages`; `POST /v1/messages/count_tokens` or bare `/messages/count_tokens`. Exact `provider/model` and endpoint IDs are authoritative. Bare picker aliases are advertised only when policy leaves one canonical provider/model target. | Non-stream native Anthropic targets receive the Messages payload directly, with only resolved model, `stream: false`, and configured gateway preamble changed. Native counting uses the first policy-eligible target; adapted targets return an approximate deterministic compact-JSON estimate marked by `X-LLMGW-Token-Count: estimate` without an upstream call. Counting does not consume inference quotas or write usage. Adapted Messages targets cover the documented core profile. Streaming remains translated and rejects that extended profile; `STREAM-001B` owns native stream fidelity. |
| Codex | In `config.toml`, select a custom `model_provider`; set that provider's `base_url = "http://127.0.0.1:8787/v1"` and `env_key = "LLMGW_API_KEY"`, then set `LLMGW_API_KEY=<LLMGW_API_KEY>`. | `POST /v1/responses`; bare `/responses` is also accepted. Set the top-level `model` to an exact `provider/model` or endpoint name. | Responses envelope, selected wire-model identity, function-call output, terminal stream events, aliases, and safe missing-model errors are fixture-covered. Stateful Responses require the separately documented private-connection rules. |
| GitHub Copilot CLI BYOK | `COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:8787/v1`; `COPILOT_PROVIDER_API_KEY=<LLMGW_API_KEY>` is accepted as a bearer token. Set `COPILOT_MODEL=<MODEL_SELECTOR>` and `COPILOT_PROVIDER_WIRE_MODEL=<PROVIDER_WIRE_MODEL>`. Use `COPILOT_PROVIDER_WIRE_API=completions` for Chat Completions or `COPILOT_PROVIDER_WIRE_API=responses` for Responses. | Chat uses `POST /v1/chat/completions` or bare `/chat/completions`; Responses uses `POST /v1/responses`. `<MODEL_SELECTOR>` is the client's selected model entry. `<PROVIDER_WIRE_MODEL>` is the exact `provider/model` or endpoint name sent in the request body, not a display label. | Chat envelope/tool calls, Responses envelope, selected wire-model identity, stream termination, aliases, and safe missing-model errors are fixture-covered. |

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

Adaptation fails closed for `thinking`, `redacted_thinking`, `cache_control`, documents, unsupported image sources/media types, error tool results, and unknown top-level or content fields. Native non-stream responses preserve those semantic JSON fields, including IDs, model, thinking/signatures, cache usage, and stop data. Token-count request bodies are limited to 32 MiB. Token-count forwarding never copies gateway authentication or arbitrary caller headers: it uses only the resolved provider credential, a valid nonempty caller `anthropic-version` (otherwise the provider default), and caller `anthropic-beta` values for native Anthropic targets.
