# UAT stack — real integrations

UAT here means testing against the real deal: this stack has normal internet
egress and exercises real provider APIs with real, operator-supplied
credentials. Nothing upstream is mocked. Mocked, deterministic regression runs
live in `test/integration/` and are explicitly *not* UAT.

One service: the gateway, built from `go/Dockerfile`, published on
`127.0.0.1:8898`. It starts with an empty state volume on purpose — the UAT
session begins exactly where a new operator begins, at the console's
first-run journey.

## Run

```bash
cp test/uat/.env.uat.example test/uat/.env.uat   # first run only, then edit

LLMGW_UAT_TAG=$(git rev-parse --short HEAD) \
  docker compose -f test/uat/docker-compose.uat.yml \
    --env-file test/uat/.env.uat \
    --project-name llmgw-uat \
    up --build --wait
```

Open `http://127.0.0.1:8898/console`, sign in with the `LLMGW_API_KEY` value
from your `.env.uat`.

## UAT checklist (mirrors the console's Get-started guide)

Operator participation is required — real keys and a real OAuth device flow.

1. **Access** — create a human owner and a project; add the owner as a member.
2. **Provider (API key)** — connect a real provider (e.g. OpenAI, Gemini,
   Groq) with a real key entered in the console.
3. **Sync catalog** — the provider's real model list appears on its detail page.
4. **Run test completion** — the provider reaches `Verified` status.
5. **Provider (OAuth)** — GitHub Copilot device authorization end to end:
   Add account → enter the code at github.com/login/device → connection stored
   encrypted for the owner.
6. **Edge TTS** — connect `edge_tts` (no credential), sync voices, run its
   test synthesis, then `POST /v1/audio/speech` returns playable MP3 audio.
7. **Route** — build a failover route across two real providers; run the
   route test and inspect the fallback trace.
8. **Key + CLI** — mint a project key and drive a real coding CLI
   (`ANTHROPIC_BASE_URL`/`OPENAI_BASE_URL` → `http://127.0.0.1:8898`).
9. **Governance** — set a project budget in Settings; verify the audit trail
   recorded every step without secret values.

## Tear down

```bash
docker compose -f test/uat/docker-compose.uat.yml \
  --env-file test/uat/.env.uat --project-name llmgw-uat down -v
```

`down -v` destroys the state volume, including encrypted credentials — a UAT
session never leaves residue. Revoke any real API keys you minted upstream if
they were created only for the session.
