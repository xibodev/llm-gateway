# Quickstart

Install the published image, open the embedded console, and connect a provider.
No Git clone, source build, Go, Node.js, or initial `config.yaml` is required.
Prefer no Docker? Use a [prebuilt native binary](#native-binary).

## Prerequisites

- Docker Desktop (Linux containers) or Docker Engine with Compose.
- `openssl` for the Linux/macOS shell example, or PowerShell 7 on Windows.
- A provider credential, unless you use a local no-auth provider such as Ollama.

## Docker Compose

Create a private installation folder of your choice, outside any repository,
and open a terminal there. Keep this folder for restarts and upgrades.
Create `compose.yaml` in it with the following contents:

```yaml
services:
  gateway:
    image: ghcr.io/xibodev/llm-gateway:0.3.1
    ports:
      - "127.0.0.1:${LLMGW_PORT:-8787}:8787"
    environment:
      LLMGW_API_KEY: ${LLMGW_API_KEY:?set LLMGW_API_KEY in .env}
      LLMGW_CREDENTIAL_ENCRYPTION_KEY: ${LLMGW_CREDENTIAL_ENCRYPTION_KEY:?set LLMGW_CREDENTIAL_ENCRYPTION_KEY in .env}
    volumes:
      - state:/state
    restart: unless-stopped

volumes:
  state:
```

The public image supports Linux amd64 and arm64. It already runs `/llmgw serve`,
listens on `0.0.0.0:8787` **inside the container**, and includes an exec-form
healthcheck. No command or healthcheck override is needed. The named volume
persists configuration and databases at `/state`, owned by container UID/GID
65532. Configure providers in the console; no config seed is needed.

This recipe pins [v0.3.1](https://github.com/xibodev/llm-gateway/releases/tag/v0.3.1).
See [latest releases](https://github.com/xibodev/llm-gateway/releases/latest)
when choosing a future version, and follow [Upgrading](UPGRADING.md).

### Save secrets once

For a **new installation only**, run one of these in the installation folder.
Each generates two independent random values and refuses to overwrite `.env`.
If you already have state, restore your original keys instead of generating new
ones, even if `.env` is missing.

POSIX shell (Linux/macOS):

```bash
(
  set -eu
  umask 077
  test ! -e .env || { printf '%s\n' '.env already exists; keep it.' >&2; exit 1; }
  admin=$(openssl rand -base64 32)
  encryption=$(openssl rand -base64 32)
  set -C
  printf '%s\n' "LLMGW_API_KEY=$admin" \
    "LLMGW_CREDENTIAL_ENCRYPTION_KEY=$encryption" > .env
)
```

PowerShell 7 (Windows):

```powershell
$ErrorActionPreference = 'Stop'
if (Test-Path -LiteralPath .env) { throw '.env already exists; keep it.' }
$admin = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$encryption = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
New-Item -ItemType File -Path .env -Value (
  "LLMGW_API_KEY=$admin`nLLMGW_CREDENTIAL_ENCRYPTION_KEY=$encryption`n"
) | Out-Null
```

`LLMGW_API_KEY` is the administrator key. `LLMGW_CREDENTIAL_ENCRYPTION_KEY`
encodes exactly 32 random bytes as base64 and enables encrypted provider
credentials and recoverable key copies. Never regenerate it with existing state.

Keep `.env` local and private; never commit or share it. The POSIX example
creates it owner-only (`chmod 600 .env` can tighten an existing file). On Windows,
the file inherits folder permissions: use a private user folder and check that
other users cannot read it. Back up `.env` securely **separately from state
backups**, which exclude these environment keys.

### Start the gateway

From the folder containing `compose.yaml` and `.env`, in either shell:

```bash
docker compose up -d
```

Compose reads `.env` automatically and downloads the pinned image if missing.
No `--build` or `--env-file` is needed for this standalone recipe. Shell values
override `.env`; remove any stale `LLMGW_API_KEY`,
`LLMGW_CREDENTIAL_ENCRYPTION_KEY`, or `LLMGW_PORT` from your shell before starting.
To choose another host port, add `LLMGW_PORT=<HOST_PORT>` to `.env`, replacing the
placeholder with your chosen number. This variable changes only the published
host port in this recipe, not the container's listener.

Open `http://127.0.0.1:8787/console` (substitute your host port if changed).
Use only the `LLMGW_API_KEY` value from your local `.env` to sign in. Never paste
the encryption key, file contents, or authenticated output into support messages.

## Verify the process

The image's built-in healthcheck checks process liveness. You can also open
`http://127.0.0.1:8787/health`: HTTP `200` with `status: ok` and build identity
means the process is running, not that a provider can serve requests.
The console checks administrator access; authenticated `/admin/api/state`
reports `auth_required: true`.

## First configuration

1. Open **Access**, create a human principal and a project, then assign the human
   the `owner` or `admin` role in that project.
2. Open **Providers** and connect an API-key, OAuth, or local provider.
3. Run **Sync catalog**. Catalog access is not proof of inference.
4. Run **Test completion** to check provider inference; hosted requests may be billable.
5. Open **Routes** and create a named endpoint from catalog-backed exact models.
6. Open **API keys**, select the project, and choose the human under **Acts as**
   when the key must use that human's private OAuth connection.
7. Copy the generated gateway key. If credential encryption was configured at
   issuance, the owner or an administrator can reveal it later.

## Send a request

Replace `<PROJECT_KEY>` with the gateway-issued key and choose a Chat-capable
`<MODEL_SELECTOR>` returned by `GET /v1/models`:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Authorization: Bearer <PROJECT_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_SELECTOR>","messages":[{"role":"user","content":"Reply with: ok"}],"max_tokens":64}'
```

Use your chosen host port throughout. An endpoint name stands alone, not as
`endpoint/model`. Model listing does not prove support for every API surface.
See [client setup](CLIENTS.md) and [provider limitations](PROVIDERS.md).

## Restart, upgrade, or stop

Keep the same installation folder, `.env`, Compose project name and state volume.
Moving/renaming the folder can change the project name and select a fresh volume.
Do not repeat secret generation.

```bash
# Start again with the saved configuration:
docker compose up -d
# Stop without deleting state:
docker compose down
```

These commands also work in PowerShell. `down` retains the volume; `down -v`
**deletes it**. Before upgrading, [back up offline](OPERATIONS.md#backup-contents),
then [change the image pin and pull](UPGRADING.md#standalone-image-installation).
Keep the original encryption key and folder through every upgrade.

## Native binary

This is an **alternative installation**, not a way to access the Compose volume.
Choose a separate private folder and download your archive plus
[SHA256SUMS](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/SHA256SUMS):

| Platform | v0.3.1 download |
| --- | --- |
| Windows x64 | [windows_amd64.zip](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/llmgw_v0.3.1_windows_amd64.zip) |
| Linux x64 | [linux_amd64.tar.gz](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/llmgw_v0.3.1_linux_amd64.tar.gz) |
| Linux ARM64 | [linux_arm64.tar.gz](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/llmgw_v0.3.1_linux_arm64.tar.gz) |
| macOS Intel | [darwin_amd64.tar.gz](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/llmgw_v0.3.1_darwin_amd64.tar.gz) |
| macOS Apple Silicon | [darwin_arm64.tar.gz](https://github.com/xibodev/llm-gateway/releases/download/v0.3.1/llmgw_v0.3.1_darwin_arm64.tar.gz) |

Before unpacking, calculate the archive's SHA-256 and compare it with the exact
filename's entry in `SHA256SUMS`. Stop if they differ. For Linux x64:

```bash
sha256sum llmgw_v0.3.1_linux_amd64.tar.gz
tar -xzf llmgw_v0.3.1_linux_amd64.tar.gz
```

On macOS use `shasum -a 256 <ARCHIVE>` and `tar -xzf <ARCHIVE>` with your actual
archive name. On Windows:

```powershell
Get-FileHash .\llmgw_v0.3.1_windows_amd64.zip -Algorithm SHA256
Expand-Archive .\llmgw_v0.3.1_windows_amd64.zip -DestinationPath .
```

For this fresh native installation, generate `.env` once using the earlier
instructions. The binary does **not** load `.env` automatically. In each new
terminal, load those two keys and explicitly select this installation's state
and config before starting (POSIX):

```bash
set -a
. ./.env
set +a
export LLMGW_STATE_DIR="$PWD/state"
export LLMGW_CONFIG="$LLMGW_STATE_DIR/config.yaml"
export LLMGW_HOST=127.0.0.1
export LLMGW_PORT=8787
./llmgw serve
```

PowerShell 7:

```powershell
Get-Content -LiteralPath .env | ForEach-Object {
  if ($_ -match '^(LLMGW_API_KEY|LLMGW_CREDENTIAL_ENCRYPTION_KEY)=(.*)$') {
    [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2], 'Process')
  }
}
$env:LLMGW_STATE_DIR = Join-Path $PWD 'state'
$env:LLMGW_CONFIG = Join-Path $env:LLMGW_STATE_DIR 'config.yaml'
$env:LLMGW_HOST = '127.0.0.1'
$env:LLMGW_PORT = '8787'
.\llmgw.exe serve
```

Use your chosen port, then follow the console setup above. No initial config
file is required. Preserve `state/`, `.env`, and these environment settings on
every restart and binary replacement. A service manager must load the same keys
and absolute state/config paths. Without overrides, native state defaults to
`~/.llmgw`. See [Operations](OPERATIONS.md) for backup and release verification.

## Developers: from source

Only use this path to build the code yourself. It is independent of the standalone
image recipe and native installation; do not mix their folders, projects or volumes.
Clone the selected source release (Git required):

```bash
git clone --branch v0.3.1 https://github.com/xibodev/llm-gateway.git
cd llm-gateway
```

For a **fresh source Compose installation**, generate a local `.env` in this
clone using [Save secrets once](#save-secrets-once), and keep it out of Git.
Copy `llmgw.config.example.yaml` to `config.local.yaml` once, using `cp` on POSIX
or `Copy-Item` in PowerShell. Do not overwrite an existing config.
Add `LLMGW_HOST_PORT=127.0.0.1:8787` to this source `.env`.
Clear stale shell overrides for the two keys and `LLMGW_HOST_PORT`, then run:

```bash
docker compose --env-file .env up -d --build
```

This uses the **repository root** `docker-compose.yml`: it builds `go/Dockerfile`,
uses `LLMGW_HOST_PORT` (not the standalone recipe's `LLMGW_PORT`), and otherwise
publishes on all interfaces by default. It seeds `/state/config.yaml` from the
read-only `config.local.yaml` on first start only. Console edits and restores
change the volume copy. Its named volume is `llmgw-state`, not standalone `state`.
Use `docker compose --env-file .env down` to stop without deleting that volume.

For a Go source run instead, install Go 1.26.6, load your intended native
environment and absolute state/config paths as above, then from `go/` run
`go run ./cmd/llmgw serve`. See [Go development](../go/README.md).

## Common failures

| Symptom | Meaning and recovery |
| --- | --- |
| Compose says a required key is unset | Check that `.env` is beside `compose.yaml`, uses both exact key names above, and that your shell has no stale override. Do not regenerate existing keys. |
| Console asks for an administrator key | Use `LLMGW_API_KEY` from `.env`; project keys cannot administer the gateway. |
| `401` from `/v1/*` | Check the gateway key's header, status and expiry in **API keys**. |
| `404` for a model | Choose an exact ID from `GET /v1/models`; do not guess retired or ambiguous aliases. |
| Catalog is empty | Check credentials, base URL, location and selected human owner; some credential types cannot list models. |
| Catalog sync succeeds but inference fails | Read **Test completion**: check the reported model/deployment and region for `404`, credential scope for `401`/`403`, and provider quota/billing for `429`. Catalog access alone grants no inference entitlement. |
| OAuth-backed model returns `403` | Mint the key as the human who owns that private connection, or configure the supported shared-credential binding. |
| Docker cannot reach a host model server | Use `host.docker.internal`, not container loopback. On Linux Engine, add `extra_hosts: ["host.docker.internal:host-gateway"]` to the standalone gateway service. |
