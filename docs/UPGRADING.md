# Compatibility and upgrading

## Before an upgrade

1. Schedule maintenance, drain traffic, and stop all gateway writers.
2. Check the installed binary's `llmgw --help`. Use its built-in backup if
   available; v0.1/v0.2 have no backup CLI. Otherwise archive the entire stopped
   state volume, including SQLite WAL/SHM, configuration, secrets and caches.
3. Record the current image digest or binary checksum.
4. Keep `LLMGW_CREDENTIAL_ENCRYPTION_KEY` available from the secret store.
5. Read the release notes for storage and compatibility changes.

Retain the same deployment environment, state mounts and any configured external
config/database/cache paths. Preserve the original snapshot unchanged and test
extraction separately. Do not start the new binary on original state to make a
backup. Follow the [repository-versioned procedure](../deploy/DEPLOY.md#upgrade-and-rollback).

Database migrations are idempotent and recorded in `schema_migrations` and
`control_metadata`. A binary refuses to inspect/restore a backup whose schema is
newer than it supports.

## Standalone image installation

Use this for the [published-image quickstart](QUICKSTART.md#docker-compose),
not the repository's source-build Compose file. The quickstart pins
`ghcr.io/xibodev/llm-gateway:0.3.1`; select future versions from the
[release notes](https://github.com/xibodev/llm-gateway/releases/latest).

1. Complete the offline backup checklist above with the **installed** image.
   Keep the original snapshot outside the state volume and automatic retention.
2. Preserve the installation folder, `compose.yaml`, `.env`, project name and
   `state:/state` volume. Keep the original administrator and encryption keys;
   do not run the key generator again. Remove stale shell overrides.
3. Change only `image:` in `compose.yaml` to the chosen published version/digest.
4. From that same installation folder (POSIX or PowerShell):

```bash
docker compose pull gateway
docker compose up -d gateway
```

No clone, `--build`, image override file or `LLMGW_IMAGE` variable is involved.
The standalone recipe reads `.env` automatically. Check startup logs, the
running version, `/health`, authenticated administrator state and intended
provider access before restoring traffic. Keep the prior image and snapshot
until acceptance; [rollback](#rollback) restores the matching old state as well.

## Native binary installation

After the same offline backup, download and verify the replacement archive from
the selected release. Stop the old process before replacing its executable.
Start with the same saved keys and absolute state/config paths, using
`llmgw serve` (or `.\llmgw.exe serve` on Windows). Do not accidentally start
against the maintenance user's default `~/.llmgw`. Preserve the old binary and
original snapshot together for rollback.

## Developers: source installation

For the [root source Compose recipe](QUICKSTART.md#developers-from-source), back
up offline first, select the intended source tag in the **same clone**, and
preserve its private `.env`, `config.local.yaml`, project name and `llmgw-state`
volume. Do not copy the example config over existing configuration. Its port
variable is `LLMGW_HOST_PORT=127.0.0.1:8787`, not standalone `LLMGW_PORT`.
After clearing stale shell overrides, rebuild from that clone:

```bash
docker compose --env-file .env up -d --build
```

For an existing production Compose stack, retain its actual files, overrides,
project, environment source and mounts; follow the
[deployment-specific procedure](../deploy/DEPLOY.md#upgrade-and-rollback).
Neither released repository Compose file supports `LLMGW_IMAGE` by itself;
that procedure explicitly adds it to private configuration before using it.

## Current compatibility aliases

| Deprecated | Replacement |
| --- | --- |
| `categories:` YAML key | `endpoints:` |
| `POST /admin/api/categories` | `POST /admin/api/endpoints` |
| `DELETE /admin/api/categories/{name}` | `DELETE /admin/api/endpoints/{name}` |
| `categories` in admin state | `endpoints` |
| `supported_endpoints` on model rows | `supported_surfaces` |

No removal release is scheduled. New integrations should use the replacement.
When both `endpoints` and `categories` exist in YAML, `endpoints` wins and they
are not merged.

## Breaking compatibility already in effect

Endpoint pseudo-model rows use `owned_by: "endpoint"`. Clients that previously
matched `owned_by: "category"` must accept the new value.

Catalog cache entries carry a schema version. Entries from an incompatible cache
schema are discarded and rediscovered rather than reinterpreted.

## Legacy state migration

On startup, applicable old state is migrated:

- plaintext legacy `keys.json` imports into hashed gateway keys and is removed
  only after a successful transaction;
- legacy `usage.db` history can be copied into `gateway.db` and current quota
  counters rebuilt;
- older provider-credential structures migrate into named connections;
- system credentials seed only missing encrypted connections and do not overwrite
  an existing database connection.

Legacy `secrets.json` remains a plaintext compatibility/config seed and must be
protected.

## Console compatibility

`/admin` redirects to `/console`. `/portal` serves owner mode. The old documents
remain at `/admin-legacy` and `/portal-legacy` for compatibility, but new
workflows should use the Preact console.

## Rollback

Storage rollback means restoring the original pre-upgrade snapshot to an empty
replacement volume/directory and running the matching previous binary/image.
Include original WAL files and external state, ownership/permissions, deployment
environment and encryption key. Never overlay the snapshot onto migrated state
or run the old image against the migrated database. There is no supported database
downgrade; rollback loses writes made after the snapshot.

Use exact image versions/digests; the release workflow does not depend on a
floating `latest` tag.
