import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { AlertCircle, ChevronDown, ChevronUp, FileAudio, Play, RefreshCw, Send, Trash2 } from "lucide-preact";
import { getJSON, requestJSON, sendJSON, type JSONRecord } from "../lib/api";
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
  useModelFilter,
} from "../components/ModelPicker";

type ChatTurn = { role: "user" | "assistant"; content: string; served?: string; latency?: number };

// modeFor picks the playground surface a model can actually be exercised on.
// A model that only synthesizes speech must not be offered a chat composer.
function modeFor(model: CatalogModel | undefined): "chat" | "tts" | "transcription" | "image" | "video" {
  if (!model) return "chat";
  if (model.capabilities.includes("chat")) return "chat";
  if (model.capabilities.includes("video")) return "video";
  if (model.capabilities.includes("image")) return "image";
  if (model.capabilities.includes("tts")) return "tts";
  if (model.capabilities.includes("transcription")) return "transcription";
  return "chat";
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
        {turns.map((turn, index) => <article class={`chat-turn chat-turn--${turn.role}`} key={index}>
          <header><span>{turn.role === "user" ? "You" : "Assistant"}</span>{turn.served ? <small class="technical">{turn.served}{turn.latency ? ` · ${turn.latency} ms` : ""}</small> : null}</header>
          <p>{turn.content}</p>
        </article>)}
        {running ? <article class="chat-turn chat-turn--assistant chat-turn--pending"><header><span>Assistant</span></header><p><RefreshCw class="spin" size={15} /> Routing…</p></article> : null}
        <div ref={endRef} />
      </div>
      {turns.length ? <button class="button button--secondary chat-thread__clear" type="button" onClick={onClear}><Trash2 size={15} /> Clear conversation</button> : null}
    </div>
  );
}

export function Playground({ data, mode, principalID, onPrincipalIDChange, preset, onPresetConsumed, onBack }: {
  data: JSONRecord;
  mode: ConsoleMode;
  principalID: string;
  onPrincipalIDChange: (principalID: string) => void;
  preset?: string;
  onPresetConsumed?: () => void;
  onBack?: () => void;
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

  const [catalog, setCatalog] = useState<JSONRecord | null>(null);
  const [catalogError, setCatalogError] = useState("");
  const [projectID, setProjectID] = useState("");
  const [model, setModel] = useState("");
  const [filter, setFilter] = useModelFilter();
  const [settingsOpen, setSettingsOpen] = useState(true);
  const [turns, setTurns] = useState<ChatTurn[]>([]);
  const [draft, setDraft] = useState("");
  const [speechText, setSpeechText] = useState("The gateway routed this request end to end.");
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
  const [running, setRunning] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const catalogRequest = useRef(0);

  const catalogPath = useMemo(() => {
    if (!projectID || !scopedPrincipalID) return "";
    const query = new URLSearchParams({ project_id: projectID });
    if (mode === "admin") query.set("principal_id", scopedPrincipalID);
    return `/models?${query.toString()}`;
  }, [mode, projectID, scopedPrincipalID]);
  const loadCatalog = async () => {
    const request = ++catalogRequest.current;
    if (!catalogPath) { setCatalogError(""); return; }
    try {
      setCatalogError("");
      const payload = await getJSON<JSONRecord>(mode, catalogPath);
      if (request === catalogRequest.current) setCatalog(payload);
    } catch (cause) {
      if (request === catalogRequest.current) setCatalogError(cause instanceof Error ? cause.message : "Model catalog could not load.");
    }
  };
  useEffect(() => {
    setProjectID((current) => eligibleProjects.some((project) => stringValue(project.id) === current) ? current : stringValue(eligibleProjects[0]?.id));
  }, [eligibleProjects, scopedPrincipalID]);
  useEffect(() => { setCatalog(null); void loadCatalog(); }, [catalogPath]);

  const models = useMemo(() => catalogModels(catalog), [catalog]);
  const visible = useMemo(() => filterModels(models, filter), [models, filter]);
  // A model handed over from a provider page wins over any default selection.
  useEffect(() => {
    if (!preset || !models.length) return;
    if (models.some((row) => row.id === preset)) {
      setModel(preset);
      setSettingsOpen(false);
      onPresetConsumed?.();
    }
  }, [preset, models]);
  useEffect(() => {
    if (preset) return;
    setModel((current) => visible.some((row) => row.id === current) ? current : (visible[0]?.id ?? ""));
  }, [visible, preset]);
  const selected = models.find((row) => row.id === model);
  const surface = modeFor(selected);
  useEffect(() => {
    setResult(null); setError(""); setAudioURL(""); setTranscript("");
    setImageURL(""); setVideoURL(""); setVideoStatus("");
    videoPoll.current += 1;
  }, [model]);

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
      setError(mode === "admin" && !principalID ? "Select a human owner, project, and model." : "Select a project and model.");
      return false;
    }
    return true;
  };

  // The composer reads its text from the element rather than component state:
  // Enter can arrive before an input-driven re-render has flushed, and a
  // dropped keystroke would silently send the previous draft.
  const sendChat = async (event: Event, override?: string) => {
    event.preventDefault();
    if (!requireScope()) return;
    const text = (override ?? draft).trim();
    if (!text) { setError("Enter a message."); return; }
    const history: ChatTurn[] = [...turns, { role: "user", content: text }];
    setTurns(history);
    setDraft("");
    setRunning(true);
    setError("");
    try {
      const body: JSONRecord = {
        project_id: projectID, model,
        messages: history.map((turn) => ({ role: turn.role, content: turn.content })),
      };
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, "/playground", "POST", body);
      setResult(payload);
      const raw = asRecord(payload.raw_response);
      const choice = asRecord(asList(raw.choices)[0]);
      const message = asRecord(choice.message);
      const anthropic = asList(raw.content).map(asRecord).map((part) => stringValue(part.text)).join("");
      const reply = stringValue(message.content, anthropic) || "(the provider returned no text)";
      const served = asRecord(payload.served);
      setTurns([...history, {
        role: "assistant", content: reply,
        served: `${stringValue(served.provider)}/${stringValue(served.model)}`,
        latency: numberValue(payload.latency_ms),
      }]);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Playground request failed.");
    } finally { setRunning(false); }
  };

  const runSpeech = async (event: Event) => {
    event.preventDefault();
    if (!requireScope()) return;
    if (!speechText.trim()) { setError("Enter text to synthesize."); return; }
    setRunning(true);
    setError("");
    setAudioURL("");
    try {
      const body: JSONRecord = { project_id: projectID, model, input: speechText.trim(), speed: Number(speechSpeed) || 1 };
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, "/playground/speech", "POST", body);
      setResult(payload);
      const base64 = stringValue(payload.audio_base64);
      if (base64) setAudioURL(`data:${stringValue(payload.content_type, "audio/mpeg")};base64,${base64}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Speech request failed.");
    } finally { setRunning(false); }
  };

  const runTranscription = async (event: Event) => {
    event.preventDefault();
    if (!requireScope()) return;
    const file = fileRef.current?.files?.[0];
    if (!file) { setError("Choose an audio file to transcribe."); return; }
    setRunning(true);
    setError("");
    setTranscript("");
    try {
      const form = new FormData();
      form.set("project_id", projectID);
      form.set("model", model);
      form.set("file", file);
      if (mode === "admin") form.set("principal_id", principalID);
      const payload = await requestJSON<JSONRecord>(mode, "/playground/transcription", { method: "POST", body: form });
      setResult(payload);
      setTranscript(stringValue(payload.text, "(the provider returned no text)"));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Transcription request failed.");
    } finally { setRunning(false); }
  };

  const runImage = async (event: Event) => {
    event.preventDefault();
    if (!requireScope()) return;
    if (!mediaPrompt.trim()) { setError("Describe the image you want."); return; }
    setRunning(true); setError(""); setImageURL("");
    try {
      const body: JSONRecord = { project_id: projectID, model, prompt: mediaPrompt.trim() };
      if (mode === "admin") body.principal_id = principalID;
      const payload = await sendJSON<JSONRecord>(mode, "/playground/image", "POST", body);
      setResult(payload);
      const base64 = stringValue(payload.image_base64);
      if (base64) setImageURL(`data:${stringValue(payload.content_type, "image/png")};base64,${base64}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Image request failed.");
    } finally { setRunning(false); }
  };

  // Video generation runs for minutes, so the console starts the job and polls
  // the operation rather than holding a request open.
  const runVideo = async (event: Event) => {
    event.preventDefault();
    if (!requireScope()) return;
    if (!mediaPrompt.trim()) { setError("Describe the video you want."); return; }
    const attempt = ++videoPoll.current;
    setRunning(true); setError(""); setVideoURL(""); setVideoStatus("Starting the generation\u2026");
    try {
      const body: JSONRecord = { project_id: projectID, model, prompt: mediaPrompt.trim() };
      if (mode === "admin") body.principal_id = principalID;
      const started = await sendJSON<JSONRecord>(mode, "/playground/video", "POST", body);
      setResult(started);
      const operation = stringValue(started.operation);
      if (!operation) throw new Error("The provider did not return an operation to poll.");
      for (let tick = 1; tick <= 80; tick += 1) {
        if (attempt !== videoPoll.current) return;
        setVideoStatus(`Generating\u2026 ${tick * 15}s elapsed`);
        await new Promise((resolve) => window.setTimeout(resolve, 15000));
        if (attempt !== videoPoll.current) return;
        const pollBody: JSONRecord = { project_id: projectID, model, operation };
        if (mode === "admin") pollBody.principal_id = principalID;
        const payload = await sendJSON<JSONRecord>(mode, "/playground/video", "POST", pollBody);
        setResult(payload);
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
      setError(cause instanceof Error ? cause.message : "Video request failed.");
      setVideoStatus("");
    } finally {
      if (attempt === videoPoll.current) setRunning(false);
    }
  };

  const served = asRecord(result?.served);
  const trace = asList(result?.fallback_trace).map(asRecord);
  const usage = asRecord(result?.usage);
  const capabilityNote = selected
    ? selected.capabilities.map((capability) => capabilityLabels[capability as keyof typeof capabilityLabels] ?? capability).join(" · ")
    : "";
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
          </div>
          <ModelFilters models={models} filter={filter} onChange={setFilter} />
          <ModelCombo models={voiceModels} filter={localeFilter} value={model} onChange={setModel} label="Model or route" />
        </div> : null}
      </section>

      <section class="playground-work">
        <div class="playground-stage">
        {surface === "chat" ? <>
          <ChatThread turns={turns} running={running} onClear={() => { setTurns([]); setResult(null); }} />
          <form class="chat-composer surface" onSubmit={sendChat}>
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

        {error ? <ErrorState title="Playground request did not complete" detail={error} /> : null}
        <p class="playground-limit"><AlertCircle size={16} /> Streaming is explicitly unavailable in this playground. Non-streaming requests follow the normal gateway route.</p>
        </div>

        <aside class="playground-panel">
        {result ? <section class="surface playground-outcome">
          <div class="section-heading"><div><p class="eyebrow">Routed result</p><h2>{stringValue(served.provider)} / {stringValue(served.model)}</h2></div><span class="technical">{numberValue(result.latency_ms)} ms</span></div>
          <dl class="compact-facts">
            <div><dt>Input tokens</dt><dd>{numberValue(usage.prompt_tokens, numberValue(usage.input_tokens))}</dd></div>
            <div><dt>Output tokens</dt><dd>{numberValue(usage.completion_tokens, numberValue(usage.output_tokens))}</dd></div>
            <div><dt>Fallback attempts</dt><dd>{trace.length}</dd></div>
          </dl>
          {trace.length ? <div class="trace-list"><h3>Fallback trace</h3>{trace.map((attempt, index) => <div key={`${index}-${stringValue(attempt.provider)}`}><span class="route-order">{index + 1}</span><span class="technical">{stringValue(attempt.provider)}/{stringValue(attempt.model)}</span><span class={`status-pill ${stringValue(attempt.status) === "served" ? "status-pill--ready" : "status-pill--attention"}`}>{stringValue(attempt.status)}</span></div>)}</div> : null}
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
