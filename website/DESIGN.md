# llm-gateway website design system

## Direction

The site is a routing-switchboard field guide: requests enter one private
gateway, traverse an explicit route, and exit through a known provider. It takes
the crisp slate, indigo, terminal, and compact-card language of the gflow-cli
reference and expands it for a larger product and documentation surface.

## Tokens

- Canvas: `#f5f7fb`
- Panel: `#ffffff`
- Ink: `#111827`
- Muted ink: `#526175`
- Hairline: `#dce2eb`
- Route indigo: `#4f46e5`
- Deep route: `#252a6a`
- Failover orange: `#d85e16`
- Success green: `#087f5b`
- Code field: `#101827`
- Radius: 6px for controls, 10px for major panels
- Content width: 1180px; reading measure: 72 characters

Use the system sans stack for fast, dependency-free rendering. Reserve the
system monospace stack for commands, paths, model IDs, statuses, and protocol
labels.

## Components

- Junction-mark wordmark: authored inline SVG and matching favicon.
- Header: solid panel, thin divider, full desktop navigation, real mobile menu.
- Routing board: labelled endpoint and provider nodes connected by directional
  rails. Orange communicates a failed hop, green a served hop, with text labels
  so color is never the only signal.
- Code blocks: dark field, filename/intent label, optional copy button.
- Cards: thin border and restrained radius; no nested card stacks.
- Tables: semantic tables with local horizontal scrolling on narrow screens.
- Documentation: persistent section navigation, bounded reading column, and
  descriptive next steps.
- Search: native dialog, keyboard shortcut, live result count, static JSON index.

## Responsive and accessibility

- Mobile first from 320px. Hero and routing board stack vertically.
- Touch targets are at least 44px.
- One `h1`, ordered headings, semantic landmarks, skip link, visible focus.
- Code and tables scroll inside their own regions; the page never overflows.
- Routing motion runs once and is disabled by `prefers-reduced-motion`.
- All content and navigation remain useful without JavaScript.

## Content rules

- Lead with exact routing, ordered failover, core protocol surfaces, governance,
  and local operation.
- Distinguish registry support from live account/model entitlement.
- Distinguish process health from provider readiness.
- State single-node, protocol, UI, credential, and provider limitations directly.
- Never invent customers, benchmarks, packages, pricing, hosted service, or a
  floating image tag.
