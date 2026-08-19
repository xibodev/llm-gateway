# Provider instances, credential upload, catalog fidelity, and Azure OpenAI

Date: 2026-08-18
Status: design — awaiting review

## Problem

Four defects share one root: **the console treats a provider *type* as if it were a
provider *instance*, and a *catalog* as if it were the truth about that instance.**

1. You cannot configure two providers of the same type from the console.
2. A file-shaped credential (a GCP service-account key) must be pasted into a
   textarea; there is no upload path, and building the request by hand in a shell
   mangles the payload.
3. The model counts a provider reports do not reflect what that provider can
   actually serve — Vertex under-reports from a hardcoded list, and an Azure
   OpenAI resource configured as `openai_compatible` over-reports by 410×.
4. `categories` (the client-facing routing layer) is called three different things
   across config, API, and console, and "endpoint" already means two other things.

Everything below is one PR, squashed to a single commit on `main`, developed on a
local feature branch with per-workstream chore branches merged and tested into it
first.

## Decision summary

Everything below is evidence-backed by live probes run 2026-08-18. This is the short
version; the detail follows.

| | Workstream | Core change | Verified constraint driving it |
|---|---|---|---|
| **A** | Provider instance identity | Server emits `instances[]`; tile aggregates, never *is*, an instance. No rollup status. | Backend already supports N-per-type; 12 same-type providers exist in production, configured around the console |
| **B** | Credential upload | Multipart on the three credential routes; file picker for file-shaped creds; explicit auth-type choice | No multipart exists on any credential path today; `vertex_ai` declares two auth methods and rejects one by provenance |
| **C** | `categories` → `endpoints` | Full rename + migration, **and** disambiguating the other two senses of "endpoint" | "Endpoint" already means 3 things; categories already leak into `/v1/models` as pseudo-models |
| **D** | Catalog fidelity | Delete the hardcoded Vertex list; fetch per instance; declare unknown when unfetchable | 6 of the curated 9 fail at call time on a `global` instance; 3 don't exist in any region |
| **E** | `azure_openai` type | New type: URL + key + deployment name. Catalog from deployments, never `/models` | `/models` returns 410 vendor-catalogue rows against 1 real deployment |
| **F** | Snippet host (minor) | `window.location.origin` instead of hardcoded `localhost:8787` | Literal string in 3 places |

**Scope boundaries, decided:**

- The catalog carries **managed models only** (`openGenerationAiStudio` + `requestAccess`).
  Self-deploy Garden models are out — that is what keeps a region under 100 rows.
- llmgw is **not an entitlement oracle**. It reports what upstream lists; a failed call
  in the playground is the operator's answer.
- **`/v1/models` stays routable-only.** It is consumed by client tooling; inflating it
  is the Azure defect.
- Vertex needs **one surface plus one adapter**, not N: OpenAI-compat
  `endpoints/openapi/chat/completions` with `<publisher>/<model>` covers google and xai;
  Anthropic-on-Vertex needs `:rawPredict`.

**Delivery:** one PR to `main`, squashed to a single commit, developed on a local
feature branch with per-workstream chore branches merged and tested into it first.

## Ground truth established during design

These were verified against the code and against a live Azure resource, not assumed.

### The backend already supports N instances per type

| Fact | Evidence |
|---|---|
| Provider config is keyed by arbitrary id, with type as an attribute | `config.go:24-48,134` — `Providers map[string]*ProviderConfig` |
| Upsert accepts any id | `server.go:121` — `POST /admin/api/providers` |
| Status already aggregates plural matches | `provider_status.go:187-196` — `matches`, `ids`, `disabledIDs` |
| The console is told the ids as a **list** | `configured_provider_ids` |
| Routing addresses instances, never types | `models.go:173-178` — `ProviderCredentialAuthorized(m.Provider, …)`, `m.Provider+"/"+m.Model` |

Per-instance data is fully computed in `configuredProviderSnapshot`
(`provider_status.go:129-135`: `id, registryID, status, catalogState, catalogRefresh,
modelCount, connectionCount, disabled, lastCheck, lastVerify, configurationIssue`)
and then discarded by the flatten loop.

### Where the console collapses it

| Site | Code | Effect |
|---|---|---|
| `ProviderHub.tsx:78` | `disabled={configured}` | ID field locks once one instance exists — no second instance |
| `shared.ts:79` | `providerIDs[0] ?? entry.id` | Test / Sync / **Remove** hit only the first instance |
| `ProviderHub.tsx:96` | `configuredProviderIDs(entry)[0]` | Credentials always land on the first instance |
| `ProviderHub.tsx:153-159` | `statusByID.get(entry.id)` | Tile identity *is* the registry id; other ids orphan into "custom" tiles |
| `ProviderDetail.tsx:146-149` | rows map `providerIDs` but render `entry.model_count` | Every instance row shows the same aggregate number |

Two aggregation bugs in the same loop:

- `provider_status.go:155` — `status = matches[0].status`. A healthy first instance
  **masks a broken second**.
- `provider_status.go:195` — `strings.Join(configurationIssues, " ")`. Two instances'
  problems concatenate into one unreadable sentence.

### Credentials

- `connectionBody.Secret` is a plain `string` (`connections.go:14-20`); an SA key
  rides as an escaped string inside a JSON body, discriminated only by
  `credential_kind`.
- Validation is `gcpauth.Parse` (`connections.go:214`), which expects raw JSON and
  reports precisely: `not valid JSON`, `private_key is not a PEM block`,
  `missing required field(s): …`.
- **No multipart exists on any credential route.** The only `FormFile` handling in the
  API is audio (`/v1/audio/transcriptions`, playground transcription).
- The reported `base64: invalid input` is **not** produced by this codebase — the
  string appears nowhere in it, and nothing on the credential path base64-decodes
  anything. It is GNU coreutils' message; the payload was destroyed client-side
  before the request existed.
- There are **two** SA entry points with different contracts:
  - `ConnectDialog` → `POST /admin/api/providers`, field `api_key`, one textarea
    labelled "API key or service account JSON", kind sniffed server-side via
    `gcpauth.LooksLikeServiceAccount` (`admin.go:265`). System/shared credential.
  - `PrivateAPIKeyDialog` → `POST …/connections`, explicit `credential_kind` select.
    Per-principal credential.
- `vertex_ai` is the only registry entry declaring `gcp_service_account`; it declares
  **both** `api_key` and `gcp_service_account`.
- **Bug:** a *custom* `vertex_ai` provider (no `registry_id`) has its api-key
  connection rejected — the fallback switch at `connections.go:242-244` lists
  `openai_compatible, openai, litellm, anthropic, bedrock` and omits `vertex_ai`.
  A registry-configured one is accepted. Same type, different answer by provenance.

### Catalog fidelity

**Vertex** (`googleai.go:642`) returns early for `SurfaceVertex` and never contacts
Google:

```go
if p.surface == SurfaceVertex {
    ...
    return vertexCuratedModels(), nil, nil   // never fetches
}
```

`vertexCuratedModels()` (`googleai.go:731-757`) is 9 literal models — 3 chat, 3 image,
3 video. The code comment gives the rationale ("Vertex exposes no public catalogue for
publisher models"). Consequences: every Vertex instance reports the same 9 regardless
of project access; new models need a code change; a model the project cannot reach
still displays as available and fails at call time. Not documented outside that comment.

**Azure**, probed live against a real resource:

| Route | HTTP | Entries |
|---|---|---|
| `<origin>/openai/v1/models` | 200 | **410** |
| `<origin>/openai/models?api-version=<current>` | 200 | 410 (identical) |
| `<origin>/openai/deployments?api-version=<current>` | 404 | — |
| `<origin>/openai/deployments?api-version=2023-03-15-preview` | 200 | **1** |
| `<origin>/v1/models`, `<origin>/v1/deployments` | 404 | — |

The 410 list is the vendor catalogue — it contains Cohere, Mistral, Llama, Grok, FLUX,
DeepSeek, Stable Diffusion and Claude families no single resource could have deployed.

Discrimination is impossible from that response:

- All 410 carry `status: "succeeded"` — not a discriminator.
- Lifecycle splits 181 deprecated / 37 preview / 167 GA — none near 1.
- The current v1 data-plane spec (`azure-v1-v1-generated.json`, 71 paths) defines
  **no deployments route at all**, only `/models` and `/models/{model}`, and
  `OpenAI.Model` has exactly four fields: `id`, `created`, `object`, `owned_by`.
  The `status` / `lifecycle_status` fields observed are Azure extensions beyond spec.

Provenance: deployment listing was a data-plane route, was moved to the **control
plane**, and the data-plane form survives only under legacy api-versions. The
supported replacement is

```
GET https://management.azure.com/subscriptions/{subscriptionId}/resourceGroups/{rg}
    /providers/Microsoft.CognitiveServices/accounts/{accountName}/deployments?api-version=2024-10-01
```

with `subscriptionId`, `resourceGroupName`, `accountName` all mandatory and
Entra/`DefaultAzureCredential` auth — the resource `api-key` does not work there.

### Production evidence (live deployment, 2026-08-18)

A probe of a live gateway's `GET /v1/models` returned **5,229 rows**. Deduplicated, the
honest catalogue is roughly **470 distinct models plus 321 TTS voices** — an inflation
factor of about **10x**. Breakdown of where it comes from:

| Source | Rows | Note |
|---|---|---|
| Azure accounts (12 separate providers, same type) | 4,698 (89.8%) | 384 of 399 distinct basenames appear in **all twelve** — the same vendor catalogue enumerated twelve times |
| TTS voices | 321 | Legitimate (one row per locale/voice) but pads the count ~6% |
| `vertex_ai` | 11 | **Exactly 9 carry capabilities — the hardcoded list.** 2 are bare aliases |
| Other upstreams | ~197 | |
| Categories | 2 | Pseudo-model rows carrying `failover` instead of capabilities |
| Unprefixed alias rows | 63 | Counted as models |

This is the strongest possible argument for the whole spec, because every defect it
designs against is already live:

1. **A is not hypothetical.** Twelve providers of the same type are configured in
   production — necessarily via config/API, since the console cannot create a second
   instance of a type. Operators are already routing around the UI.
2. **D/E are not hypothetical.** The Azure vendor-catalogue inflation is present at
   scale, multiplied by instance count. 4,614 of those rows carry no capability
   metadata at all — they are bare id strings.
3. **The hardcoded-9 signature is visible in production**, exactly as predicted.

Two further defects surfaced that were not in the original design:

- **`supported_endpoints` is present on only 488 of 5,229 rows (9.3%)** — the other
  4,741 are bare `{id, object, owned_by}`. Any consumer treating the field as reliable
  is wrong most of the time.
- **Two spellings of the same surface.** Some providers advertise `/chat/completions`,
  others `/v1/chat/completions`, while both actually serve on `/v1/...`. Inconsistent
  metadata for one concept — this belongs in C, which is already disambiguating the
  word "endpoint".

### Client-facing model-id asymmetry (gateway's own responses)

Distinct from the Azure-specific issue above, and confirmed live: the gateway strips
the `<provider>/` prefix and echoes the raw upstream id. A request for
`<provider>/<model>` returns `model: "<model>"`; a request for a category returns the
id of whichever failover member served it. **A client that round-trips the response
`model` into its next request gets a 404** unless a bare alias happens to exist.

The 404 body is well-formed and explains the two-namespace routing model, so the
failure is loud rather than silent. But the asymmetry should be a documented part of
the response contract rather than folklore.

### Terminology

"Endpoint" already carries three meanings:

| Sense | Where |
|---|---|
| HTTP surface (`/v1/chat/completions`) | `supported_endpoints` (`models.go:157,194,266`); "Supported endpoints" column; ModelPicker infers modality from these |
| Upstream URL (`base_url`) | `ProviderHub.tsx:78`, `OAuthConnectDialog` ×4 |
| Page label | `navigation.ts:37`, `ModelsEndpoints.tsx:62` — actually catalog + transport snippets |

And `category` already leaks into model-space (`models.go:187-196`): every category is
emitted into `/v1/models` as a pseudo-model row, `owned_by: "category"`, carrying its
own `capabilities` and `supported_endpoints`.

Blast radius of a rename: `go/internal/api` 127, `go/internal/config` 25,
`console/src` 23, `website` 7, `go/internal/providers` 7, `test` 4, `docs` 3;
17 test files; plus the live `categories:` key in `llmgw.config.example.yaml:22` and
the published `POST /admin/api/categories` route.

## Design

### A · Provider instance identity

**Approach: additive `instances[]`.** Stop discarding `matches`; serialize each through
the existing `providerSnapshotRow` helper. Keep every current aggregate field so the
grid card's summary keeps working. No new route, no breaking change.

```jsonc
{ "id": "vertex_ai", "label": "Vertex AI",
  "configured_provider_ids": ["vertex-prod", "vertex-dev"],
  "model_count": 84,
  "instances": [
    {"id": "vertex-prod", "status": "healthy", "model_count": 42, "catalog_state": "fresh", ...},
    {"id": "vertex-dev",  "status": "error",   "model_count": 42, "configuration_issue": "...", ...}
  ]}
```

**No rolled-up tile status.** Instances are separate entities — routing only ever
addresses a unique upstream instance and its models; nothing downstream has a concept
of "the Vertex tile". A single status for a tile would be a fiction that can only
mislead. The card shows **composition** (`2 healthy · 1 error`); each instance carries
its own status. This deletes the `matches[0].status` masking bug rather than patching it.
`configuration_issue` becomes per-instance instead of space-joined.

Console changes:

- Tile expands to an instance list with per-instance status and actions, plus an
  explicit **Add instance**.
- Un-pin the provider-ID field for the add path (`disabled={configured}` applies only
  when editing an existing instance).
- Replace the three `[0]` resolutions with an explicit instance target.
- Fix the `statusByID` merge so instances whose id differs from the registry id stay
  under their tile instead of orphaning into "custom".
- Bind `ProviderDetail`'s existing per-instance table to per-instance data.

### B · Credential upload (multipart)

`POST /admin/api/principals/{id}/connections`, `POST /user/api/connections`, and
`POST /admin/api/providers` learn `multipart/form-data` alongside JSON, mirroring the
audio handlers. The JSON path is unchanged and stays supported.

```
POST /admin/api/principals/{id}/connections
Content-Type: multipart/form-data

  provider_id=vertex-prod
  credential_kind=gcp_service_account
  secret=@service-account.json      <- FormFile
```

so that `curl -F secret=@sa.json …` works without hand-building a JSON body.

Also in B:

- Console uses a file picker for file-shaped credentials; paste remains for API keys.
- Split `ConnectDialog`'s single "API key or service account JSON" textarea into an
  explicit auth-type choice. Kind sniffing stays server-side as a compatibility
  fallback but is no longer how the console decides — you cannot file-pick an API key.
- Fix the custom-vertex api-key rejection (`connections.go:242-244`).
- Depends on A: today the credential target is `configuredProviderIDs(entry)[0]`, so
  "upload this key" has no well-defined destination until a tile knows which instance.

### C · `categories` → `endpoints`

Full rename across config YAML, API routes, console, and docs, with a config migration
and a deprecation window on `POST /admin/api/categories`.

Because "endpoint" is already overloaded, the rename must also disambiguate the other
two senses rather than deepen the collision. Proposed vocabulary:

| Concept | Name |
|---|---|
| Client-facing routing target (was `categories`) | **endpoint** |
| HTTP surface (`/v1/chat/completions`) | **surface** (`supported_surfaces`) |
| Upstream address (`base_url`) | **upstream URL** — prose only, no identifier |

The `owned_by: "category"` pseudo-model rows in `/v1/models` become `owned_by: "endpoint"`,
which is a client-visible change and needs a deprecation note.

### D · Catalog fidelity

**Principle: a catalog reflects what *this instance* can serve, or says plainly that it
cannot be determined.** A number that is not that must never be displayed as if it were.

- **Vertex**: the claim in `googleai.go:727-730` — "Vertex exposes no public catalogue
  for publisher models, so this list is explicit rather than discovered" — was tested
  live and is **wrong as a justification**. A real discovery route exists:

  ```
  GET https://{LOCATION}-aiplatform.googleapis.com/v1beta1/publishers/{PUBLISHER}/models?pageSize=200
      # host is plain aiplatform.googleapis.com when LOCATION == global
      Authorization: Bearer <OAuth token from the SA key, cloud-platform scope>
  ```

  The comment is *half*-true, in two ways that would each mislead someone re-testing it:

  1. **The collection route is `v1beta1` only; the resource route exists on both.**
     Verified:

     | Route | `v1` | `v1beta1` |
     |---|---|---|
     | Collection — `…/publishers/google/models` | **404 (HTML)** | **200** |
     | Resource — `…/publishers/google/models/{id}` | **200** | **200** |

     The distinction is decisive and easy to misread: the `v1` **collection** URL
     returns a *generic Google front-end HTML 404* (`server: ESF`), meaning it never
     reaches a handler at all — whereas `v1beta1` with a deliberately bogus query
     param returns a **JSON `400 INVALID_ARGUMENT`**, proving a real handler validates
     that path. Same HTML 404 on the regional host. So "v1 works" is true of the
     single-model card and false of the listing.

     Consequence: **discovery (`v1beta1`) and inference (`v1`) do need different API
     versions on the same provider.** The single-model card may use either.

     `?listAllVersions=true` is accepted and validated but changed nothing on the
     project tested — identical 23 rows with and without it, every row carrying
     `versionId: "default"`. Do not rely on it to expand anything; treat any extra
     rows it returns as a bonus, not a contract.
  2. **Collection-scoped, not project-path-scoped.** `projects/*/locations/*/publishers/
     google/models` supports action verbs (`:generateContent`) but has no `list` and no
     `get` — both 404. The project is taken from the token, not the path.

  The accurate statement is: *there is no unauthenticated public catalogue, but there
  is an authenticated, region-scoped one.* It is not anonymous — unauthenticated
  returns 401, and attributing the call to an unrelated project returns 403.

  **The inference path needs no change.** Generation uses

  ```
  POST https://aiplatform.googleapis.com/v1/projects/{PROJECT}/locations/global
       /publishers/google/models/{MODEL}:generateContent
  ```

  — `v1`, `global`, plain host with no region prefix, Bearer token from the SA key with
  the project taken from the SA JSON. `modelURL` (`googleai.go:119-142`) already builds
  exactly this, including the `location != "global"` region-prefix rule, and a live
  smoke test against it returned 200. Only the **catalog** path is defective; the
  inference path is correct as written.

  **The catalogue is region-specific, definitively.** Counts of `publishers/google/models`
  across 16 regions ranged from 2 to 128, union 134 unique ids, and the sets are **not
  nested** — the global endpoint carries ids `us-central1` lacks and vice versa. Other
  publishers vary the same way.

  Therefore:

  - Replace `vertexCuratedModels()` with real discovery when the instance holds a
    service-account credential.
  - Keep the curated list **only** as a labelled supplement for families that discovery
    does not return (see below), never as an unlabelled substitute for a real catalog.
  - An instance holding only an API key cannot discover (see credential-kind rule
    below) and must report its catalog as undiscoverable.

#### The curated list is already stale (measured)

Cross-checking `vertexCuratedModels()` against the discovered union of 134 ids:

| Curated id | Exists upstream? |
|---|---|
| `gemini-3.5-flash` | yes |
| `gemini-3.1-pro` | **no** — only `gemini-3.1-pro-preview` exists |
| `gemini-3.1-flash` | **no** — only `-lite` / `-image` / `-tts-preview` variants |
| `gemini-3.1-flash-image` | yes |
| `gemini-3-pro-image` | yes |
| `imagen-4.0-generate-001` | **no** — zero `imagen*` ids in any region |
| `veo-3.1-lite-generate-001` | yes, **us-central1 only** |
| `veo-3.1-generate-001` | yes, **us-central1 only** |
| `veo-3.1-fast-generate-001` | yes, **us-central1 only** |

**Three of nine are not real publisher-model ids anywhere.** Three more exist only in
`us-central1` and are therefore absent from a `global`-configured instance. So on a
global Vertex instance, six of the nine advertised models fail at call time — the exact
failure this spec predicted, now measured rather than reasoned.

The counter-finding matters too: a discovery-only catalogue **drops Imagen entirely**.
If Imagen support is required, discovery needs a labelled curated supplement for that
family — which is why the curated list is retained as a supplement rather than deleted.

#### Per-publisher listing: enumerable, but must NOT drive the routable catalog

Vertex serves many publishers, not just `google`. Two questions were tested: can
publishers be enumerated, and does the API say how to *call* each model.

**Enumeration works, via an undocumented wildcard.** There is no publisher-list method
— `/v1beta1/publishers` returns an HTML 404, and the discovery document confirms no
`aiplatform.publishers.list` method and no `modelGardenSources` resource exist. But:

```
GET /v1beta1/publishers/*/models?pageSize=300     # '*' works; '-' does not
```

returns models across all publishers, paginated. Publisher ids are then derived from
each `name` (`publishers/{pub}/models/{model}`). Caveats: it is **per-region**, and `*`
is **undocumented** — an unversioned dependency that may vanish without notice.

**The scale is the finding:**

| Host | Models | Distinct publishers |
|---|---|---|
| global | 43 | 5 (`google` 23, `anthropic` 11, `mistralai` 4, `xai` 4, +1 internal) |
| us-central1 | **14,594** | **4,730** — of which 4,675 are Hugging Face mirrors (`hf-*`); 55 first-class |

Enumerating publishers into the routable catalog would produce **14,594 rows for one
region of one instance** — catalog inflation ~35× worse than the Azure defect this spec
exists to fix, multiplied again by instance and region.

**The API cannot tell you how to call a model.** This is decisive and was tested hard:

- `supportedActions` is a **console call-to-action** structure, not an RPC list. The
  schema enum is `createApplication, deploy, deployGke, multiDeployVertex,
  openEvaluationPipeline, openFineTuningPipeline(s), openGenerationAiStudio, openGenie,
  openNotebook(s), openPromptTuningPipeline, requestAccess, viewRestApi`.
  **`generateContent`, `rawPredict`, `predict` are not members and can never appear.**
- `openGenerationAiStudio` appears on **both** `google/gemini-2.0-flash-001`
  (`:generateContent`, Gemini body) **and** `anthropic/claude-opus-4-1`
  (`:rawPredict`, Anthropic Messages body). Same marker, incompatible protocols.
- `viewRestApi` — the one key that might describe the API shape — was populated on
  **zero of ~14,600 models examined**.
- `predictSchemata` appears on <0.01% of models and, where present, points at a generic
  schema that does not match how those models are actually invoked.
- `google/gemini-3.7-flash` returns **four fields and no `supportedActions` at all** —
  the flagship carries strictly less protocol information than a random HF mirror.

**Therefore the protocol mapping must be maintained in llmgw** — the listing tells you
a model exists, never how to talk to it. But the mapping is far smaller than
"one adapter per publisher", because Vertex genuinely aggregates. Measured:

| Publisher | `:generateContent` (Gemini body) | `endpoints/openapi/chat/completions` | `:rawPredict` |
|---|---|---|---|
| `google` | **200** | **200** | — |
| `xai` | **200** | **200** | 429 (quota) |
| `anthropic` | **404 — verb not served** | **404** | **429 — route resolves** |
| `mistralai` | 404 | 404 | 404 (not entitled on this project) |

**The Anthropic result is proven, not inferred.** On the *same* host, project and
location, `:rawPredict` resolves the model and fails at the quota check (429) while
`:generateContent` fails at *resolution* (404). Resolution succeeding on one verb and
failing on the other for an identical path means `generateContent` is **not served for
that publisher** — this is not an entitlement problem. Claude on Vertex requires its
native protocol (`:rawPredict` / `:streamRawPredict`, body carrying
`anthropic_version: "vertex-2023-10-16"`).

**The OpenAI-compat surface is the right primary path.**
`POST /v1beta1/projects/{P}/locations/{L}/endpoints/openapi/chat/completions` covers
`google` and `xai` uniformly with one protocol. Model id must be
**`<publisher>/<model>`** — a bare id returns `400 INVALID_ARGUMENT` naming the
requirement. It is a translation layer over the same resolution path as
`:generateContent`, so it unlocks nothing extra — notably **it does not unlock Claude**.

So Vertex support is **one surface plus one adapter**, not N adapters:

1. `endpoints/openapi/chat/completions` with `<publisher>/<model>` — the default path.
2. An Anthropic-on-Vertex adapter using `:rawPredict`. `anthropic.go` already builds
   the Messages body; this needs the Vertex URL, the OAuth token, and `anthropic_version`.

#### Implementation traps (measured, all would ship as bugs)

- **`generateContent` and `rawPredict` draw on different quota buckets.** `xai` returned
  429 on `:rawPredict` and 200 on `:generateContent` in the same breath. **Never treat a
  `rawPredict` 429 as "model unusable"** — it says nothing about the other verb.
- **The OpenAI-compat response is not shape-uniform.** `google/gemini-2.5-flash` under a
  low `max_tokens` returned `choices[0]` with **no `message` key at all** (only
  `finish_reason`, `index`, `logprobs`) because reasoning consumed the budget, and a
  `usage` block with **no `completion_tokens`**. Any code doing
  `choices[0].message.content` **panics/KeyErrors on Gemini**. Must be tolerated.
- **Error envelopes differ by surface.** The OpenAI-compat surface wraps errors in a
  **JSON array** (`[{"error":{…}}]`); the native surfaces return a bare object. Both
  must parse.
- **The translation is not schema-faithful.** On native `:generateContent`, `xai`
  returns `content.role: "assistant"` rather than Gemini's `"model"`.
- **Hidden preamble on xai.** A one-word `"hi"` prompt billed `prompt_tokens: 672`
  (`cached_tokens: 664`) against grok, versus `1` for gemini-2.5-flash. Vertex injects a
  large system preamble for xai models on both surfaces — relevant to cost reporting.
- The project-scoped list route `/projects/{P}/locations/{L}/publishers/{pub}/models`
  is **not served** (HTML 404). Only the collection form `publishers/{pub}/models` lists.

**Design consequence — the catalog is MANAGED MODELS ONLY.**

Of the ~14,600 rows on a large region, llmgw cares about exactly two `supportedActions`
classes:

| Class | us-central1 count | In catalog? |
|---|---|---|
| `openGenerationAiStudio` — managed, callable now | 38 | **yes** |
| `requestAccess` — managed, behind an acceptance flow | 40 | **yes** |
| `deploy` / `multiDeployVertex` / `deployGke` — self-deploy Garden models | 11,841 | **no** |

Self-deploy models require the operator to stand up their own endpoint before anything
can call them; they are not gateway-routable and are out of scope. **This reduces a
region's catalog from ~14,594 to under 100** — the noise problem dissolves at the source
rather than being managed in the UI.

Combined with per-instance scoping (an instance is one project + one region), a
`global` instance shows ~43 and a `us-central1` instance shows ~78. There is no wall of
rows to hide, group, or paginate.

The undocumented `publishers/*` wildcard is therefore **not needed for the routable
path**. Enumerate per-publisher for publishers with a protocol adapter; leave the
wildcard as an optional operator-facing exploration view only if it later earns its keep.

Parameterize `publishers/{publisher}` in `modelURL` (`googleai.go:139` currently
hardcodes `google`) so adding a publisher becomes an adapter registration rather than a
rewrite — but keep the routable set gated on implemented protocols. Do not make the
undocumented `*` wildcard load-bearing for routing.

**Two traps for whoever implements this:**

- **No publisher name ever 404s.** An unknown publisher and a real-but-empty-in-region
  publisher both return `200 {}`. A 200 does not validate a name; a 0 count is a
  *region* fact, not a *name* fact — `meta`, `openai`, `deepseek-ai`, `nvidia`, `ai21`
  and `qodo` all return 0 on `global` yet carry models on `us-central1`.
- **`mistralai` and `mistral-ai` are both real, distinct publishers.** A hand-written
  publisher list will get this wrong; derivation from `name` will not.

**Listed ≠ entitled — and that is explicitly NOT llmgw's problem to solve.**
`supportedActions.requestAccess` marks models behind an acceptance flow but reads
**identically for entitled and unentitled projects**; real entitlement would need
`:fetchPublisherModelConfig` (global-only) or attempting the call.

llmgw does not try to resolve this. **The tool is not an oracle.** Its contract is:
report what the upstream says exists, faithfully. Whether the operator's project has
accepted a given EULA is the operator's concern, and a failed call in the playground
tells them immediately and accurately. Chasing entitlement would mean per-model probes,
POST-based EULA checks, and a permanently stale cache — for a question the upstream
answers definitively the moment a real call is made.

This bounds D's "declare unknown" state precisely: it applies when llmgw **cannot
fetch a catalog at all** (e.g. Vertex holding only an API key), not when it fetched a
catalog whose entries might individually require acceptance.

#### Google surface map (measured — do not re-derive this from one credential)

Google serves generative models through **three** distinct front doors, and llmgw
already models all three as separate registry entries. Conflating them produces wrong
conclusions; this table was established with two different credentials and is the
reference:

| Surface | Registry id | Base URL | List models | Run inference | Auth |
|---|---|---|---|---|---|
| AI Studio, native | `ai_studio` | `generativelanguage.googleapis.com/v1beta` | **yes** (50) | **yes** | `x-goog-api-key` or `?key=` |
| AI Studio, OpenAI-compat | `gemini` | `…/v1beta/openai` | **yes** (51) | **yes** | **`Authorization: Bearer` only** — `x-goog-api-key` returns 404 here |
| Vertex AI | `vertex_ai` | `{loc}-aiplatform.googleapis.com` | **no** with an API key | yes (Express Mode) | discovery requires OAuth2 |

Notes that matter for implementation:

- `/v1beta/models` returns 50; `/v1/models` returns 15. **`v1` is a stable subset, not
  a truncated page** — `pageSize=1000` returns the same counts with no `nextPageToken`.
  llmgw's `ai_studio` base URL correctly targets `v1beta`.
- The OpenAI-compat layer carries **`models/`-prefixed ids** (`"id": "models/gemini-2.5-flash"`),
  which is not the usual OpenAI convention. It accepts bare ids on input regardless.
  `modelURL` already strips the prefix.
- The two AI Studio lists differ by exactly one entry.

**Distinguishing the three rejection reasons is the whole game.** They mean different
things and must not be collapsed:

| Reason | Meaning | Actionable? |
|---|---|---|
| `CREDENTIALS_MISSING` | **The surface refuses API keys by design.** Observed only on Vertex `ListPublisherModels`, identically under both auth styles, on global and regional hosts. No API key of any kind passes. | No — requires OAuth |
| `SERVICE_DISABLED` | Credential accepted and resolved to a project; the **API is not enabled on that project**. Auth is fine. | Yes — enable the API |
| `API_KEY_SERVICE_BLOCKED` | **That specific key's own restrictions** exclude the service. Says nothing about the surface. | Yes — widen key restrictions |

An earlier probe in this design saw `API_KEY_SERVICE_BLOCKED` from a restricted
Vertex-scoped key and wrongly concluded that AI Studio refuses API keys. It does not.
A second probe with an unrestricted AI Studio key returned 200 on every AI Studio and
OpenAI-compat route. **Only `CREDENTIALS_MISSING` is a statement about a surface.**

Out of scope but recorded: `POST /v1beta/interactions` is a real, working AI Studio
surface with a Responses-API shape (flat `model` + `input`; a `steps[]` array carrying
`thought` steps with opaque signatures, rather than `candidates[]`). llmgw does not
support it today.

#### Discoverability depends on credential kind

Measured, not assumed:

| Credential | Vertex inference | Vertex discovery |
|---|---|---|
| Service-account key (OAuth token) | yes | **yes** — the route above |
| API key alone | yes (Express Mode) | **never** — Google returns `CREDENTIALS_MISSING`: *"API keys are not supported by this API. Expected OAuth2 access token…"* |

This couples B and D: the credential kind an instance holds determines what its catalog
can honestly report. A Vertex instance with an API key must display *undiscoverable*;
the same instance with an SA key must display a real, region-scoped list. Because the
catalogue is region-specific, **two Vertex instances in different regions legitimately
have different model lists** — which the current single-hardcoded-list design cannot
express at all, and which A's per-instance catalog makes representable.
- **Azure**: see below.
- **Console**: an instance whose catalog is undiscoverable displays that state instead
  of a model count.

### E · `azure_openai` provider type

A new registry entry, deliberately **separate from the `openai_compatible` umbrella**.
Configuration is full URL + key + model (deployment) name — no project, no Azure
identity, no subscription scope, no api-version knob.

#### Verified inference contract (live probe, 2026-08-18)

```
POST <endpoint>/chat/completions          # endpoint already carries /openai/v1
Headers: api-key: <key>                   # NOT Authorization: Bearer
         Content-Type: application/json
Query:   (none — no api-version)
Body:    {"model": "<deployment-name>", "messages": [...], "max_completion_tokens": N}
```

HTTP 200, `finish_reason: stop`, standard OpenAI-shaped response. `max_completion_tokens`
accepted as-is. **One credential covers both listing and inference** — the same `api-key`
header returns 200 on `<endpoint>/models`.

#### Why it cannot ride the `openai_compatible` umbrella

Four measured divergences, not stylistic ones:

1. **Auth header.** `api-key: <key>`, not `Authorization: Bearer <key>`.
2. **Catalog route.** Deployments, not `/models` — `/models` returns the 410-entry
   vendor catalogue (see above).
3. **Extra response fields.** Responses carry `prompt_filter_results` and
   `service_tier` at the top level, and an Azure-only `latency_checkpoint` object
   inside `usage`. **A strict/exhaustive deserializer breaks here** — unknown fields
   must be tolerated at both levels.
4. **Model id asymmetry.** The request `model` is the deployment name
   (`gpt-5.6-sol`); the response `model` is the resolved version-stamped id
   (`gpt-5.6-sol-2026-07-09`). **These are different identifiers — never round-trip
   the response `model` back as a request `model`,** and never treat it as a catalog
   key. Any code that echoes a response model id into routing or catalog state will
   silently mismatch.

   Checked against the current codebase: the obvious sites do not round-trip it —
   `logging.go:177-181` reads `model` from the **request** body, and `googleai.go:325`
   / `ollama.go:171` construct responses from the requested id. So this is a
   **forward-looking constraint on the new provider**, not a latent bug being fixed.
   It must be asserted in tests, because nothing in the current design prevents a
   future contributor from introducing it.

Note also that `AZURE_OPENAI_API_VERSION` and `AZURE_OPENAI_RESOURCE_NAME` are unused
by this contract; the endpoint URL is self-sufficient. The api-version in the operator's
env is stale relative to the management route and is only relevant to the legacy
deployments listing.

Catalog resolution, in order:

1. `GET <origin>/openai/deployments?api-version=2023-03-15-preview` with header
   `api-key`. Probe-verified: returns the true deployment list.
2. Otherwise, an operator-declared `deployments:` list in provider config.
3. Otherwise, report **catalog not discoverable**.

**Never fall back to `/openai/v1/models`.** That is the 410-entry trap, and because
every entry reads `status: "succeeded"` the wrong answer looks like a valid one.

The legacy pin is chosen with eyes open: the current v1 spec defines no deployments
route, and the supported alternative requires Entra plus subscription/resource-group
scope. The legacy data-plane route is the only key-auth way to get a truthful answer.
Step 3 exists precisely because that route may eventually be withdrawn — and when it
is, the failure mode is an honest "unknown", not a silent 410.

### F · Setup snippets show `localhost` on a remote console (minor)

`ModelsEndpoints.tsx:53,55,57` hardcode `http://localhost:8787` into the three
copy-paste setup snippets:

```ts
{ title: "OpenAI-compatible clients", value: `export OPENAI_BASE_URL=http://localhost:8787/v1
{ title: "Anthropic gateway clients", value: `export ANTHROPIC_BASE_URL=http://localhost:8787
{ title: "Codex and Responses clients", value: `export OPENAI_BASE_URL=http://localhost:8787/v1
```

This is not a DNS-resolution failure — nothing is resolved; the string is a literal.
An operator viewing the console on a deployed host copies a snippet that points at
their own machine. The console is served by the gateway, so `window.location.origin`
is already the correct base URL. Fold into C, which touches this file anyway.

## Testing

- Unit: instance aggregation emits per-instance rows; no rollup status; per-instance
  `configuration_issue`.
- Unit: multipart and JSON paths produce identical stored credentials; a mangled
  payload still yields `gcpauth.Parse`'s specific error.
- Regression: custom `vertex_ai` accepts an api-key connection.
- Unit: Azure catalog resolution order, including that a 404 on deployments never
  falls through to `/models`.
- Migration: a config using `categories:` loads and round-trips after the rename.
- Integration: full automated suite after visual verification of a locally running
  instance, per the agreed workflow.

## Not open questions (resolved during design)

- `AZURE_OPENAI_RESOURCE_NAME` differing from the endpoint host is **expected**. It is
  the operator's own inventory label for numbered Azure endpoints (`azure-01`, …), not
  a URL component. Nothing derives a URL from it. No action.
- An Azure OpenAI resource needs **endpoint URL + key + model (deployment) name** and
  nothing else. The endpoint value is already complete, including its path prefix.
  The deployment name is obtained from the deployments route, which is verified
  working (1 entry, against 410 from `/models`). This is the design's primary path,
  not a workaround.

## Open questions

None blocking. The one that was open — whether to retain the Vertex curated list as a
labelled supplement for Imagen — is **resolved as: delete it.** Zero `imagen*` ids exist
under `publishers/google/models` in any of the 16 regions probed, so the curated entry
advertises a model this project cannot call through this path. Keeping it as a
"supplement" would be keeping a guess, which is exactly what this work exists to remove.
If Imagen is later needed, it should be added only once a route that actually serves it
is verified.
