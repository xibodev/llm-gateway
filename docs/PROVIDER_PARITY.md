
# Provider, model and quota product contract

## Status

This document defines the approved target contract for the provider-parity
program on `feat/provider-connections-vnext`. It is not a claim that every
surface below is implemented today.

Implementation status, dependencies, acceptance evidence and deferred scope are
maintained in [`PROVIDER_PARITY_BACKLOG.md`](PROVIDER_PARITY_BACKLOG.md).

The canonical implementation is the Go gateway under `go/`. Existing IAM,
encrypted provider connections, API-key/project governance, usage, audit and
the current pairwise API translators are foundations to extend, not replace.

## Goal

Provide one governed endpoint through which applications and coding clients can:

- connect a broad range of real LLM providers;
- use official API-key, OAuth, imported-token and local/no-auth connections;
- translate supported client and provider API dialects through one explicit
  many-to-many compatibility matrix;
- inspect and manage provider accounts and models;
- test a provider or model in its real routing context;
- see verified upstream quota windows and account health;
- distribute provider quota across gateway-issued API keys;
- route by account health, remaining quota, reset window, cost and priority.

The benchmark is the connected OmniRoute workflow around providers, protocol
translation, models, Playground, provider quota, quota sharing and account-aware
routing. OmniRoute source code, proprietary copy and unrelated features are not
implementation inputs.

## Provider catalog

The registry must be data-driven and validated at build/test time.

Each provider template must declare:

- stable ID, aliases, display label and category;
- protocol and runtime transport;
- accepted request, response and streaming dialects;
- translator capability/adapter IDs required to reach the transport;
- supported authentication methods;
- default endpoint/region and required onboarding fields;
- model-discovery behavior;
- supported endpoint/capability metadata;
- optional quota-adapter ID;
- official documentation URL;
- risk level and operator warning where applicable.

### Breadth acceptance gate

Before parity UAT, the catalog must contain at least 100 independently curated
text/coding LLM provider templates sourced from official documentation. It must
cover:

- direct model vendors;
- OpenAI-compatible and Anthropic-compatible endpoints;
- aggregators and model gateways;
- major cloud/enterprise providers;
- local and self-hosted providers;
- officially supported free/no-auth providers.

An entry counts as available only when its authentication, transport, model
discovery and required translation path are registered and tested. Catalog-only
entries remain planned/unavailable.

Image, video, web-fetch, search, agent/plugin and MITM provider families are not
part of this milestone unless separately approved.

## Authentication families and risk

| Level | Family | Default policy |
| --- | --- | --- |
| Green | Official API key, documented OAuth, local/no-auth | Available when the provider adapter is complete and tested |
| Yellow | Official CLI token import or subscription OAuth with uncertain proxy terms | Human-owner only, disabled by default, explicit warning and audit event |
| Red | Browser-cookie extraction, MITM interception, stealth/session scraping | Out of scope and unavailable |

All OAuth/import connections are:

- private to one human principal;
- AES-256-GCM encrypted;
- never returned to browser or API clients;
- isolated in provider/session caches;
- invalidated immediately after rotation or revocation.

Service principals cannot own or borrow human subscription credentials.

## Protocol translation

Protocol translation is a first-class provider capability, not a special case
inside one transport.

The target is many-to-many compatibility implemented through a typed canonical
intermediate representation, rather than a growing set of direct pairwise
converters. Each supported dialect supplies:

- an ingress decoder into canonical request, tool and content types;
- an egress encoder from canonical requests to the provider dialect;
- a response decoder and client-dialect response encoder;
- streaming event decoders/encoders;
- explicit capability and loss metadata.

The compatibility matrix covers at least:

- system/developer instructions and ordered message history;
- text and supported multimodal content blocks;
- tool definitions, tool choice, calls, results and stable call IDs;
- reasoning/thinking controls and outputs where the dialect supports them;
- structured-output/JSON-schema controls;
- sampling, stop and token-limit controls;
- finish/stop reasons, usage accounting, provider errors and streaming events.

Every matrix cell is classified as `exact`, `lossy` with an explicit documented
reason, or `unsupported`. Unsupported paths fail before provider dispatch.
Fields are never silently dropped, flattened to text or emulated as streaming
while being advertised as full parity.

The current OpenAI Chat pivot, Anthropic Messages converters, Chat/Responses
adaptation and inline Ollama-native conversion are seed implementations only.
They must be migrated behind the translator registry with behavior-preserving
conformance tests.

Provider and model availability is ingress-aware: a model is exposed to a client
dialect only when the gateway can construct a tested request, response and
streaming path for the requested capabilities. This translation layer is the
shared enabler for Antigravity and other providers whose native API is not
OpenAI Chat compatible.

## Provider accounts

One provider may have multiple named accounts. Each account requires safe
operational metadata:

- enabled/default/priority state;
- health and consecutive failure state;
- cooldown and last successful request;
- token expiry/account label/tier;
- optional proxy assignment;
- last quota refresh and quota source.

Routes and combos may select a fixed account or request dynamic account
selection. The current default-connection fallback remains available for
backward compatibility.

## Models

Provider model management must support:

- remote catalog refresh/import;
- provider/account-scoped visibility;
- aliases and custom model IDs;
- display names and capability metadata;
- API source format and target format;
- translator path and exact/lossy/unsupported compatibility state;
- supported endpoints;
- blocked/allowed request parameters;
- opt-in learning from explicit unsupported-parameter responses;
- copy, test and hide/show actions.

Hidden or disallowed models must not leak through `/v1/models`.

## Test and Playground

Provider and model testing is an end-to-end user journey, not a catalog probe.

- Provider **Test** opens a provider-scoped Playground.
- Model **Play/Test** opens the same Playground with that model selected.
- The Playground can select owner, project, gateway key, provider account,
  model or route.
- It supports streaming and non-streaming requests.
- It displays latency, usage, selected account/provider/model, safe fallback
  trace and scoped logs.
- Credentials remain server-side.
- Project and key policies remain enforced.

A separate lightweight catalog-health action may remain, but it must not be
labelled as the provider/model Test journey.

## Quota terminology

### Downstream gateway quota

Already implemented for gateway-issued API keys and projects:

- request limits;
- token limits;
- estimated cost;
- model credits;
- minute/day/month counters.

### Upstream provider quota

New provider-account data supplied by verified adapters:

- quota dimension and unit;
- limit, used and remaining;
- reset time/window;
- account tier;
- source, confidence and refresh time.

Unknown upstream quota stays unknown. Traffic patterns must never be presented
as a fabricated provider limit.

## Provider quota controls

Operators need a dedicated Provider Quota surface with:

- account/tier/token-expiry status;
- all verified quota windows;
- manual refresh;
- source/freshness;
- per-model minimum-remaining thresholds used by routing.

The existing Usage page remains the downstream consumption view.

## Quota sharing

A quota-sharing pool binds one provider account to gateway API keys.

Each pool supports:

- dimensions measured in percent, requests, tokens or cost;
- 5-hour, hourly, daily, weekly and monthly windows;
- per-key weights and optional caps;
- borrowing from unused allocation;
- a hard provider/account cap;
- exclusive keys that only see the pool's virtual models;
- secret-free utilization and borrowing telemetry.

Virtual model IDs use the gateway-owned namespace:

```text
quota/<group>/<provider>/<model>
```

Quota-sharing enforcement composes with, but does not replace, existing key and
project policies.

## Routing and combos

The minimum strategy set is:

- priority/failover;
- weighted;
- round-robin;
- power-of-two-choices;
- least-used;
- cost-first;
- reset-aware;
- headroom;
- last-known-good.

Strategies operate over provider/model/account targets and emit a safe decision
trace. A target is eligible only when a valid translator plan exists for the
request's ingress dialect and required capabilities. Unknown quota must have
explicit, deterministic treatment.

Automatic templates must cover coding, reasoning, fast, chat, vision, free and
premium use cases. Templates are derived from connected capabilities at runtime,
not hard-coded account credentials.

## Required UAT evidence

Automated tests use isolated local provider/OAuth fixtures. Human UAT uses real
integrations and OAuth with synthetic project/user records.

Parity is not accepted until local Docker UAT proves:

1. broad provider search and guided onboarding;
2. API-key, official OAuth and risk-gated import flows;
3. exact/lossy/unsupported translation-matrix evidence across non-streaming,
   streaming, tools and supported multimodal content;
4. multiple accounts with health/cooldown/default/priority behavior;
5. model refresh, visibility, custom model and compatibility controls;
6. provider/model Test to preselected Playground;
7. verified provider quota and threshold-based account exclusion;
8. weighted quota sharing with borrowing and hard caps;
9. account-aware strategy selection and fallback;
10. secret-free audit, usage and routing traces;
11. application consumption through `/v1/models`, Chat, Responses and Messages,
    including one Antigravity end-to-end proof if its approved official flow is
    available, otherwise an operator-approved native non-OpenAI dialect
    substitute.

No push, deployment, DNS or live environment mutation is part of local parity
execution.
