import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Check, Copy, Terminal } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";
import { ModelFilters, catalogModels, filterModels, useModelFilter } from "../components/ModelPicker";
import { SearchSelect, dataTable, useTableView, type TableColumn } from "../components/DataTable";

function CopySnippet({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    await navigator.clipboard?.writeText(value);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
  };
  return <button class="icon-button" type="button" aria-label="Copy setup snippet" onClick={copy}>{copied ? <Check size={16} /> : <Copy size={16} />}</button>;
}

function valueList(value: unknown): string { return asList(value).map(String).join(" · "); }
function capabilityList(value: unknown): string { return Object.keys(asRecord(value)).join(" · "); }

// clientSnippets follows the client profiles in docs/CLIENTS.md. Clients use a
// gateway-issued project key: the administrator keys (LLMGW_API_KEY and
// LLMGW_API_KEYS) skip key and project policy, so no snippet may name them.
export function clientSnippets(origin: string): { title: string; value: string }[] {
  return [
    { title: "OpenAI SDKs", value: `export OPENAI_BASE_URL=${origin}/v1
export OPENAI_API_KEY='<GATEWAY_PROJECT_KEY>'` },
    { title: "Claude Code", value: `export ANTHROPIC_BASE_URL=${origin}
export ANTHROPIC_API_KEY='<GATEWAY_PROJECT_KEY>'` },
    { title: "Codex", value: `# ~/.codex/config.toml
# Codex reads the key from LLMGW_PROJECT_KEY:
#   export LLMGW_PROJECT_KEY='<GATEWAY_PROJECT_KEY>'
model_provider = "llmgw"
[model_providers.llmgw]
base_url = "${origin}/v1"
env_key = "LLMGW_PROJECT_KEY"
wire_api = "responses"
requires_openai_auth = false` },
    { title: "Copilot CLI BYOK", value: `export COPILOT_PROVIDER_BASE_URL=${origin}/v1
export COPILOT_PROVIDER_API_KEY='<GATEWAY_PROJECT_KEY>'
export COPILOT_PROVIDER_WIRE_API=completions
export COPILOT_PROVIDER_WIRE_MODEL=PROVIDER_OR_ENDPOINT/MODEL` },
  ];
}

export function ModelsEndpoints({ data, mode, principalID, onPrincipalIDChange }: { data: JSONRecord; mode: ConsoleMode; principalID: string; onPrincipalIDChange: (principalID: string) => void }) {
  const humans = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  useEffect(() => {
    if (mode === "admin" && !principalID && humans.length) {
      onPrincipalIDChange(stringValue(humans[0].id));
    }
  }, [mode, principalID, humans]);
  const [catalog, setCatalog] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useModelFilter();
  const catalogRequest = useRef(0);
  const catalogPath = mode === "admin" ? (principalID ? `/models?principal_id=${encodeURIComponent(principalID)}&diagnostics=1` : "") : "/models";
  const load = async () => {
    const request = ++catalogRequest.current;
    if (!catalogPath) {
      setError("");
      return;
    }
    setError("");
    try {
      const payload = await getJSON<JSONRecord>(mode, catalogPath);
      if (request === catalogRequest.current) setCatalog(payload);
    } catch (cause) {
      if (request === catalogRequest.current) setError(cause instanceof Error ? cause.message : "Model catalog could not load.");
    }
  };
  useEffect(() => {
    setCatalog(null);
    setFilter((current) => ({ ...current, provider: "all", capability: "all" }));
    void load();
  }, [mode, principalID]);
  const rows = asList(catalog?.data).map(asRecord);
  const models = useMemo(() => catalogModels(catalog), [catalog]);
  const visibleIDs = useMemo(() => new Set(filterModels(models, filter).map((model) => model.id)), [models, filter]);
  const filtered = useMemo(() => rows.filter((row) => visibleIDs.has(stringValue(row.id))), [rows, visibleIDs]);
  const modelColumns: TableColumn<JSONRecord>[] = [
    { id: "model", header: "Model", sortValue: (row) => stringValue(row.id), cell: (row) => <><strong class="technical">{stringValue(row.id)}</strong>{stringValue(row.display_name) ? <small class="table-subtitle">{stringValue(row.display_name)}</small> : null}</> },
    { id: "provider", header: "Provider", sortValue: (row) => stringValue(row.owned_by), cell: (row) => stringValue(row.owned_by) },
    {
      id: "publication", header: "Publication", sortValue: (row) => stringValue(row.publication_state, "configured"), cell: (row) => {
        const state = stringValue(row.publication_state);
        return state ? <><span class={`status-pill ${state === "verified" ? "status-pill--success" : "status-pill--warning"}`}>{state}</span>{state !== "verified" ? <small class="table-subtitle">Disabled for public routing{stringValue(row.failure_code) ? ` · ${stringValue(row.failure_code)}` : ""}</small> : null}</> : "Configured";
      },
    },
    { id: "capabilities", header: "Capabilities", cell: (row) => capabilityList(row.capabilities) || "Not supplied" },
    { id: "surfaces", header: "Supported surfaces", class: "technical", cell: (row) => valueList(row.supported_surfaces ?? row.supported_endpoints) || "Catalog did not declare" },
  ];
  const modelView = useTableView(filtered, modelColumns);
  // The console is served BY the gateway, so its own origin is always the
  // correct base URL — a hardcoded localhost fails silently on any deployed host.
  const snippets = clientSnippets(window.location.origin);
  return (
    <div class="page-stack">
      <PageHeading eyebrow="Catalog and transport" title="Models & endpoints" detail="Catalog rows come from configured upstreams. Setup snippets use placeholders only: replace them with a project key minted under API keys, never the administrator key." />
      <section class="endpoint-grid">
        {snippets.map((snippet) => <article class="endpoint-card" key={snippet.title}><header><Terminal size={18} /><h2>{snippet.title}</h2><CopySnippet value={snippet.value} /></header><pre class="technical">{snippet.value}</pre></article>)}
      </section>
      <section class="surface endpoint-capabilities"><div class="section-heading"><div><p class="eyebrow">Gateway surfaces</p><h2>Documented endpoint capabilities</h2></div><span class="status-pill status-pill--muted">Gateway documented</span></div><div class="capability-list"><div><strong>OpenAI core</strong><span class="technical">/v1/models · /v1/chat/completions · /v1/responses · /v1/embeddings</span></div><div><strong>Anthropic core</strong><span class="technical">/v1/messages · /v1/messages/count_tokens</span></div><div><strong>Media</strong><span class="technical">/v1/audio/transcriptions · /v1/audio/speech · /v1/images/generations · /v1/videos/generations</span></div><div><strong>Codex</strong><span>Official owner-private connection routes through the gateway Responses surface.</span></div></div></section>
      <section class="surface"><div class="section-heading"><div><p class="eyebrow">Real catalog</p><h2>Available models</h2></div><button class="button button--secondary" type="button" onClick={() => void load()}>Refresh list</button></div><div class="model-toolbar">{mode === "admin" ? <SearchSelect label="Catalog owner" noun="owners" value={principalID} options={[{ value: "", label: "Select a human owner" }, ...humans.map((principal) => ({ value: stringValue(principal.id), label: stringValue(principal.display_name, stringValue(principal.id)) }))]} onChange={onPrincipalIDChange} /> : null}</div><ModelFilters models={models} filter={filter} onChange={setFilter} />{error ? <ErrorState title="Model catalog is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load()}>Retry</button>} /> : catalog === null ? <LoadingState title="Loading configured model catalogs" /> : filtered.length === 0 ? <EmptyState title="No models match this filter" detail="Sync a provider catalog, or widen the provider and capability filters." /> : dataTable(modelView, modelColumns, { label: "Available models", rowKey: (row) => stringValue(row.id) })}</section>
    </div>
  );
}
