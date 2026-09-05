# Multi-user control plane

The production Go gateway remains one static binary with `gateway.db` as its
authoritative SQLite control-plane store. The current build also writes optional
local usage/failover sidecars; their removal from request latency is tracked in
`BACKLOG.md`. The gateway does not require Postgres, Redis, an embedded IdP,
SMTP or notification SDKs.

## Identity model

- **Human principal**: provisioned from Authentik SSO; may connect named private
  provider API keys, its own Copilot OAuth entitlement, and use `/portal`.
- **Service principal**: created by an administrator for a workload; cannot own
  a human Copilot subscription. It may use a gateway-managed provider credential
  only through an explicit project/provider/`service` binding.
- **Project**: membership and aggregate policy boundary.
- **Membership roles**: owner, admin, member, viewer.
- **API key**: belongs to exactly one principal + project; requests authenticate
  against its SHA-256 hash, while an encrypted token copy can be revealed only by
  that principal or an administrator.

## SSO trust boundary

Caddy/Authentik protects `/admin`, `/portal`, `/admin/api/*` and `/user/api/*`.
It forwards `X-Authentik-Uid`, username, email, name and groups, and overwrites
`X-LLMGW-SSO-Secret` with a value shared with the gateway. The container port is
loopback-only. The gateway rejects SSO mutations without a same-origin `Origin`.

`LLMGW_API_KEY` remains a recovery/admin credential. A project API key is never
an admin credential.

## Provider connections

Each human may own multiple named connections for one configured provider, such
as `gemini/personal` and `gemini/work`. Connections are AES-256-GCM encrypted,
never returned by list APIs, and cannot be owned by service principals. One
connection is the deterministic default for that human/provider pair. Revoking
the default promotes another active connection when one exists.

Resolution order is:

1. the calling human's default connection;
2. the encrypted system connection;
3. the legacy config/secrets-store credential.

The current runtime resolves the deterministic default connection for a
human/provider pair. Earlier account-aware routing ideas remain in
`docs/PROVIDER_PARITY.md` as design research; they are not committed product
scope.

Config credentials seed a missing system connection only when
`LLMGW_CREDENTIAL_ENCRYPTION_KEY` is configured. Existing database connections
are never overwritten on restart. An explicit admin provider update is the
credential-rotation path.

## Copilot BYOC

Each human completes GitHub's device-code flow. The OAuth token is encrypted with
AES-256-GCM using `LLMGW_CREDENTIAL_ENCRYPTION_KEY`. Copilot session caches are
fingerprint-isolated. Provider instances and catalogs are cached per project and
principal for service bindings, preventing cross-project credential/session reuse.
Human BYOC caches remain principal-scoped, preserving existing client catalogs.

Credential resolution is deterministic:

1. an active human principal's own active credential for the provider;
2. an active gateway-owned credential selected by an active binding for the
   caller's exact active project, provider and principal kind;
3. otherwise no credential, so the provider contributes no models and cannot be
   routed.

Membership, project status, principal status, binding status and credential status
are checked on every resolution, including before a cached catalog is returned.
Key and project model/provider policies remain a separate intersecting gate.

## Shared provider credential admin API

All endpoints require admin authentication. Responses and audit details contain
metadata only, never OAuth/provider tokens.

- `POST /admin/api/providers/{provider}/shared-credential/import` with
  `{"source":"configured"}` encrypts the currently configured gateway credential
  under the gateway system principal and returns credential metadata.
- `POST /admin/api/projects/{project}/provider-credential-bindings` with
  `provider_id`, `principal_kind` and `credential_id` creates or replaces the
  exact binding.
- `GET /admin/api/projects/{project}/provider-credential-bindings` lists binding
  metadata.
- `POST /admin/api/projects/{project}/provider-credential-bindings/status` sets
  an existing binding to `active`, `disabled` or `revoked`.
- `POST /admin/api/provider-credentials/status` sets an encrypted credential to
  `active`, `disabled` or `revoked` by `id`.

Each mutation writes an audit event and invalidates provider/catalog caches.

## Quotas

Key and project policies support:

- model/provider allowlists,
- requests per minute/day/month,
- daily input/output tokens,
- monthly total tokens,
- daily/monthly estimated micro-USD,
- daily/monthly model credits.

SQLite transactions consume request slots before dispatch. Usage counters are
reconciled after the response. Token/cost limits can exceed by at most one
in-flight request; request-count and RPM limits are strict.

These are **downstream gateway quotas** applied to gateway-issued keys and
projects. They are distinct from upstream provider quotas such as subscription
reset periods. Upstream quota adapters and quota-sharing ideas are deferred
design research, not current routing behavior.

## Usage, audit and notifications

`gateway.db` records principal/project/key attribution, endpoint, model,
provider, status, latency, tokens, estimated cost and credits. Admin statistics
group by project, principal, key, provider and model. Every management mutation
is audit logged.

Alert rules create deduplicated durable outbox events for quota thresholds and
key expiry. Windmill drains the outbox and owns email/Chatwoot formatting and
delivery; see `deploy/windmill/`.

## Migration and backup

On first Go startup:

1. plaintext `keys.json` is transactionally imported as hashed keys and removed;
2. existing `usage.db` history is copied into `gateway.db` and current-period
   quota counters are rebuilt;
3. legacy single-provider credentials are exposed as the default named
   connection without changing their ciphertext;
4. configured system credentials seed the encrypted connection store only when
   absent;
5. migrations are idempotent through `schema_migrations` and `control_metadata`;
6. `provider_credential_bindings` is added without changing existing
   principals, projects, keys or human provider credentials.

Back up the entire state directory (including `gateway.db`, `-wal`, `-shm`,
config, encrypted credentials and Copilot session cache) with the container
stopped or via SQLite's online backup mechanism. Keep the encryption key in the
secret manager; database backups are unusable for BYOC recovery without it.

## Console and official OAuth

The primary console is `/console`; owner portal mode is `/portal`. The same local bundle chooses the user API boundary in portal mode and the admin API boundary in console mode. Legacy documents are retained at `/admin-legacy` and `/portal-legacy`.

OAuth connections use encrypted provider-connection envelopes with safe expiry/account metadata only. GitHub Copilot uses its official device flow. OpenAI Codex uses official device authorization and a PKCE authorization-code exchange, configured with `openai_codex_client_id`; the resulting connection is experimental, owner-local, and not assignable to services. Claude Code is a gateway client using the documented Anthropic protocol, with supported Anthropic API/gateway credentials rather than personal OAuth.

Owner/admin playground requests require an explicit project and record the selected human/project attribution. They consume project request counters without creating a browser API key. Provider subscription quota values remain `unknown` unless an adapter supplies verified data; unknown is never rendered as a numeric remaining percentage.

Before any rollout, back up `gateway.db` together with its WAL/SHM files and the
full state directory. Rollback restores that backup and the prior immutable
image; a binary older than a schema change should not be paired with state that
has already migrated past it.
