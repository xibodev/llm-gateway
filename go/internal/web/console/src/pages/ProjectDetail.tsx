import { useEffect, useRef, useState } from "preact/hooks";
import { ArrowLeft, KeyRound, LoaderCircle, RefreshCw, Save, Trash2, Users } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import { quotaValueFromDraft } from "../lib/key-policy";
import { formatKeyTime, keyTimes } from "../lib/key-dates";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { formatUsageMetric, usageMetricTotal, usageTotals } from "../lib/usage";
import { EmptyState, ErrorState, LoadingState } from "../components/PageState";
import { AddMembershipDialog } from "../components/AddMembershipDialog";
import { ShortID, dataTable, serverTableView, tablePageSizes, useTableView, type ServerTableState, type TableColumn } from "../components/DataTable";
import { keysPath } from "../lib/directory";
import { LimitUsageTable } from "../components/LimitUsage";

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
  { key: "daily_credits_milli", label: "Daily model credits (milli-credits)", help: "Model-credit budget per UTC day; 1,000 milli-credits = 1 credit" },
  { key: "monthly_credits_milli", label: "Monthly model credits (milli-credits)", help: "Model-credit budget per calendar month; 1,000 milli-credits = 1 credit" },
];

// ProjectPolicyEditor edits one project's budgets and allowlists.
export function ProjectPolicyEditor({ projectID, onSaved }: { projectID: string; onSaved: (message: string) => void }) {
  const [policy, setPolicy] = useState<JSONRecord | null>(null);
  const [allowedModels, setAllowedModels] = useState("");
  const [allowedProviders, setAllowedProviders] = useState("");
  const [numbers, setNumbers] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // Each load supersedes the one before. A late response for a project the
  // operator already left must not fill the form shown for the next project,
  // where saving would write the first project's limits to the second.
  const loadRequest = useRef(0);

  const load = async (targetID: string) => {
    const request = ++loadRequest.current;
    if (!targetID) { setPolicy(null); return; }
    setError("");
    setPolicy(null);
    try {
      const payload = await getJSON<JSONRecord>("admin", `/projects/${encodeURIComponent(targetID)}/policy`);
      if (request !== loadRequest.current) return;
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
      if (request !== loadRequest.current) return;
      setError(cause instanceof Error ? cause.message : "Project policy could not load.");
    }
  };
  useEffect(() => { void load(projectID); }, [projectID]);

  const save = async (event: Event) => {
    event.preventDefault();
    if (busy) return;
    const body: JSONRecord = {
      allowed_models: allowedModels.split(",").map((value) => value.trim()).filter(Boolean),
      allowed_providers: allowedProviders.split(",").map((value) => value.trim()).filter(Boolean),
    };
    for (const field of policyFields) {
      const value = quotaValueFromDraft(numbers[field.key]);
      if (value === null) {
        setError(`${field.label} must be a nonnegative whole number.`);
        return;
      }
      body[field.key] = value;
    }
    // The project saved is the one whose policy the form shows.
    const target = projectID;
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", `/projects/${encodeURIComponent(target)}/policy`, "POST", body);
      onSaved("Project policy saved. Empty fields mean no limit.");
      await load(target);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Project policy could not be saved.");
    } finally { setBusy(false); }
  };

  return (
    <form class="policy-editor" onSubmit={save}>
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

const usageDays = 30;

// keyState is the status a key's row shows: revoked, expired, disabled or
// active.
function keyState(key: JSONRecord): string {
  if (stringValue(key.status) === "revoked") return "revoked";
  const expires = keyTimes(key).expires;
  if (key.expired === true || (expires > 0 && expires * 1000 <= Date.now())) return "expired";
  return stringValue(key.status, "active");
}

// ProjectDetail is one project's page: its limits with their usage and the
// policy that sets them, its keys, its members and its recent usage.
export function ProjectDetail({ projectID, data, onChanged, onNavigate, onBack }: {
  projectID: string; data: JSONRecord; onChanged: () => Promise<void>;
  onNavigate: (page: PageID, detail?: string) => void; onBack: () => void;
}) {
  const [notice, setNotice] = useState<{ success: boolean; detail: string } | null>(null);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState("");
  const [limits, setLimits] = useState<JSONRecord[] | null>(null);
  const [limitsError, setLimitsError] = useState("");
  const [usage, setUsage] = useState<JSONRecord | null>(null);
  const [usageError, setUsageError] = useState("");
  const path = encodeURIComponent(projectID);
  const loadLimits = async () => {
    setLimitsError("");
    try { setLimits(asList((await getJSON<JSONRecord>("admin", `/projects/${path}/limits`)).limits).map(asRecord)); }
    catch (cause) { setLimitsError(cause instanceof Error ? cause.message : "The project's limits could not load."); }
  };
  const loadUsage = async () => {
    setUsageError("");
    const to = Math.floor(Date.now() / 1000) + 1;
    const query = new URLSearchParams({ bucket: "day", from: String(to - usageDays * 24 * 60 * 60), to: String(to), project_id: projectID });
    try { setUsage(await getJSON<JSONRecord>("admin", `/usage?${query.toString()}`)); }
    catch (cause) { setUsageError(cause instanceof Error ? cause.message : "The project's usage could not load."); }
  };
  useEffect(() => { void loadLimits(); void loadUsage(); }, [projectID]);
  // The server pages and sorts the project's keys, of every status, newest
  // first unless a header asks otherwise.
  const [keyListing, setKeyListing] = useState<{ keys: JSONRecord[]; total: number } | null>(null);
  const [keyError, setKeyError] = useState("");
  const [keyTable, setKeyTable] = useState<ServerTableState>({ page: 0, pageSize: tablePageSizes[0], sort: null });
  const keyRequest = useRef(0);
  const keyPath = keysPath({ projectID, status: "all", limit: keyTable.pageSize, offset: keyTable.page * keyTable.pageSize, sort: keyTable.sort?.id, descending: keyTable.sort?.descending });
  useEffect(() => {
    const request = ++keyRequest.current;
    setKeyError("");
    getJSON<JSONRecord>("admin", keyPath).then((payload) => {
      if (request === keyRequest.current) setKeyListing({ keys: asList(payload.keys).map(asRecord), total: numberValue(payload.total) });
    }).catch((cause) => {
      if (request === keyRequest.current) setKeyError(cause instanceof Error ? cause.message : "The project's keys could not load.");
    });
  }, [keyPath]);
  const keys = keyListing?.keys ?? [];
  const keyTotal = keyListing?.total ?? 0;

  const project = asList(data.projects).map(asRecord).find((candidate) => stringValue(candidate.id) === projectID);
  const memberships = asList(data.memberships).map(asRecord).filter((membership) => stringValue(membership.project_id) === projectID);
  // Memberships name their principal.
  const principalName = (id: string) => stringValue(memberships.find((membership) => stringValue(membership.principal_id) === id)?.principal_name, id);
  const name = project ? stringValue(project.name, stringValue(project.slug, projectID)) : projectID;
  const removeMembership = async (principalID: string) => {
    setBusy(`member-${principalID}`);
    try {
      await sendJSON<JSONRecord>("admin", `/memberships?project_id=${path}&principal_id=${encodeURIComponent(principalID)}`, "DELETE");
      setNotice({ success: true, detail: `${principalName(principalID)} was removed from the project.` });
      await onChanged();
    } catch (cause) {
      setNotice({ success: false, detail: cause instanceof Error ? cause.message : "The membership could not be removed." });
    } finally { setBusy(""); }
  };
  const keyColumns: TableColumn<JSONRecord>[] = [
    { id: "name", header: "Name", sortable: true, cell: (key) => stringValue(key.name, "Gateway key") },
    { id: "prefix", header: "Prefix", class: "technical", cell: (key) => stringValue(key.prefix, "—") },
    { id: "owner", header: "Owner", sortable: true, cell: (key) => stringValue(key.principal, stringValue(key.principal_id)) },
    {
      id: "status", header: "Status", sortable: true, cell: (key) => {
        const state = keyState(key);
        return <span class={`status-pill ${state === "active" ? "status-pill--ready" : state === "disabled" ? "status-pill--muted" : "status-pill--attention"}`}>{state}</span>;
      },
    },
    { id: "last_used", header: "Last used", class: "technical", sortable: true, cell: (key) => formatKeyTime(keyTimes(key).lastUsed, "Never") },
  ];
  const memberColumns: TableColumn<JSONRecord>[] = [
    { id: "principal", header: "Principal", sortValue: (membership) => principalName(stringValue(membership.principal_id)), cell: (membership) => principalName(stringValue(membership.principal_id)) },
    { id: "kind", header: "Kind", cell: (membership) => stringValue(membership.principal_kind, "—") },
    { id: "role", header: "Role", sortValue: (membership) => stringValue(membership.role), cell: (membership) => stringValue(membership.role) },
    {
      id: "actions", header: "Actions", cell: (membership) => {
        const principalID = stringValue(membership.principal_id);
        return <div class="row-actions"><button class="icon-button icon-button--compact" type="button" aria-label={`Remove ${principalName(principalID)} from ${name}`} title="Remove" disabled={busy === `member-${principalID}`} onClick={() => void removeMembership(principalID)}><Trash2 size={14} /></button></div>;
      },
    },
  ];
  const keyView = serverTableView(keys, keyTotal, keyTable, setKeyTable);
  const memberView = useTableView(memberships, memberColumns);
  const back = <nav class="detail-breadcrumb"><button class="button button--secondary" type="button" onClick={onBack}><ArrowLeft size={16} /> Access</button></nav>;
  if (!project) {
    return <div class="page-stack">{back}<EmptyState title="Unknown project" detail="This project does not exist. Projects are listed on the Access page." /></div>;
  }
  const activePrincipals = numberValue(asRecord(data.counts).active_principals);
  const status = stringValue(project.status, "active");
  const series = asList(usage?.series).map(asRecord);
  const totals = usageTotals(series);
  const keyUsage = asList(asRecord(asRecord(usage?.control_plane).groups).key).map(asRecord);


  return <div class="page-stack">
    {back}
    <header class="detail-heading surface">
      <div class="detail-heading__body">
        <div class="detail-heading__title"><h1>{name}</h1><span class={`status-pill ${status === "active" ? "status-pill--ready" : "status-pill--muted"}`}>{status}</span></div>
        <p>A project scopes API keys, budgets and memberships. Its limits apply to every key in it, together with each key's own.</p>
        <dl class="compact-facts">
          <div><dt>Slug</dt><dd class="technical">{stringValue(project.slug, "—")}</dd></div>
          <div><dt>ID</dt><dd><ShortID id={projectID} label="project ID" /></dd></div>
          <div><dt>Keys</dt><dd>{keyTotal}</dd></div>
          <div><dt>Members</dt><dd>{memberships.length}</dd></div>
        </dl>
      </div>
      <div class="detail-heading__actions">
        <button class="button button--primary" type="button" onClick={() => onNavigate("keys", `project=${path}`)}><KeyRound size={15} /> Manage keys</button>
        <button class="button button--secondary" type="button" disabled={!activePrincipals} title={activePrincipals ? undefined : "Create an active principal first"} onClick={() => setAdding(true)}><Users size={15} /> Add member</button>
      </div>
    </header>
    {notice ? <section class={`action-notice ${notice.success ? "action-notice--success" : "action-notice--warning"}`} role="status"><strong>{notice.success ? "Saved" : "Not saved"}</strong><span>{notice.detail}</span></section> : null}
    <section class="surface">
      <div class="section-heading"><div><p class="eyebrow">Limits</p><h2>What the project's windows have used</h2></div><button class="button button--secondary" type="button" onClick={() => void loadLimits()}><RefreshCw size={15} /> Refresh</button></div>
      {limitsError ? <ErrorState title="The project's limits are unavailable" detail={limitsError} action={<button class="button button--secondary" type="button" onClick={() => void loadLimits()}>Retry</button>} />
        : limits === null ? <LoadingState title="Loading limits" />
        : <LimitUsageTable limits={limits} empty="This project sets no limit. Its keys' own limits and the gateway's per-caller limit still apply." />}
      <h3 class="policy-editor__heading">Budgets and allowlists</h3>
      <p class="muted-copy">Limits apply to every key minted in the project. Empty fields mean no limit; allowlists restrict which models and providers project keys may use.</p>
      <ProjectPolicyEditor projectID={projectID} onSaved={(detail) => { setNotice({ success: true, detail }); void loadLimits(); }} />
    </section>
    <section class="surface">
      <div class="section-heading"><div><p class="eyebrow">Keys</p><h2>Keys in this project</h2></div><span>{keyTotal} key{keyTotal === 1 ? "" : "s"}</span></div>
      {keyError ? <ErrorState title="The project's keys are unavailable" detail={keyError} />
        : keyListing === null ? <LoadingState title="Loading keys" />
        : keyTotal === 0 ? <EmptyState title="No keys in this project" detail="Create one from Manage keys." />
        : dataTable(keyView, keyColumns, { label: `Keys in ${name}`, rowKey: (key) => stringValue(key.id) })}
    </section>
    <section class="surface">
      <div class="section-heading"><div><p class="eyebrow">Members</p><h2>Who can act in this project</h2></div><span>{memberships.length} member{memberships.length === 1 ? "" : "s"}</span></div>
      {memberships.length === 0 ? <EmptyState title="No members yet" detail="Add a principal so it can hold keys and run project-attributed requests." /> : dataTable(memberView, memberColumns, { label: `Members of ${name}`, rowKey: (membership) => stringValue(membership.principal_id) })}
    </section>
    <section class="surface table-wrap">
      <div class="section-heading"><div><p class="eyebrow">Usage</p><h2>The last {usageDays} days</h2></div><button class="button button--secondary" type="button" onClick={() => void loadUsage()}><RefreshCw size={15} /> Refresh</button></div>
      {usageError ? <ErrorState title="The project's usage is unavailable" detail={usageError} action={<button class="button button--secondary" type="button" onClick={() => void loadUsage()}>Retry</button>} />
        : usage === null ? <LoadingState title="Loading usage" />
        : <>
          <dl class="compact-facts">
            <div><dt>Requests</dt><dd>{totals.requests.toLocaleString()}</dd></div>
            <div><dt>Failed</dt><dd>{totals.errors.toLocaleString()}</dd></div>
            <div><dt>Tokens</dt><dd>{usageMetricTotal(series, "tokens").toLocaleString()}</dd></div>
            <div><dt>Estimated cost</dt><dd>{formatUsageMetric(usageMetricTotal(series, "cost"), "cost")}</dd></div>
          </dl>
          {keyUsage.length === 0 ? <p class="muted-copy">No recorded usage in this project in the last {usageDays} days.</p> : <table><thead><tr><th>Key</th><th>Requests</th><th>Failed</th><th>Tokens</th><th>Estimated cost</th></tr></thead><tbody>{keyUsage.map((row, index) => <tr key={`${stringValue(row.key_id)}-${index}`}><td>{stringValue(row.key_name) || stringValue(row.key_id) || "Playground or external keys"}</td><td>{numberValue(row.requests).toLocaleString()}</td><td>{numberValue(row.errors).toLocaleString()}</td><td>{(numberValue(row.input_tokens) + numberValue(row.output_tokens)).toLocaleString()}</td><td>{formatUsageMetric(numberValue(row.cost_microusd), "cost")}</td></tr>)}</tbody></table>}
        </>}
    </section>
    {adding ? <AddMembershipDialog project={project} onClose={() => setAdding(false)} onCreated={onChanged} /> : null}
  </div>;
}
