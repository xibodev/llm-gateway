# Security policy

llm-gateway is an internal, self-hosted gateway, not a public SaaS relay. Run it
on loopback or a private network, terminate TLS at a trusted reverse proxy, and
do not expose the raw listener to untrusted clients.

## Supported versions

Security fixes are delivered on the latest published release line. Before
reporting an issue, reproduce it against the latest release when doing so does
not risk data or credential exposure.

## Private reporting

Report vulnerabilities through GitHub's private vulnerability reporting for
`xibodev/llm-gateway`:

<https://github.com/xibodev/llm-gateway/security/advisories/new>

Do not open a public issue for an undisclosed vulnerability. Never include real
keys, tokens, hostnames, account identifiers, personal data, production logs, or
runtime configuration. Use synthetic reproduction data.

## Authentication and authorization

- **Administrator**: static `LLMGW_API_KEY` or verified SSO identity in the
  configured admin group. Project keys cannot call admin APIs.
- **Human portal**: verified reverse-proxy SSO identity. Mutations must be
  same-origin.
- **Data plane**: static gateway key or active project key, unless deliberate
  unauthenticated local mode is enabled.
- **Gateway key**: assigned to one principal/project and authenticated against a
  SHA-256 hash.
- **Recoverable key**: encrypted with AES-GCM only when credential encryption was
  configured at issuance; reveal is owner/admin-only, no-store, and audited.
- **Governance**: project/key allowlists and request, token, estimated-cost, and
  credit limits execute in the request path.

Static admin-key comparison is constant-time.

## SSO boundary

The gateway trusts Authentik-style headers only when accompanied by
`X-LLMGW-SSO-Secret`. A trusted reverse proxy must authenticate the client,
overwrite that header, forward verified identity fields, and block direct access
to the gateway.

The included Caddy configuration is a TLS/static-admin starting point. It is not
a complete SSO deployment.

## Credentials and state

Human provider API-key and OAuth connections use AES-256-GCM with
`LLMGW_CREDENTIAL_ENCRYPTION_KEY`. List, audit, and playground responses expose
metadata rather than provider token values.

System provider keys entered through the administration path can also remain in
owner-only plaintext `secrets.json` for compatibility. Therefore the project does
not claim universal encryption at rest. Protect the entire state directory.

Generic API-key provider resolution can fall back to a system connection without
a project binding. The supported exact project/provider/principal-kind binding
boundary applies to gateway-owned Copilot credentials used by services. Personal
Copilot and Codex OAuth connections remain human-private.

## Diagnostics and logging

Credential-shaped upstream diagnostics are sanitized before being surfaced or
persisted. Audit details are structured-sanitized.

Request logging is off by default. `LLMGW_LOG_REQUESTS=1` records metadata only.
`LLMGW_LOG_REQUEST_BODIES=1` is an unsafe separate opt-in and can persist prompts,
responses, credentials, personal data, and proprietary source. Request logs are
excluded from built-in backups.

## Backup security

Built-in archives can contain configuration, encrypted connections, plaintext
compatibility secrets, API-key recovery ciphertext, provider caches, and local
usage state. They are created with owner-only permissions; Windows uses a
protected current-user/SYSTEM DACL.

Checksums and SQLite integrity checks detect corruption, not malicious archive
replacement. Protect backups from disclosure and tampering. Keep
`LLMGW_CREDENTIAL_ENCRYPTION_KEY` separately; it is deliberately not archived.

## Containers and releases

- Runtime image: static binary in a non-root distroless container with no shell
  or package manager.
- Console: local embedded assets; Node.js is build-time only.
- Release inputs: GitHub action revisions, Node, Syft, Buildx, BuildKit,
  Dockerfile frontend, and base images are pinned in the workflow. GitHub-hosted
  runner images remain platform-managed.
- Release outputs: verified checksums, per-binary SPDX SBOMs, GitHub provenance,
  and Linux amd64/arm64 image SPDX/SLSA attestations.
- Manual release dispatch: non-publishing and produces no retained artifact.

Healthy release probes are `govulncheck ./...`, `npm audit --audit-level=high`,
workflow lint, Dockerfile validation, and the repository test suites.

## SSRF is intentionally not blocked

Operators configure provider base URLs, including loopback and LAN services such
as Ollama, LocalAI, LM Studio, vLLM, and llama.cpp. Blocking private addresses
would break a core feature. Treat provider administration as privileged and do
not expose it to untrusted users.

## Provider-specific boundaries

- GitHub Copilot gateway use is a personal-use grey area, not a sanctioned public
  provider API. Respect provider terms and keep entitlements owner-private.
- OpenAI does not currently document third-party Codex client registration. The
  gateway does not embed the official CLI's first-party client ID.
- Claude personal-subscription OAuth, browser-cookie extraction, MITM
  interception, and stealth session reuse are not implemented.
- Edge TTS uses an unofficial public read-aloud service and may change.

See [`docs/LIMITATIONS.md`](docs/LIMITATIONS.md) for the complete product
boundary and [`docs/OPERATIONS.md`](docs/OPERATIONS.md) for secure deployment and
recovery.
