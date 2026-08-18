import { useMemo, useState } from "preact/hooks";
import { Search } from "lucide-preact";
import type { JSONRecord } from "../lib/api";
import { asList, asRecord, stringValue } from "../lib/records";

// A catalog row reduced to what every picker and mode-switch needs.
export type CatalogModel = {
  id: string;
  provider: string;
  label: string;
  capabilities: string[];
  endpoints: string[];
  isCategory: boolean;
};

// Capability names the console reasons about. A model may carry several; the
// list is ordered so the most specific modality wins when picking a default.
export const capabilityOrder = ["video", "image", "tts", "transcription", "vision", "chat"] as const;
export type Capability = (typeof capabilityOrder)[number];

export const capabilityLabels: Record<Capability, string> = {
  chat: "Chat",
  image: "Image generation",
  video: "Video generation",
  tts: "Text to speech",
  transcription: "Transcription",
  vision: "Vision",
};

// capabilitiesFor derives a model's modalities from catalog metadata, falling
// back to chat only when the catalog declares nothing that says otherwise.
export function capabilitiesFor(row: JSONRecord): string[] {
  const declared = Object.entries(asRecord(row.capabilities))
    .filter(([, value]) => value !== false)
    .map(([key]) => key.toLowerCase());
  const endpoints = asList(row.supported_endpoints).map((value) => String(value).toLowerCase());
  const out = new Set<string>();
  for (const name of declared) {
    if (name === "tts" || name === "speech") out.add("tts");
    else if (name === "transcription" || name === "stt" || name === "asr") out.add("transcription");
    else if (name === "video") out.add("video");
    else if (name === "image") out.add("image");
    else if (name === "vision" || name === "multimodal") out.add("vision");
    else if (name === "chat" || name === "completion" || name === "tools") out.add("chat");
  }
  for (const endpoint of endpoints) {
    if (endpoint.includes("/audio/speech")) out.add("tts");
    if (endpoint.includes("/audio/transcriptions")) out.add("transcription");
    if (endpoint.includes("/images/generations")) out.add("image");
    if (endpoint.includes("/videos/generations")) out.add("video");
    if (endpoint.includes("/chat/completions") || endpoint.includes("/messages") || endpoint.includes("/responses")) out.add("chat");
  }
  // A row that declares no modality at all is a chat model by convention.
  if (!out.size) out.add("chat");
  // Generation-only rows (speech, transcription, image, video) must not
  // masquerade as chat models.
  if ((out.has("tts") || out.has("transcription") || out.has("image") || out.has("video")) && !declared.includes("chat") &&
      !endpoints.some((endpoint) => endpoint.includes("/chat/completions") || endpoint.includes("/messages") || endpoint.includes("/responses"))) {
    out.delete("chat");
  }
  return [...out];
}

export function catalogModels(payload: JSONRecord | null): CatalogModel[] {
  return asList(payload?.data).map(asRecord).map((row) => {
    const id = stringValue(row.id);
    const owner = stringValue(row.owned_by);
    return {
      id,
      provider: owner || "gateway",
      label: stringValue(row.display_name, id),
      capabilities: capabilitiesFor(row),
      endpoints: asList(row.supported_endpoints).map(String),
      isCategory: owner === "category",
    };
  }).filter((row) => row.id);
}

export type ModelFilterState = { provider: string; capability: string; search: string };

export const emptyModelFilter: ModelFilterState = { provider: "all", capability: "all", search: "" };

export function filterModels(models: CatalogModel[], filter: ModelFilterState): CatalogModel[] {
  const search = filter.search.trim().toLowerCase();
  return models.filter((model) => {
    if (filter.provider !== "all" && model.provider !== filter.provider) return false;
    if (filter.capability !== "all" && !model.capabilities.includes(filter.capability)) return false;
    if (search && !`${model.id} ${model.label}`.toLowerCase().includes(search)) return false;
    return true;
  });
}

// ModelFilters renders provider -> capability -> search, in that order, so a
// catalog of hundreds of rows narrows before the model list is ever read.
export function ModelFilters({ models, filter, onChange, includeCategories = true }: {
  models: CatalogModel[];
  filter: ModelFilterState;
  onChange: (filter: ModelFilterState) => void;
  includeCategories?: boolean;
}) {
  const providers = useMemo(() => {
    const names = new Set<string>();
    for (const model of models) {
      if (!includeCategories && model.isCategory) continue;
      names.add(model.provider);
    }
    return [...names].sort();
  }, [models, includeCategories]);
  const capabilities = useMemo(() => {
    const names = new Set<string>();
    for (const model of models) {
      if (filter.provider !== "all" && model.provider !== filter.provider) continue;
      for (const capability of model.capabilities) names.add(capability);
    }
    return capabilityOrder.filter((capability) => names.has(capability));
  }, [models, filter.provider]);

  const matches = filterModels(models, filter).length;
  return (
    <div class="model-filters">
      <label>Provider<select value={filter.provider} onInput={(event) => onChange({ ...filter, provider: (event.currentTarget as HTMLSelectElement).value, capability: "all" })}>
        <option value="all">All providers</option>
        {providers.map((provider) => <option value={provider} key={provider}>{provider}</option>)}
      </select></label>
      <label>Capability<select value={filter.capability} onInput={(event) => onChange({ ...filter, capability: (event.currentTarget as HTMLSelectElement).value })}>
        <option value="all">All capabilities</option>
        {capabilities.map((capability) => <option value={capability} key={capability}>{capabilityLabels[capability]}</option>)}
      </select></label>
      <label class="search-field"><Search size={17} /><span class="sr-only">Search models</span>
        <input value={filter.search} onInput={(event) => onChange({ ...filter, search: (event.currentTarget as HTMLInputElement).value })} placeholder="Filter model id or label" />
      </label>
      <span class="model-filters__count">{matches} of {models.length}</span>
    </div>
  );
}

// ModelCombo is a type-ahead over the narrowed list. Nobody memorises model
// ids, and a select of hundreds is unusable, so the input filters as you type
// and the list is bounded.
export function ModelCombo({ models, filter, value, onChange, label = "Model", limit = 12 }: {
  models: CatalogModel[];
  filter: ModelFilterState;
  value: string;
  onChange: (modelID: string) => void;
  label?: string;
  limit?: number;
}) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const pool = filterModels(models, filter);
  const needle = query.trim().toLowerCase();
  const suggestions = (needle
    ? pool.filter((model) => `${model.id} ${model.label}`.toLowerCase().includes(needle))
    : pool).slice(0, limit);
  const commit = (modelID: string) => { onChange(modelID); setQuery(""); setOpen(false); setActive(0); };

  return (
    <div class="model-combo">
      <label>{label}
        <input
          value={open ? query : value}
          placeholder={pool.length ? "Type to search models…" : "No model matches these filters"}
          disabled={!pool.length}
          onFocus={() => { setOpen(true); setQuery(""); }}
          onBlur={() => window.setTimeout(() => setOpen(false), 140)}
          onInput={(event) => { setQuery((event.currentTarget as HTMLInputElement).value); setOpen(true); setActive(0); }}
          onKeyDown={(event) => {
            if (event.key === "ArrowDown") { event.preventDefault(); setActive((index) => Math.min(index + 1, suggestions.length - 1)); }
            else if (event.key === "ArrowUp") { event.preventDefault(); setActive((index) => Math.max(index - 1, 0)); }
            else if (event.key === "Enter" && open && suggestions[active]) { event.preventDefault(); commit(suggestions[active].id); }
            else if (event.key === "Escape") { setOpen(false); }
          }}
        />
      </label>
      {open && suggestions.length ? <ul class="model-combo__list" role="listbox">
        {suggestions.map((model, index) => <li key={model.id}>
          <button class={index === active ? "is-active" : ""} type="button" role="option" aria-selected={index === active}
            onMouseDown={(event) => { event.preventDefault(); commit(model.id); }}>
            <strong class="technical">{model.id}</strong>
            {model.label !== model.id ? <small>{model.label}</small> : null}
          </button>
        </li>)}
        {pool.length > suggestions.length ? <li class="model-combo__more">{pool.length - suggestions.length} more — keep typing to narrow</li> : null}
      </ul> : null}
    </div>
  );
}

// ModelSelect is the narrowed model list itself, driven by the same filter.
export function ModelSelect({ models, filter, value, onChange, label = "Model", emptyHint = "No model matches these filters." }: {
  models: CatalogModel[];
  filter: ModelFilterState;
  value: string;
  onChange: (modelID: string) => void;
  label?: string;
  emptyHint?: string;
}) {
  const visible = filterModels(models, filter);
  if (!visible.length) {
    return <label>{label}<select disabled><option>{emptyHint}</option></select></label>;
  }
  return <label>{label}<select value={value} onInput={(event) => onChange((event.currentTarget as HTMLSelectElement).value)}>
    {!visible.some((model) => model.id === value) ? <option value="">Select a model</option> : null}
    {visible.map((model) => <option value={model.id} key={model.id}>{model.id}{model.label !== model.id ? ` — ${model.label}` : ""}</option>)}
  </select></label>;
}

export function useModelFilter(initial: Partial<ModelFilterState> = {}) {
  return useState<ModelFilterState>({ ...emptyModelFilter, ...initial });
}

// Pager renders a bounded window over a long list. Catalogs run to hundreds of
// rows (Edge TTS alone ships 321 voices), so lists are paged rather than dumped.
export function Pager({ total, page, pageSize, onPage }: {
  total: number; page: number; pageSize: number; onPage: (page: number) => void;
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(page, pages - 1);
  if (total <= pageSize) return null;
  const first = current * pageSize + 1;
  const last = Math.min(total, (current + 1) * pageSize);
  return (
    <nav class="pager" aria-label="Pagination">
      <span>{first}–{last} of {total}</span>
      <div class="pager__controls">
        <button class="button button--secondary" type="button" disabled={current === 0} onClick={() => onPage(0)}>First</button>
        <button class="button button--secondary" type="button" disabled={current === 0} onClick={() => onPage(current - 1)}>Previous</button>
        <span class="pager__position">Page {current + 1} of {pages}</span>
        <button class="button button--secondary" type="button" disabled={current >= pages - 1} onClick={() => onPage(current + 1)}>Next</button>
        <button class="button button--secondary" type="button" disabled={current >= pages - 1} onClick={() => onPage(pages - 1)}>Last</button>
      </div>
    </nav>
  );
}

export const defaultPageSize = 20;
