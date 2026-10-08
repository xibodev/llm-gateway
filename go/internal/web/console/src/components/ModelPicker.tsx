import { useEffect, useId, useMemo, useState } from "preact/hooks";
import { Search } from "lucide-preact";
import type { JSONRecord } from "../lib/api";
import { asList, asRecord, stringValue } from "../lib/records";

// A catalog row reduced to what every picker and mode-switch needs.
export type CatalogModel = {
  id: string;
  provider: string;
  label: string;
  capabilities: string[];
  surfaces: string[];
  free: boolean;
  isCategory: boolean;
  nativeSurfaces: string[];
  emulatedSurfaces: string[];
  tools: string;
  streaming: string;
  statefulResponses: string;
  discoveredAt: string;
  verifiedAt: string;
  publicationState: string;
  published: boolean;
  disabled: boolean;
  failureCode: string;
};

// Capability names the console reasons about. A model may carry several; the
// list is ordered so the most specific modality wins when picking a default.
export const capabilityOrder = ["video", "image", "tts", "transcription", "embedding", "vision", "chat"] as const;
export type Capability = (typeof capabilityOrder)[number];

export const capabilityLabels: Record<Capability, string> = {
  chat: "Chat",
  image: "Image generation",
  video: "Video generation",
  tts: "Text to speech",
  transcription: "Transcription",
  embedding: "Embeddings",
  vision: "Vision",
};

export function modelOptionLabel(id: string, displayName: string): string {
  const name = displayName.replace(/\s+/g, " ").trim();
  return name && name !== id && name.length <= 64 ? `${id} — ${name}` : id;
}

// capabilitiesFor derives only modalities the catalog affirmatively declares.
// Missing metadata is unknown, not evidence that a chat or media control works.
export function capabilitiesFor(row: JSONRecord): string[] {
  const declared = Object.entries(asRecord(row.capabilities))
    .filter(([, value]) => value !== false)
    .map(([key]) => key.toLowerCase());
  const typed = asRecord(row.typed_capabilities);
  const operations = asRecord(typed.operations);
  const typedSurfaces = asRecord(typed.surfaces);
  const inputs = asRecord(typed.inputs);
  // supported_surfaces is canonical; supported_endpoints is the pre-rename
  // key, kept as a fallback so this still works against a server that has
  // not been updated yet.
  const surfaces = asList(row.supported_surfaces ?? row.supported_endpoints).map((value) => String(value).toLowerCase());
  const out = new Set<string>();
  if (operations.chat === "supported" || typedSurfaces.chat_completions === "supported" || typedSurfaces.responses === "supported" || typedSurfaces.messages === "supported") out.add("chat");
  if (operations.image === "supported") out.add("image");
  if (operations.video === "supported") out.add("video");
  if (operations.audio_in === "supported") out.add("transcription");
  if (operations.audio_out === "supported") out.add("tts");
  if (operations.embeddings === "supported") out.add("embedding");
  if (inputs.image === "supported") out.add("vision");
  for (const name of declared) {
    if (name === "tts" || name === "speech") out.add("tts");
    else if (name === "transcription" || name === "stt" || name === "asr") out.add("transcription");
    else if (name === "video") out.add("video");
    else if (name === "image") out.add("image");
    else if (name === "embedding" || name === "embeddings") out.add("embedding");
    else if (name === "vision" || name === "multimodal") out.add("vision");
    else if (name === "chat" || name === "completion" || name === "tools") out.add("chat");
  }
  for (const surface of surfaces) {
    if (surface.includes("/audio/speech")) out.add("tts");
    if (surface.includes("/audio/transcriptions")) out.add("transcription");
    if (surface.includes("/images/generations")) out.add("image");
    if (surface.includes("/videos/generations")) out.add("video");
    if (surface.includes("/embeddings")) out.add("embedding");
    if (surface.includes("/chat/completions") || surface.includes("/messages") || surface.includes("/responses")) out.add("chat");
  }
  // Generation-only rows (speech, transcription, image, video) must not
  // masquerade as chat models.
  if ((out.has("tts") || out.has("transcription") || out.has("embedding") || out.has("image") || out.has("video")) && !declared.includes("chat") &&
      !surfaces.some((surface) => surface.includes("/chat/completions") || surface.includes("/messages") || surface.includes("/responses"))) {
    out.delete("chat");
  }
  return [...out];
}

export function catalogModels(payload: JSONRecord | null): CatalogModel[] {
  return asList(payload?.data).map(asRecord).map((row) => {
    const id = stringValue(row.id);
    const owner = stringValue(row.owned_by);
    const typed = asRecord(row.typed_capabilities);
    const freshness = asRecord(typed.freshness);
    return {
      id,
      provider: owner || "gateway",
      label: stringValue(row.display_name, id),
      capabilities: capabilitiesFor(row),
      surfaces: asList(row.supported_surfaces ?? row.supported_endpoints).map(String),
      free: row.free === true,
      // Routing-chain pseudo-model rows report owned_by "endpoint" on the wire.
      isCategory: owner === "endpoint",
      nativeSurfaces: asList(row.native_surfaces).map(String),
      emulatedSurfaces: asList(row.emulated_surfaces).map(String),
      tools: stringValue(typed.tools, "unknown"),
      streaming: stringValue(typed.streaming, "unknown"),
      statefulResponses: stringValue(typed.stateful_responses, "unknown"),
      discoveredAt: stringValue(freshness.discovered_at),
      verifiedAt: stringValue(freshness.verified_at),
      publicationState: stringValue(row.publication_state),
      published: row.published !== false,
      disabled: row.disabled === true,
      failureCode: stringValue(row.failure_code),
    };
  }).filter((row) => row.id);
}

export function transportForSurface(model: CatalogModel | undefined, surface: string): "native" | "translated" | "unknown" {
  const normalized = surface.replace(/^\/v1/, "");
  if (model?.nativeSurfaces.some((candidate) => candidate.replace(/^\/v1/, "") === normalized)) return "native";
  if (model?.emulatedSurfaces.some((candidate) => candidate.replace(/^\/v1/, "") === normalized)) return "translated";
  return "unknown";
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

// matchRank orders a model that matches needle: its id starting with it,
// then its model name after the provider, then its label, then any word of
// its id or label, then a match anywhere. Lower ranks first; -1 is no match.
function matchRank(model: CatalogModel, needle: string): number {
  const id = model.id.toLowerCase();
  const label = model.label.toLowerCase();
  if (!`${id} ${label}`.includes(needle)) return -1;
  if (id.startsWith(needle)) return 0;
  if (id.slice(id.lastIndexOf("/") + 1).startsWith(needle)) return 1;
  if (label.startsWith(needle)) return 2;
  if (`${id} ${label}`.split(/[\s/._:-]+/).some((word) => word.startsWith(needle))) return 3;
  return 4;
}

// rankModels returns the models matching query, prefix matches first and
// otherwise in the order given; an empty query matches every model.
export function rankModels(models: CatalogModel[], query: string): CatalogModel[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return models;
  return models
    .map((model, index) => ({ model, index, rank: matchRank(model, needle) }))
    .filter((entry) => entry.rank >= 0)
    .sort((left, right) => left.rank - right.rank || left.index - right.index)
    .map((entry) => entry.model);
}

// modelComboPageStep is how far Page Up and Page Down move the highlight:
// about one list's height of options.
export const modelComboPageStep = 6;

// ModelCombo is a type-ahead over the narrowed list. Nobody memorises model
// ids, and a select of hundreds is unusable, so the input filters as you
// type, prefix matches first. The list renders pageSize options at a time
// and more as it is scrolled or the highlight moves past them, so even a
// catalog of thousands can be scrolled through to its end.
export function ModelCombo({ models, filter, value, onChange, label = "Model", pageSize = 50 }: {
  models: CatalogModel[];
  filter: ModelFilterState;
  value: string;
  onChange: (modelID: string) => void;
  label?: string;
  pageSize?: number;
}) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const [visible, setVisible] = useState(pageSize);
  // ARIA 1.2 combobox: focus stays on the input, which names the highlighted
  // option through aria-activedescendant, so the listbox and options need ids.
  const listID = useId();
  const pool = filterModels(models, filter).filter((model) => !model.disabled);
  const matches = rankModels(pool, query);
  const shown = matches.slice(0, visible);
  const current = Math.min(active, Math.max(matches.length - 1, 0));
  const expanded = open && matches.length > 0;
  const optionID = (index: number) => `${listID}-option-${index}`;
  const commit = (modelID: string) => { onChange(modelID); setQuery(""); setOpen(false); setActive(0); setVisible(pageSize); };
  // move highlights the option at index, within the matches, rendering
  // enough of the list to hold it.
  const move = (index: number) => {
    const next = Math.max(0, Math.min(index, matches.length - 1));
    setActive(next);
    if (next >= visible) setVisible(Math.min(matches.length, (Math.floor(next / pageSize) + 1) * pageSize));
  };
  // The highlighted option stays in view as the keyboard moves it.
  useEffect(() => {
    if (!expanded || typeof document === "undefined") return;
    document.getElementById(optionID(current))?.scrollIntoView({ block: "nearest" });
  }, [current, expanded]);

  return (
    <div class="model-combo">
      <label>{label}
        <input
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={expanded}
          aria-controls={listID}
          aria-activedescendant={expanded && shown[current] ? optionID(current) : undefined}
          value={open ? query : value}
          placeholder={pool.length ? "Type to search models…" : "No model matches these filters"}
          disabled={!pool.length}
          onFocus={() => { setOpen(true); setQuery(""); setActive(0); setVisible(pageSize); }}
          onBlur={() => window.setTimeout(() => setOpen(false), 140)}
          onInput={(event) => { setQuery((event.currentTarget as HTMLInputElement).value); setOpen(true); setActive(0); setVisible(pageSize); }}
          onKeyDown={(event) => {
            const keys: Record<string, () => void> = {
              ArrowDown: () => { if (open) move(current + 1); else { setOpen(true); setActive(0); } },
              ArrowUp: () => move(current - 1),
              PageDown: () => move(current + modelComboPageStep),
              PageUp: () => move(current - modelComboPageStep),
              Home: () => move(0),
              End: () => move(matches.length - 1),
            };
            if (keys[event.key] && (open || event.key === "ArrowDown")) { event.preventDefault(); keys[event.key](); }
            else if (event.key === "Enter" && expanded && shown[current]) { event.preventDefault(); commit(shown[current].id); }
            else if (event.key === "Escape") { setOpen(false); }
          }}
        />
      </label>
      <ul
        id={listID}
        class="model-combo__list"
        role="listbox"
        aria-label={label}
        hidden={!expanded}
        onScroll={(event) => {
          const list = event.currentTarget as HTMLUListElement;
          if (list.scrollTop + list.clientHeight >= list.scrollHeight - 48) setVisible((count) => Math.min(count + pageSize, matches.length));
        }}
      >
        {expanded ? shown.map((model, index) => {
          const isFree = model.free;
          return (
            <li
              key={model.id}
              id={optionID(index)}
              class={index === current ? "is-active" : ""}
              role="option"
              aria-selected={index === current}
              aria-setsize={matches.length}
              aria-posinset={index + 1}
              onMouseDown={(event) => { event.preventDefault(); commit(model.id); }}
            >
              <div style={{ display: "flex", alignItems: "center", gap: "6px", width: "100%" }}>
                <strong class="technical">{model.id}</strong>
                {isFree ? <span class="status-pill status-pill--success" style={{ fontSize: "10px", padding: "1px 5px", lineHeight: "14px" }}>Free</span> : null}
              </div>
              {model.label !== model.id && model.label.length <= 64 ? <small>{model.label}</small> : null}
            </li>
          );
        }) : null}
        {expanded && matches.length > shown.length ? <li class="model-combo__more" role="presentation">{shown.length} of {matches.length} matches — scroll for more</li> : null}
      </ul>
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
  const visible = filterModels(models, filter).filter((model) => !model.disabled);
  if (!visible.length) {
    return <label>{label}<select disabled><option>{emptyHint}</option></select></label>;
  }
  return <label>{label}<select value={value} onInput={(event) => onChange((event.currentTarget as HTMLSelectElement).value)}>
    {!visible.some((model) => model.id === value) ? <option value="">Select a model</option> : null}
    {visible.map((model) => <option value={model.id} key={model.id}>{modelOptionLabel(model.id, model.label)}</option>)}
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
