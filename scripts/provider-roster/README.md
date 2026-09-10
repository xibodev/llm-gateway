# Provider roster collector

Node standard-library-only collector for the schema in
[`docs/PROVIDER_ROSTER.md`](../../docs/PROVIDER_ROSTER.md). Requires Node 22 or
newer. It produces unsigned `payload.json` and `report.json`; signing is a
separate operation over the exact payload bytes.
The collector caps entries at 5,000, matching Go. The signer must enforce the
4 MiB **signed envelope** limit, including base64 expansion, signature and key ID;
that limit is not a 4 MiB decoded-payload allowance.
Consumer string bounds are UTF-8 bytes: names 256, IDs 128, descriptions 8,192,
and URLs 4,096. Producer names are truncated only between complete Unicode
codepoints; final validation rejects oversized fields, including retained data.
The shared offline fixture includes a long CJK name for the Go pipeline check.

## Run

```sh
node scripts/provider-roster/build.mjs --output /path/to/artifacts
node scripts/provider-roster/build.mjs --output /path/to/artifacts --previous /path/to/payload.json
node scripts/provider-roster/build.mjs --output /path/to/artifacts --no-probe
node scripts/provider-roster/build.mjs --output /path/to/artifacts --fixtures scripts/provider-roster/fixtures --offline
node --test scripts/provider-roster/roster.test.mjs
```

- `--output DIR` is required. Files are replaced atomically, individually. Consumers
  must check successful process exit before using either artifact.
- `--previous PATH` takes a decoded payload, not a signed transport envelope.
  Supply the last accepted payload to preserve stale entries and incident state.
- `--fixtures DIR` reads the four fixed snapshot filenames and `snapshots.json`
  with a full commit per repository. These are synthetic test snapshots, not an
  upstream dataset or real commit attestations.
- `--offline` disables **all** network activity, including probes and logos.
  Without fixtures it can retain a previous payload, or fails if none exists.
- `--no-probe` skips endpoint probes; source and approved logo collection still run.
- Favicon discovery runs automatically for reviewed official signup origins in
  `catalog.mjs`, trying `/favicon.ico` then `/favicon.png`. `--no-favicon` disables
  that discovery but keeps the explicit raster mapping. `--favicon` is accepted
  for compatibility and is now the default.
  Standalone PNGs and validated PNG images inside bounded ICO containers are
  accepted. BMP-only ICOs, SVG, HTML and animations are rejected.

Healthy collection produces a contract-valid nonempty payload and reports `ok`
for all four sources. The report records source counts, skips, stale retention,
conflicts, probe results, logo outcomes, API evidence and attribution notices.
Source failures can still exit successfully when a nonempty payload can be
preserved; inspect `sources` and `all_sources_stale` before deciding to publish.
Revisions identify builds, not successful source refreshes. Failed sources retain
`status: stale` and their prior commit even when a new revision is built; consumers
must use source status when computing freshness.
An initial complete failure exits nonzero and never overwrites an existing
payload with an empty replacement.

## Sources and interpretation

The collector resolves each public repository's HEAD to a commit and fetches
only its commit-pinned raw file. It executes no upstream code and uses no GitHub
or provider credentials.

| Repository | Extracted structure | License |
| --- | --- | --- |
| `mnfst/awesome-free-llm-apis` | `data.json` → `providers[]` | CC0-1.0 |
| `nejib1/Free-LLM` | Marked permanent/renewable/trial tables joined to quick reference | MIT |
| `open-free-llm-api/awesome-freellm-apis` | Marked directory tables joined to quick reference; HTML links | MIT |
| `oniondas/Awesome-Hidden-AI-Credits-Free` | Bullets within five specific hosted-service sections | CC0-1.0 in README |

Malformed or unexpectedly empty source structures mark the whole source stale.
An explicitly blank endpoint and signup link is a reported skip. Local tools,
featured duplicates and unrelated links are not harvested. Requirements retain
source qualifications, including registration and deposit conditions. Relative
expiry such as “30 days” or a date without a year never becomes an invented
absolute timestamp.

Only exact API roots reviewed in `catalog.mjs` receive a supported protocol.
Known native APIs and generic homepage links remain `unknown`/`candidate`, even
under an upstream claim that all providers support OpenAI. Reviewed API-key
requirements can supply known auth; “no card”, a Get Key link, and an HTTP result
cannot establish anonymous access. Explicit contradictory offers/auth resolve
to `unknown` with visible, sorted conflicts. Unknown evidence cannot erase an
existing conflict. Names and signup URLs use lexical tie-breaking; disagreements
remain visible. No inference requests or model checks are made.

Identity is `endpoint-` plus the first 16 hexadecimal digits of SHA-256 over
`protocol + "\n" + canonicalURL`. Canonicalization normalizes host/default port,
unreserved percent escapes and an ordinary trailing slash. Path case, doubled
slashes, regions, protocols and independent gateways remain distinct. Referral
queries/fragments are removed from site/signup links; endpoint queries,
credentials and fragments are rejected. Homepage discoveries intentionally do
not deduplicate into a known API root without a reviewed identity mapping.

Previous expiry timestamps, report objects, quarantines and withdrawals carry
forward. Ten recorded confirmations quarantine an entry; this collector does
not count accounts or fetch issues. Removed entries become withdrawn tombstones.
A maintainer restore/incident-epoch workflow must supply the reviewed previous
state; recollection cannot restore a tombstone itself.
Source disappearance always creates a withdrawn tombstone, including when the
entry was quarantined. Its incident report remains intact. Clearing a community
incident does not clear source withdrawal, and a returning source remains
withdrawn until a separate maintainer review deliberately restores that entry.
An expired offer becomes `unknown`, retains its expiry timestamp and gains an
expired-offer requirement. It does not withdraw the endpoint or clear quarantine;
consumers can exclude expired entries from free-offer filters using that timestamp.

## Network and logos

All collection/probe/logo requests share an eight-request scheduler with a
two-request per-host limit. HTTPS only; all DNS answers must be public. DNS is
resolved once per request and the connection lookup is pinned to an approved
address while TLS verifies the original hostname. Private, special-purpose,
mapped and transition address ranges, URL credentials, custom ports, redirects
and encoded bodies are rejected. There are total request deadlines (including
DNS), header bounds and byte limits. No environment proxy, cookies, ambient
credentials, browser or runtime image fetches are used.

Probes GET only the known API root, at most 64 KiB in six seconds. A provider
can legitimately return 404 at that root; `failed` does not establish an
inference outage. HTTP 2xx means reachability only, not entitlement or free use.

Automatic logos first use the reviewed official raster mapping in `catalog.mjs`
for known services, then fixed favicon paths on an approved official
signup origin. A community-provided hostname is not proof of an official site:
unreviewed origins, API-host guessing, HTML discovery and subdomain matching do
not authorize downloads. Add reviewed signup origins to `APPROVED_SIGNUP_ORIGINS`
to expand coverage without inventing an asset URL. Common favicon paths are
candidates, not claims that a working PNG was verified there.

Downloads are cached by URL across all entries and signup paths, including
failures, with at most two candidates per host and 128 candidates per build.
Candidates are reserved in stable entry-ID order, so budget allocation does not
depend on network timing. Requests share the existing eight-global/two-per-host
safe fetch scheduler. Downloads are bounded to 64 KiB,
validated for PNG signature, chunk CRCs, structure, dimensions and bounded
decompression, then embedded with SHA-256 and source/rights metadata. ICO directory
bounds, image overlap and declared dimensions are checked before PNG extraction. Static
noninterlaced PNGs up to 512×512 are accepted. Failed downloads retain a valid
previous logo. Missing logos omit image data so the consumer can use initials.
Adding services requires reviewing their official signup origin or raster URL; no SVG renderer
or heavyweight rasterization dependency is needed.

The `sources` objects include additive `license`, `license_url` and `notice`
metadata. MIT notices are carried inside the payload as well as the report.
Raster metadata records provider trademark rights, not a claim that assets have
an open-source license. Discovered favicons use literal `license: "unknown"` and
retain the exact `source_url`; this asserts no redistribution permission or
independently verified provider-logo claim. No visible attribution is generated.
Upstream licensing/branding changes require review.

## API

Import from `scripts/provider-roster/index.mjs`:

- `buildRoster(options)` → `{ payload, report }`; supports `previous`, `fixtures`,
  `offline`, `noProbe`, `favicon`, `now`, `fetcher` and `snapshotLoader`.
- `loadSnapshot(source, fetcher, options)`, `SOURCES`, `extractSource(repo, text)`.
- `normalizeEntry(raw, provenance)`, `canonicalURL`, `stableID`, `mergeEntries`,
  `carryState`, `classifyAuth`, `classifyOffer`, `sha256`.
- `createSafeFetcher`, `Scheduler`, `publicURL`, `isPublicAddress`, `FetchError`,
  `probeEndpoint`.
- `collectLogos`, `validatePNG`, `validateLogo`, `validatePayload`.

Transport/resolver and snapshot injection points support deterministic tests;
production CLI uses the pinned HTTPS implementation. The fixtures reproduce
only structural shapes using synthesized rows and placeholder domains. They do
not reproduce a full copyrighted dataset. Keep production artifacts outside
the source tree.

## Standalone favicon discovery experiment

```sh
node scripts/provider-roster/discover-favicons.mjs --input /path/to/entries.json --output /path/to/discovery --playwright-module /absolute/path/to/playwright/index.mjs --concurrency 4 --timeout-ms 10000
node --test scripts/provider-roster/discover-favicons.test.mjs
```

Input is `{ "entries": [{ "id": "example", "name": "Example", "page_url": "https://example.com/signup", "page_kind": "signup", "existing_logo": false }] }`.
Supply the merged provider list; `page_kind` also accepts `documentation` or `none`.
Playwright and Chromium must already be available; the optional absolute module
path selects an external installation (otherwise imports `playwright`). Nothing
is installed. Concurrency is 1–8 (default 4); each fetch chain has a 100–30000 ms
deadline (default 10000), with at most three redirects and two requests per host.

The script reads static HTML only, ranks declared icon / Apple touch / mask links,
then tries `/favicon.ico`, `/favicon.png`, and `/apple-touch-icon.png` on the final
page origin. If the page fetch fails, those fixed paths are tried only on the
original validated public origin; `page_failure` retains the navigation failure
and `final_page_url_status: "unvisited"` distinguishes that original URL from a
successfully fetched page. Root fallbacks reject cross-origin redirects, including
authentication-provider icons. Blocked input URLs authorize no fallback requests.
It uses a script-local pinned
HTTPS fetcher, revalidates public DNS on every redirect, sends no credentials or
cookies, and accepts only public cache/presentation/referral query parameters.
Wire and decoded bodies are capped at 2 MiB per page and 512 KiB per icon.
There are at most 32 candidates per page; pages and assets (including failures)
are cached. Manifest icons and JavaScript-generated markup are not inspected.
PNG/ICO/SVG/JPEG/WebP are decoded as inert data images in isolated offline Chromium
contexts with external requests blocked, a five-second decode deadline, and a
4096-pixel dimension cap. Accepted images fit within a transparent 128×128 PNG;
fully transparent and 1×1 images are rejected. Output PNGs pass `validatePNG`
and the 64 KiB limit. No downloaded HTML or raw image is saved.

Outputs: `results.json`, `results.csv`, `report.md`, `contact-sheet.html`, and
content-hashed `logos/*.png`. Reports distinguish inaccessible pages, unsupported
icons, and absent/blocked public pages; source URLs are audit evidence, not
verified branding or redistribution permission. This experiment changes no
roster payload, runtime logo selection, or application assets. For optional
offline browser tests, set `FAVICON_TEST_PLAYWRIGHT_MODULE` to the absolute module
path before running the test command.
