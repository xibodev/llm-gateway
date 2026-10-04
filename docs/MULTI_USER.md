# Multi-user governance

llm-gateway is a single-node internal gateway. Its multi-user boundary combines
trusted reverse-proxy SSO for humans with scoped gateway keys for CLIs and
services.

## Identity model

- **Human principal**: provisioned from verified reverse-proxy identity headers.
  Can own named private API-key and OAuth connections and use `/portal`.
- **Service principal**: created by an administrator for a workload. Cannot own a
  human subscription connection.
- **System principal**: built-in owner for gateway-managed shared credentials.
- **Project**: membership and aggregate-policy boundary.
- **Membership**: `owner`, `admin`, `member`, or `viewer`.
- **Gateway key**: belongs to exactly one principal and project.

## Authentication boundaries

### Administrator

`/admin/api/*` accepts either:

- a static recovery/administrator key from `LLMGW_API_KEY` or
  `LLMGW_API_KEYS`; or
- a verified SSO identity in `LLMGW_SSO_ADMIN_GROUP`.

A gateway-issued project key is never an administrator credential.

### Human portal

`/user/api/*` requires verified SSO identity headers. SSO-authenticated mutations
must also be same-origin.

### Data plane

`/v1/*` accepts an active gateway-issued project key. The resolved principal
carries project, role, model/provider policy, and quota limits through the
request path. The static administrator keys are accepted too, but belong to no
principal or project, so key and project allowlists and quotas do not apply to
them; give clients project keys.

## SSO trust boundary

The gateway does not implement an embedded identity provider or generic OIDC
flow. A trusted reverse proxy must:

1. authenticate the human;
2. overwrite `X-LLMGW-SSO-Secret` with the configured shared secret;
3. forward the verified `X-Authentik-Uid`, username, email, name, and groups;
4. prevent direct client access to the gateway listener;
5. terminate TLS for remote access.

The included Caddy example is a TLS/static-admin starting point, not a turnkey
Authentik deployment. Add and test your own identity middleware before enabling
multi-user access.

## Provider connections

Humans can own multiple named private connections for a provider. One active
connection is the deterministic default. Revoking the default promotes another
active connection when one exists.

Generic API-key provider resolution is:

1. calling human's active default private connection;
2. encrypted system connection;
3. legacy YAML/environment/`secrets.json` credential.

The generic system fallback is not project-binding-gated today.

### Copilot shared-service exception

Copilot applies a stricter credential boundary:

1. an active human uses that human's own active OAuth connection;
2. a service can use a gateway-owned credential only through an active binding
   for the exact active project, provider, and `service` principal kind;
3. otherwise the provider contributes no models and cannot be routed.

Membership, project, principal, binding, and credential status are checked on
resolution. Project and key allowlists remain a separate intersecting gate.

### Codex

Codex OAuth connections are experimental, human-private, and not assignable to
services or the system principal. The gateway embeds no Codex OAuth client ID:
sign-in runs through the companion daemon, which uses its own client unless
[`LLMGW_OPENAI_CODEX_CLIENT_ID`](CONFIGURATION.md#credential-and-identity-boundary)
names one for the gateway to forward. Configure only a client you are authorized
to use.

## Shared Copilot credential API

These administrator APIs expose metadata only:

- `POST /admin/api/providers/{provider}/shared-credential/import`
- `GET /admin/api/projects/{project}/provider-credential-bindings`
- `POST /admin/api/projects/{project}/provider-credential-bindings`
- `POST /admin/api/projects/{project}/provider-credential-bindings/status`
- `POST /admin/api/provider-credentials/status`

They import the supported gateway-owned credential, bind it to an exact project
and principal kind, and manage status. These mutations are audited and invalidate
credential-dependent provider/catalog caches.

## Gateway keys

Issued tokens contain 192 random bits and use a SHA-256 hash for authentication.
When `LLMGW_CREDENTIAL_ENCRYPTION_KEY` is configured at issuance, an AES-GCM
encrypted copy supports explicit reveal:

- a human can reveal only a key owned by that principal;
- an administrator can reveal any recoverable key;
- reveal responses are `no-store`/`no-cache` and create an audit event;
- keys issued before encrypted recovery remain valid but return `409` on reveal;
- restore requires the original encryption key.

Disabled, expired, and revoked state controls authentication. Reveal authorization
is based on ownership/admin access rather than key status.

Removing a principal from a project revokes that principal's keys in the
project, including disabled ones; adding the membership back does not restore
them.

`admin_managed` is a server-controlled, key-only flag. New administrator-issued
keys are admin-managed; an administrator updating a key's policy, status, or
expiry also sets the flag. Owners cannot change those fields through the portal
afterward, including disabling or re-enabling a key. They can still reveal a
recoverable token and revoke their key. Clients cannot clear the flag.

Existing persisted keys remain owner-editable until an administrator updates
them, regardless of who originally issued them. Upgrading alone does not lock
their policy; see [`UPGRADING.md`](UPGRADING.md).

## Policy and quotas

Key and project policy support:

- model and provider allowlists;
- requests per minute, day, and month;
- daily input/output tokens and monthly total tokens;
- daily/monthly estimated micro-USD;
- daily/monthly model credits.

The key editor exposes every key quota field for both creation and later edits.
Zero (or a blank draft saved as zero) means no additional limit at that key
layer; project limits still apply independently.

Keys additionally support `allowed_routes` and `routes_only`; these and
`admin_managed` are not project policy fields. A non-empty `allowed_routes`
restricts named endpoints but does not itself block direct models. `routes_only`
requires an allowed route and blocks direct model IDs and bare model aliases.
Grants follow endpoint edits and are bound to the name, including deletion and
recreation under that name. They still intersect with key/project model and
provider policy and credential eligibility. They neither bind an upstream
account/connection nor provide per-HTTP-surface ACLs. See
[`ROUTING.md`](ROUTING.md).

Project and key allowlists intersect. Every limit is checked, and the request
counted, in one SQLite transaction just before provider dispatch and after
request validation, so request-count limits are strict. Token, estimated-cost,
and credit counters settle only after a response completes, and a request is
admitted while they are below their limit, so concurrent requests can each pass
and together exceed it. Quota windows are fixed UTC calendar minutes, days, and
months, not rolling windows.

A model that neither the built-in price table nor
[`savings.price_catalog`](CONFIGURATION.md#legacy-savings-ledger) prices records
zero estimated cost, so cost limits never stop it. Keys from an external key
document (`LLMGW_EXTERNAL_KEYS_FILE` or `LLMGW_EXTERNAL_KEYS_URL`) have no quota
limits of their own but count against their project's limits.

A stream that ends early, because its client left (`499`) or its upstream failed
(`502`), still counts the tokens it consumed against the model that served it.
When the upstream reported no usage, they are estimated at about one token per
four bytes of the prompt sent and of the text streamed. A failed request is
charged a credit only when it consumed tokens.

These are downstream gateway limits, not provider subscription quotas. The
gateway does not currently perform quota-aware account scheduling. Unknown
upstream quota remains unknown.

## Usage, audit, and notifications

`gateway.db` records request attribution, status, latency, requested and served
models, provider, tokens, estimated cost, credits, and error code. Admin reports
group by project, principal, key, provider, and model.

Audit history is append-only during its configured retention window. Many
security-sensitive and identity/credential mutations are audited, but the project
does not claim every management mutation is currently covered. An event whose
administrator authenticated with a static key carries `actor_key_fingerprint`,
the first 12 hexadecimal digits of that key's SHA-256, which tells the static
keys apart without storing them.

Quota and key-expiry rules create a durable, deduplicated outbox. The gateway
does not send email or chat messages itself; an external worker such as the
example under `deploy/windmill/` claims and settles outbox events.

A quota rule fires at a share of the key or project limit for its period, so
only metric and period combinations that such a limit covers are accepted;
others are refused with `400`:

| Metric | Periods |
| --- | --- |
| `requests` | `day`, `month` |
| `input_tokens`, `output_tokens` | `day` |
| `total_tokens` | `month` |
| `cost_microusd`, `credits_milli` | `day`, `month` |

An outbox event whose delivery has failed 10 times is no longer claimed; it
stays in the outbox listing with its last error.

## Console and portal

`/admin` redirects to the embedded `/console` administration SPA. `/portal`
serves the same bundle in owner mode and calls only `/user/api/*`.

The primary console supports provider lifecycle, models, endpoint routes,
playground, keys, access, usage, alerts, project policy, and retained audit
history. Management APIs exist for shared Copilot bindings even though the
primary console does not yet expose every binding control.

## Storage and recovery

The authoritative control plane is local SQLite. No Postgres, Redis, embedded
IdP, SMTP SDK, or runtime Node service is required. Backup, retention, migration,
and restore behavior are documented in [`OPERATIONS.md`](OPERATIONS.md).
