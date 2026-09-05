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

## Back up and restore

Stop the gateway container before using the binary's maintenance commands. The
archive is sensitive and must be stored outside the public repository:

```bash
llmgw backup create /secure/path/llmgw-state.tar.gz
llmgw backup inspect /secure/path/llmgw-state.tar.gz
llmgw backup restore /secure/path/llmgw-state.tar.gz --force
```

Restore also requires the original `LLMGW_CREDENTIAL_ENCRYPTION_KEY` from the
secret store; the archive deliberately does not contain it.

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
