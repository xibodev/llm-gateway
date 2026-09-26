# llm-gateway brand kit

Canonical source assets for the llm-gateway identity. The formal product name
remains `llm-gateway`; `llmgw` is the compact visible wordmark.

## Contents

- `BRAND.md`: idea, audience, positioning, construction, usage, and voice.
- `tokens.json` and `tokens.css`: portable brand tokens.
- `logos/`: light, inverse, and monochrome logo variants.
- `icons/`: favicon and application-icon variants.
- `og/og-default.svg`: editable 1200x630 social-image master.
- `og/og-default.png`: deterministic 1200x630 social-image raster.
- `preview.html`: local, dependency-free asset preview.
- `provenance.json` and `LICENSES.md`: origin and licensing notes.

`brand/` is the editable master. `website/` contains exact runtime projections
for GitHub Pages. Consumers should use a canonical source asset for design work
and the corresponding `website/` file for the published site; do not edit both
independently. There are no SHA locks or brand lock files.

## Regeneration

The only generated source artifact is the PNG projection of the OG SVG. Its
lettering is SVG geometry, not a bundled font. The script obtains temporary,
exactly pinned `@resvg/resvg-js` `2.6.2` and `@resvg/resvg-js-cli`
`2.6.2-beta.1` packages through npm; neither becomes a permanent repository or
website dependency. The output is independent of a browser or locally installed
font. From the repository root:

```bash
node scripts/render-brand-assets.mjs
```

The script renders `brand/og/og-default.png` and then copies all approved runtime
projections to `website/`. It accepts `--check` to verify without writing:

```bash
node scripts/render-brand-assets.mjs --check
node scripts/check-docs.mjs
node scripts/build-website.mjs
```

Open `preview.html` directly in a browser for visual review. It loads only files
in this directory and has no external fonts, scripts, images, or CDN dependency.
