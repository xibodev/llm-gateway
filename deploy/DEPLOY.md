# Deploying llm-gateway

The Go build ships as a ~16 MB static binary / ~2 MB distroless image, so it runs
comfortably on the cheapest Linux box. This directory has everything for a
**cheap EC2 + docker compose + Caddy** deployment.

## Files
- `docker-compose.prod.yml` — gateway (loopback) + Caddy (TLS termination).
- `Caddyfile` — automatic HTTPS + SSE-friendly reverse proxy.
- `.env.example` — the secrets/flags the compose reads.
- `smoke.sh` — a safe live smoke test (`health`, `models`, one tiny chat).

## First deploy (fresh box)
```bash
git clone <repo> && cd llm-gateway
cp deploy/.env.example deploy/.env      # set LLMGW_API_KEY etc.
mkdir -p deploy/state && sudo chown 65532:65532 deploy/state   # distroless nonroot uid
docker compose -f deploy/docker-compose.prod.yml up -d --build

# Sign Copilot in (device-code) + connect providers in the panel:
#   https://<domain>/admin
```

## Configure identity, providers and keys

Administrators use `/admin` for humans/services, projects, memberships, policies,
providers, audit and alerts. Human users use `/portal` for their own keys, usage
and Copilot BYOC.

The control plane persists to `deploy/state/gateway.db` (SQLite WAL). API keys
authenticate by SHA-256 hash; encrypted copies let their owner or an administrator
reveal them. API-key recovery, provider connections, and BYOC credentials require
`LLMGW_CREDENTIAL_ENCRYPTION_KEY`. `deploy/state/secrets.json` remains a
compatibility/config seed; an existing encrypted system connection is not
overwritten on restart.

For remote multi-user deployment, place Authentik/Caddy in front of `/admin`,
`/portal`, `/admin/api/*` and `/user/api/*`, overwrite `X-LLMGW-SSO-Secret`, and
forward the `X-Authentik-*` identity headers. See `docs/MULTI_USER.md`.

## Smoke test after deploy
```bash
BASE_URL=https://<domain> KEY=llmgw_... ./deploy/smoke.sh
```

## Upgrade and rollback

This is the repository-versioned operator procedure; it does not depend on a
published website. Read the target release's notes, [deprecations](../README.md#deprecations)
and [wire compatibility](../docs/CLI_COMPATIBILITY.md) before scheduling downtime.
Startup opens `gateway.db` and applies missing migrations, each in a transaction,
before serving HTTP. There is no separate migration CLI or supported database
downgrade. Rehearse with a copy of your state; a successful upgrade of one
installation is not a guarantee for every configuration or release pair.

For `v0.1.0` to `v0.2.0`, the schema advances from migration 13 to 14, adding
encrypted API-key recovery columns. Existing hash-only keys remain valid but do
not become revealable merely by upgrading. Neither tag has a `backup` command.
The current source includes further migrations and backup support; do not treat
its features as already present in those published releases.

### Pin and snapshot

1. Record the running image digest and version, and select the exact target image
   from the release/registry metadata. Keep both images available locally. Set
   `OLD_IMAGE` and `NEW_IMAGE` to full immutable image references, preferably
   digests, for the old and new releases. Do not infer an image tag from a Git tag:
   older release workflows published binary archives only. Do not use `latest`.
2. Use your existing deployment's Compose files, project name and environment
   source. The commands below assume `COMPOSE_FILE`, `COMPOSE_PROJECT` and
   `ENV_FILE` point to that deployment, with any existing override files retained.
   In its private Compose configuration, set the gateway's `image` to
   `${LLMGW_IMAGE:?pin the gateway image}`. The repository Compose examples contain
   `build:` and local image names; `--no-build` below avoids rebuilding them.
3. Preserve the same state mount and environment, especially `LLMGW_STATE_DIR`,
   `LLMGW_CONFIG`, provider flags, authentication and
   `LLMGW_CREDENTIAL_ENCRYPTION_KEY`. The root Compose example uses a named volume
   at `/state`; the production example binds `deploy/state` there. Changing the
   project name can silently select a new named volume. Do not run `down -v`.
4. Retain the original encryption key separately in the secret store, plus the
   deployment configuration/environment and any configured state outside `/state`
   (for example, an external config, savings database or OAuth cache). A database
   backup without its encryption key cannot recover encrypted credentials.
5. Drain traffic and stop **all** writers before taking the snapshot. For releases
   without a backup CLI, archive the entire stopped state volume, including any
   SQLite `-wal` and `-shm` files, config, secrets and caches, not just `gateway.db`.
   Do not start a newer binary against the original state to make this snapshot.

Example for the existing gateway container, in a POSIX shell. `BACKUP_DIR` must
already be a private directory outside the state volume and repository. Stop on
any command failure; the archive is sensitive and is not encrypted:

```bash
set -eu
umask 077
set -C
test -d "$BACKUP_DIR"
export LLMGW_IMAGE="$OLD_IMAGE"
GATEWAY_CONTAINER=$(docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" ps -q gateway)
test -n "$GATEWAY_CONTAINER"
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" stop gateway
docker cp "$GATEWAY_CONTAINER":/state/. - > "$BACKUP_DIR/pre-upgrade.tar"
tar -tf "$BACKUP_DIR/pre-upgrade.tar" >/dev/null
```

Use a new archive filename for each attempt; never overwrite the original
rollback snapshot. Verify that it includes all expected state and test extraction
into a separate location with ownership/permissions preserved. Archive listing
alone is not a restore test. Snapshot externally configured state while the same
writers remain stopped. Preserve the original snapshot unchanged through acceptance.

### Start and verify

Change only the pinned gateway image, retaining the deployment settings above:

```bash
export LLMGW_IMAGE="$NEW_IMAGE"
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" pull gateway
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up -d --no-build --no-deps gateway
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" exec -T gateway /llmgw version
```

- Review startup logs for migration, configuration and credential errors. Confirm
  the running image digest and that `version`, `GET /health` and authenticated
  `GET /admin/api/state` report the intended version. Current-source builds also
  report commit/build provenance; those fields are absent in the older tags.
- Check admin state using the static admin key or the configured trusted SSO
  admin identity, not a minted project key. Confirm existing providers, endpoints,
  keys and project bindings remain present; health alone proves no provider access.
- Call `GET /v1/models` and a small inference request using each intended human or
  service/project key and its exact model or endpoint. Human BYOC and service
  project bindings are separate credential scopes; admin success is not proof for
  either. Check the native API and streaming/tool behavior your clients actually
  use. For a Chat-capable target, `deploy/smoke.sh <provider/model>` accepts
  `BASE_URL` and `KEY` from your secure runtime environment; it makes a billable
  request. Do not rely on its automatic model selection for acceptance.
- Keep traffic drained until checks pass. On failure, stop the new gateway,
  preserve its failed state separately for diagnosis, and follow rollback below.

### Roll back

Stop all writers. Restore the **original pre-upgrade snapshot** into an empty
replacement state volume/directory, including its original WAL files and any
external state/configuration. Do not overlay it onto the migrated database or
leave newer WAL files behind. Restore the original ownership/permissions (the
supplied container runs as UID/GID 65532), environment and encryption key. Point
the same state mount at the restored state, set `LLMGW_IMAGE="$OLD_IMAGE"`, and
repeat `up -d --no-build --no-deps gateway` and the verification checks above.
Never run the old image against the migrated database. Rollback discards writes
made after the snapshot; account for those before reopening traffic.

## Back up and restore

The current source implements the following commands; `v0.1.0` and `v0.2.0` do
not. Check the chosen release's `llmgw --help` before use. Run offline with the
same state/config environment as the service, not the maintenance user's default
home. Restore destinations, including `LLMGW_CONFIG`, must be writable; an older
deployment's read-only config mount cannot be replaced in place. Stop every
gateway writer for the entire operation:

```bash
llmgw backup create /secure/path/llmgw-state.tar.gz
llmgw backup inspect /secure/path/llmgw-state.tar.gz
llmgw backup restore /secure/path/llmgw-state.tar.gz --force
```

For a stopped Compose service whose pinned image has these commands, use
`docker compose ... run --rm --no-deps gateway backup create /state/backups/upgrade.tar.gz`
with the same project, files and environment arguments as above; substitute
`backup inspect` or `backup restore ... --force` as needed. Copy the resulting
archive out of the state volume to protected storage. There is no shell in the
gateway image. These commands expect the built-in manifest/checksum archive, not
the raw `pre-upgrade.tar` snapshot above. Inspection checks integrity and reports
the schema; it does not migrate the database or prove provider access.

Restore also requires the original `LLMGW_CREDENTIAL_ENCRYPTION_KEY` from the
secret store; the archive deliberately does not contain it. The local Compose
stack seeds its read-only config mount into writable state on first start, so the
offline restore command can replace the state copy; keep the container stopped
for the entire maintenance operation.
Built-in backups include configured SQLite state and supported config/secret/cache
files, but exclude request logs. Protect archives from replacement as well as
disclosure: checksums are not authentication. Keep the pre-upgrade rollback
snapshot separate from newer backups and automatic backup retention.

## Alternative: systemd (no Docker)
`go build -o llmgw ./cmd/llmgw`, ship the binary, and run it under systemd behind
Caddy/nginx — see `go/README.md` for the unit. Disable proxy buffering so SSE
streams.

---

# Replacing `llm.example.com` (production cutover)

This gateway is intended to **replace the existing `llm.example.com`** proxy.
That is a live service other things may depend on, and a DNS/endpoint cutover is
a one-way door — so it is **not** done automatically. The steps and the decisions
that need an operator sign-off:

### Plan
1. **Stand up the new gateway** on the chosen box (compose above), reachable on a
   temporary hostname (e.g. `llm-next.example.com`) or the box IP behind Caddy.
2. **Port the config**: recreate providers/endpoints, sign Copilot in, mint the
   keys the current consumers use (or import the old ones). Verify with `smoke.sh`
   against the temporary hostname.
3. **Dual-run**: point one non-critical consumer at the new endpoint; watch
   `/admin` usage + telemetry for a day.
4. **Cutover**: repoint the `llm.example.com` DNS record (or the Caddy upstream on
   the box that serves it) to the new gateway. Keep the old one warm for rollback.
5. **Smoke** `BASE_URL=https://llm.example.com ./deploy/smoke.sh`, then
   decommission the old proxy once stable.
6. **Register the service** in your infrastructure registry so it stays tracked.

### Needs operator confirmation before the cutover (per the ops rules)
- **Which box** hosts it — pick from `your host inventory`; never a remembered IP.
- **Which AWS profile / account** if provisioning new infra (default `<aws-profile>`
  for personal projects — confirm).
- **What currently serves `llm.example.com`** (which box / Caddy site / gateway
  gateway) so the cutover repoints the right thing and nothing else breaks.
- **DNS provider + record** to change (Cloudflare/GoDaddy) and the TTL.
- **Consumers** currently using `llm.example.com` (so their keys/models keep working).

> Ask before running any write/deploy/DNS/infra command. A quick "which box +
> profile?" beats guessing on a live endpoint.

## Console transition note

After a local image or binary is started, use `/console` as the primary administrative surface and `/portal` for human owner mode. `/admin` redirects to `/console`; `/admin-legacy` and `/portal-legacy` are retained for rollback during a local transition. The Go Docker build runs the frontend build stage before compiling the static binary, so no Node runtime ships in the final image.

Configure only supported upstream credentials. GitHub Copilot and OpenAI Codex use their official device flows; Codex also needs `openai_codex_client_id`. Claude Code is configured as an Anthropic gateway client, not a personal OAuth provider. Do not perform real OAuth login, deployment, DNS changes, or remote writes as part of local validation.
