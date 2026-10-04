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
quotas. `LLMGW_ALLOW_UNAUTHENTICATED_API=1` disables only data-plane
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
starts. Chat Completions and Responses accept request-level controls for the
failover deadline and member order; see
[failover budget and affinity](ROUTING.md#failover-budget-and-affinity).

Opt-in Chat-to-Responses adaptation uses `force_api_support` and catalog
`supported_surfaces`. Adapted non-streaming responses include
`X-LLMGW-Adapted`. Do not assume every streaming path includes that header.

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

Streaming uses a narrower compatibility profile. Adaptive/enabled thinking,
`redacted_thinking`, cache controls, structured output, documents, unsupported
images, error tool results, and unknown fields fail closed when they cannot be
preserved.

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

## Errors and cancellation

Upstream errors preserve meaningful status while credential-shaped diagnostics
are sanitized. Client cancellation on covered coding endpoints stops upstream
work and suppresses success terminals, retry, and failover after abort.

Errors on `/v1/messages` and `/v1/messages/count_tokens` use Anthropic's
envelope, `{"type":"error","error":{"type":...,"message":...}}`, with the HTTP
status and any `Retry-After` header kept. The type follows the status:
`invalid_request_error` for `400`, `413` and `422`, `authentication_error` for
`401`, `permission_error` for `403`, `not_found_error` for `404`,
`rate_limit_error` for `429`, `overloaded_error` for `529`, and `api_error` for
other `5xx`. Other routes use `{"error":{"message":...,"type":...,"code":...}}`.

A stream whose upstream fails after the first byte, or closes the stream
before its end, ends with the surface's error event and never with its success
terminal: Chat Completions sends an `error` data event without `[DONE]`,
Messages sends `event: error` without `message_delta` or `message_stop`, and
Responses sends `response.failed`. Usage records such a stream as a `502`.

## Management APIs

`/admin/api/*` and `/user/api/*` power the embedded console and portal. They are
authenticated but do not yet have a formal, versioned management OpenAPI
contract. External automation should expect those routes to evolve until such a
contract is introduced.
