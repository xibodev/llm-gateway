# Working in this repository

Instructions for any AI coding agent (and any human) making changes here.

---

## STOP — read this before every commit

This repository is **public**. A commit is not a draft: once pushed, treat
anything in it as permanently disclosed, because forks, clones, caches and
search indexes copy it within minutes and a later force-push does not recall it.

**Before every commit, check all three of these. Content is only the first.**

### 1. File contents

Never commit:

- API keys, tokens, passwords, private keys, session cookies, connection strings
- Real hostnames, IP addresses, internal DNS names, server or box identifiers
- Cloud account ids, ARNs, project ids, subscription ids, profile names
- Customer names, email addresses, or any personal data
- Internal ticket ids, sprint names, or internal tool and system names

### 2. Commit messages

A clean diff does not mean a clean commit. Commit messages are part of the
permanent record and are **not** covered by a scan of the working tree. Do not
paste logs, stack traces, curl commands, environment dumps, or internal
hostnames into a message.

### 3. Author and committer identity

`git log` carries the name and email of whoever made each commit, and those
fields are not file content, so no content scan will ever surface them. Verify
what you are about to publish:

```bash
git log -1 --format='%an <%ae> / %cn <%ce>'
```

Use a role address or a GitHub `noreply` address rather than a personal one.

---

## Never commit configuration

Only **example** configuration belongs in this repository:

| Commit this | Never commit this |
|---|---|
| `llmgw.config.example.yaml` | `llmgw.config.yaml`, `config.local.yaml` |
| `deploy/.env.example` | `.env`, `deploy/.env` |
| `test/*/.env.*.example` | `test/*/.env.*` |

Example files must contain **placeholders only** — never a real value, not even
an expired or "test" one. A key that reaches a public commit is compromised the
moment it lands, so it must be rotated regardless of how briefly it was there.

Real deployment values (hosts, digests, secrets) belong in the deployment
repository or a secret store, never here.

## Credentials during development

When a task needs a real credential, **reference it by path** and let the program
read it at runtime:

```bash
LLMGW_LIVE_GCP_KEY=/path/to/key.json go test ./internal/gcpauth -run Live -v
```

Do not open the file, paste its contents into a conversation, or copy it into a
test fixture. Tests must synthesise their own key material — see
`internal/gcpauth/gcpauth_test.go`, which generates a throwaway RSA key so the
suite needs no credential and can run in CI.

Never log a secret, and never include one in an error message. Errors are
frequently pasted into issues. `internal/gcpauth` has a test that pins this
invariant; keep that pattern when adding a credential type.

## If a secret is committed

1. **Rotate it first.** Removing it from history does not un-disclose it.
2. Then purge it from history and force-push.
3. On GitHub, `refs/pull/*` refs survive a force-push and keep the old commits
   reachable. Contact GitHub Support to purge them, or accept the exposure.

---

## Before you push

```bash
cd go && go build ./... && go vet ./... && go test ./...
cd internal/web/console && npm ci && npm run lint && npm test && npm run check:dist
```

`check:dist` matters: `dist/` is committed because the Go binary embeds it, so a
console source change **must** be rebuilt and committed with it or CI fails.

## Conventions

- Match the surrounding code: comment density, naming, and error style.
- Comments explain **why**, not what. Prefer a comment that records a decision or
  a trap over one that restates the code.
- Keep the dependency tree small. This project has **three** direct Go
  dependencies and cross-compiles to four platforms with `CGO_ENABLED=0`. Prefer
  the standard library; a new dependency needs a reason that survives review.
- No AI attribution trailers in commit messages.
- Do not commit dates or version stamps into documentation — git records history.
