# llm-gateway brand

## Identity

The formal product and repository name is **llm-gateway**. The compact visual
wordmark is **llmgw**. Use `llmgw` in brand lockups and compact interface labels;
use `llm-gateway` in prose, page titles, accessible names, package references,
and repository references.

The identity should feel exact, operational, and calm. It represents a request
entering one gateway, splitting into explicit routes, and terminating at known
targets.

## Optical Rail Splitter

The mark uses a `0 0 100 100` view box and this fixed construction:

- Input rail: `(16,50)` to `(42,50)`, 5 units wide with round caps.
- Input terminal: circle at `(16,50)` with radius `3.5`.
- Splitter: polygon `42,32 58,50 42,68`.
- Upper rail: `M54 44 L66 32 L84 32`, 3.5 units wide with round caps and joins.
- Center rail: `M58 50 L84 50`, 4.5 units wide with round caps.
- Lower rail: `M54 56 L66 68 L84 68`, 3.5 units wide with round caps and joins.
- Output terminals: circles at `(84,32)`, `(84,50)`, and `(84,68)`, each with
  radius `3.5`.

On Carbon Obsidian, the splitter and output rails are Laser Amber; the input
rail and all terminals are white. On light surfaces, use Accessible Dark Amber
for the splitter and rails and Carbon Obsidian for the input and terminals.
Do not redraw, rotate, skew, crop, or rearrange the mark.

Keep clear space around the mark equal to at least the input terminal diameter.
Use the mark at 24 CSS pixels or larger in interfaces. Use the supplied icon
assets, rather than shrinking a lockup, below that size.

## Color

| Token | Value | Use |
| --- | --- | --- |
| Carbon Obsidian | `#090A0F` | Primary dark field and light-surface detail |
| Laser Amber | `#F59E0B` | Brand signal on Carbon Obsidian |
| Accessible Dark Amber | `#B45309` | Brand text and required detail on light surfaces |
| Signal White | `#FFFFFF` | Detail and wordmark on Carbon Obsidian |

Laser Amber on Carbon Obsidian has a contrast ratio of `9.21:1`. Accessible
Dark Amber on white has a contrast ratio of `5.02:1`. Do not use Laser Amber
for text or required graphical detail on white.

Failure orange/red and success green are semantic status colors, not brand
colors. Always pair status color with a text label or symbol, and never describe
those colors as part of the llm-gateway identity.

## Wordmark and typography

The visual wordmark is lowercase `llmgw`. Do not substitute `llm-gateway` inside
the supplied lockup. Keep the formal name in nearby accessible text when the
context does not already identify the product.

The assets and preview use system sans-serif and monospace stacks. No web font,
font download, or CDN is required. Product interfaces should continue to use
their local system stacks.

## Voice

Lead with concrete behavior:

- Exact routing.
- Explicit, ordered failover.
- Self-hosted operation.
- A single Go binary.

Preserve the product's documented limitations. Do not imply high availability,
weighted or quota-aware scheduling, universal protocol compatibility, hosted
operation, or provider entitlement. Prefer specific nouns and verbs over broad
claims: route, select, fail over, verify, operate, and recover.

## Asset use

- Use `logos/lockup.svg` on light surfaces and `logos/lockup-inverse.svg` on
  Carbon Obsidian or equivalently dark surfaces.
- Use `logos/mark.svg` and `logos/mark-inverse.svg` where the product name is
  already visible or available as an accessible name.
- Use `logos/mono-black.svg` and `logos/mono-white.svg` only when reproduction
  is restricted to one color.
- Use the files under `icons/` for browser and application icon consumers.
- Use `og/og-default.svg` for the default social preview.

SVG images used as decoration should have empty HTML `alt` text. Linked marks
must get the accessible name `llm-gateway` from their link or surrounding UI.
