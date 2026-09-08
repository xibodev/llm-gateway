# Configuration reference

llm-gateway combines built-in defaults, a YAML configuration file, and
`LLMGW_*` environment overrides.

## Precedence

Configuration resolves in this order:

1. built-in defaults;
2. `LLMGW_CONFIG`, or `<state>/config.yaml` when unset;
3. environment overrides.

`LLMGW_STATE_DIR` defaults to `~/.llmgw`. `LLMGW_CONFIG_SEED` is copied to the
configured path only when that path does not yet exist. Local Compose uses this
to turn a read-only repository example into writable volume state.

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

The released configuration loader does **not** apply the YAML `policies` block.
Although administration saves can serialize fields such as `retry_max_attempts`,
hand-editing those fields does not configure retry or circuit behavior on restart.
Do not rely on persisted resilience overrides. Circuit state is process-local.

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
| `LLMGW_STATE_DIR` | `~/.llmgw` | Writable state directory. |
| `LLMGW_CONFIG` | `<state>/config.yaml` | Writable runtime configuration path. |
| `LLMGW_CONFIG_SEED` | unset | Read-once seed copied only when `LLMGW_CONFIG` is missing. |
| `LLMGW_API_KEY` | unset | Static recovery/administrator key and accepted data-plane key. |
| `LLMGW_API_KEYS` | unset | Comma-separated additional static gateway keys. |
| `LLMGW_ALLOW_UNAUTHENTICATED_API` | `0` | Disable data-plane authentication for deliberate local use only; never disables admin authentication. |
| `LLMGW_GATEWAY_PREAMBLE` | unset | Optional gateway-owned system preamble. |

### Credential and identity boundary

| Variable | Purpose |
| --- | --- |
| `LLMGW_CREDENTIAL_ENCRYPTION_KEY` | Base64 or hex encoding of exactly 32 bytes. Required for private provider connections and revealable gateway keys. |
| `LLMGW_SSO_ENABLED` | Enable trusted reverse-proxy identity assertions. |
| `LLMGW_SSO_SHARED_SECRET` | Secret that must be overwritten by the trusted proxy. |
| `LLMGW_SSO_ADMIN_GROUP` | SSO group allowed to call admin APIs. |
| `LLMGW_SSO_AUTO_PROVISION` | Provision verified human identities automatically. |
| `LLMGW_OPENAI_CODEX_CLIENT_ID` | OAuth client ID the operator is authorized to use for owner-private Codex connections. |
| `LLMGW_ALLOW_COPILOT_PROXY` | Enable the personal Copilot provider boundary. |
| `LLMGW_EXPERIMENTAL_COPILOT_PROVIDER` | Compatibility enable flag for Copilot provider use. |
| `LLMGW_GITHUB_COPILOT_CACHE_DIR` | Copilot session-cache location. |

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

## Persistence and console saves

The administration console writes providers, endpoints, savings configuration,
and the Codex client ID. It also serializes provider policies, but the released
loader does not reapply them (see [Provider resilience](#provider-resilience)). Environment-only secrets are not
written to YAML. Prefer environment variables for other process settings so an
administration save cannot turn a runtime secret into file content.

Provider keys entered in the administration UI create an encrypted authoritative
connection when credential encryption is configured. The current compatibility
path can also retain a plaintext `0600` `secrets.json` seed. Protect the state
directory and its backups accordingly.

## State files

| Path | Purpose |
| --- | --- |
| `config.yaml` | Provider instances, endpoints, and operational configuration. Serialized resilience policies are not loaded by the released binary. Real runtime config is private and must not be committed. |
| `gateway.db` | IAM, hashed/recoverable keys, encrypted connections, usage, quotas, audit, alerts, and outbox. |
| `catalog.json` | Regenerable provider/model catalog with schema versioning. |
| `telemetry.db` | Interesting failover-chain events. |
| `usage.db` | Optional legacy savings ledger. |
| `secrets.json` | Plaintext compatibility/config seed; owner-only permissions. |
| `cache/` | Copilot OAuth/session cache. |
| `requests.jsonl` | Optional request metadata or unsafe body log. |
| `backups/` | Built-in retention-managed backup directory. |
