# Data-plane API

The gateway exposes core OpenAI- and Anthropic-shaped HTTP surfaces. It does not
claim every endpoint or field in either vendor API.

## Authentication

`GET /health` is unauthenticated. Other public data-plane routes accept:

- `x-api-key: <gateway key>`;
- `Authorization: Bearer <gateway key>`.

When both are present, `x-api-key` takes precedence. An invalid `x-api-key` is
not rescued by a valid bearer token. Active gateway-issued project keys are
accepted; give them to clients. The static administrator keys (`LLMGW_API_KEY`,
`LLMGW_API_KEYS`) are also accepted but bypass key and project allowlists and
quotas; this is deprecated, and `LLMGW_ADMIN_KEYS_ON_DATA_PLANE=false` refuses
them with `403`. `LLMGW_ALLOW_UNAUTHENTICATED_API=1` disables only data-plane
authentication and is for deliberate local use; admin authentication remains
required. In that mode any token, or none, is accepted, and browser requests
from a non-loopback `Origin` are refused with `403`; see
[unauthenticated local mode](CONFIGURATION.md#unauthenticated-local-mode).

## Routes

| Method | Canonical path | Behavior |
| --- | --- | --- |
| `GET` | `/health` | Process liveness and build identity. It does not probe providers or database freshness. |
| `GET` | `/v1/models` | Configured, credential- and policy-eligible models, endpoint pseudo-models, and unambiguous aliases. |
| `POST` | `/v1/chat/completions` | OpenAI-shaped chat, tools, streaming, usage, and vision target filtering. |
| `POST` | `/v1/responses` | Native Responses or strict compatible stateless Chat fallback. |
| `POST` | `/v1/messages` | Native Anthropic Messages or strict Chat adaptation. |
| `POST` | `/v1/messages/count_tokens` | Native count when supported; otherwise a marked deterministic estimate. |
| `POST` | `/v1/embeddings` | Single-target OpenAI-shaped embedding proxy. |
| `POST` | `/v1/audio/transcriptions` | Multipart single-target speech-to-text proxy. |
| `POST` | `/v1/audio/speech` | Single-target speech synthesis, including native Edge TTS. |
| `POST` | `/v1/images/generations` | Image generation from a capable provider; returns inline `b64_json`. |
| `POST` | `/v1/videos/generations` | Starts a video operation or polls one when `operation` is supplied. |

## Bare aliases

The same handlers accept:

```text
/models
/chat/completions
/responses
/completions              -> Chat Completions, not legacy Completions
/messages
/messages/count_tokens
/embeddings
/audio/transcriptions
/audio/speech
/images/generations
/videos/generations
```

There is no `/v1/completions` route.

## Models

The request `model` is an exact `provider/model`, named endpoint, or advertised
bare alias. Response model fields identify what actually served the request.
Catalog inclusion proves caller-scoped routing eligibility, not fresh provider
discovery, current inference health, or the caller's upstream entitlement.

`GET /v1/models` returns only configured rows eligible under the caller's
credential and policy. It is not a global list of every model a provider sells.

## Chat Completions

The core profile supports non-streaming and SSE streaming, ordinary chat
messages, function tools, tool calls, usage normalization, vision filtering, and
request cancellation. A streaming request's `stream_options` reaches the
upstream, so `include_usage` returns the stream's usage where the upstream
reports it. Ordered endpoint failover is possible before output
starts. Chat Completions, Responses, and Messages accept request-level controls
for the failover deadline and member order; see
[failover budget and affinity](ROUTING.md#failover-budget-and-affinity).

Opt-in Chat-to-Responses adaptation uses `force_api_support` and catalog
`supported_surfaces`. Adapted non-streaming responses include
`X-LLMGW-Adapted`. Do not assume every streaming path includes that header.

### Request fields

`fallback_timeout_ms`, `affinity_key`, and `force_api_support` are the
gateway's own and never reach the upstream. Every other field, including
`response_format`, `n`, `seed`, `logprobs`, `parallel_tool_calls`, `user`, and
fields the gateway does not know, reaches an OpenAI-compatible, OpenAI, or
LiteLLM target unchanged, in transparent mode too. Other targets send what their
wire carries:

| Target | Fields sent besides `model`, `messages`, `stream`, and `stream_options` |
| --- | --- |
| OpenAI-compatible, OpenAI, LiteLLM | Every field. |
| Bedrock, GitHub Copilot | `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `tools`, `tool_choice`, `reasoning_effort`, `metadata`, `parallel_tool_calls`, `thinking`. |
| OpenAI Codex, Google Antigravity, anonymous OpenCode Zen, Anthropic setup token | The same, without `parallel_tool_calls` and `thinking`. |
| Azure OpenAI | Every request property of Azure's v1 Chat Completions, and `service_tier`: `audio`, `frequency_penalty`, `function_call`, `functions`, `logit_bias`, `logprobs`, `max_completion_tokens`, `max_tokens`, `metadata`, `modalities`, `n`, `parallel_tool_calls`, `prediction`, `presence_penalty`, `prompt_cache_key`, `prompt_cache_retention`, `reasoning_effort`, `response_format`, `safety_identifier`, `seed`, `service_tier`, `stop`, `store`, `temperature`, `tool_choice`, `tools`, `top_logprobs`, `top_p`, `user`, `user_security_context`, `verbosity`. |
| Chat adapted to a Responses-only model (`force_api_support`) | `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `tools`, `tool_choice`, `metadata`, `reasoning_effort`. |
| Anthropic | `temperature`, `top_p`, `max_tokens`, `stop`, `tools`, `metadata`, `thinking`, `output_config`. |
| Google AI Studio, Vertex AI | `temperature`, `max_tokens`. |
| Ollama | `temperature`, `top_p`, `max_tokens`, `tools`. |

Anthropic, Google and Ollama targets read `max_completion_tokens` as their
output limit when `max_tokens` is unset.

A request that sets a field whose loss would change the answer, and that its
target does not send, fails with `400` naming the field before anything is sent;
for example `response_format`, `n` above 1, `logprobs`, `stop`, or `tools` on a
target that cannot carry them. Advisory fields are dropped for such targets, as
earlier releases dropped them, and do not refuse the request: `user`,
`metadata`, `store`, `service_tier`, `prompt_cache_key`, `safety_identifier`,
`seed`, `top_p`, `presence_penalty`, `frequency_penalty`, and
`reasoning_effort`. A value that asks for nothing does not count either:
`null`, `false`, an empty value, `n: 1`, a zero `top_logprobs`, a
`response_format` of type `text`, text-only `modalities`, `tool_choice: "auto"`,
and `parallel_tool_calls: true`. In an endpoint, a member that cannot send a
field is skipped as capability filtering skips one, and the request fails only
when no member can send it. Field names that begin with `_` are reserved and
refused.

## Responses

Native Responses targets preserve the Responses envelope and streaming events.
Compatible stateless requests can fall back through a strict Chat translation.

`background: true` is rejected even for native targets. Built-in tools, non-empty `include`,
reasoning summaries, encrypted reasoning state, and other stateful behavior
require a native Responses target with the specific capability and account
entitlement. Native transport alone is not a compatibility guarantee. Stateful requests require an exact model and
a private human connection; route failover cannot preserve provider state.

## Anthropic Messages

Native Anthropic targets receive non-streaming Messages payloads directly after
resolved-model and gateway-preamble changes. Adapted targets reject unsupported
or lossy fields before dispatch.

A stream whose every target is a native Anthropic target is sent the same way,
with `stream: true`, and each record the target sends reaches the client
unchanged, `ping` and thinking deltas included. Its usage is read from those
records: input and cache tokens from `message_start`, output tokens from
`message_delta`. A stream with any adapted target, such as an endpoint that
mixes native and adapted members, is translated and uses a narrower
compatibility profile: adaptive/enabled thinking, `redacted_thinking`, cache
controls, structured output, documents, unsupported images, error tool results,
and unknown fields fail closed when they cannot be preserved.

## Token counting

Native Anthropic counting uses the first eligible target. Other targets receive a
compact-JSON estimate marked by `X-LLMGW-Token-Count: estimate` without an
upstream inference call. Counting does not consume inference quotas or write a
usage event. Its request bodies are limited to 32 MiB, or to the general
[request size limit](#request-size) when that is lower.

## Embeddings

Embeddings deliberately use one target. Falling back to a different embedding
model would return vectors from another vector space and silently corrupt search
quality. The handler still enforces authentication, routing eligibility, policy,
and usage accounting.

## Audio

Transcription accepts multipart form data with `file` and `model`. Speech accepts
an OpenAI-shaped JSON body. OpenAI-compatible providers receive proxied requests;
`edge_tts` synthesizes MP3 through its native provider implementation.

Audio resolves one target. Transcription uploads count against the
[request size limit](#request-size).

## Images and video

Image output is returned inline as base64 and supports only
`response_format: b64_json` or an omitted format. Native generation is currently
implemented by capable Google AI Studio/Vertex providers.

Video start returns `202` with an operation ID. Poll with the same model selector
and the `operation` value. Canceling the HTTP request does not cancel an upstream
job after the operation was created.

## Request size

Request bodies are limited to 64 MiB unless `LLMGW_MAX_REQUEST_BODY_BYTES`
[changes the limit](CONFIGURATION.md#server-and-state). A larger body is refused
with `413` in the route's error envelope: before it is read when it declares a
`Content-Length`, and as soon as it crosses the limit otherwise. Multipart
uploads count in full. The same limit bounds the management APIs.

## Request IDs

Every response carries an `X-Request-Id` header naming its request, errors
included; a stream carries it before its first event. A client may name its
own request by sending `X-Request-Id` with 1 to 128 letters, digits, `.`, `_`,
`:` or `-`, and the response carries that value back. Without one, or with any
other value, the gateway assigns an ID: `req_` followed by 32 hexadecimal
digits.

Quote the ID when reporting a failed request: it is how an operator finds the
request's usage record, request log entry and audit event; see
[correlating a request](OPERATIONS.md#correlating-a-request). Send a different
ID for every request, retries included. A usage record's ID is unique, so a
request that repeats an ID an earlier one was recorded under is recorded under
an ID the gateway assigns instead.

## Errors and cancellation

Upstream errors preserve meaningful status while credential-shaped diagnostics
are sanitized. Client cancellation on covered coding endpoints stops upstream
work and suppresses success terminals, retry, and failover after abort.

Errors on `/v1/messages` and `/v1/messages/count_tokens` use Anthropic's
envelope,
`{"type":"error","error":{"type":...,"message":...},"request_id":...}`, with
the HTTP status and any `Retry-After` header kept. The type follows the status:
`invalid_request_error` for `400`, `413` and `422`, `authentication_error` for
`401`, `permission_error` for `403`, `not_found_error` for `404`,
`rate_limit_error` for `429`, `overloaded_error` for `529`, and `api_error` for
other `5xx`. Other routes use
`{"error":{"message":...,"type":...,"code":...,"request_id":...}}`. In both,
`request_id` repeats the response's `X-Request-Id`.

A stream whose upstream fails after the first byte, or closes the stream
before its end, ends with the surface's error event and never with its success
terminal: Chat Completions sends an `error` data event without `[DONE]`,
Messages sends `event: error` without `message_delta` or `message_stop`, and
Responses sends `response.failed`. A native Messages stream whose upstream
sent its own `event: error` ends with that one. Usage records such a stream as
a `502`.

## Management APIs

`/admin/api/*` and `/user/api/*` power the embedded console and portal. They are
authenticated but do not yet have a formal, versioned management OpenAPI
contract. External automation should expect those routes to evolve until such a
contract is introduced.
