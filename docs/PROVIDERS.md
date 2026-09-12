# Provider integrations

The curated registry describes configuration and onboarding paths built into the
console. An `available` row means the transport/auth path exists; it does not
guarantee access to a particular account, model, deployment, region, or quota.
Use **Sync catalog** and **Test completion** against your own provider.

## Vocabulary

- **Provider integration**: a curated registry row such as `openai` or
  `github_copilot`.
- **Provider instance**: one configured ID, runtime type, endpoint, and settings.
- **Provider connection**: one encrypted API-key or OAuth envelope owned by a
  human or the gateway system principal.
- **Exact target**: `provider-instance/model-id`.
- **Endpoint**: a named, ordered chain of exact targets.
- **API surface**: an HTTP route such as `/v1/responses`.

The registry's `categories` metadata is provider taxonomy. It is unrelated to
the deprecated top-level configuration spelling `categories`, whose replacement
is `endpoints`.

## Curated registry

| Integration | Protocol/runtime | Authentication | Scope | Notes |
| --- | --- | --- | --- | --- |
| GitHub Copilot | OpenAI-shaped Copilot transport | Official device authorization | Personal | Disabled by default; owner-private; general gateway use is a grey area. |
| Claude Code | Anthropic gateway client | Gateway key | Client only | Client setup, not an upstream provider; no Claude personal OAuth. |
| OpenAI Codex | Native Responses-oriented transport | Device authorization plus PKCE | Personal | Experimental; requires an OAuth client ID the operator is authorized to use. |
| OpenAI | OpenAI-compatible | API key | System or personal | Standard project API-key integration. |
| Anthropic | Native Anthropic | API key | System or personal | Native Messages and token counting. |
| Google Gemini | Google's OpenAI-compatible endpoint | API key | System or personal | Distinct from native AI Studio. |
| Google AI Studio | Native Gemini | API key | System or personal | Chat, image, and video; native chat streaming is not implemented. |
| Vertex AI (Agent Platform) | Native Gemini/Vertex | API key or service-account JSON | System or personal | Project/location required; discovery credential requirements can differ from inference. |
| OpenRouter | OpenAI-compatible | API key | System or personal | Multi-vendor aggregator. |
| Groq | OpenAI-compatible | API key | System or personal | Hosted low-latency inference. |
| Mistral AI | OpenAI-compatible | API key | System or personal | Hosted Mistral models. |
| Cerebras | OpenAI-compatible | API key | System or personal | Hosted inference. |
| Amazon Bedrock | OpenAI-compatible Bedrock endpoint | Bearer API key plus region | System or personal | No IAM role or SigV4 implementation. |
| Azure OpenAI | Dedicated Chat transport | Azure `api-key` plus resource URL | System or personal | Request model is the deployment name; discovery uses a pinned legacy API version. |
| Ollama | Native Ollama | None | System | Local `/api/tags` discovery and chat/tools. |
| LocalAI | OpenAI-compatible | Optional API key | System or personal | Audio capabilities may be inferred from model names. |
| Edge TTS | Native speech | None by default | System | Unofficial public Edge read-aloud endpoint/token; may change. |
| OpenCode Zen | OpenAI-compatible | None by default | System or personal | Free anonymous multi-model European inference endpoint. |
| Custom OpenAI-compatible | OpenAI-compatible | Optional bearer key | System or personal | Requires a base URL; actual feature support depends on the upstream. |
| Custom Anthropic-compatible | Native Messages | API key | System or personal | Requires a Messages-compatible base URL. |

`litellm` is also available as an advanced runtime type without a curated
registry card.

## Connection resolution

Generic API-key transports resolve:

1. the calling human's active default private connection;
2. the encrypted system connection;
3. the legacy YAML/environment/`secrets.json` credential.

This generic system fallback is not project-binding-gated today.

Copilot is stricter:

- a human uses only that human's active OAuth connection;
- a service can use a gateway-owned credential only through an exact active
  project/provider/`service` binding;
- otherwise the provider contributes no models and cannot be routed.

Codex OAuth is human-private and is not assignable to services or the system
principal. OAuth subscription connections cannot be copied between humans.

## Private and system credentials

Human API-key and OAuth connections are AES-256-GCM encrypted with
`LLMGW_CREDENTIAL_ENCRYPTION_KEY`. List and audit APIs expose metadata, not token
values.

Provider keys entered through the administrator provider form create an
encrypted system connection when encryption is configured and can also remain in
the owner-only plaintext `secrets.json` compatibility seed. Do not claim all
provider credentials are encrypted at rest; protect the state directory and its
backups.

## Google integrations

Google exposes three distinct paths:

| Integration | API grammar | Typical authentication | Distinguishing behavior |
| --- | --- | --- | --- |
| `gemini` | Google's OpenAI-compatible API | API key | OpenAI-shaped client compatibility. |
| `ai_studio` | Native Gemini API | API key | Native image and Veo video support. |
| `vertex_ai` | Native Gemini/Vertex API | Service-account JSON or eligible API key | Project and location scope; Cloud billing. |

Model IDs and regional availability differ. Configure separate provider
instances for locations that expose different model catalogs.

## Local detection

The console can probe common loopback and `host.docker.internal` addresses for
Ollama, LocalAI, LM Studio, vLLM, Jan, text-generation web UI, and llama.cpp.
Detection only reports candidates; the operator chooses what to add. Silent
auto-add requires `LLMGW_AUTODISCOVER_LOCAL=1`.

## Lifecycle evidence

- **Check reachability** proves a catalog request reached the provider.
- **Sync catalog** stores model/capability rows.
- **Test completion** is the only provider action that proves an inference call.
- **Clear cache and retry** invalidates local provider/catalog cache; it cannot
  repair a bad credential, URL, deployment, region, or entitlement.

## Risk boundaries

- Copilot gateway use is not a sanctioned public provider API; keep personal
  entitlements owner-private and respect provider terms.
- OpenAI does not currently document third-party Codex client registration; the
  project does not embed the official CLI's first-party client ID.
- Edge TTS uses an unofficial read-aloud service and may change independently.
- Bedrock uses bearer API keys; IAM/SigV4 role authentication is not implemented.
- Azure deployment discovery depends on a pinned legacy API version.
- Browser-cookie extraction, MITM interception, and stealth session reuse are out
  of scope.

The machine-readable source of this table is
`go/internal/providers/registry_manifest.json`; CI verifies every registry label
appears in this reference and on the website.
