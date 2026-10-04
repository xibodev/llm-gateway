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

### Unknown selector

Unknown IDs return `404`. Read `GET /v1/models` and select an advertised ID.

## Failover boundary

For Chat, Responses-compatible fallback, and adapted Messages paths, endpoint
members can advance after supported upstream failures such as throttling, server
errors, or timeouts.

Streaming has a hard boundary: a request can move to the next target only before
the first response byte. Once output starts, a later failure is surfaced to the
client; the gateway does not replay partial output through another model.

Native Anthropic Messages advances past retryable upstream statuses and
statusless provider failures such as malformed responses. It does not hide
definitive client/request HTTP errors by trying another model.

## Failover budget and affinity

Chat Completions and Responses run each routed request under one budget that
covers every attempt in the chain, provider retries included, and the response
stream once one opens. The budget defaults to 2 minutes. When it runs out, the
gateway stops trying further members and the request fails; a stream that is
still open is cut off.

Set another budget per request, in whole milliseconds, with the
`X-LLMGW-Fallback-Timeout-Ms` header or the `fallback_timeout_ms` body field.
The header takes precedence. A zero, negative, or non-integer value keeps the
default, and the body field currently also ignores JSON numbers of 1,000,000 or
more; use the header for longer budgets.

`X-LLMGW-Affinity-Key`, or the `affinity_key` body field, chooses where an
endpoint chain starts; the header takes precedence. When more than one member
is eligible, the gateway hashes the key with SHA-256 and rotates the member
order to start at the member the hash selects; the rest follow in their
configured order, wrapping around. For the same eligible members, the same key
always starts at the same member, while different keys spread across members.
Without a key, the configured order applies.

`/v1/messages` reads neither control. Its streaming chain always uses the
default budget, and its non-streaming chain does not apply the budget.

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
responses, local credential-state failures, and definitive upstream 4xx
responses do not repeat against the same target. Circuit state is process-local
and resets on restart.

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
