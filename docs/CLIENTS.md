# Client profiles

These profiles describe the gateway wire contract covered by automated fixtures.
They do not certify every future installed client release. Use an exact model ID
or endpoint returned by `GET /v1/models`.

## Claude Code

For normal use, merge these settings into your private `~/.claude/settings.json`.
Replace the placeholder with a gateway-issued key, never an upstream provider key:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ANTHROPIC_AUTH_TOKEN": "<GATEWAY_PROJECT_KEY>"
  }
}
```

On Claude versions supporting gateway discovery, optionally set
`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` in that `env` object to let the
picker use the gateway's model list. This changes discovery/picker behavior only;
it neither adds model capabilities nor grants account access. Use an exact
eligible model when the installed client does not support discovery. A listed
model is not proof that thinking, tools, caching, or every Claude workflow works
through translation. No subscription or Sonnet entitlement is implied.

Process-scoped API-key setup for a controlled compatibility test:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY='<LLMGW_API_KEY>'
claude --bare --model '<MODEL_SELECTOR>'
```

`ANTHROPIC_AUTH_TOKEN` supplies a bearer token instead. When both gateway headers
are present, `x-api-key` is read first; an invalid `x-api-key` is not rescued by a
valid bearer token.

Claude settings can override process environment. Override the URL per invocation
when necessary:

```bash
claude --bare \
  --settings '{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8787"}}' \
  --model '<MODEL_SELECTOR>'
```

Claude uses `/v1/models`, `/v1/messages`, and
`/v1/messages/count_tokens`; bare aliases are accepted. Native Anthropic targets
receive the non-streaming Messages payload directly. Adapted targets use a strict
compatibility profile and reject fields that cannot be preserved.

For a Chat-only target, disable thinking and prompt caching for a core-profile
smoke:

```json
{
  "alwaysThinkingEnabled": false,
  "showThinkingSummaries": false,
  "env": {"DISABLE_PROMPT_CACHING": "1"}
}
```

Adaptive/enabled thinking, structured output, cache controls, unsupported image
forms, documents, error tool results, and unknown fields fail closed when a
translation cannot preserve them.

## Codex

Add a custom provider to `~/.codex/config.toml`:

```toml
model_provider = "llmgw"
model = "<MODEL_SELECTOR>"

[model_providers.llmgw]
name = "llm-gateway"
base_url = "http://127.0.0.1:8787/v1"
env_key = "LLMGW_API_KEY"
wire_api = "responses"
requires_openai_auth = false
```

Then set the project key in the process:

```bash
export LLMGW_API_KEY='<LLMGW_API_KEY>'
codex
```

Use a native Responses-capable model for built-in tools, reasoning summaries,
encrypted reasoning state, non-empty `include`, and stateful behavior. Strict
Chat fallback is intended for compatible stateless requests and does not silently
discard advanced fields.

## GitHub Copilot CLI BYOK

```bash
export COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:8787/v1
export COPILOT_PROVIDER_API_KEY='<LLMGW_API_KEY>'
export COPILOT_MODEL='<CLIENT_MODEL_ENTRY>'
export COPILOT_PROVIDER_WIRE_MODEL='<MODEL_SELECTOR>'
export COPILOT_PROVIDER_WIRE_API=completions
copilot
```

Use `COPILOT_PROVIDER_WIRE_API=responses` only for a native Responses target.
`COPILOT_PROVIDER_WIRE_MODEL` is the exact `provider/model` or endpoint placed in
the request body, not a display label.

For a no-tool prompt smoke, use an allowlist value that cannot match an installed
tool:

```bash
copilot --available-tools=__llmgw_no_tools__
```

An empty `--available-tools=` does not filter tools. The gateway does not silently
discard native tool schemas.

## OpenAI-compatible SDKs

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY='<LLMGW_API_KEY>'
```

The supported core surfaces are Chat Completions, Responses, models, embeddings,
audio, images, and videos. This is not a claim of every OpenAI API or field.

## Anthropic SDKs

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY='<LLMGW_API_KEY>'
```

Messages and token counting are supported. Token counting uses a native provider
route when available and otherwise returns a marked deterministic estimate.

## Model selection

- Exact `provider/model`: one exact target.
- Endpoint name: ordered failover chain.
- Bare provider-native alias: only when one unambiguous eligible target exists.

Response `model` fields identify the model actually served rather than repeating
an endpoint or alias selected by the client.

## Compatibility gate

Automated fixtures pin model discovery, non-streaming and streaming envelopes,
tools, token counting, errors, and safe metadata. Real-provider and installed
client checks are described in [`../test/uat/README.md`](../test/uat/README.md).
Cancellation follows the same HTTP request lifecycle for CLIs, SDKs, browsers,
and backend clients.
