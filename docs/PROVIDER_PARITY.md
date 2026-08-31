# Provider platform design research

This file preserves earlier research into turning `llm-gateway` into a broad
provider, account, quota, and routing platform. It is not the current product
target or an implementation commitment.

The active scope and all implementation status live in
[`BACKLOG.md`](../BACKLOG.md). Where this file and the backlog differ, the
backlog wins.

## Explored scope

The research considered:

- a large curated provider registry;
- a typed canonical representation for many-to-many protocol translation;
- multiple selectable accounts per provider;
- verified upstream quota adapters and account health;
- quota pools shared across gateway keys;
- weighted, round-robin, least-used, cost, reset, and headroom routing;
- automatically generated route combinations;
- a broad management UI and UAT matrix for those capabilities.

These ideas were removed from the committed plan because they describe a
provider-management platform rather than the current single-node coding-client
gateway. They should not be implemented speculatively.

## Retained principles

Any future demand-driven provider work should retain these constraints:

- Prefer official API-key, OAuth, and local/no-auth integrations.
- Keep human subscription credentials private to one human principal.
- Never implement browser-cookie extraction, MITM interception, or stealth
  session reuse.
- Keep credentials out of API responses, logs, traces, quota metadata, and
  audit details.
- Treat unknown upstream quota as unknown, never as zero or a fabricated
  percentage.
- Do not advertise a model or protocol surface unless its auth, transport,
  discovery, request, response, error, and streaming path is fixture-tested for
  the capabilities being advertised.
- Reject unsupported or lossy behavior before provider dispatch instead of
  silently dropping required fields.
- Isolate credential-dependent caches by the identity necessary to prevent
  cross-principal or cross-project reuse.
- Preserve simple exact `provider/model` addressing and ordered endpoint
  failover as the compatibility baseline.

## Re-entry rule

Move one idea back into `BACKLOG.md` only when a concrete operator or client
need names:

1. the provider, account, quota, or routing behavior required;
2. why existing exact routing and ordered failover are insufficient;
3. the smallest fixture-backed acceptance test;
4. the credential and data-isolation boundary;
5. the expected single-node capacity impact.

Add one narrow item at a time. This archive is not authority to revive the
whole platform program.
