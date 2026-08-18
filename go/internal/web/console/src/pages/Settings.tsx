import { useEffect, useState } from "preact/hooks";
import { LoaderCircle, RefreshCw, Save, ShieldCheck, Users } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

function safeDetail(value: unknown): JSONRecord {
  const detail = asRecord(value);
  const out: JSONRecord = {};
  for (const [key, child] of Object.entries(detail)) {
    const lower = key.toLowerCase();
    if (lower.includes("token") || lower.includes("secret") || lower.includes("authorization") || lower.includes("api_key")) continue;
    out[key] = child;
  }
  return out;
}

// Numeric policy fields editable in the project policy form. Zero means "no
// limit" and is stored by omission.
const policyFields: { key: string; label: string; help: string }[] = [
  { key: "rpm", label: "Requests per minute", help: "Sustained per-minute ceiling" },
  { key: "daily_requests", label: "Daily requests", help: "Requests per UTC day" },
  { key: "monthly_requests", label: "Monthly requests", help: "Requests per calendar month" },
  { key: "daily_input_tokens", label: "Daily input tokens", help: "Prompt tokens per day" },
  { key: "daily_output_tokens", label: "Daily output tokens", help: "Completion tokens per day" },
  { key: "monthly_total_tokens", label: "Monthly total tokens", help: "All tokens per month" },
  { key: "daily_cost_microusd", label: "Daily cost (µUSD)", help: "Estimated spend per day, micro-USD" },
  { key: "monthly_cost_microusd", label: "Monthly cost (µUSD)", help: "Estimated spend per month, micro-USD" },
];

function ProjectPolicyEditor({ projects, onSaved }: { projects: JSONRecord[]; onSaved: (message: string) => void }) {
  const [projectID, setProjectID] = useState(stringValue(projects[0]?.id));
  const [policy, setPolicy] = useState<JSONRecord | null>(null);
  const [allowedModels, setAllowedModels] = useState("");
  const [allowedProviders, setAllowedProviders] = useState("");
  const [numbers, setNumbers] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = async (targetID: string) => {
    if (!targetID) { setPolicy(null); return; }
    setError("");
    setPolicy(null);
    try {
      const payload = await getJSON<JSONRecord>("admin", `/projects/${encodeURIComponent(targetID)}/policy`);
      setPolicy(payload);
      setAllowedModels(asList(payload.allowed_models).map(String).join(", "));
      setAllowedProviders(asList(payload.allowed_providers).map(String).join(", "));
      const next: Record<string, string> = {};
      for (const field of policyFields) {
        const value = numberValue(payload[field.key]);
        next[field.key] = value ? String(value) : "";
      }
      setNumbers(next);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Project policy could not load.");
    }
  };
  useEffect(() => { void load(projectID); }, [projectID]);

  const save = async (event: Event) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body: JSONRecord = {
        allowed_models: allowedModels.split(",").map((value) => value.trim()).filter(Boolean),
        allowed_providers: allowedProviders.split(",").map((value) => value.trim()).filter(Boolean),
      };
      for (const field of policyFields) {
        const raw = (numbers[field.key] ?? "").trim();
        const value = raw ? Number(raw) : 0;
        if (raw && (!Number.isFinite(value) || value < 0)) {
          setError(`${field.label} must be a non-negative number.`);
          setBusy(false);
          return;
        }
        body[field.key] = value;
      }
      await sendJSON<JSONRecord>("admin", `/projects/${encodeURIComponent(projectID)}/policy`, "POST", body);
      onSaved("Project policy saved. Empty fields mean no limit.");
      await load(projectID);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Project policy could not be saved.");
    } finally { setBusy(false); }
  };

  if (!projects.length) {
    return <EmptyState title="No projects yet" detail="Create a project on the Access page, then set its budgets and allowlists here." />;
  }
  return (
    <form class="policy-editor" onSubmit={save}>
      <label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
      {error ? <p class="form-error" role="alert">{error}</p> : null}
      {policy === null && !error ? <p class="muted-copy"><LoaderCircle class="spin" size={15} /> Loading policy…</p> : policy !== null ? <>
        <label>Allowed models (comma-separated, empty = all)<input value={allowedModels} onInput={(event) => setAllowedModels((event.currentTarget as HTMLInputElement).value)} placeholder="cat-coding, copilot/gpt-4o-mini" /></label>
        <label>Allowed providers (comma-separated, empty = all)<input value={allowedProviders} onInput={(event) => setAllowedProviders((event.currentTarget as HTMLInputElement).value)} placeholder="copilot, openai" /></label>
        <div class="policy-editor__grid">
          {policyFields.map((field) => <label key={field.key}>{field.label}<input inputMode="numeric" value={numbers[field.key] ?? ""} onInput={(event) => setNumbers((current) => ({ ...current, [field.key]: (event.currentTarget as HTMLInputElement).value }))} placeholder="No limit" /><small>{field.help}</small></label>)}
        </div>
        <footer><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <Save size={16} />} Save project policy</button></footer>
      </> : null}
    </form>
  );
}

export function Settings({ data, mode, onNavigate }: { data: JSONRecord; mode: ConsoleMode; onNavigate: (page: PageID) => void }) {
  const [audit, setAudit] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const loadAudit = async () => {
    try { setError(""); setAudit(await getJSON<JSONRecord>(mode, "/audit?limit=30")); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Audit log could not load."); }
  };
  useEffect(() => { void loadAudit(); }, [mode]);
  const sso = asRecord(data.sso);
  const memberships = asList(data.memberships).map(asRecord);
  const principals = asList(data.principals).map(asRecord);
  const projects = asList(data.projects).map(asRecord).filter((project) => stringValue(project.status, "active") === "active");
  const events = asList(audit?.events).map(asRecord);
  const ssoEnabled = sso.enabled === true;
  return (
    <div class="page-stack">
      <PageHeading eyebrow="Governance" title="Settings" detail="Project budgets and allowlists, authentication posture, and the immutable audit history." actions={<button class="button button--secondary" type="button" onClick={() => void loadAudit()}><RefreshCw size={16} /> Refresh audit</button>} />
      {notice ? <section class="action-notice action-notice--success" role="status"><strong>Saved</strong><span>{notice}</span></section> : null}
      <section class="settings-grid">
        <article class="surface"><ShieldCheck size={20} /><p class="eyebrow">Authentication</p><h2>{mode === "portal" ? "Private portal session" : ssoEnabled ? "Single sign-on" : "Gateway administrator access"}</h2><p>Mutation requests require same-origin validation. Provider credentials and API-key values are never represented in settings state. SSO and encryption settings are environment-driven — see the deployment guide.</p></article>
        <article class="surface"><Users size={20} /><p class="eyebrow">Identity</p><h2>{mode === "portal" ? `${memberships.length} memberships` : `${principals.length} principals`}</h2><p>Principals, projects, and memberships are managed on the Access page.</p>{mode === "admin" ? <button class="button button--secondary" type="button" onClick={() => onNavigate("access")}><Users size={15} /> Open Access</button> : null}</article>
      </section>
      {mode === "admin" ? <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Project policy</p><h2>Budgets and allowlists</h2></div><span>{projects.length} active project{projects.length === 1 ? "" : "s"}</span></div>
        <p class="muted-copy">Limits apply to every key minted in the project. Empty fields mean no limit; allowlists restrict which models and providers project keys may use.</p>
        <ProjectPolicyEditor projects={projects} onSaved={setNotice} />
      </section> : null}
      <section class="surface"><div class="section-heading"><div><p class="eyebrow">Audit</p><h2>Immutable operational activity</h2></div><span class="status-pill status-pill--muted">Secret-free records</span></div>{error ? <ErrorState title="Audit log is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void loadAudit()}>Retry</button>} /> : audit === null ? <LoadingState title="Loading audit records" /> : events.length === 0 ? <EmptyState title="No audit records yet" detail="Governance and connection changes will appear here without secret values." /> : <div class="table-wrap"><table><thead><tr><th>Time</th><th>Action</th><th>Target</th><th>Result</th><th>Detail</th></tr></thead><tbody>{events.map((event) => <tr key={String(event.id)}><td class="technical">{new Date(Number(event.ts) * 1000).toLocaleString()}</td><td>{stringValue(event.action)}</td><td>{stringValue(event.target_type)} {stringValue(event.target_id)}</td><td><span class="status-pill status-pill--ready">{stringValue(event.result, "success")}</span></td><td class="technical">{JSON.stringify(safeDetail(event.detail))}</td></tr>)}</tbody></table></div>}</section>
    </div>
  );
}
