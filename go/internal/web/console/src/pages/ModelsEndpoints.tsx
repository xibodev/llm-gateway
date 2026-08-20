import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Check, Copy, Terminal } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";
import { ModelFilters, catalogModels, filterModels, useModelFilter } from "../components/ModelPicker";

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

export function ModelsEndpoints({ data, mode, principalID, onPrincipalIDChange }: { data: JSONRecord; mode: ConsoleMode; principalID: string; onPrincipalIDChange: (principalID: string) => void }) {
  const humans = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const [catalog, setCatalog] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useModelFilter();
  const catalogRequest = useRef(0);
  const catalogPath = mode === "admin" ? (principalID ? `/models?principal_id=${encodeURIComponent(principalID)}` : "") : "/models";
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
  // The console is served BY the gateway, so its own origin is always the
  // correct base URL — a hardcoded localhost fails silently on any deployed host.
  const gatewayOrigin = window.location.origin;
  const snippets = [
    { title: "OpenAI-compatible clients", value: `export OPENAI_BASE_URL=${gatewayOrigin}/v1
export OPENAI_API_KEY=YOUR_GATEWAY_KEY` },
    { title: "Anthropic gateway clients", value: `export ANTHROPIC_BASE_URL=${gatewayOrigin}
export ANTHROPIC_API_KEY=YOUR_GATEWAY_KEY` },
    { title: "Codex and Responses clients", value: `export OPENAI_BASE_URL=${gatewayOrigin}/v1
export OPENAI_API_KEY=YOUR_GATEWAY_KEY` },
  ];
  return (
    <div class="page-stack">
      <PageHeading eyebrow="Catalog and transport" title="Models & endpoints" detail="Catalog rows come from configured upstreams. Gateway setup snippets use placeholders only and never include a real secret." />
      <section class="endpoint-grid">
        {snippets.map((snippet) => <article class="endpoint-card" key={snippet.title}><header><Terminal size={18} /><h2>{snippet.title}</h2><CopySnippet value={snippet.value} /></header><pre class="technical">{snippet.value}</pre></article>)}
      </section>
      <section class="surface endpoint-capabilities"><div class="section-heading"><div><p class="eyebrow">Gateway surfaces</p><h2>Documented endpoint capabilities</h2></div><span class="status-pill status-pill--muted">Gateway documented</span></div><div class="capability-list"><div><strong>OpenAI</strong><span class="technical">/v1/chat/completions · /v1/responses · /v1/models</span></div><div><strong>Anthropic</strong><span class="technical">/v1/messages · /v1/messages/count_tokens</span></div><div><strong>Codex</strong><span>Official owner-private connection routes through the gateway Responses surface.</span></div></div></section>
      <section class="surface"><div class="section-heading"><div><p class="eyebrow">Real catalog</p><h2>Available models</h2></div><button class="button button--secondary" type="button" onClick={() => void load()}>Refresh list</button></div><div class="model-toolbar">{mode === "admin" ? <label>Catalog owner<select value={principalID} onInput={(event) => onPrincipalIDChange((event.currentTarget as HTMLSelectElement).value)}><option value="">Select a human owner</option>{humans.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))}</option>)}</select></label> : null}</div><ModelFilters models={models} filter={filter} onChange={setFilter} />{error ? <ErrorState title="Model catalog is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load()}>Retry</button>} /> : catalog === null ? <LoadingState title="Loading configured model catalogs" /> : filtered.length === 0 ? <EmptyState title="No models match this filter" detail="Sync a provider catalog, or widen the provider and capability filters." /> : <div class="table-wrap"><table><thead><tr><th>Model</th><th>Provider</th><th>Capabilities</th><th>Supported surfaces</th></tr></thead><tbody>{filtered.map((row) => <tr key={stringValue(row.id)}><td><strong class="technical">{stringValue(row.id)}</strong>{stringValue(row.display_name) ? <small class="table-subtitle">{stringValue(row.display_name)}</small> : null}</td><td>{stringValue(row.owned_by)}</td><td>{capabilityList(row.capabilities) || "Not supplied"}</td><td class="technical">{valueList(row.supported_surfaces ?? row.supported_endpoints) || "Catalog did not declare"}</td></tr>)}</tbody></table></div>}</section>
    </div>
  );
}
