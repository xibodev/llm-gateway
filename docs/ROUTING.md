# Routing and failover

llm-gateway resolves every model selector deterministically before provider
dispatch.

## Selectors

### Exact target

```text
provider-id/model-id
```

This selects one configured provider/model pair. The provider's retry policy can
retry that target, but the request does not move to another target.

### Named endpoint

```yaml
endpoints:
  coding:
    failover:
      - { provider: hosted, model: <model-a> }
      - { provider: local, model: <model-b> }
```

The client requests `coding`. Eligible chat execution walks the ordered members
until one succeeds or the chain is exhausted.

### Bare provider-native alias

Version punctuation and Claude picker context tags are normalized. A bare alias
is advertised only when it resolves to one policy- and credential-eligible exact
target. Aliases that collide across providers, with an endpoint, or with a
provider namespace are omitted rather than guessed.

Bare-alias resolution and the catalog checks of a routed request, such as
whether an OpenAI-compatible model serves Responses natively, read the caller's
stored catalog. A catalog past its one-hour lifetime still answers and is
refreshed in the background, one refresh per catalog at a time, each limited to
30 seconds. Only a catalog that was never stored is discovered while the
request waits, and that discovery ends when the request ends or after 30
seconds.

### Unknown selector

Unknown IDs return `404`. Read `GET /v1/models` and select an advertised ID.

## Failover boundary

For Chat, Responses-compatible fallback, and adapted Messages paths, endpoint
members can advance after supported upstream failures such as throttling, server
errors, timeouts, and [billing and request-shape refusals](#billing-and-request-shape-refusals).

Streaming has a hard boundary: a request can move to the next target only before
the first response byte. Once output starts, a later failure is surfaced to the
client; the gateway does not replay partial output through another model.

Native Anthropic Messages advances past retryable upstream statuses, statusless
provider failures such as malformed responses, and billing and request-shape
refusals. It does not hide other definitive client/request HTTP errors by trying
another model.

### Billing and request-shape refusals

Some refusals say why the request was refused, and the gateway routes on that
reason rather than on the status alone:

- A billing refusal, for want of credit or paid quota, is a `402`, or a refusal
  whose body names billing, such as OpenAI's `insufficient_quota` or Anthropic's
  low credit balance. It moves an endpoint to its next member and counts against
  the provider's circuit, so once the circuit's failure threshold is reached the
  provider is held back for its cooldown instead of refusing every request.
- A request-shape refusal says the request is longer than the model's context,
  or carries tools or images the model does not take. It moves an endpoint to its
  next member without counting against the circuit: the provider is healthy, and
  another member may take the request.

Neither is repeated against the same target, a `429` included. When every member
refuses, the request fails with the last refusal's status and message.

## Failover budget and affinity

Chat Completions, Responses, and Messages run each routed request under a
failover budget, 2 minutes by default. The budget covers choosing a target and
waiting for its answer to begin, provider retries included. It never cuts an
answer that has begun:

- A stream begins when it opens. A stream that has not opened when the budget
  runs out is abandoned, no further member is tried, and the request fails with
  `504`. A stream that has opened runs until it ends or the client disconnects,
  however long that takes.
- A non-streaming answer arrives whole, so the gateway cannot tell a slow
  target from one still producing a long answer. When the budget runs out, the
  gateway starts no further member and no further provider retry, but it does
  not cut the attempt under way; the provider's
  [`timeout`](CONFIGURATION.md#providers) bounds that attempt instead. If it
  then fails, the request fails with `504` when members were left to try, or
  with that attempt's own error when it was the last.

Set another budget per request, in whole milliseconds, with the
`X-LLMGW-Fallback-Timeout-Ms` header or the `fallback_timeout_ms` body field.
The header takes precedence. A zero, negative, or non-integer value keeps the
default.

`X-LLMGW-Affinity-Key`, or the `affinity_key` body field, chooses where an
endpoint chain starts; the header takes precedence. When more than one member
is eligible, the gateway hashes the key with SHA-256 and rotates the member
order to start at the member the hash selects; the rest follow in their
configured order, wrapping around. For the same eligible members, the same key
always starts at the same member, while different keys spread across members.
Without a key, the configured order applies.

On `/v1/messages`, both body fields are gateway controls: they are removed from
the request before it reaches a provider.

## Single-target surfaces

These handlers resolve one eligible target and do not walk a cross-model chain:

- embeddings, because different models produce incompatible vector spaces;
- transcription and speech;
- image generation;
- video start and polling.

An endpoint selector can still be used, but only its first eligible member is
selected for these surfaces.

## Retries and circuit state

Provider retry policy is separate from endpoint failover. YAML `policies`
defaults and exact provider overrides load at startup; circuit counters and
cooldowns remain process-local. See the
[configuration reference](CONFIGURATION.md#provider-resilience).

Retries happen inside one provider target. Endpoint failover moves between
targets. Retryable transport failures, 408, 429, and transient 500/502/503/504,
520-524 (CDN edge) and 529 (overloaded) responses can repeat; malformed
responses, local credential-state failures, definitive upstream 4xx responses,
and billing and request-shape refusals, a 429 among them, do not repeat against
the same target. Circuit state is process-local and resets on restart.

A provider keeps a separate circuit for each caller scope that resolves its
credentials separately: callers limited to the gateway's shared credentials,
each principal, and each service principal within a project. A failing or
rate-limited credential therefore opens only its own scope's circuit, and
answers in one scope do not end a failure streak in another.

The console's provider pages show an open circuit with whose requests it
refuses, its failures in a row and when it admits requests again. In the
administrator state each provider instance lists its open circuits as
`open_circuits`, with the `principal_id` and, for a service principal, the
`project_id` whose requests each refuses; both are empty for callers on the
gateway's shared credentials. A portal user's view lists only the circuit of
the user's own requests. Circuits are process-local, so these are the
answering process's.

Each retry waits a random time between zero and the policy's exponential
backoff (full jitter), so requests that failed together do not retry together.
An upstream `Retry-After` lengthens that wait to at most 5 seconds; a target
that asks for longer is not retried, and the request moves on to the next
member. When the request fails on such a response, the gateway answers with the
upstream's status and passes on the `Retry-After` the provider reported, in
whole seconds rounded up, for OpenAI-compatible, Anthropic, Azure OpenAI,
Google and Ollama providers. Google's quota refusals carry their wait in a
`RetryInfo` detail of the error body rather than a header; the gateway reads
it the same way.

## Policy and credentials

Resolution applies configured-provider status, principal/project membership,
credential availability, project and key model/provider allowlists, and endpoint
membership. If policy removes every target, the request fails closed.

`GET /v1/models` applies the same eligibility boundary, so it is the authoritative
list for a caller's key or human identity.

## Vision filtering

When a chat request contains images, known non-vision targets are removed. If all
candidate capability metadata is unknown, the gateway preserves the original
targets rather than pretending the catalog proved they support vision. Provider
errors remain authoritative in that case.

## API adaptation

`force_api_support` is experimental and off by default. When enabled on the
provider or request, the persisted catalog can direct a Chat Completions request
to a Responses-only model and strictly translate the result.

Adaptation is catalog-driven, not trial-and-error. Unsupported or lossy fields
are rejected before provider dispatch. It is not a universal canonical
translation framework.

## Cancellation

A canceled HTTP request stops retry/failover and closes upstream work for the
covered coding paths. A client that merely stops reading while leaving the
request open has not canceled it. After an asynchronous video operation ID is
returned, canceling the initiating HTTP request does not guarantee provider job
cancellation.
