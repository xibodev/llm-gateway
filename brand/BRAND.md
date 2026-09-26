# llm-gateway brand

## Idea

The Optical Rail Splitter makes the product model visible: one request enters,
an explicit decision point separates the route, and known provider targets sit
at the ends. The identity is exact, operational, calm, and intentionally unlike
an opaque model-selection cloud.

## Audience

The primary audience is the operator or developer who needs to route coding
clients and core OpenAI/Anthropic HTTP consumers through a self-hosted gateway.
They value deterministic selectors, documented failover, inspectable state, and
clear limits more than broad automation claims.

## Positioning

`llm-gateway` is a self-hosted, multi-provider LLM gateway. It provides exact
model routing, explicit ordered failover on supported request paths, governance,
and local single-node operation in one Go binary. Registry support describes a
configuration path; it does not prove account, region, deployment, model, or
entitlement availability.

## Canonical naming

The formal product and repository name is **llm-gateway**. The compact visible
wordmark is **llmgw**.

- Use `llmgw` in supplied lockups and compact interface labels.
- Use `llm-gateway` in prose, titles, descriptions, repository references, and
  package or command context where that is the actual identifier.
- When `llmgw` is the visible text of a linked home treatment, its accessible
  name must begin with `llmgw` and may then expand the formal name, for example
  `llmgw, llm-gateway home`.
- Do not rename the product, repository, packages, or formal metadata to `llmgw`.

## Mark geometry

The mark uses a `0 0 100 100` view box and this fixed construction:

- Input line: `x1=16 y1=50 x2=42 y2=50`, white, 5 units wide, round cap.
- Input terminal: circle at `(16,50)`, radius `3.5`, white.
- Splitter: polygon `42,32 58,50 42,68`, Laser Amber.
- Upper rail: `M54 44 L66 32 L84 32`, 3.5 units wide, round cap and join.
- Center rail: `M58 50 L84 50`, 4.5 units wide, round cap.
- Lower rail: `M54 56 L66 68 L84 68`, 3.5 units wide, round cap and join.
- Output terminals: circles at `(84,32)`, `(84,50)`, and `(84,68)`, each with
  radius `3.5`, white.

For light surfaces only, replace white details with `#111827` and Laser Amber
with Accessible Dark Amber `#B45309`. Do not redraw, rotate, skew, crop, add an
output, change stroke widths, or rearrange the mark.

## Variants

- `logos/mark.svg`: light-surface mark using `#111827` and `#B45309`.
- `logos/mark-inverse.svg`: primary dark-surface mark using white and `#F59E0B`.
- `logos/lockup.svg` and `logos/lockup-inverse.svg`: mark with visible `llmgw`.
- `logos/wordmark.svg` and `logos/wordmark-inverse.svg`: compact wordmark only.
- `logos/mono-black.svg` and `logos/mono-white.svg`: one-color lockups for
  genuinely restricted reproduction.
- `icons/`: Carbon Obsidian tiles containing the primary inverse mark.

## Clearspace and minimum sizes

Keep clear space around marks and lockups equal to at least 7 mark units, the
diameter of a terminal. Nothing except the icon tile may enter that area.

- Standalone mark in an interface: 24 CSS pixels minimum.
- Lockup on screen: 96 CSS pixels wide minimum.
- App icon: use the supplied purpose-sized file; do not shrink a lockup into it.
- Favicon: use `icons/favicon.svg`; do not derive it from a screenshot.

## Color

| Token | Value | Use |
| --- | --- | --- |
| Carbon Obsidian | `#090A0F` | Primary tile and dark field |
| Detail Ink | `#111827` | Mark details and body ink on light surfaces |
| Laser Amber | `#F59E0B` | Brand signal on Carbon Obsidian |
| Accessible Dark Amber | `#B45309` | Brand signal on light surfaces |
| Signal White | `#FFFFFF` | Mark detail and wordmark on Carbon Obsidian |

Laser Amber on Carbon Obsidian has a `9.21:1` contrast ratio. Accessible Dark
Amber on white has a `5.02:1` contrast ratio and on the website canvas has a
`4.68:1` ratio. Do not use Laser Amber for text or required detail on light
surfaces.

Failure crimson `#B42318` and success green `#087F5B` are semantic status colors,
not brand colors. Always pair status color with a text label or symbol.

## Typography

The supplied SVG lockups use portable generic sans-serif text. The brand kit and
website otherwise use local system sans-serif and monospace stacks. No font
binary, web font, remote stylesheet, or CDN is required. Use monospace only for
commands, paths, model IDs, statuses, protocol labels, and measurements.

## Accessibility

- Use the light mark only on white or equivalently light surfaces, and the
  inverse mark only on Carbon Obsidian or equivalently dark surfaces.
- Decorative SVG images need empty HTML `alt` text.
- Standalone informative marks need a concise name such as `Optical Rail
  Splitter`.
- Linked logo treatments get their accessible name from the link; do not repeat
  the same text through both image alt text and link text.
- Color never carries route state, failure, or success by itself.
- Preserve mark geometry and contrast under forced colors or supply equivalent
  text when the image is unavailable.

## Voice

Lead with concrete behavior and name the boundary in the same context.

| Prefer | Avoid |
| --- | --- |
| `Select one exact provider/model target.` | `Intelligently picks the best model.` |
| `An ordered endpoint can fail over before output starts.` | `Never lose a request.` |
| `Health proves process liveness, not provider readiness.` | `Everything is healthy.` |
| `Registry support does not guarantee entitlement.` | `Supports every listed model.` |
| `Single-node SQLite state.` | `Enterprise-grade high availability.` |

Use specific verbs: route, select, fail over, verify, operate, recover. Preserve
the documented protocol, provider, UI, credential, and single-node limitations.
Do not imply weighted or quota-aware scheduling, universal compatibility, hosted
operation, provider entitlement, or hidden automation.

## Imagery and motion

Favor route diagrams, explicit endpoints, terminal fields, and real interface
or command context. Avoid generic AI brains, robots, nebulous clouds, stock
datacenters, provider-logo collages, and invented dashboard screenshots.

If animated, show one request moving from input to a selected terminal. Motion
runs once, never suggests random routing, and must stop under
`prefers-reduced-motion`. Do not make every rail pulse or loop continuously.

## Do and don't

- Do preserve exact geometry and approved colors.
- Do keep `llmgw` visible in compact brand treatments while retaining the formal
  name in product metadata and prose.
- Do use source assets from this directory and project runtime copies from them.
- Do keep failure and success colors separate from brand amber.
- Don't recolor, outline, add effects to, or place text over the mark.
- Don't use Laser Amber as small text on a light background.
- Don't pair the mark with an invented tagline or unsupported product claim.
- Don't use a lockup where the mark is too small to remain legible.

## Touchpoint matrix

| Touchpoint | Asset or naming | Notes |
| --- | --- | --- |
| Website header | `website/logo-mark.svg` + visible `llmgw` | Accessible link begins `llmgw` |
| Browser favicon | `website/favicon.svg` | Exact projection of canonical favicon |
| Web app manifest | `website/icon-192.svg`, `website/icon-512.svg` | Purpose-sized published SVG icons |
| Social preview | `website/og-default.png` | 1200x630 primary metadata image |
| README/docs | Formal `llm-gateway`; lockup when artwork is needed | Keep claims factual |
| CLI and binary | Existing `llmgw` executable where factual | Do not rename the product |
| Console/product UI | Light or inverse standalone mark | Preserve existing semantic colors |
| Monochrome print | `mono-black.svg` or `mono-white.svg` | Only when color is unavailable |
| Source distribution | `brand/` | Editable masters and usage guidance |
