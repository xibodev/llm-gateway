import { useState } from "preact/hooks";
import { ArrowDown, ArrowLeft, LoaderCircle, Play, Trash2 } from "lucide-preact";
import { sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, PageHeading } from "../components/PageState";

type RouteMember = { provider: string; model: string };

export function RouteDetail({ routeName, data, mode, onChanged, onBack }: {
  routeName: string;
  data: JSONRecord;
  mode: ConsoleMode;
  onChanged: () => Promise<void>;
  onBack: () => void;
}) {
  const categories = asRecord(data.categories);
  const routeValue = categories[routeName];
  const members: RouteMember[] = asList(asRecord(routeValue).failover).map(asRecord)
    .map((member) => ({ provider: stringValue(member.provider), model: stringValue(member.model) }));

  const humans = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const projects = asList(data.projects).map(asRecord).filter((project) => stringValue(project.status, "active") === "active");
  const [principalID, setPrincipalID] = useState(stringValue(humans[0]?.id));
  const [projectID, setProjectID] = useState(stringValue(projects[0]?.id));
  const [prompt, setPrompt] = useState("Reply with the single word: ok");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<JSONRecord | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);

  if (mode !== "admin") {
    return <ErrorState title="Routes are managed by an administrator" detail="Owner portal mode uses approved routes but does not change shared failover policy." />;
  }
  if (routeValue === undefined) {
    return <div class="page-stack"><PageHeading eyebrow="Route" title="Unknown route" detail="This route does not exist in the current configuration." /><button class="button button--secondary" type="button" onClick={onBack}><ArrowLeft size={16} /> Back to routes</button></div>;
  }

  const runTest = async (event: Event) => {
    event.preventDefault();
    if (!principalID || !projectID) {
      setError("Select a human owner and a project — route tests run as real project-attributed requests.");
      return;
    }
    setBusy(true);
    setError("");
    setResult(null);
    try {
      const payload = await sendJSON<JSONRecord>("admin", "/playground", "POST", {
        principal_id: principalID,
        project_id: projectID,
        model: routeName,
        messages: [{ role: "user", content: prompt || "Reply with the single word: ok" }],
        max_tokens: 64,
      });
      setResult(payload);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Route test failed.");
    } finally { setBusy(false); }
  };

  const removeRoute = async () => {
    if (!window.confirm(`Delete route ${routeName}?`)) return;
    setDeleteBusy(true);
    try {
      await sendJSON<JSONRecord>("admin", `/categories/${encodeURIComponent(routeName)}`, "DELETE");
      await onChanged();
      onBack();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Route could not be deleted.");
      setDeleteBusy(false);
    }
  };

  const served = asRecord(result?.served);
  const usage = asRecord(result?.usage);
  const trace = asList(result?.fallback_trace).map(asRecord);

  return (
    <div class="page-stack">
      <nav class="detail-breadcrumb"><button class="button button--secondary" type="button" onClick={onBack}><ArrowLeft size={16} /> Routes</button></nav>
      <PageHeading eyebrow="Category route" title={routeName} detail="The ordered failover chain below is exactly what the gateway walks for this route name: the first healthy member serves the request." actions={<button class="button button--danger" type="button" disabled={deleteBusy} onClick={() => void removeRoute()}><Trash2 size={16} /> Delete route</button>} />
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Failover chain</p><h2>Members in order</h2></div><span>{members.length} member{members.length === 1 ? "" : "s"}</span></div>
        <ol class="route-chain">{members.map((member, index) => <li key={`${index}-${member.provider}-${member.model}`}><span class="route-order">{index + 1}</span><span class="technical">{member.provider}/{member.model}</span>{index < members.length - 1 ? <ArrowDown size={16} aria-label="then" /> : <span class="route-terminal">served</span>}</li>)}</ol>
        <p class="form-help">Edit membership and ordering from the Routes list. Deleting a member's provider breaks the chain at that hop.</p>
      </section>
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Route test</p><h2>Run this route end to end</h2></div></div>
        {!humans.length || !projects.length ? <EmptyState title="A human owner and a project are required" detail="Route tests are real project-attributed gateway requests. Create them on the Access page first." /> : <form class="route-test-form" onSubmit={runTest}>
          <div class="route-test-form__row">
            <label>Run as<select value={principalID} onInput={(event) => setPrincipalID((event.currentTarget as HTMLSelectElement).value)}>{humans.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))}</option>)}</select></label>
            <label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
          </div>
          <label>Prompt<input value={prompt} onInput={(event) => setPrompt((event.currentTarget as HTMLInputElement).value)} /></label>
          {error ? <p class="form-error" role="alert">{error}</p> : null}
          <footer><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <Play size={16} />} Run route test</button></footer>
        </form>}
        {result ? <div class="playground-result"><p class="eyebrow">Routed result</p><h2>{stringValue(served.provider)} / {stringValue(served.model)}</h2><dl class="compact-facts"><div><dt>Latency</dt><dd>{numberValue(result.latency_ms)} ms</dd></div><div><dt>Input tokens</dt><dd>{numberValue(usage.prompt_tokens, numberValue(usage.input_tokens))}</dd></div><div><dt>Output tokens</dt><dd>{numberValue(usage.completion_tokens, numberValue(usage.output_tokens))}</dd></div><div><dt>Fallback attempts</dt><dd>{trace.length}</dd></div></dl><div class="trace-list"><h3>Fallback trace</h3>{trace.map((attempt, index) => <div key={`${index}-${stringValue(attempt.provider)}`}><span class="route-order">{index + 1}</span><span class="technical">{stringValue(attempt.provider)}/{stringValue(attempt.model)}</span><span class={`status-pill ${stringValue(attempt.status) === "served" ? "status-pill--ready" : "status-pill--attention"}`}>{stringValue(attempt.status)}</span></div>)}</div></div> : null}
      </section>
    </div>
  );
}
