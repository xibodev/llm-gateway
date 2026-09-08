# Data-plane API

The gateway exposes core OpenAI- and Anthropic-shaped HTTP surfaces. It does not
claim every endpoint or field in either vendor API.

## Authentication

`GET /health` is unauthenticated. Other public data-plane routes accept:

- `x-api-key: <gateway key>`;
- `Authorization: Bearer <gateway key>`.

When both are present, `x-api-key` takes precedence. An invalid `x-api-key` is
not rescued by a valid bearer token. Static gateway keys and active
gateway-issued project keys are accepted. `LLMGW_ALLOW_UNAUTHENTICATED_API=1`
disables only data-plane authentication and is for deliberate local use; admin
authentication remains required.

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

`GET /v1/models` returns only configured rows eligible under the caller's
credential and policy. It is not a global list of every model a provider sells.

## Chat Completions

The core profile supports non-streaming and SSE streaming, ordinary chat
messages, function tools, tool calls, usage normalization, vision filtering, and
request cancellation. Ordered endpoint failover is possible before output
starts.

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
usage event. Request bodies are limited to 32 MiB.

## Embeddings

Embeddings deliberately use one target. Falling back to a different embedding
model would return vectors from another vector space and silently corrupt search
quality. The handler still enforces authentication, routing eligibility, policy,
and usage accounting.

## Audio

Transcription accepts multipart form data with `file` and `model`. Speech accepts
an OpenAI-shaped JSON body. OpenAI-compatible providers receive proxied requests;
`edge_tts` synthesizes MP3 through its native provider implementation.

Audio resolves one target. The public transcription parser permits multipart
input up to 128 MiB.

## Images and video

Image output is returned inline as base64 and supports only
`response_format: b64_json` or an omitted format. Native generation is currently
implemented by capable Google AI Studio/Vertex providers.

Video start returns `202` with an operation ID. Poll with the same model selector
and the `operation` value. Canceling the HTTP request does not cancel an upstream
job after the operation was created.

## Errors and cancellation

Upstream errors preserve meaningful status while credential-shaped diagnostics
are sanitized. Client cancellation on covered coding endpoints stops upstream
work and suppresses success terminals, retry, and failover after abort.

## Management APIs

`/admin/api/*` and `/user/api/*` power the embedded console and portal. They are
authenticated but do not yet have a formal, versioned management OpenAPI
contract. External automation should expect those routes to evolve until such a
contract is introduced.
