# Operations

This runbook covers deployment, health, logging, retention, backup/restore,
release verification, and rollback for the single-node gateway.

## Deployment profiles

### Published image (recommended)

Use the standalone [Quickstart Compose recipe](QUICKSTART.md#docker-compose),
pinned to an exact release image, `ghcr.io/xibodev/llm-gateway:<VERSION>`, from
the [latest release](https://github.com/xibodev/llm-gateway/releases/latest).
No clone, build or config seed is required. From the private installation
folder containing `compose.yaml` and the generate-once `.env`:

```bash
docker compose up -d
docker compose down
```

The first command starts the gateway; the second stops it without deleting
state. The same commands work in PowerShell. Preserve this folder, Compose
project and `state:/state` volume. `.env` supplies both required keys; remove
stale shell overrides before managing Compose. Its optional `LLMGW_PORT` selects
the loopback host port only; the container stays on port 8787. The image runs
as UID/GID 65532 and already includes `/llmgw health` as an exec-form healthcheck.
Do not replace it with shell/curl commands: the runtime image has no shell.

A stop lets requests in flight finish for up to `LLMGW_SHUTDOWN_TIMEOUT_SECONDS`
(default 25), so a container's stop grace period must exceed that timeout.

Keep the encryption key separately from state backups and never regenerate it
with existing state. Use [image upgrades](UPGRADING.md#standalone-image-installation)
instead of rebuilding, and never use `down -v` for routine maintenance.

### Native binary

[Download, verify and unpack a prebuilt binary](QUICKSTART.md#native-binary).
Load the saved keys on every invocation; the binary does not automatically read
`.env`. Keep explicit, persistent state/config paths for both the service and
maintenance commands. Native state is independent of a Compose volume.

### Developers: source build

The [source-only recipe](QUICKSTART.md#developers-from-source) uses the repository
root `docker-compose.yml`, not the standalone `compose.yaml`. It builds the
worktree, requires `config.local.yaml` as a first-start seed, and uses its own
`llmgw-state` volume. It publishes on `127.0.0.1:8787` unless `LLMGW_HOST_PORT`
in that clone's private `.env` names another `address:port`; a port without an
address publishes on all interfaces.
From that clone, after preparing the source recipe and clearing stale shell
overrides:

```bash
docker compose --env-file .env up -d --build
```

Console edits and restores update `/state/config.yaml`, not the read-only seed.
Keep source and standalone installation folders, projects and volumes separate.

### Production Compose with TLS

`deploy/docker-compose.prod.yml` and `deploy/Caddyfile` are a starting point for
a single-user or static-admin deployment behind Caddy. The gateway service runs
the image that `LLMGW_IMAGE` names and has no `build:` section; Compose stops
with an error while the variable is unset. Set it in `deploy/.env` to an exact
published version or digest from the
[latest release](https://github.com/xibodev/llm-gateway/releases/latest), not an
assumed floating tag. Set a real domain, keep unauthenticated mode off, and
protect the state directory. [`deploy/DEPLOY.md`](../deploy/DEPLOY.md) has the
setup commands. The gateway closes keep-alive connections that stay idle for
three minutes, so a proxy in front must close its idle upstream connections
sooner; Caddy's two-minute default does.

To change images, complete the [pre-upgrade checklist](UPGRADING.md#before-an-upgrade),
including draining traffic and retaining the original rollback snapshot, then
follow the [production Compose upgrade](UPGRADING.md#production-compose-deployment).
The image entrypoint is `/llmgw`; from the repository root,
`docker compose -f deploy/docker-compose.prod.yml exec -T gateway /llmgw version`
checks the running build.

### Multi-user SSO deployment

The included stack is not turnkey SSO. Add a trusted Authentik-compatible proxy
flow that authenticates users, overwrites the SSO secret header, forwards verified
identity headers, and prevents direct access to the gateway. See
[`MULTI_USER.md`](MULTI_USER.md).

## Health and build identity

```bash
curl -fsS https://<GATEWAY_HOST>/health
llmgw version
```

Both report semantic version, source commit, and commit-derived RFC3339 build
time. `/health` proves process liveness, not provider readiness or database
freshness. Run provider **Test completion** to prove inference.

### Anonymous provider checks

When `LLMGW_ANONYMOUS_PROVIDER_AUTOMATION=true`, the gateway connects only the
curated no-key profiles and runs one catalog refresh plus one minimal free-model
completion per provider at startup, subject to a durable 24-hour claim. The
claim survives restarts, so repeatedly restarting the service does not hammer
public endpoints. Admin Settings may override the environment default.

Turning automation off is non-destructive: existing providers, routes, catalog
history, and checks remain. Disable an individual provider to suppress it
without allowing automation to create a replacement. Inspect **Providers** for
the latest catalog and verification status; public rate limits can produce a
legitimate failed check without indicating gateway failure.

## Request logs

Request logging is off by default. `LLMGW_LOG_REQUESTS=1` records metadata for
`POST /v1/*` only. Full prompt and response bodies require the separate unsafe
`LLMGW_LOG_REQUEST_BODIES=1` opt-in.

Body logs can contain credentials, personal data, and proprietary source. Files
are owner-only and rotate at `LLMGW_LOG_REQUESTS_MAX_BYTES`, retaining the active
file and one `.1` generation. Request logs are intentionally excluded from
built-in backups.

## Retention

Retention runs asynchronously after startup and every 24 hours. Deletions occur
in 500-row transactions so the gateway's sole SQLite connection is released
between batches.

| Store | Default |
| --- | ---: |
| Usage events | 90 days |
| Failover telemetry | 90 days |
| Optional legacy savings ledger | 90 days |
| Audit events | 365 days |
| Delivered outbox tombstones | 400 days minimum |
| Completed quota periods | Removed after period rollover |
| Request logs | Two size-bounded generations |
| Verified built-in backups | Keep 7 |

Pending/failed outbox events, current quota periods, identities, credentials,
provider state, alert rules, and policies are never age-pruned. Invalid or
under-minimum environment values fall back to safe defaults.

Only archives created in `<state>/backups` participate in
`LLMGW_BACKUP_KEEP`. Explicit external archive paths are operator-managed.

## Credential encryption key

`LLMGW_CREDENTIAL_ENCRYPTION_KEY` encrypts the provider connections, legacy
provider credentials, recoverable gateway keys and OAuth client profiles in
`gateway.db`. The database records a check value sealed with that key, and every
start verifies it before decrypting anything. A start with a different key
fails:

```text
initialize IAM control plane: the configured credential encryption key does not match the key this database was encrypted with
```

Configure the key the database was encrypted with. A key that changed before
anything was encrypted is recorded instead, because it protects nothing yet. A
database an earlier release wrote has no check value; the first start whose key
decrypts one of its credentials records it, and until then a start logs a
warning when the key decrypts none.

### Rotate the key

`llmgw credentials rekey` re-encrypts every stored credential with a new key.
It runs offline under the same state lock as `serve` and `backup`, so it refuses
to run while a gateway holds the state. Check that the installed binary's
`llmgw --help` lists it, then:

1. Stop the gateway.
2. Create a backup with `llmgw backup create` and keep the current key with it:
   the archive stays encrypted with that key.
3. Generate a new key, for example with `openssl rand -base64 32`, and store it
   with the deployment's secrets before using it. With the current key still in
   `LLMGW_CREDENTIAL_ENCRYPTION_KEY` and the new one in
   `LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY`, both loaded the way the service loads
   its keys rather than typed on a command line, run:

   ```bash
   llmgw credentials rekey
   ```

4. Replace `LLMGW_CREDENTIAL_ENCRYPTION_KEY` in the service environment, such as
   the Compose `.env`, with the new key, and remove
   `LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY`.
5. Start the gateway.

For Compose, run the command in a one-shot container of the installed image
with the same environment, project and state mount, and pass it
`LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY` as well.

The command re-encrypts provider connections, legacy provider credentials,
recoverable gateway-key copies and OAuth client profiles in one transaction,
binds each value to the same row identity as before, and records the new key's
check value. It prints how many values it re-encrypted in each table, never the
values or keys. It refuses a current key that does not match the database, and
changes nothing when any stored value fails to decrypt or any write fails.

## Backup contents

Check the **installed** binary with `llmgw --help` first. Source documentation
does not establish what an installed binary supports, so use the commands below
only when its help lists them.

Stop all gateway writers before maintenance and retain the service's actual
state/config environment, not the maintenance user's default home. `serve`,
`backup create`, `backup restore`, and `credentials rekey` share an exclusive
state lock.

If the installed binary lacks backup support, archive the **entire stopped state
volume**, including SQLite `-wal` and `-shm` files, configuration, secrets and
caches. Snapshot configured external config, savings database and OAuth cache
paths while the same writers remain stopped. Retain the deployment environment
and original encryption key separately. Do not start a new binary against
original state just to obtain a backup. Keep the original snapshot unchanged
outside automatic retention and test extraction separately.

```bash
llmgw backup create
llmgw backup inspect <ARCHIVE>
llmgw backup restore <ARCHIVE> --force
```

`backup create` without a path writes a retention-managed archive under
`<state>/backups`. A supplied path writes there and is not automatically pruned.

The archive can contain:

- runtime `config.yaml`;
- checkpointed `gateway.db`, `telemetry.db`, and configured legacy `usage.db`;
- `catalog.json`;
- recognized Copilot OAuth/session cache files;
- legacy `keys.json` and plaintext `secrets.json` when present.

It excludes request logs and environment-only values, including
`LLMGW_CREDENTIAL_ENCRYPTION_KEY` and the static administrator key.

The command uses standalone SQLite snapshots, per-file SHA-256 checksums, an
entry allowlist, global extraction bounds, SQLite integrity/foreign-key checks,
and owner-only files. On Windows, backup and temporary paths receive a protected
current-user/SYSTEM DACL.

Checksums detect corruption; they do not authenticate who supplied an archive.
Protect archives from disclosure and replacement.

## Restore behavior

Restore validates the complete archive before changing state. It replaces known
state; it is not a merge. Known current files absent from the archive are
removed.

The restore journal records prepared and committed phases. A later `serve` or
restore invocation rolls back an interrupted prepared transaction or completes
cleanup of a committed transaction before opening SQLite.

When an archived custom savings DB or Copilot cache points outside the state
directory, configure the same destination in the current process before restore.
This prevents an archive from redirecting writes to an arbitrary path. Restore
rejects a schema newer than the current binary.

After restore, provide the original credential-encryption key and verify:

```bash
llmgw backup inspect <ARCHIVE>
llmgw serve
```

`backup create`, `backup inspect` and `backup restore` report `credential_key`:
`matches` when the configured key opens the check value in the archived
database, `does not match` when it does not, `not recorded` for an archive
without one, `not configured` without a key, and `invalid` for a key that does
not decode to 32 bytes. Configure the key the archive matches before starting
the gateway on restored state.

From another terminal, once the service is running:

```bash
curl -fsS http://127.0.0.1:8787/health
curl -fsS -H 'Authorization: Bearer <ADMIN_KEY>' \
  http://127.0.0.1:8787/admin/api/state
```

For Compose, stop the service and run the release binary with the state volume
and archive mounted, or use an equivalent one-shot container. Do not restore
through a running gateway.

## Alerts and delivery

The gateway creates quota/key-expiry outbox events. The sample Windmill worker
claims events from `/admin/api/outbox`, calls an operator-configured webhook, and
marks delivery or retryable failure. Keep delivery credentials outside the
repository.

## Release artifacts

Stable tags have the form `vMAJOR.MINOR.PATCH` and must point to an approved
`main` commit. The consolidated release workflow produces:

- Windows amd64 and arm64 ZIPs;
- Linux amd64, arm64, and riscv64 tarballs;
- macOS amd64 and arm64 tarballs;
- a FreeBSD amd64 tarball;
- one SPDX JSON SBOM per binary;
- `SHA256SUMS`;
- GitHub build-provenance attestations;
- a versioned GHCR image for Linux amd64 and arm64 with SPDX and SLSA
  attestations.

Manual workflow dispatch is a non-publishing dry run. It creates no retained
Actions artifact, draft release, or registry tag.

Verify the downloaded archive's SHA-256 against its exact filename in
`SHA256SUMS` before unpacking; use `Get-FileHash -Algorithm SHA256` on Windows
or `shasum -a 256` on macOS. If you downloaded the complete artifact set:

```bash
sha256sum -c SHA256SUMS
gh attestation verify llmgw_v<VERSION>_linux_amd64.tar.gz \
  --repo xibodev/llm-gateway
```

Inspect a versioned image:

```bash
docker buildx imagetools inspect \
  ghcr.io/xibodev/llm-gateway:<VERSION>
```

Actions, major build tools, Dockerfile frontend, and base images are pinned.
GitHub-hosted runner images remain managed by GitHub and are not an immutable
part of the repository.

## Rollback

1. Stop all writers and preserve failed upgraded state separately for diagnosis.
2. Restore the **original pre-upgrade snapshot** into an empty replacement state
   volume/directory, including original WAL files and external state/configuration.
   Do not overlay it onto migrated databases or leave newer WAL files behind.
3. Restore original ownership/permissions, deployment environment, mounts and
   encryption key. Keep the same Compose project name; never run `down -v`.
4. Start the matching previous image by exact version or digest.
5. Verify `/health`, authenticated admin state, and model listing plus one small
   completion for each intended human and service/project credential scope.

There is no supported database downgrade. Never run the old image against the
migrated database. Rollback discards writes made after the snapshot. Built-in
restore expects its manifest/checksum archive, not a raw volume tarball; see
[restore behavior](#restore-behavior).
