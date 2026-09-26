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
| Anthropic | Native Anthropic | API key or setup token | System or personal | Native Messages and token counting; setup tokens are stored only as encrypted connections. |
| Google Gemini | Google's OpenAI-compatible endpoint | API key | System or personal | Distinct from native AI Studio. |
| Google AI Studio | Native Gemini | API key | System or personal | Chat, image, and video; native chat streaming is not implemented. |
| Vertex AI (Agent Platform) | Native Gemini/Vertex | API key or service-account JSON | System or personal | Project/location required; discovery credential requirements can differ from inference. Optional request type selects default, PayGo, or dedicated provisioned throughput. |
| Google Antigravity | Cloud Code Assist `v1internal` | Browser OAuth | Personal | Experimental undocumented API; exact upstream catalog, non-streaming chat, tools, and reasoning. |
| OpenRouter | OpenAI-compatible | API key | System or personal | Multi-vendor aggregator. |
| Groq | OpenAI-compatible | API key | System or personal | Hosted low-latency inference. |
| Mistral AI | OpenAI-compatible | API key | System or personal | Hosted Mistral models. |
| Cerebras | OpenAI-compatible | API key | System or personal | Hosted inference. |
| Amazon Bedrock | OpenAI-compatible Bedrock endpoint | Bearer API key plus region | System or personal | No IAM role or SigV4 implementation. |
| Azure OpenAI | Dedicated Chat transport | Azure `api-key` plus resource URL | System or personal | Request model is the deployment name; discovery uses a pinned legacy API version. |
| Ollama | Native Ollama | None | System | Local `/api/tags` discovery and chat/tools. |
| LocalAI | OpenAI-compatible | Optional API key | System or personal | Audio capabilities may be inferred from model names. |
| Edge TTS | Native speech | None by default | System | Unofficial public Edge read-aloud endpoint/token; may change. |
| ElevenLabs | Native speech | API key | System or personal | Direct speech-to-text and text-to-speech through the OpenAI audio surfaces; voice IDs remain request parameters. |
| Xiaomi MiMo | Chat Completions audio | API key | System or personal | Direct text-to-speech; the gateway maps OpenAI speech requests to MiMo's documented audio contract. |
| OpenCode Zen | OpenAI-compatible | None by default | System or personal | Free anonymous multi-model European inference endpoint. |
| Kilo Code | OpenAI-compatible | None by default | System or personal | Free anonymous multi-model inference endpoint with auto/free routing. |
| LLM7.io | OpenAI-compatible | None by default | System or personal | Anonymous turbo-tier chat models. |
| OVH AI Endpoints | OpenAI-compatible | None by default | System or personal | Anonymous per-IP inference tier; availability is rate-limited. |
| Pollinations.ai | OpenAI-compatible | None by default | System or personal | Anonymous text inference with provider-specific discovery paths. |
| Custom OpenAI-compatible | OpenAI-compatible | Optional bearer key | System or personal | Requires a base URL; actual feature support depends on the upstream. |
| Custom Anthropic-compatible | Native Messages | API key | System or personal | Requires a Messages-compatible base URL. |

`litellm` is also available as an advanced runtime type without a curated
registry card.

## Connection resolution

Generic API-key providers resolve:

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
The gateway embeds the verified public Codex client ID and supports official
device authorization plus browser PKCE. Browser sign-in uses OpenAI's registered
loopback redirect and accepts the final redirect URL pasted back into the console;
no client secret, browser cookie, or local Codex credential file is imported.
Connections created before client-profile binding was introduced must be
reauthorized once: older encrypted envelopes do not contain the OAuth client
identity required for safe token refresh, and the gateway does not guess it.

Codex models are native to Responses. Chat Completions and Messages requests
reach them through a translated facade. `max_tokens`, `temperature`, `top_p`,
and `stop` are ignored, because the official Codex client never sends them.
Fields that change the structure of the answer are rejected with a
configuration error instead of being dropped. These include a non-text
`response_format`, `n` above 1, `logprobs`, a `tool_choice` other than `auto`,
and audio output.

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

Google exposes four distinct paths:

| Integration | API grammar | Typical authentication | Distinguishing behavior |
| --- | --- | --- | --- |
| `gemini` | Google's OpenAI-compatible API | API key | OpenAI-shaped client compatibility. |
| `ai_studio` | Native Gemini API | API key | Native image and Veo video support. |
| `vertex_ai` | Native Gemini/Vertex API | Service-account JSON or eligible API key | Project and location scope; Cloud billing. `vertex_request_type: dedicated` requires matching provisioned capacity and never silently falls back. |
| `google_antigravity` | Cloud Code Assist `v1internal` | Browser OAuth | Experimental owner-private catalog and non-streaming chat; requires an operator-provided OAuth client. |

Model IDs and regional availability differ. Configure separate provider
instances for locations that expose different model catalogs.

## Local detection

The console can probe common loopback and `host.docker.internal` addresses for
Ollama, LocalAI, LM Studio, vLLM, Jan, text-generation web UI, and llama.cpp.
Detection only reports candidates; the operator chooses what to add. Silent
auto-add requires `LLMGW_AUTODISCOVER_LOCAL=1`.

## Anonymous provider automation

`LLMGW_ANONYMOUS_PROVIDER_AUTOMATION=true` opts a deployment into connecting
the reviewed remote no-key providers. An administrator can override that
default from **Settings → Automatic anonymous providers** or return to the
deployment default.

Automation is deliberately narrower than the community roster. Eligibility is
compiled into the curated registry and currently covers OpenCode Zen, Kilo
Code, LLM7.io, OVH AI Endpoints, and Pollinations.ai. Each provider has a
reviewed catalog shape, free-model discriminator, request path, and preferred
verification model. A roster claim alone cannot enroll a provider.

On startup and then at most once every 24 hours per provider, the worker adds a
missing default instance, refreshes its filtered free catalog, sends one minimal
completion, and records the result. It never creates credentials, changes
routes, overwrites an existing provider ID, re-enables a disabled provider, or
deletes configuration when turned off. Customized or credentialed instances are
left unmanaged; rate limits and outages are recorded without removing them.

## Lifecycle evidence

- **Check reachability** proves a catalog request reached the provider.
- **Sync catalog** stores model/capability rows.
- **Test completion** is the only provider action that proves an inference call.
- **Clear cache and retry** invalidates local provider/catalog cache; it cannot
  repair a bad credential, URL, deployment, region, or entitlement.

## Risk boundaries

- Copilot gateway use is not a sanctioned public provider API; keep personal
  entitlements owner-private and respect provider terms.
- Codex subscription transport uses OpenAI's public Codex OAuth client and
  ChatGPT backend. Keep it owner-private and expect upstream compatibility to change.
- Google Antigravity uses undocumented Cloud Code Assist `v1internal` APIs and
  can change or stop working independently of this project.
- Edge TTS uses an unofficial read-aloud service and may change independently.
- Bedrock uses bearer API keys; IAM/SigV4 role authentication is not implemented.
  Without a `base_url` it is reached at its `region`'s endpoint, and a region
  that is not an AWS region name is refused. A request without a configured
  `timeout` gives up after 300 seconds.
- Azure deployment discovery depends on a pinned legacy API version.
- Browser-cookie extraction, MITM interception, and stealth session reuse are out
  of scope.

The machine-readable source of this table is
`go/internal/providers/registry_snapshot.json`, the effective registry: the
reviewed `llmgw-core` manifest plus the gateway overlay
`go/internal/providers/registry_overlay.json`. CI verifies every registry label
appears in this reference and on the website.
