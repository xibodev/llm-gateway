# Real-provider UAT

This stack builds the current worktree and talks to operator-supplied real
providers. Nothing upstream is mocked. It binds only `127.0.0.1:8898`.

## Create a fresh session

```bash
cp test/uat/.env.uat.example test/uat/.env.uat
docker compose -f test/uat/docker-compose.uat.yml \
  --env-file test/uat/.env.uat --project-name llmgw-uat down -v

git diff --quiet && git diff --cached --quiet
LLMGW_UAT_TAG="$(git rev-parse --short HEAD)" \
LLMGW_BUILD_COMMIT="$(git rev-parse HEAD)" \
LLMGW_BUILD_TIME="$(git show -s --format=%cI HEAD)" \
docker compose -f test/uat/docker-compose.uat.yml \
  --env-file test/uat/.env.uat --project-name llmgw-uat \
  up --build --wait
```

`down -v` before `up` guarantees fresh state. The named volume otherwise
persists between runs.

Open `http://127.0.0.1:8898/console` with the disposable `LLMGW_API_KEY`.

For Antigravity manual-code UAT, put the caller-owned profile values only in
the gitignored `test/uat/.env.uat`: set the profile to `consumer_manual`, choose
an explicit `public` or `confidential` client mode, and set the exact registered
redirect URI. A confidential profile also requires its client secret. Start the
stack with the existing `--env-file` command; do not use `docker compose config`
or shell tracing because both can print resolved environment values. In the
console choose **Manual code**, leave the optional administrator profile fields
blank, approve access in the opened browser, then paste either the code or the
full redirect URL.

## First-run acceptance

1. Create a human and project; assign the human `owner` or `admin`.
2. Connect a real API-key provider, sync its catalog, and run **Test completion**.
3. Complete personal Copilot device authorization when that integration is in
   scope.
4. When Antigravity is in scope, complete manual-code authorization and verify
   that the provider appears only after the pasted response succeeds.
5. Connect `edge_tts`, sync voices, and run a real MP3 synthesis.
6. Create and test an endpoint with two eligible providers.
7. Mint a key that **acts as the human owner** when it must use that human's
   private OAuth connection.
8. Set project policy and inspect audit/usage without recording secret values.

## Direct endpoint gate

```bash
export BASE_URL=http://127.0.0.1:8898
export KEY=<DISPOSABLE_PROJECT_KEY>
export MODEL=<EXACT_PROVIDER_MODEL>

curl -fsS -H "Authorization: Bearer $KEY" "$BASE_URL/v1/models"
curl -fsS -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with: ok\"}],\"max_tokens\":64}" \
  "$BASE_URL/v1/chat/completions"
curl -fsS -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"input\":\"Reply with: ok\"}" \
  "$BASE_URL/v1/responses"
curl -fsS -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with: ok\"}],\"max_tokens\":64}" \
  "$BASE_URL/v1/messages"
curl -fsS -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"count this\"}]}" \
  "$BASE_URL/v1/messages/count_tokens"
```

Choose models whose catalog declares the required surface.

## Installed-client gate

Use [`../../docs/CLIENTS.md`](../../docs/CLIENTS.md) with the UAT URL and a
disposable key. Keep variables process-scoped or use an ephemeral client config.

Verify:

1. Claude's interactive model picker displays and selects the intended ID.
2. Claude completes one prompt and one harmless Read-only tool flow.
3. Physical Ctrl+C stops a stream without retry/failover.
4. Codex completes one native Responses request.
5. Copilot CLI BYOK completes one request on its configured wire API.
6. Usage identifies the expected served provider/model.
7. Revoking each disposable key produces `401` on reuse.

Record only public client versions, model IDs, served provider/model, and
pass/fail. Never record provider or gateway credentials.

## Tear down

```bash
docker compose -f test/uat/docker-compose.uat.yml \
  --env-file test/uat/.env.uat --project-name llmgw-uat down -v
```

Revoke disposable upstream credentials when applicable.
