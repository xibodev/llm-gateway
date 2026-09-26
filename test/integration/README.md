# Mocked integration stack

This local Docker stack runs the gateway against WireMock fixtures. It proves
deterministic endpoint routing without a real provider credential. It is not
real-provider UAT.

| Service | Role |
| --- | --- |
| `gateway` | Current `go/Dockerfile` worktree build |
| `wiremock` | In-network mocked provider responses |
| `edge` | Loopback-only host bridge at `127.0.0.1:8887` |

The gateway and WireMock share an internal network. The edge container also joins
a bridge used only to publish its loopback port, so the entire stack is not
network-isolated. Provider base URLs point only at WireMock.

## Run

```bash
cp test/integration/.env.test.example test/integration/.env.test

LLMGW_UAT_TAG="$(git rev-parse --short HEAD)" \
LLMGW_BUILD_COMMIT="$(git rev-parse HEAD)" \
LLMGW_BUILD_TIME="$(git show -s --format=%cI HEAD)" \
docker compose -f test/integration/docker-compose.test.yml \
  --env-file test/integration/.env.test \
  --project-name llmgw-int \
  up --build --wait
```

For attributable release evidence, require a clean worktree first:

```bash
git diff --quiet && git diff --cached --quiet
```

The Dockerfile rebuilds the console from source. A dirty worktree is tested but
cannot be identified by the commit alone.

## Verify

```bash
curl -fsS http://127.0.0.1:8887/health
curl -fsS \
  -H 'Authorization: Bearer replace-with-a-local-uat-key' \
  http://127.0.0.1:8887/v1/models
```

Use the repository's automated API/integration tests for assertions. The stack
itself does not contain a browser journey harness or SSO-header injector.

| Fixture endpoint | Expected behavior |
| --- | --- |
| `cat-429-failover` | Primary `429`, fallback returns `200` |
| `cat-5xx-failover` | Primary `500`, fallback returns `200` |
| `cat-timeout-failover` | First target exceeds its timeout, fallback returns `200` |
| `cat-direct-fallback` | One successful target |
| `cat-priority-chain` | `429`, then `500`, then successful target |

The `cat-` names are legacy fixture identifiers; product terminology is
**endpoint**.

## Tear down

```bash
docker compose -f test/integration/docker-compose.test.yml \
  --env-file test/integration/.env.test \
  --project-name llmgw-int down -v
```
