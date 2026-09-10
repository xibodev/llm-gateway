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

- the static `LLMGW_API_KEY` recovery/administrator credential; or
- a verified SSO identity in `LLMGW_SSO_ADMIN_GROUP`.

A gateway-issued project key is never an administrator credential.

### Human portal

`/user/api/*` requires verified SSO identity headers. SSO-authenticated mutations
must also be same-origin.

### Data plane

`/v1/*` accepts a static gateway key or an active gateway-issued project key.
The resolved principal carries project, role, model/provider policy, and quota
limits through the request path.

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
services or the system principal. The operator must supply an OAuth client ID
they are authorized to use; the gateway does not embed the official CLI's
first-party client ID.

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

Keys additionally support `allowed_routes` and `routes_only`; these and
`admin_managed` are not project policy fields. A non-empty `allowed_routes`
restricts named endpoints but does not itself block direct models. `routes_only`
requires an allowed route and blocks direct model IDs and bare model aliases.
Grants follow endpoint edits and are bound to the name, including deletion and
recreation under that name. They still intersect with key/project model and
provider policy and credential eligibility. They neither bind an upstream
account/connection nor provide per-HTTP-surface ACLs. See
[`ROUTING.md`](ROUTING.md).

Project and key allowlists intersect. Request-count slots are consumed in SQLite
before provider dispatch. Token, estimated-cost, and credit counters reconcile
after response completion, so those limits may exceed by one in-flight request;
request-count limits are strict.

These are downstream gateway limits, not provider subscription quotas. The
gateway does not currently perform quota-aware account scheduling. Unknown
upstream quota remains unknown.

## Usage, audit, and notifications

`gateway.db` records request attribution, status, latency, requested and served
models, provider, tokens, estimated cost, credits, and error code. Admin reports
group by project, principal, key, provider, and model.

Audit history is append-only during its configured retention window. Many
security-sensitive and identity/credential mutations are audited, but the project
does not claim every management mutation is currently covered.

Quota and key-expiry rules create a durable, deduplicated outbox. The gateway
does not send email or chat messages itself; an external worker such as the
example under `deploy/windmill/` claims and settles outbox events.

## Console and portal

`/admin` redirects to the embedded `/console` administration SPA. `/portal`
serves the same bundle in owner mode and calls only `/user/api/*`.

The primary console supports provider lifecycle, models, endpoint routes,
playground, keys, access, usage, alerts, project policy, and retained audit
history. Management APIs exist for shared Copilot bindings even though the
primary console does not yet expose every binding control.

Legacy documents remain at `/admin-legacy` and `/portal-legacy` for compatibility;
new deployments should use `/console` and `/portal`.

## Storage and recovery

The authoritative control plane is local SQLite. No Postgres, Redis, embedded
IdP, SMTP SDK, or runtime Node service is required. Backup, retention, migration,
and restore behavior are documented in [`OPERATIONS.md`](OPERATIONS.md).
