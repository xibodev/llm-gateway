# Product Website

Static HTML, CSS, JSON, SVG and JavaScript for
<https://xibodev.github.io/llm-gateway/>. No framework, CDN, external fonts, or
production Node runtime. Repository guides under `docs/` own product facts;
keep their website summaries in sync.

## Installation Content

[`docs/QUICKSTART.md`](../docs/QUICKSTART.md) is the canonical installation guide;
`quickstart.html` is its manually maintained website counterpart. Keep the
homepage snippet, docs landing page, operations/upgrading pages, and `search.json`
aligned with it. The default is the published
`ghcr.io/xibodev/llm-gateway:0.3.1` image, not a repository clone or build.

The standalone recipe uses an installation-local `compose.yaml` and generate-once
`.env`, two required keys, a loopback host mapping with optional `LLMGW_PORT`,
and a named `state:/state` volume. No initial config file is required. Retain the
folder, project, volume and original keys on restart/upgrade; back up `.env`
separately from state. Image upgrades change the pin, pull, then start without
building. Native downloads include all five release archives and checksum guidance.

Keep developer source instructions separate: the released root Compose file
builds source, uses `LLMGW_HOST_PORT`, seeds from `config.local.yaml` once and uses
`llmgw-state`. It does not read `LLMGW_IMAGE`; any private override must explicitly
introduce that variable. Do not mix the independent installation methods.

## Static Validation

With the repository's CI Node version, from the repository root:

```bash
node --check website/site.js
node --check scripts/check-docs.mjs
node --check scripts/build-website.mjs
node scripts/check-docs.mjs
node scripts/build-website.mjs
git diff --check
node scripts/build-website.mjs --clean
```

These checks do not run the gateway, Docker, inference, or a preview server.

## Optional Preview

Only when browser QA is in scope, run `node scripts/serve-website.mjs` after the
static build. It is not needed for documentation-only validation.
Open `http://127.0.0.1:8080/llm-gateway/`. The preview serves the generated
`.website-dist` at the actual project subpath, including nested 404 fallback.
Set `PORT` to change the loopback port. Stop the preview with Ctrl+C.

The checker validates documentation links, HTML links/fragments and duplicate
IDs, one h1 per page, dialog labels, copy targets, search/manifest/sitemap URLs,
API paths, provider labels, known stale claims, and an asset-size budget.
It deliberately does not lint legacy application UI copy. Rebuild after edits.

Browser QA should cover desktop and 320/390px layouts; menu and no-JavaScript
navigation; search success, no results, failed fetch and retry; keyboard focus,
Escape and arrow keys; code-copy success/fallback; reduced motion; nested 404;
and no page-level horizontal overflow. Code blocks and tables scroll locally.

## Publication Gate

The build copies only publishable static files into `.website-dist`, excluding
this README and DESIGN.md. Do not commit generated output or QA screenshots.
Remove build output with `node scripts/build-website.mjs --clean` after validation.
`.github/workflows/pages.yml` checks pull requests and permits artifact upload
and deployment only on `main` push or main-targeted manual dispatch. Deployment
alone has `pages: write` and `id-token: write`.

Publishing requires a separately authorized merge/push and GitHub Pages configured
to use GitHub Actions. The workflow does not enable Pages or alter repository
settings. Before that authorization, preview and validate locally only. Retain
the existing Go/console release gates; website checks do not replace them.
