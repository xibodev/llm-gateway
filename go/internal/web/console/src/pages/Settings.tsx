import { useEffect, useState } from "preact/hooks";
import { LoaderCircle, RefreshCw, Save, ScrollText, ShieldCheck, Users } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import { auditTone, safeAuditDetail } from "../lib/activity";
import type { ConsoleMode } from "../lib/mode";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

function AnonymousProviderAutomation({ onSaved }: { onSaved: (message: string) => void }) {
  const [state, setState] = useState<JSONRecord | null>(null);
  const [selection, setSelection] = useState("inherit");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const load = async () => {
    setError("");
    setState(null);
    try {
      const payload = await getJSON<JSONRecord>("admin", "/settings/anonymous-provider-automation");
      setState(payload);
      setSelection(stringValue(payload.override, "inherit"));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Provider automation setting could not load.");
    }
  };
  useEffect(() => { void load(); }, []);
  const save = async (event: Event) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const payload = await sendJSON<JSONRecord>("admin", "/settings/anonymous-provider-automation", "POST", { override: selection });
      setState(payload);
      setSelection(stringValue(payload.override, "inherit"));
      onSaved(payload.effective === true
        ? "Automatic anonymous providers are on. Connection and verification continue in the background."
        : "Automatic anonymous providers are off. Existing provider configuration was not removed.");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Provider automation setting could not be saved.");
    } finally { setBusy(false); }
  };
  const effective = state?.effective === true;
  const deploymentDefault = state?.deployment_default === true;
  return <section class="surface provider-automation" aria-busy={busy}>
    <div class="section-heading"><div><p class="eyebrow">Provider automation</p><h2>Automatic anonymous providers</h2></div>{state ? <span class={`status-pill ${effective ? "status-pill--ready" : "status-pill--muted"}`}>Effective: {effective ? "On" : "Off"}</span> : null}</div>
    <p id="anonymous-provider-automation-help" class="muted-copy">When on, the gateway adds only reviewed no-key providers and runs one minimal completion for each at startup and about every 24 hours. It never creates credentials, changes routes, or replaces an existing provider.</p>
    {error ? <ErrorState title="Provider automation setting is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load()}>Retry</button>} /> : state === null ? <LoadingState title="Loading provider automation setting" /> : <form class="provider-automation__form" onSubmit={save}>
      {state.environment_valid === false ? <p class="form-error" role="alert">The deployment environment value is invalid, so inherited automation is safely Off.</p> : null}
      <label>Connection and verification<select value={selection} disabled={busy} aria-describedby="anonymous-provider-automation-help anonymous-provider-automation-source" onInput={(event) => setSelection((event.currentTarget as HTMLSelectElement).value)}><option value="inherit">Use deployment default ({deploymentDefault ? "On" : "Off"})</option><option value="on">On</option><option value="off">Off</option></select></label>
      <p id="anonymous-provider-automation-source" class="form-help">Deployment default: <code>LLMGW_ANONYMOUS_PROVIDER_AUTOMATION</code>. Turning this off leaves existing providers configured.</p>
      <button class="button button--primary" type="submit" disabled={busy || selection === stringValue(state.override, "inherit")}>{busy ? <LoaderCircle class="spin" size={16} /> : <Save size={16} />} {busy ? "Saving…" : "Save automation setting"}</button>
    </form>}
  </section>;
}

export function Settings({ data, mode, onNavigate }: { data: JSONRecord; mode: ConsoleMode; onNavigate: (page: PageID) => void }) {
  const isAdmin = mode === "admin";
  const [audit, setAudit] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // Administrators search the whole history on the audit log page; the portal
  // lists the signed-in person's own events here.
  const loadAudit = async () => {
    try { setError(""); setAudit(await getJSON<JSONRecord>(mode, "/audit?limit=30")); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Audit log could not load."); }
  };
  useEffect(() => { if (!isAdmin) void loadAudit(); }, [mode]);
  const sso = asRecord(data.sso);
  const memberships = asList(data.memberships).map(asRecord);
  const principals = asList(data.principals).map(asRecord);
  const events = asList(audit?.events).map(asRecord);
  const ssoEnabled = sso.enabled === true;
  return (
    <div class="page-stack">
      <PageHeading eyebrow="Governance" title="Settings" detail="Gateway-wide options: authentication posture, provider automation, and retained audit history." actions={isAdmin ? undefined : <button class="button button--secondary" type="button" onClick={() => void loadAudit()}><RefreshCw size={16} /> Refresh audit</button>} />
      {notice ? <section class="action-notice action-notice--success" role="status"><strong>Saved</strong><span>{notice}</span></section> : null}
      <section class="settings-grid">
        <article class="surface"><ShieldCheck size={20} /><p class="eyebrow">Authentication</p><h2>{mode === "portal" ? "Private portal session" : ssoEnabled ? "Single sign-on" : "Gateway administrator access"}</h2><p>Mutation requests require same-origin validation. Provider credentials and API-key values are never represented in settings state. SSO and encryption settings are environment-driven — see the deployment guide.</p></article>
        <article class="surface"><Users size={20} /><p class="eyebrow">Identity</p><h2>{mode === "portal" ? `${memberships.length} memberships` : `${principals.length} principals`}</h2><p>Principals, projects, and memberships are managed on the Access page, where each project's page holds its budgets and allowlists.</p>{mode === "admin" ? <button class="button button--secondary" type="button" onClick={() => onNavigate("access")}><Users size={15} /> Open Access</button> : null}</article>
      </section>
      {mode === "admin" ? <AnonymousProviderAutomation onSaved={setNotice} /> : null}
      {isAdmin ? <section class="surface"><div class="section-heading"><div><p class="eyebrow">Audit</p><h2>Operational activity</h2></div><span class="status-pill status-pill--muted">Secret-free records</span></div><p class="muted-copy">Search the retained audit history by action, actor, result and target, and page back through it.</p><button class="button button--secondary" type="button" onClick={() => onNavigate("audit")}><ScrollText size={15} /> Open audit log</button></section>
        : <section class="surface"><div class="section-heading"><div><p class="eyebrow">Audit</p><h2>Your operational activity</h2></div><span class="status-pill status-pill--muted">Secret-free records</span></div>{error ? <ErrorState title="Audit log is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void loadAudit()}>Retry</button>} /> : audit === null ? <LoadingState title="Loading audit records" /> : events.length === 0 ? <EmptyState title="No audit records yet" detail="Governance and connection changes will appear here without secret values." /> : <div class="table-wrap"><table><thead><tr><th>Time</th><th>Action</th><th>Target</th><th>Result</th><th>Detail</th></tr></thead><tbody>{events.map((event) => <tr key={String(event.id)}><td class="technical">{new Date(Number(event.ts) * 1000).toLocaleString()}</td><td>{stringValue(event.action)}</td><td>{stringValue(event.target_type)} {stringValue(event.target_id)}</td><td><span class={`status-pill status-pill--${auditTone(stringValue(event.result, "success"))}`}>{stringValue(event.result, "success")}</span></td><td class="technical">{JSON.stringify(safeAuditDetail(event.detail))}</td></tr>)}</tbody></table></div>}</section>}
    </div>
  );
}
