import { memo } from "preact/compat";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { AlertCircle, ChevronDown, ChevronUp, FileAudio, Play, RefreshCw, Send, Trash2 } from "lucide-preact";
import { APIError, getJSON, requestJSON, sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, PageHeading } from "../components/PageState";
import {
  ModelCombo,
  ModelFilters,
  type CatalogModel,
  capabilityLabels,
  catalogModels,
  filterModels,
  transportForSurface,
  useModelFilter,
} from "../components/ModelPicker";

type TextSurface = "/v1/chat/completions" | "/v1/responses" | "/v1/messages";
type ChatTurn = { role: "user" | "assistant"; content: string; reasoning?: string; toolCalls?: JSONRecord[]; wire?: unknown[]; served?: string; latency?: number };
export type PlaygroundFailure = { message: string; status: number; code: string; retry: string; action: string };

const textSurfaces: { path: TextSurface; label: string }[] = [
  { path: "/v1/chat/completions", label: "Chat Completions" },
  { path: "/v1/responses", label: "Responses" },
  { path: "/v1/messages", label: "Anthropic Messages" },
];

function normalizedSurface(surface: string): string {
  return surface.replace(/^\/v1/, "");
}

export function supportedTextSurfaces(model: CatalogModel | undefined): TextSurface[] {
  if (!model) return [];
  const supported = new Set([...model.surfaces, ...model.nativeSurfaces, ...model.emulatedSurfaces].map(normalizedSurface));
  return textSurfaces.filter(({ path }) => supported.has(normalizedSurface(path))).map(({ path }) => path);
}

export function defaultTextSurface(model: CatalogModel | undefined): TextSurface {
  const supported = supportedTextSurfaces(model);
  const native = supported.find((surface) => transportForSurface(model, surface) === "native");
  const translated = supported.find((surface) => transportForSurface(model, surface) === "translated");
  return native ?? translated ?? supported[0] ?? "/v1/chat/completions";
}

function textParts(value: unknown): string {
  if (typeof value === "string") return value;
  return asList(value).map(asRecord).map((part) => stringValue(part.text, stringValue(part.output_text))).join("");
}

function parseTextResponse(surface: TextSurface, raw: JSONRecord): Pick<ChatTurn, "content" | "reasoning" | "toolCalls" | "wire"> {
  if (surface === "/v1/chat/completions") {
    const message = asRecord(asRecord(asList(raw.choices)[0]).message);
    return {
      content: textParts(message.content) || "(the provider returned no text)",
      reasoning: textParts(message.reasoning_content ?? message.reasoning),
      toolCalls: asList(message.tool_calls).map(asRecord),
      wire: [message],
    };
  }
  if (surface === "/v1/messages") {
    const content = asList(raw.content).map(asRecord);
    return {
      content: content.filter((part) => stringValue(part.type) === "text").map((part) => stringValue(part.text)).join("") || "(the provider returned no text)",
      reasoning: content.filter((part) => ["thinking", "reasoning"].includes(stringValue(part.type))).map((part) => stringValue(part.thinking, stringValue(part.text))).join("\n"),
      toolCalls: content.filter((part) => stringValue(part.type) === "tool_use"),
      wire: content,
    };
  }
  const output = asList(raw.output).map(asRecord);
  const messages = output.filter((item) => stringValue(item.type) === "message");
  const reasoning = output.filter((item) => stringValue(item.type) === "reasoning");
  return {
    content: stringValue(raw.output_text) || messages.map((item) => textParts(item.content)).join("") || "(the provider returned no text)",
    reasoning: reasoning.map((item) => textParts(item.summary ?? item.content)).join("\n"),
    toolCalls: output.filter((item) => ["function_call", "tool_call"].includes(stringValue(item.type))),
    wire: output,
  };
}

function requestHistory(surface: TextSurface, turns: ChatTurn[]): unknown[] {
  return turns.flatMap((turn) => {
    if (turn.role === "user") return [{ role: "user", content: turn.content }];
    if (surface === "/v1/responses" && turn.wire?.length) return turn.wire;
    if (surface === "/v1/messages" && turn.wire?.length) return [{ role: "assistant", content: turn.wire }];
    if (surface === "/v1/chat/completions" && turn.wire?.length) return turn.wire;
    return [{ role: "assistant", content: turn.content }];
  });
}

function toolsForSurface(surface: TextSurface, tools: unknown[]): JSONRecord[] {
  return tools.map(asRecord).map((tool) => {
    const fn = asRecord(tool.function);
    if (surface === "/v1/responses") return fn.name ? { type: "function", name: fn.name, description: fn.description, parameters: fn.parameters } : tool;
    if (surface === "/v1/messages") return fn.name ? { name: fn.name, description: fn.description, input_schema: fn.parameters } : tool;
    return tool;
  });
}

const ChatTurnView = memo(function ChatTurnView({ turn }: { turn: ChatTurn }) {
  return <article class={`chat-turn chat-turn--${turn.role}`}>
    <header><span>{turn.role === "user" ? "You" : "Assistant"}</span>{turn.served ? <small class="technical">{turn.served}{turn.latency ? ` · ${turn.latency} ms` : ""}</small> : null}</header>
    <p>{turn.content}</p>
    {turn.reasoning ? <details class="chat-turn__detail"><summary>Reasoning</summary><pre>{turn.reasoning}</pre></details> : null}
    {turn.toolCalls?.length ? <details class="chat-turn__detail"><summary>Tool calls ({turn.toolCalls.length})</summary><pre>{JSON.stringify(turn.toolCalls, null, 2)}</pre></details> : null}
  </article>;
});

// modeFor picks the playground surface a model can actually be exercised on.
// A model that only synthesizes speech must not be offered a chat composer.
type PlaygroundMode = "chat" | "tts" | "transcription" | "embedding" | "image" | "video" | "unknown";

export function modesFor(model: CatalogModel | undefined): PlaygroundMode[] {
  if (!model) return [];
  return ["video", "image", "tts", "transcription", "embedding", "chat"].filter((mode) => model.capabilities.includes(mode)) as PlaygroundMode[];
}

export function modeFor(model: CatalogModel | undefined): PlaygroundMode {
  return modesFor(model)[0] ?? "unknown";
}

export function playgroundFailure(cause: unknown, fallback = "Playground request failed."): PlaygroundFailure {
  if (!(cause instanceof APIError)) {
    return { message: cause instanceof Error ? cause.message : fallback, status: 0, code: "", retry: "Unknown", action: "Review the request and try again." };
  }
  const retryable = cause.retryable ?? [408, 429, 500, 502, 503, 504].includes(cause.status);
  let action = cause.action;
  if (!action) {
    if (cause.status === 401) action = "Reconnect or sign in, then retry the request.";
    else if (cause.status === 403) action = "Review project access and the selected provider connection.";
    else if (cause.status === 404) action = "Refresh the catalog and select an available model or endpoint.";
    else if (cause.status === 429) action = cause.retryAfter ? `Wait until ${cause.retryAfter}, then retry.` : "Wait for the provider limit to reset, then retry.";
    else if (retryable) action = "Retry the request; if it repeats, inspect the connected provider.";
    else action = "Correct the request or provider configuration before retrying.";
  }
  return {
    message: cause.message,
    status: cause.status,
    code: cause.code,
    retry: cause.retryAfter ? `After ${cause.retryAfter}` : retryable ? "Yes" : "No",
    action,
  };
}

// localeOf reads the BCP-47 prefix of a voice id like "pt-PT-RaquelNeural".
function localeOf(modelID: string): string {
  const name = modelID.includes("/") ? modelID.slice(modelID.indexOf("/") + 1) : modelID;
  const match = /^([a-z]{2,3}-[A-Z][A-Za-z]{1,7})/.exec(name);
  return match ? match[1] : "";
}

function ChatThread({ turns, running, onClear }: { turns: ChatTurn[]; running: boolean; onClear: () => void }) {
  const endRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => { endRef.current?.scrollIntoView({ block: "end", behavior: "smooth" }); }, [turns.length, running]);
  return (
    <div class="chat-thread">
      <div class="chat-thread__scroll">
        {!turns.length ? <p class="muted-copy chat-thread__hint">Send a message to start. Every turn is replayed as real conversation history through the selected route.</p> : null}
        {turns.map((turn, index) => <ChatTurnView turn={turn} key={index} />)}
        {running ? <article class="chat-turn chat-turn--assistant chat-turn--pending"><header><span>Assistant</span></header><p><RefreshCw class="spin" size={15} /> Routing…</p></article> : null}
        <div ref={endRef} />
      </div>
      {turns.length ? <button class="button button--secondary chat-thread__clear" type="button" onClick={onClear}><Trash2 size={15} /> Clear conversation</button> : null}
    </div>
  );
}

export function Playground({ data, mode, principalID, onPrincipalIDChange, preset, onPresetConsumed, onBack, onChanged }: {
  data: JSONRecord;
  mode: ConsoleMode;
  principalID: string;
  onPrincipalIDChange: (principalID: string) => void;
  preset?: string;
  onPresetConsumed?: () => void;
  onBack?: () => void;
  onChanged?: () => Promise<void>;
}) {
  const projects = asList(data.projects).map(asRecord);
  const memberships = asList(data.memberships).map(asRecord);
  const humans = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const signedInPrincipalID = stringValue(asRecord(data.principal).id);
  const scopedPrincipalID = mode === "admin" ? principalID : signedInPrincipalID;
  const eligibleProjects = useMemo(() => {
    const allowed = new Set(memberships.filter((membership) => {
      const role = stringValue(membership.role);
      return stringValue(membership.principal_id) === scopedPrincipalID && (role === "owner" || role === "admin");
    }).map((membership) => stringValue(membership.project_id)));
    return projects.filter((project) =>
      stringValue(project.status, "active") === "active" && allowed.has(stringValue(project.id)));
  }, [data, scopedPrincipalID]);

  useEffect(() => {
    if (mode === "admin" && !principalID && humans.length) {
      onPrincipalIDChange(stringValue(humans[0].id));
    }
  }, [mode, principalID, humans]);

  const [catalog, setCatalog] = useState<JSONRecord | null>(null);
  const [catalogSource, setCatalogSource] = useState("");
  const [catalogError, setCatalogError] = useState("");
  const [projectID, setProjectID] = useState("");
  const [model, setModel] = useState("");
  const [filter, setFilter] = useModelFilter();
  const [settingsOpen, setSettingsOpen] = useState(true);
  const [turns, setTurns] = useState<ChatTurn[]>([]);
  const [textSurface, setTextSurface] = useState<TextSurface>("/v1/chat/completions");
  const [previousResponseID, setPreviousResponseID] = useState("");
  const [draft, setDraft] = useState("");
  const [toolsOpen, setToolsOpen] = useState(false);
  const [toolDefinitions, setToolDefinitions] = useState("");
  const [toolResult, setToolResult] = useState("");
  const [toolCallID, setToolCallID] = useState("");
  const [speechText, setSpeechText] = useState("The gateway routed this request end to end.");
	const [embeddingInput, setEmbeddingInput] = useState("The gateway routed this embedding request end to end.");
	const [operationMode, setOperationMode] = useState<PlaygroundMode>("unknown");
  const [speechSpeed, setSpeechSpeed] = useState("1");
  const [locale, setLocale] = useState("all");
  const [audioURL, setAudioURL] = useState("");
  const [transcript, setTranscript] = useState("");
  const [uploadName, setUploadName] = useState("");
  const [mediaPrompt, setMediaPrompt] = useState("A single origami crane on a plain wooden desk, soft morning light");
  const [imageURL, setImageURL] = useState("");
  const [videoURL, setVideoURL] = useState("");
  const [videoStatus, setVideoStatus] = useState("");
  const videoPoll = useRef(0);
  const [result, setResult] = useState<JSONRecord | null>(null);
  const [rawOpen, setRawOpen] = useState(true);
  const [error, setError] = useState("");
  const [failure, setFailure] = useState<PlaygroundFailure | null>(null);
  const [running, setRunning] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const catalogRequest = useRef(0);
  const executionRequest = useRef(0);
  const executionPending = useRef(false);
  const executionAbort = useRef<AbortController | null>(null);
  const appliedPreset = useRef("");

  const catalogPath = useMemo(() => {
    if (!projectID || !scopedPrincipalID) return "";
    const query = new URLSearchParams({ project_id: projectID });
    if (mode === "admin") query.set("principal_id", scopedPrincipalID);
    if (mode === "admin") query.set("diagnostics", "1");
    return `/models?${query.toString()}`;
  }, [mode, projectID, scopedPrincipalID]);
  const loadCatalog = async () => {
    const request = ++catalogRequest.current;
    if (!catalogPath) { setCatalogError(""); setCatalogSource(""); return; }
    try {
      setCatalogError("");
      const payload = await getJSON<JSONRecord>(mode, catalogPath);
      if (request === catalogRequest.current) {
        setCatalog(payload);
        setCatalogSource(catalogPath);
      }
    } catch (cause) {
      if (request === catalogRequest.current) setCatalogError(cause instanceof Error ? cause.message : "Model catalog could not load.");
    }
  };
  const refreshEvidence = async () => {
    await Promise.allSettled([loadCatalog(), onChanged?.() ?? Promise.resolve()]);
  };
  useEffect(() => {
    setProjectID((current) => eligibleProjects.some((project) => stringValue(project.id) === current) ? current : stringValue(eligibleProjects[0]?.id));
  }, [eligibleProjects, scopedPrincipalID]);
  useEffect(() => { setCatalog(null); setCatalogSource(""); void loadCatalog(); }, [catalogPath]);

  const models = useMemo(() => {
    const rows = catalogSource === catalogPath ? catalogModels(catalog) : [];
    // Portal exposes only its chat playground route. Admin-only media handlers
    // must never be advertised as runnable self-service actions.
    return mode === "portal"
      ? rows.filter((row) => row.capabilities.includes("chat"))
      : rows.filter((row) => !row.capabilities.length || row.capabilities.some((capability) => ["chat", "tts", "transcription", "embedding", "image", "video"].includes(capability)));
  }, [catalog, catalogSource, catalogPath, mode]);
  const visible = useMemo(() => filterModels(models, filter), [models, filter]);
  // A model handed over from a provider page wins over any default selection.
  useEffect(() => {
    if (!preset) {
      appliedPreset.current = "";
      return;
    }
    const presetScope = `${scopedPrincipalID}|${projectID}|${preset}`;
    if (appliedPreset.current === presetScope || !models.length) return;
    if (models.some((row) => row.id === preset && !row.disabled)) {
      appliedPreset.current = presetScope;
      setModel(preset);
      setSettingsOpen(false);
      onPresetConsumed?.();
    } else {
      setModel("");
    }
  }, [preset, models, projectID, scopedPrincipalID]);
  useEffect(() => {
    const presetScope = `${scopedPrincipalID}|${projectID}|${preset}`;
    if (preset && appliedPreset.current !== presetScope) return;
    setModel((current) => visible.some((row) => row.id === current && !row.disabled) ? current : (visible.find((row) => !row.disabled)?.id ?? ""));
  }, [visible, preset]);
  const selected = models.find((row) => row.id === model);
	const availableModes = mode === "portal" ? (selected?.capabilities.includes("chat") ? ["chat" as PlaygroundMode] : []) : modesFor(selected);
	const surface = availableModes.includes(operationMode) ? operationMode : (availableModes[0] ?? "unknown");
  const availableTextSurfaces = supportedTextSurfaces(selected);
  const toolsUnsupported = selected?.tools === "unsupported" || !selected?.capabilities.includes("chat");
  useEffect(() => {
    executionAbort.current?.abort();
    executionAbort.current = null;
    executionRequest.current += 1;
    executionPending.current = false;
    setTurns([]); setRunning(false);
    setPreviousResponseID("");
    setResult(null); setError(""); setFailure(null); setAudioURL(""); setTranscript("");
    setImageURL(""); setVideoURL(""); setVideoStatus("");
    videoPoll.current += 1;
	}, [model, projectID, scopedPrincipalID, textSurface, surface]);
  useEffect(() => {
		setTextSurface(defaultTextSurface(selected));
		setOperationMode(availableModes[0] ?? "unknown");
    setPreviousResponseID("");
  }, [model, mode]);
  useEffect(() => () => {
    executionAbort.current?.abort();
    executionAbort.current = null;
    executionRequest.current += 1;
    executionPending.current = false;
    videoPoll.current += 1;
  }, []);

  // Voice catalogs are locale-keyed; without this a 321-voice list is unusable.
  const locales = useMemo(() => {
    if (surface !== "tts") return [];
    const names = new Set<string>();
    for (const row of visible) {
      const code = localeOf(row.id);
      if (code) names.add(code);
    }
    return [...names].sort();
  }, [visible, surface]);
  const localeFilter = useMemo(
    () => ({ ...filter, search: filter.search }),
    [filter],
  );
  const voiceModels = useMemo(
    () => locale === "all" ? models : models.filter((row) => localeOf(row.id) === locale),
    [models, locale],
  );

  const requireScope = () => {
    if (!projectID || !model || (mode === "admin" && !principalID)) {
      setFailure(null);
      setError(mode === "admin" && !principalID ? "Select a human owner, project, and model." : "Select a project and model.");
      return false;
    }
    return true;
  };
  const reportError = (cause: unknown, fallback?: string) => {
    const next = playgroundFailure(cause, fallback);
    setError(next.message);
    setFailure(next.status ? next : null);
  };

  // The composer reads its text from the element rather than component state:
  // Enter can arrive before an input-driven re-render has flushed, and a
  // dropped keystroke would silently send the previous draft.
  const sendChat = async (event: Event, override?: string) => {
    event.preventDefault();
    if (executionPending.current) return;
    if (!requireScope()) return;
    const text = (override ?? draft).trim();
    if (!text) { setFailure(null); setError("Enter a message."); return; }
    const history: ChatTurn[] = [...turns, { role: "user", content: text }];
    const request = ++executionRequest.current;
    const controller = new AbortController();
    executionAbort.current = controller;
    executionPending.current = true;
    setTurns(history);
    setDraft("");
    setRunning(true);
    setError(""); setFailure(null);
    setResult(null);
    try {
      const body: JSONRecord = { project_id: projectID, model };
      const statefulResponses = textSurface === "/v1/responses" && selected?.statefulResponses === "supported" && previousResponseID;
      if (textSurface === "/v1/responses") {
        body.input = statefulResponses ? text : requestHistory(textSurface, history);
        if (statefulResponses) body.previous_response_id = previousResponseID;
      } else {
        body.messages = requestHistory(textSurface, history);
        if (textSurface === "/v1/messages") body.max_tokens = 1024;
      }
      if (toolsOpen && toolDefinitions.trim()) {
        let parsed: unknown;
        try { parsed = JSON.parse(toolDefinitions); } catch { throw new Error("Tool definitions must be valid JSON."); }
        if (!Array.isArray(parsed)) throw new Error("Tool definitions must be a JSON array.");
        body.tools = toolsForSurface(textSurface, parsed);
      }
      if (toolsOpen && toolResult.trim()) {
        if (!toolCallID.trim()) throw new Error("Enter the provider's tool call ID before adding a tool result.");
        if (textSurface === "/v1/responses") body.input = [...asList(body.input), { type: "function_call_output", call_id: toolCallID.trim(), output: toolResult.trim() }];
        else if (textSurface === "/v1/messages") body.messages = [...asList(body.messages), { role: "user", content: [{ type: "tool_result", tool_use_id: toolCallID.trim(), content: toolResult.trim() }] }];
        else body.messages = [...asList(body.messages), { role: "tool", content: toolResult.trim(), tool_call_id: toolCallID.trim() }];
      }
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, `/playground${textSurface}`, "POST", body, controller.signal);
      if (request !== executionRequest.current) return;
      setResult(payload);
      const raw = asRecord(payload.raw_response);
      const parsed = parseTextResponse(textSurface, raw);
      if (textSurface === "/v1/responses" && selected?.statefulResponses === "supported") setPreviousResponseID(stringValue(raw.id));
      const served = asRecord(payload.served);
      setTurns([...history, {
        role: "assistant", ...parsed,
        served: `${stringValue(served.provider)}/${stringValue(served.model)}`,
        latency: numberValue(payload.latency_ms),
      }]);
    } catch (cause) {
      if (request !== executionRequest.current) return;
      setResult(null);
      setTurns(turns);
      setDraft((current) => current || text);
      reportError(cause);
    } finally {
      if (request === executionRequest.current) {
        executionAbort.current = null;
        executionPending.current = false;
        setRunning(false);
        void refreshEvidence();
      }
    }
  };

  const runSpeech = async (event: Event) => {
    event.preventDefault();
    if (executionPending.current) return;
    if (!requireScope()) return;
    if (!speechText.trim()) { setFailure(null); setError("Enter text to synthesize."); return; }
    const request = ++executionRequest.current;
    const controller = new AbortController();
    executionAbort.current = controller;
    executionPending.current = true;
    setRunning(true);
    setError(""); setFailure(null);
    setResult(null);
    setAudioURL("");
    try {
      const body: JSONRecord = { project_id: projectID, model, input: speechText.trim(), speed: Number(speechSpeed) || 1 };
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, "/playground/speech", "POST", body, controller.signal);
      if (request !== executionRequest.current) return;
      setResult(payload);
      const base64 = stringValue(payload.audio_base64);
      if (base64) setAudioURL(`data:${stringValue(payload.content_type, "audio/mpeg")};base64,${base64}`);
    } catch (cause) {
      if (request !== executionRequest.current) return;
      setResult(null);
      reportError(cause, "Speech request failed.");
    } finally {
      if (request === executionRequest.current) {
        executionAbort.current = null;
        executionPending.current = false;
        setRunning(false);
        void refreshEvidence();
      }
    }
  };

	const runEmbeddings = async (event: Event) => {
		event.preventDefault();
		if (executionPending.current) return;
		if (!requireScope()) return;
		if (!embeddingInput.trim()) { setFailure(null); setError("Enter text to embed."); return; }
		const request = ++executionRequest.current;
		const controller = new AbortController();
		executionAbort.current = controller;
		executionPending.current = true;
		setRunning(true); setError(""); setFailure(null); setResult(null);
		try {
			const body: JSONRecord = { project_id: projectID, model, input: embeddingInput.trim() };
			if (mode === "admin") body.principal_id = principalID;
			const payload = await sendJSON<JSONRecord>(mode, "/playground/embeddings", "POST", body, controller.signal);
			if (request !== executionRequest.current) return;
			setResult(payload);
		} catch (cause) {
			if (request !== executionRequest.current) return;
			setResult(null); reportError(cause, "Embeddings request failed.");
		} finally {
			if (request === executionRequest.current) {
				executionAbort.current = null; executionPending.current = false; setRunning(false); void refreshEvidence();
			}
		}
	};

  const runTranscription = async (event: Event) => {
    event.preventDefault();
    if (executionPending.current) return;
    if (!requireScope()) return;
    const file = fileRef.current?.files?.[0];
    if (!file) { setFailure(null); setError("Choose an audio file to transcribe."); return; }
    const request = ++executionRequest.current;
    const controller = new AbortController();
    executionAbort.current = controller;
    executionPending.current = true;
    setRunning(true);
    setError(""); setFailure(null);
    setResult(null);
    setTranscript("");
    try {
      const form = new FormData();
      form.set("project_id", projectID);
      form.set("model", model);
      form.set("file", file);
      if (mode === "admin") form.set("principal_id", principalID);
      const payload = await requestJSON<JSONRecord>(mode, "/playground/transcription", { method: "POST", body: form, signal: controller.signal });
      if (request !== executionRequest.current) return;
      setResult(payload);
      setTranscript(stringValue(payload.text, "(the provider returned no text)"));
    } catch (cause) {
      if (request !== executionRequest.current) return;
      setResult(null);
      reportError(cause, "Transcription request failed.");
    } finally {
      if (request === executionRequest.current) {
        executionAbort.current = null;
        executionPending.current = false;
        setRunning(false);
        void refreshEvidence();
      }
    }
  };

  const runImage = async (event: Event) => {
    event.preventDefault();
    if (executionPending.current) return;
    if (!requireScope()) return;
    if (!mediaPrompt.trim()) { setFailure(null); setError("Describe the image you want."); return; }
    const request = ++executionRequest.current;
    const controller = new AbortController();
    executionAbort.current = controller;
    executionPending.current = true;
    setRunning(true); setError(""); setFailure(null); setResult(null); setImageURL("");
    try {
      const body: JSONRecord = { project_id: projectID, model, prompt: mediaPrompt.trim() };
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, "/playground/image", "POST", body, controller.signal);
      if (request !== executionRequest.current) return;
      setResult(payload);
      const base64 = stringValue(payload.image_base64);
      if (base64) setImageURL(`data:${stringValue(payload.content_type, "image/png")};base64,${base64}`);
    } catch (cause) {
      if (request !== executionRequest.current) return;
      setResult(null);
      reportError(cause, "Image request failed.");
    } finally {
      if (request === executionRequest.current) {
        executionAbort.current = null;
        executionPending.current = false;
        setRunning(false);
        void refreshEvidence();
      }
    }
  };

  // Video generation runs for minutes, so the console starts the job and polls
  // the operation rather than holding a request open.
  const runVideo = async (event: Event) => {
    event.preventDefault();
    if (executionPending.current) return;
    if (!requireScope()) return;
    if (!mediaPrompt.trim()) { setFailure(null); setError("Describe the video you want."); return; }
    const attempt = ++videoPoll.current;
    const controller = new AbortController();
    executionAbort.current = controller;
    executionPending.current = true;
    setRunning(true); setError(""); setFailure(null); setResult(null); setVideoURL(""); setVideoStatus("Starting the generation\u2026");
    try {
      const body: JSONRecord = { project_id: projectID, model, prompt: mediaPrompt.trim() };
      if (mode === "admin") body.principal_id = principalID;
      const started = await sendJSON<JSONRecord>(mode, "/playground/video", "POST", body, controller.signal);
      if (attempt !== videoPoll.current) return;
      setResult(started);
      void refreshEvidence();
      const operation = stringValue(started.operation);
      if (!operation) throw new Error("The provider did not return an operation to poll.");
      for (let tick = 1; tick <= 80; tick += 1) {
        if (attempt !== videoPoll.current) return;
        setVideoStatus(`Generating\u2026 ${tick * 15}s elapsed`);
        await new Promise((resolve) => window.setTimeout(resolve, 15000));
        if (attempt !== videoPoll.current) return;
        const pollBody: JSONRecord = { project_id: projectID, model, operation };
        if (mode === "admin") pollBody.principal_id = principalID;
        const payload = await sendJSON<JSONRecord>(mode, "/playground/video", "POST", pollBody, controller.signal);
        if (attempt !== videoPoll.current) return;
        setResult(payload);
        void refreshEvidence();
        if (stringValue(payload.status) === "completed") {
          const base64 = stringValue(payload.video_base64);
          if (base64) {
            setVideoURL(`data:${stringValue(payload.mime_type, "video/mp4")};base64,${base64}`);
            setVideoStatus("");
          } else {
            setVideoStatus(`Ready upstream at ${stringValue(payload.video_uri, "an unnamed location")}`);
          }
          return;
        }
      }
      setVideoStatus("Still generating after 20 minutes \u2014 the job may have stalled upstream.");
    } catch (cause) {
      if (attempt !== videoPoll.current) return;
      setResult(null);
      reportError(cause, "Video request failed.");
      setVideoStatus("");
    } finally {
      if (attempt === videoPoll.current) {
        executionAbort.current = null;
        executionPending.current = false;
        setRunning(false);
        void refreshEvidence();
      }
    }
  };

  const served = asRecord(result?.served);
  const trace = asList(result?.fallback_trace).map(asRecord);
  const usage = asRecord(result?.usage);
  const capabilityNote = selected
    ? selected.capabilities.map((capability) => capabilityLabels[capability as keyof typeof capabilityLabels] ?? capability).join(" · ")
    : "";
  const selectedSurface = surface === "chat" ? textSurface : {
		tts: "/v1/audio/speech", transcription: "/v1/audio/transcriptions", embedding: "/v1/embeddings",
    image: "/v1/images/generations", video: "/v1/videos/generations",
  }[surface as "tts" | "transcription" | "image" | "video"] ?? "Unknown";
  const expectedTransport = transportForSurface(selected, selectedSurface);
  const formatFreshness = (value: string) => value ? new Date(value).toLocaleString() : "Not available";
  const scopeSummary = [
    humans.find((principal) => stringValue(principal.id) === principalID),
    eligibleProjects.find((project) => stringValue(project.id) === projectID),
  ];

  return (
    <div class="page-stack playground-page">
      <PageHeading
        eyebrow="Gateway request path"
        title="Playground"
        detail="Keyless, project-attributed requests through normal routing. The surface follows the model's capability."
        actions={onBack ? <button class="button button--secondary" type="button" onClick={onBack}>Back to provider</button> : undefined}
      />
      {catalogError ? <ErrorState title="Model catalog is unavailable" detail={catalogError} action={<button class="button button--secondary" type="button" onClick={() => void loadCatalog()}>Retry catalog</button>} /> : null}

      {/* Scope and model selection collapse into a single bar so the conversation owns the page. */}
      <section class="surface playground-settings">
        <button class="playground-settings__toggle" type="button" aria-expanded={settingsOpen} onClick={() => setSettingsOpen(!settingsOpen)}>
          {settingsOpen ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
          <span class="playground-settings__summary">
            <strong class="technical">{model || "No model selected"}</strong>
            <small>{[stringValue(scopeSummary[0]?.display_name), stringValue(scopeSummary[1]?.name, stringValue(scopeSummary[1]?.slug)), capabilityNote].filter(Boolean).join(" · ") || "Configure scope"}</small>
          </span>
        </button>
        {settingsOpen ? <div class="playground-settings__body">
          <div class="playground-settings__scope">
            {mode === "admin" ? <label>Human owner<select value={principalID} onInput={(event) => onPrincipalIDChange((event.currentTarget as HTMLSelectElement).value)}><option value="">Select a human owner</option>{humans.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))}</option>)}</select></label> : null}
            <label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}><option value="">Select a project</option>{eligibleProjects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
            {surface === "tts" && locales.length > 1 ? <label>Language<select value={locale} onInput={(event) => setLocale((event.currentTarget as HTMLSelectElement).value)}><option value="all">All languages ({locales.length})</option>{locales.map((code) => <option value={code} key={code}>{code}</option>)}</select></label> : null}
			{surface === "chat" && availableTextSurfaces.length > 1 ? <label>Text surface<select value={textSurface} onInput={(event) => setTextSurface((event.currentTarget as HTMLSelectElement).value as TextSurface)}>{textSurfaces.filter(({ path }) => availableTextSurfaces.includes(path)).map(({ path, label }) => <option value={path} key={path}>{label} · {transportForSurface(selected, path)}</option>)}</select></label> : null}
			{availableModes.length > 1 ? <label>Operation<select value={surface} onInput={(event) => setOperationMode((event.currentTarget as HTMLSelectElement).value as PlaygroundMode)}>{availableModes.map((candidate) => <option value={candidate} key={candidate}>{capabilityLabels[candidate as keyof typeof capabilityLabels] ?? candidate}</option>)}</select></label> : null}
          </div>
          <ModelFilters models={models} filter={filter} onChange={setFilter} />
          <ModelCombo models={voiceModels} filter={localeFilter} value={model} onChange={setModel} label="Model or route" />
          {selected && !selected.capabilities.length ? <p class="form-help"><strong>Capabilities unknown.</strong> The catalog did not prove a runnable chat, embeddings, image, speech, transcription, or video surface, so execution controls are disabled.</p> : null}
          {selected?.publicationState === "unverified" && selected.published ? <p class="form-help"><strong>Published by administrator opt-in.</strong> This model is runnable without successful completion verification.</p> : null}
          {selected?.publicationState === "failed" ? <p class="form-help"><strong>Publication blocked.</strong> Verification failed{selected.failureCode ? ` (${selected.failureCode})` : ""}; this model is not runnable.</p> : null}
          {selected ? <dl class="playground-capability-facts compact-facts">
            <div><dt>Catalog freshness</dt><dd>{formatFreshness(selected.discoveredAt)}</dd></div>
            <div><dt>Verification freshness</dt><dd>{formatFreshness(selected.verifiedAt)}</dd></div>
            <div><dt>Expected transport</dt><dd>{expectedTransport}</dd></div>
            <div><dt>Surface</dt><dd class="technical">{selectedSurface}</dd></div>
          </dl> : null}
        </div> : null}
      </section>

      <section class="playground-work">
        <div class="playground-stage">
        {surface === "chat" ? <>
          <ChatThread turns={turns} running={running} onClear={() => { executionAbort.current?.abort(); executionAbort.current = null; executionRequest.current += 1; executionPending.current = false; setRunning(false); setTurns([]); setPreviousResponseID(""); setResult(null); setError(""); }} />
          <form class="chat-composer surface" onSubmit={sendChat}>
            <details class="playground-tools" open={toolsOpen} onToggle={(event) => setToolsOpen((event.currentTarget as HTMLDetailsElement).open)}>
              <summary>Tools {toolsUnsupported ? "(unsupported by this model)" : "(optional)"}</summary>
              <label>Tool definitions (JSON array)<textarea value={toolDefinitions} rows={5} disabled={toolsUnsupported} placeholder='[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]' onInput={(event) => setToolDefinitions((event.currentTarget as HTMLTextAreaElement).value)} /></label>
              <label>Tool call ID<input value={toolCallID} disabled={toolsUnsupported} placeholder="call_..." onInput={(event) => setToolCallID((event.currentTarget as HTMLInputElement).value)} /></label>
              <label>Tool result for follow-up (optional)<textarea value={toolResult} rows={2} disabled={toolsUnsupported} onInput={(event) => setToolResult((event.currentTarget as HTMLTextAreaElement).value)} /></label>
            </details>
            <textarea
              value={draft}
              rows={3}
              placeholder="Send a message…  (Enter to send, Shift+Enter for a new line)"
              onInput={(event) => setDraft((event.currentTarget as HTMLTextAreaElement).value)}
              onKeyDown={(event) => {
                if (event.key !== "Enter" || event.shiftKey) return;
                event.preventDefault();
                void sendChat(event, (event.currentTarget as HTMLTextAreaElement).value);
              }}
            />
            <button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Send size={16} />} Send</button>
          </form>
        </> : null}

		{surface === "tts" ? <form class="surface form-stack" onSubmit={runSpeech}>
          <label>Text to speak<textarea value={speechText} rows={4} onInput={(event) => setSpeechText((event.currentTarget as HTMLTextAreaElement).value)} /></label>
          <label class="playground-speed">Speed<input inputMode="decimal" value={speechSpeed} onInput={(event) => setSpeechSpeed((event.currentTarget as HTMLInputElement).value)} /></label>
          <button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Play size={16} />} Synthesize speech</button>
          {audioURL ? <div class="playground-audio"><p class="eyebrow">Synthesized audio</p><audio controls src={audioURL} /><p class="form-help">{numberValue(result?.audio_bytes)} bytes · {stringValue(result?.audio_format)}</p></div> : null}
		</form> : null}

		{surface === "embedding" ? <form class="surface form-stack" onSubmit={runEmbeddings}>
			<label>Text to embed<textarea value={embeddingInput} rows={4} onInput={(event) => setEmbeddingInput((event.currentTarget as HTMLTextAreaElement).value)} /></label>
			<button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Play size={16} />} Create embedding</button>
			{result ? <p class="form-help">{numberValue(result.vectors)} vector · {numberValue(result.dimensions)} dimensions</p> : null}
		</form> : null}

        {surface === "image" ? <form class="surface form-stack" onSubmit={runImage}>
          <label>Describe the image<textarea value={mediaPrompt} rows={3} onInput={(event) => setMediaPrompt((event.currentTarget as HTMLTextAreaElement).value)} /></label>
          <button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Play size={16} />} Generate image</button>
          {imageURL ? <figure class="playground-media"><img src={imageURL} alt="Generated result" /><figcaption>{numberValue(result?.image_bytes)} bytes \u00b7 {stringValue(result?.content_type)}</figcaption></figure> : null}
        </form> : null}

        {surface === "video" ? <form class="surface form-stack" onSubmit={runVideo}>
          <label>Describe the video<textarea value={mediaPrompt} rows={3} onInput={(event) => setMediaPrompt((event.currentTarget as HTMLTextAreaElement).value)} /></label>
          <p class="form-help">Video generation takes minutes. The job runs upstream; this page polls it, so navigating away stops the polling but not the job.</p>
          <button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Play size={16} />} Generate video</button>
          {videoStatus ? <p class="playground-progress"><RefreshCw class={running ? "spin" : ""} size={15} /> {videoStatus}</p> : null}
          {videoURL ? <figure class="playground-media"><video controls src={videoURL} /><figcaption>{numberValue(result?.video_bytes)} bytes \u00b7 {stringValue(result?.mime_type)}</figcaption></figure> : null}
        </form> : null}

        {surface === "transcription" ? <form class="surface form-stack" onSubmit={runTranscription}>
          <label>Audio file<input ref={fileRef} type="file" accept="audio/*" onInput={(event) => setUploadName((event.currentTarget as HTMLInputElement).files?.[0]?.name ?? "")} /></label>
          {uploadName ? <p class="form-help"><FileAudio size={15} /> {uploadName}</p> : null}
          <button class="button button--primary" type="submit" disabled={running || !model}>{running ? <RefreshCw class="spin" size={16} /> : <Play size={16} />} Transcribe audio</button>
          {transcript ? <div class="playground-transcript"><p class="eyebrow">Transcript</p><p>{transcript}</p></div> : null}
        </form> : null}

        {error ? <ErrorState title={failure ? `Request failed · HTTP ${failure.status}${failure.code ? ` · ${failure.code}` : ""}` : "Playground request did not complete"} detail={error} /> : null}
        <p class="playground-limit"><AlertCircle size={16} /> Streaming is unavailable in this playground. Capability-proven controls and routes are enabled for the selected model only.</p>
        </div>

        <aside class="playground-panel">
        {error ? <section class="surface playground-outcome playground-outcome--error">
          <p class="eyebrow" style={{ color: "var(--color-danger, #e5484d)" }}>Request failed</p>
          <h2>{model || "Routing error"}</h2>
          {failure ? <dl class="compact-facts">
            <div><dt>Status</dt><dd class="technical">HTTP {failure.status}</dd></div>
            <div><dt>Code</dt><dd class="technical">{failure.code || "Not supplied"}</dd></div>
            <div><dt>Retry</dt><dd>{failure.retry}</dd></div>
            <div><dt>Action</dt><dd>{failure.action}</dd></div>
          </dl> : null}
          <p class="form-help">Catalog discovery does not guarantee current inference availability. Provider and verification evidence refreshes after this request.</p>
        </section> : result ? <section class="surface playground-outcome">
          <div class="section-heading"><div><p class="eyebrow">Routed result</p><h2>{stringValue(served.provider)} / {stringValue(served.model)}</h2></div><span class="technical">{numberValue(result.latency_ms)} ms</span></div>
          <dl class="compact-facts">
            <div><dt>Input tokens</dt><dd>{numberValue(usage.prompt_tokens, numberValue(usage.input_tokens))}</dd></div>
            <div><dt>Output tokens</dt><dd>{numberValue(usage.completion_tokens, numberValue(usage.output_tokens))}</dd></div>
            <div><dt>Fallback attempts</dt><dd>{trace.length}</dd></div>
            <div><dt>Transport mode</dt><dd>{stringValue(result.transport_mode, expectedTransport)}</dd></div>
          </dl>
          {trace.length ? <div class="trace-list"><h3>Fallback and exclusion diagnostics</h3>{trace.map((attempt, index) => <div key={`${index}-${stringValue(attempt.provider)}`}><span class="route-order">{index + 1}</span><span class="technical">{stringValue(attempt.provider)}/{stringValue(attempt.model)}</span><span class={`status-pill ${stringValue(attempt.status) === "served" ? "status-pill--ready" : "status-pill--attention"}`}>{stringValue(attempt.status)}</span></div>)}</div> : <p class="form-help">No fallback was needed. Capability-incompatible route members are excluded before execution.</p>}
          <details class="raw-response" open={rawOpen} onToggle={(event) => setRawOpen((event.currentTarget as HTMLDetailsElement).open)}>
            <summary>Raw gateway response</summary>
            <pre class="technical">{JSON.stringify(result.raw_response ?? result, null, 2)}</pre>
          </details>
        </section> : <section class="surface playground-outcome playground-outcome--idle"><p class="eyebrow">Response</p><h2>Nothing routed yet</h2><p class="muted-copy">The served provider, latency, token usage, fallback trace and the raw gateway payload appear here after a request.</p></section>}
        </aside>
      </section>

      {!eligibleProjects.length ? <EmptyState title="No eligible project is available" detail="A project owner or administrator membership is required before using the playground." /> : null}
    </div>
  );
}
