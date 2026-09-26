# Deployment starting point

Canonical deployment, backup, retention, release verification, and rollback
instructions live in [`../docs/OPERATIONS.md`](../docs/OPERATIONS.md).

This directory provides:

- `docker-compose.prod.yml`: gateway plus Caddy;
- `Caddyfile`: TLS and SSE-friendly reverse proxy starting point;
- `.env.example`: local secret/retention settings;
- `smoke.sh`: health, model-list, and one tiny authenticated Chat request;
- `windmill/`: optional alert-outbox delivery worker example.

## Static-admin deployment

```bash
git clone https://github.com/xibodev/llm-gateway.git
cd llm-gateway
cp deploy/.env.example deploy/.env
```

Set placeholder values in `deploy/.env`, set `LLMGW_DOMAIN`, then:

```bash
mkdir -p deploy/state
sudo chown 65532:65532 deploy/state
docker compose -f deploy/docker-compose.prod.yml up -d
```

The Compose file uses a versioned GHCR image through `LLMGW_IMAGE`. Set it to an
explicit semantic-version tag or immutable digest. Do not commit `deploy/.env`.

Verify:

```bash
BASE_URL=https://<GATEWAY_HOST> KEY=<PROJECT_OR_ADMIN_KEY> ./deploy/smoke.sh
```

## Multi-user SSO

The provided Caddyfile does not configure Authentik. Before enabling SSO, add a
trusted authentication flow that overwrites `X-LLMGW-SSO-Secret`, forwards
verified identity/group headers, and prevents direct client access to the gateway.
See [`../docs/MULTI_USER.md`](../docs/MULTI_USER.md).

## Maintenance

Stop the gateway before backup or restore. The release binary includes:

```bash
llmgw backup create
llmgw backup inspect <ARCHIVE>
llmgw backup restore <ARCHIVE> --force
```

Archives are sensitive, do not include the encryption key, and must be kept
outside the public repository. Follow the complete Compose/standalone procedure
in [`../docs/OPERATIONS.md`](../docs/OPERATIONS.md).
