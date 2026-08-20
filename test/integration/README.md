# Sealed integration stack

A local, fully offline gateway stack for deterministic end-to-end regression
testing against MOCKED upstreams. This is NOT UAT: UAT exercises real external
APIs and lives in `test/uat/`. Use this stack to prove failover mechanics and
console flows without spending tokens or needing credentials. Three services:

| Service | Role |
|---|---|
| `gateway` | the Go build, from `go/Dockerfile` |
| `wiremock` | every upstream provider, stubbed |
| `edge` | nginx reverse proxy, the only host bridge (`127.0.0.1:8887`) |

The `uat` network is `internal: true`, so nothing in the stack can reach the
internet. No real provider credential is required or accepted.

## Run

```bash
cp test/integration/.env.test.example test/integration/.env.test   # first run only, then edit

LLMGW_UAT_TAG=$(git rev-parse --short HEAD) \
  docker compose -f test/integration/docker-compose.test.yml \
    --env-file test/integration/.env.test \
    --project-name llmgw-int \
    up --build --wait
```

Always derive `LLMGW_UAT_TAG` from `git rev-parse`. Hand-typing it is how an
image ends up labelled with a commit it was not built from — which makes every
result gathered against it unattributable.

Note that `go/Dockerfile` rebuilds the console from source, so the stack tests
your working tree, not the committed `dist/`. Commit first if you want the two
to agree.

## Tear down

```bash
docker compose -f test/integration/docker-compose.test.yml \
  --env-file test/integration/.env.test --project-name llmgw-int down -v
```

## What the stubs cover

`config.test.yaml` defines four providers over WireMock and five endpoints
that exercise the failover contract:

| Provider | Behaviour |
|---|---|
| `primary` | models OK, completions → 429 |
| `error` | models OK, completions → 500 |
| `timeout` | delayed past the 2s provider timeout |
| `fallback` | full happy path |

| Endpoint | Asserts |
|---|---|
| `cat-429-failover` | rate-limit cascade |
| `cat-5xx-failover` | upstream-error cascade |
| `cat-timeout-failover` | timeout cascade |
| `cat-direct-fallback` | no-error baseline |
| `cat-priority-chain` | 3-hop ordered chain |

Each should return HTTP 200 with the fallback body.
