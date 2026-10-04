# Configuration reference

llm-gateway combines built-in defaults, a YAML configuration file, and
`LLMGW_*` environment overrides.

## Precedence

Configuration resolves in this order:

1. built-in defaults;
2. `LLMGW_CONFIG`, or `<state>/config.yaml` when unset;
3. environment overrides.

Anonymous-provider automation is the deliberate exception: the environment is
the deployment default, and an administrator can persist an explicit On or Off
override in `gateway.db`. Selecting **Use deployment default** removes that UI
override.

`LLMGW_STATE_DIR` defaults to `~/.llmgw`. `LLMGW_CONFIG_SEED` is copied to the
configured path only when that path does not yet exist. Local Compose uses this
to turn a read-only repository example into writable volume state.

A missing configuration file means the built-in defaults, while a file that
cannot be read or parsed, or a seed that cannot be read, parsed, or copied, stops
startup with an error naming the file and YAML line, and administration saves
refuse to overwrite such a file.

## YAML structure

### Providers

Each configured provider instance has an operator-chosen ID:

```yaml
providers:
  local-chat:
    type: ollama
    base_url: http://127.0.0.1:11434

  hosted-chat:
    type: openai_compatible
    registry_id: openrouter
    base_url: https://openrouter.ai/api/v1
    api_key: ${ENV:OPENROUTER_API_KEY}
    timeout: 120
    force_api_support: false
```

Supported fields depend on the runtime type:

| Field | Meaning |
| --- | --- |
| `type` | Runtime transport, such as `openai_compatible`, `anthropic`, `ollama`, `github_copilot`, `vertex_ai`, or `edge_tts`. |
| `registry_id` | Optional curated integration whose defaults and onboarding metadata describe this instance. |
| `base_url` | Provider API base URL. Private/LAN addresses are allowed by design. |
| `api_key` | Literal or `${ENV:NAME}` reference. Environment references are recommended; never commit literal secrets. |
| `timeout` | Provider request timeout in seconds. |
| `region` | Provider region, used by Bedrock. |
| `project` | Cloud project, used by Vertex AI. |
| `location` | Cloud location, used by Vertex AI. |
| `vertex_request_type` | Optional Vertex invocation mode: `default`, `paygo`, or `dedicated`. Unset preserves Google's default routing; `dedicated` requires matching provisioned throughput and does not fall back. |
| `default_voice` | Default voice for speech providers. |
| `disabled` | Keep the instance configured while removing it from routing. |
| `force_api_support` | Opt the instance into experimental catalog-driven API adaptation. |

The curated integration matrix is in [`PROVIDERS.md`](PROVIDERS.md).

### Endpoints

`endpoints` is the canonical name for ordered failover chains:

```yaml
endpoints:
  coding:
    failover:
      - { provider: hosted-chat, model: <hosted-model-id> }
      - { provider: local-chat, model: <local-model-id> }
```

The deprecated `categories` key still loads when `endpoints` is absent. When both
are present, `endpoints` wins and the maps are not merged.

### Provider resilience

The configuration loader applies the YAML `policies` block at startup. Defaults
apply to every provider unless an exact provider ID has an entry under
`policies.overrides`. Administration saves persist these values without exposing
credentials. Supported fields include `retry_max_attempts`, retry backoff values,
`circuit_failure_threshold`, and `circuit_cooldown_seconds`. Circuit counters and
cooldown state remain process-local and reset on restart.

Use the supported provider and endpoint fields in the
[released example configuration](../llmgw.config.example.yaml). Endpoint failover
is separate from retries within a provider; see [routing](ROUTING.md).

### Legacy savings ledger

`gateway.db` is the authoritative usage store. The older synchronous savings
ledger is disabled by default:

```yaml
savings:
  enabled: false
  # db_path: /absolute/path/to/usage.db
  # baseline_model: provider/model
```

Enable it only for compatibility. Its configured database is included in built-in
backups and pruned with the usage-retention window.

## Environment variables

### Server and state

| Variable | Default | Purpose |
| --- | --- | --- |
| `LLMGW_HOST` | `127.0.0.1` | Listen host. Containers set `0.0.0.0` and publish loopback or place a proxy in front. |
| `LLMGW_PORT` | `8787` | Listen port. |
| `LLMGW_MAX_REQUEST_BODY_BYTES` | `67108864` | Largest request body accepted, in bytes; a larger one is refused with `413`. Values below `1048576` are raised to it, and invalid values use the default. |
| `LLMGW_SHUTDOWN_TIMEOUT_SECONDS` | `25` | Whole seconds a stop (SIGINT or SIGTERM) gives requests in flight, streams included, to finish before closing them; `0` closes them at once. A second signal exits immediately. |
| `LLMGW_STATE_DIR` | `~/.llmgw` | Writable state directory. |
| `LLMGW_CONFIG` | `<state>/config.yaml` | Writable runtime configuration path. |
| `LLMGW_CONFIG_SEED` | unset | Read-once seed copied only when `LLMGW_CONFIG` is missing. |
| `LLMGW_API_KEY` | unset | Static recovery/administrator key for the console and `/admin/api/*`. `/v1/*` also accepts it, without key or project allowlists and quotas; give clients gateway-issued project keys instead. |
| `LLMGW_API_KEYS` | unset | Comma-separated additional administrator keys, each with the same powers as `LLMGW_API_KEY`. Entries are used verbatim, so do not add spaces. When any are set, the gateway logs a startup warning with their count, never their values. |
| `LLMGW_EXTERNAL_KEYS_FILE` | unset | Opt in to a reloadable version-1 JSON gateway-key document. |
| `LLMGW_EXTERNAL_KEYS_URL` | unset | Opt in to a reloadable version-1 JSON gateway-key endpoint; supports ETag/304. |
| `LLMGW_EXTERNAL_KEYS_HTTP_TOKEN` | unset | Optional bearer credential for the external key endpoint. |
| `LLMGW_EXTERNAL_KEYS_REFRESH_INTERVAL` | `30s` | File/HTTP key snapshot refresh interval. |
| `LLMGW_EXTERNAL_KEYS_HTTP_TIMEOUT` | `5s` | Per-request deadline for the external key endpoint. |
| `LLMGW_EXTERNAL_KEYS_MAX_STALENESS` | `0` | Optional duration after which an unrefreshed source is excluded (fail closed); zero keeps last-known-good indefinitely. |
| `LLMGW_ALLOW_UNAUTHENTICATED_API` | `0` | Disable data-plane authentication for deliberate local use only; never disables admin authentication. See [unauthenticated local mode](#unauthenticated-local-mode). |
| `LLMGW_GATEWAY_PREAMBLE` | unset | Optional gateway-owned system preamble. |
| `LLMGW_ANONYMOUS_PROVIDER_AUTOMATION` | `false` | Deployment default for connecting and checking reviewed no-key providers. Admin Settings can override it. |

An Antigravity provider may instead declare a caller-owned public OAuth client:

```yaml
providers:
  antigravity:
    type: google_antigravity
    public_oauth_client_id: <registered-public-client-id>
```

This selects `public_pkce` and sends no client secret. Without this provider
setting, the runtime client ID and secret select `client_secret_post`. The
gateway stores the selected nonsecret profile identity with the OAuth token so
refresh continues to use the same client and mode.

Antigravity also supports the explicit `consumer_manual` profile for OAuth
clients whose registered redirect cannot call the gateway. Configure it at
runtime with the variables below, or enter the same values in the administrator
OAuth dialog to store them encrypted. The gateway always generates PKCE, opens
the authorization URL, and accepts a pasted code or full redirect URL. A full
URL must carry the flow's matching state. The redirect URI is fixed by the
configured profile and is reused exactly for exchange and refresh binding.

### Unauthenticated local mode

`LLMGW_ALLOW_UNAUTHENTICATED_API=1` serves `/v1/*` without a gateway key, as a
local principal that can use every configured provider. Any token, or none, is
accepted, because CLIs and SDKs often insist on sending a placeholder key. A
token that is an active gateway-issued key is still served as that key, with its
policy and quotas. Admin and portal APIs keep their own authentication.

Browsers add an `Origin` header to the requests web pages make. In local mode a
request whose `Origin` host is not `localhost`, a name under `.localhost`, an
address in `127.0.0.0/8`, or `::1` is refused with `403` unless it carries a
gateway-issued key, so a web page open in the operator's browser cannot use the
gateway. Requests without `Origin`, as CLIs and SDKs send them, are unaffected.

At startup the gateway logs a warning when local mode is on and `LLMGW_HOST` is
not a loopback address, because every client that can reach the listener can
then use the data plane. A container listens on `0.0.0.0`, so publish its port
on loopback only.

### Credential and identity boundary

| Variable | Purpose |
| --- | --- |
| `LLMGW_CREDENTIAL_ENCRYPTION_KEY` | Base64 or hex encoding of exactly 32 bytes. Required for private provider connections and revealable gateway keys. |
| `LLMGW_SSO_ENABLED` | Enable trusted reverse-proxy identity assertions. |
| `LLMGW_SSO_SHARED_SECRET` | Secret that must be overwritten by the trusted proxy. |
| `LLMGW_SSO_ADMIN_GROUP` | SSO group allowed to call admin APIs. |
| `LLMGW_SSO_AUTO_PROVISION` | Provision verified human identities automatically. |
| `LLMGW_OPENAI_CODEX_CLIENT_ID` | OAuth client ID the operator is authorized to use for owner-private Codex connections. |
| `LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_ID` | Confidential OAuth client ID used with explicit `client_secret_post` for experimental owner-private Antigravity connections. |
| `LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_SECRET` | Matching confidential-client secret; runtime-only and never written to gateway config. |
| `LLMGW_GOOGLE_ANTIGRAVITY_OAUTH_PROFILE` | Set to `consumer_manual` to enable the manual-code profile from runtime configuration. |
| `LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_MODE` | Required manual profile mode: `public` or `confidential`. |
| `LLMGW_GOOGLE_ANTIGRAVITY_REDIRECT_URI` | Exact registered redirect URI used by the manual profile. |
| `LLMGW_OAUTH_PUBLIC_BASE_URL` | Public HTTP(S) origin used for browser OAuth callbacks; required outside loopback. Register `<origin>/oauth/callback/google_antigravity` as the exact redirect URI. |
| `LLMGW_GITHUB_COPILOT_OAUTH_TOKEN` | Optional GitHub OAuth token that an administrator can import as the shared credential of a `github_copilot` provider (`source: configured`). |
| `LLMGW_GITHUB_COPILOT_CACHE_DIR` | Copilot session-cache location. |

### Providers and discovery

| Variable | Default | Purpose |
| --- | --- | --- |
| `LLMGW_OPENAI_COMPATIBLE_BASE_URL` | `https://api.openai.com/v1` | Base URL for `openai_compatible`, `openai`, and `litellm` providers that set no `base_url`. |
| `LLMGW_OPENAI_COMPATIBLE_TIMEOUT_SECONDS` | `300` | Request timeout for `openai_compatible`, `openai`, `litellm`, `azure_openai`, `elevenlabs`, and `mimo` providers that set no `timeout`. |
| `LLMGW_OLLAMA_BASE_URL` | `http://127.0.0.1:11434` | Base URL for `ollama` providers that set no `base_url`. |
| `LLMGW_OLLAMA_TIMEOUT_SECONDS` | `30` | Request timeout for `ollama` providers that set no `timeout`. |
| `LLMGW_EXTENSION_URL` | `http://127.0.0.1:18888` | Address of the optional companion daemon, distributed separately, that serves the GitHub Copilot, OpenAI Codex, Google Antigravity, Edge TTS, anonymous OpenCode Zen, and Anthropic setup-token provider types. Must be an absolute `http` or `https` URL without credentials, query, or fragment. |
| `LLMGW_EXTENSION_SECRET` | unset | Shared secret sent to that daemon as a bearer token. Set it, and start the daemon with the same value: a daemon started without a secret accepts any caller. |
| `LLMGW_ANTHROPIC_DISCOVERY_ALIASES` | `true` | `GET /v1/models` also lists chat models whose IDs start with `claude` or `anthropic` under their bare ID, so Claude Code's gateway model discovery shows them. Affects listing only, not routing. |
| `LLMGW_ANTHROPIC_DISCOVERY_ALL_MODELS` | `false` | Lets every other model be requested as `claude-<model-id>`, and lists chat models that way while discovery aliases are on. |
| `LLMGW_AUTODISCOVER_LOCAL` | `0` | When `1`, `true`, `yes`, or `on`, `serve` adds every reachable [local model server](PROVIDERS.md#local-detection) that is not configured yet and saves it to the configuration. |

### Provider roster

| Variable | Default | Purpose |
| --- | --- | --- |
| `LLMGW_PROVIDER_ROSTER_URL` | `https://xibodev.github.io/llm-gateway/roster/payload.json` | Discovery feed; must be a public HTTPS URL. `serve` applies the default only when `LLMGW_PROVIDER_ROSTER_DISABLE` is unset. |
| `LLMGW_PROVIDER_ROSTER_DISABLE` | unset | Any non-empty value stops `serve` from applying the default feed URL; an explicit `LLMGW_PROVIDER_ROSTER_URL` still applies. |
| `LLMGW_PROVIDER_ROSTER_AUTO_REFRESH` | `true` | `true` or `false`: refresh the feed in the background. |
| `LLMGW_PROVIDER_ROSTER_PUBLIC_KEY` | unset | Optional base64 Ed25519 public key; when set, the feed must be a signed envelope. |
| `LLMGW_PROVIDER_ROSTER_KEY_ID` | `staging` | Expected envelope key ID; used only with a public key. |

See the [provider roster guide](PROVIDER_ROSTER.md#gateway-environment-and-refresh)
for refresh and trust behavior.

### Logging and retention

| Variable | Default | Purpose |
| --- | --- | --- |
| `LLMGW_LOG_REQUESTS` | `0` | Append metadata for `POST /v1/*` requests. |
| `LLMGW_LOG_REQUEST_BODIES` | `0` | Unsafe explicit opt-in for prompt and response bodies. |
| `LLMGW_LOG_REQUESTS_MAX_BYTES` | `104857600` | Rotate the active request log at this size and keep one `.1` generation. |
| `LLMGW_RETENTION_USAGE_DAYS` | `90` | Usage, failover telemetry, and optional savings history. |
| `LLMGW_RETENTION_AUDIT_DAYS` | `365` | Audit-history window. |
| `LLMGW_RETENTION_DELIVERED_OUTBOX_DAYS` | `400` | Delivered notification tombstones; lower values fall back to 400. |
| `LLMGW_BACKUP_KEEP` | `7` | Verified archives retained only in the built-in default backup directory. |

Retention runs after startup and then every 24 hours. Current quota periods,
pending/failed outbox work, identities, credentials, provider state, and policies
are not age-pruned.

### Variables without effect

These variables currently have no effect:

- `LLMGW_RATE_LIMIT_PER_MINUTE` is parsed but not enforced. Use per-key or
  project requests-per-minute limits instead; see
  [policy and quotas](MULTI_USER.md#policy-and-quotas).
- `LLMGW_ALLOW_COPILOT_PROXY` and `LLMGW_EXPERIMENTAL_COPILOT_PROVIDER`.
- `LLMGW_OPENAI_COMPATIBLE_API_KEY`.
- `LLMGW_GITHUB_COPILOT_USE_GH_CLI`, `LLMGW_GITHUB_COPILOT_TIMEOUT_SECONDS`,
  `LLMGW_GITHUB_COPILOT_EDITOR_VERSION`, and
  `LLMGW_GITHUB_COPILOT_INTEGRATION_ID`.

## Persistence and console saves

The administration console writes providers, endpoints, provider policies,
savings configuration, and the Codex client ID. The loader reapplies those
values at startup (see [Provider resilience](#provider-resilience)).
Environment-only secrets are not written to YAML. Prefer environment variables
for other process settings so an administration save cannot turn a runtime
secret into file content.

External key documents are opt-in and replace their source atomically after a
successful refresh. File documents use `{"version":1,"keys":[...]}`; HTTP
documents also require `count` equal to the number of keys. A key record requires
`name`, an existing active `project` slug, and exactly one of `key` or `key_sha256`, and may set
`expires_at` (RFC3339), `allowed_models`, `allowed_routes`, and
`allowed_providers`. Failed refreshes retain the last accepted snapshot unless
`LLMGW_EXTERNAL_KEYS_MAX_STALENESS` excludes it. Existing administrator and IAM
keys keep precedence and behavior.

Provider keys entered in the administration UI create an encrypted authoritative
connection when credential encryption is configured. The current compatibility
path can also retain a plaintext `0600` `secrets.json` seed. Protect the state
directory and its backups accordingly.

## State files

| Path | Purpose |
| --- | --- |
| `config.yaml` | Provider instances, endpoints, resilience policies, and operational configuration. Real runtime config is private and must not be committed. |
| `gateway.db` | IAM, hashed/recoverable keys, encrypted connections, usage, quotas, audit, alerts, and outbox. |
| `catalog.json` | Regenerable provider/model catalog with schema versioning. |
| `telemetry.db` | Interesting failover-chain events. |
| `usage.db` | Optional legacy savings ledger. |
| `secrets.json` | Plaintext compatibility/config seed; owner-only permissions. |
| `cache/` | Copilot OAuth/session cache. |
| `requests.jsonl` | Optional request metadata or unsafe body log. |
| `backups/` | Built-in retention-managed backup directory. |
