import { useEffect, useRef, useState } from "preact/hooks";
import { RefreshCw } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import { emptyRequestFilter, modelSummary, nextCursor, requestsPath, statusTone, tokenSummary, type RequestFilter } from "../lib/activity";
import { errorCodeLabel } from "../lib/limits";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

// Requests lists the requests the gateway recorded, newest first, with the
// failover chains it kept, so an operator can find what happened to the
// request a client reports by its X-Request-Id. In the portal it lists the
// signed-in user's own requests; failover chains span every caller, so only
// administrators see them.
export function Requests({ data, mode }: { data: JSONRecord; mode: ConsoleMode }) {
  const portal = mode === "portal";
  const [draft, setDraft] = useState<RequestFilter>(emptyRequestFilter);
  const [applied, setApplied] = useState<RequestFilter>(emptyRequestFilter);
  const [rows, setRows] = useState<JSONRecord[] | null>(null);
  const [cursor, setCursor] = useState(0);
  const [failovers, setFailovers] = useState<JSONRecord[]>([]);
  // The providers the user's requests used, which the portal listing names.
  const [usedProviders, setUsedProviders] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // A page that arrives after a newer search must not mix into its rows.
  const search = useRef(0);
  const providerIDs = portal ? usedProviders : asList(data.providers).map((item) => stringValue(asRecord(item).id));
  if (draft.provider !== "all" && !providerIDs.includes(draft.provider)) providerIDs.push(draft.provider);
  const keys = asList(data.keys).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const keyNames = new Map(keys.map((key) => [stringValue(key.id), stringValue(key.name, stringValue(key.prefix))]));
  const projectNames = new Map(projects.map((project) => [stringValue(project.id), stringValue(project.name, stringValue(project.slug))]));

  const load = async (filter: RequestFilter, beforeID = 0) => {
    const request = beforeID ? search.current : ++search.current;
    setBusy(true);
    setError("");
    try {
      const listing = await getJSON<JSONRecord>(mode, requestsPath(filter, beforeID));
      if (request !== search.current) return;
      const page = asList(listing.requests).map(asRecord);
      setRows((current) => beforeID && current ? [...current, ...page] : page);
      setCursor(nextCursor(listing));
      setApplied(filter);
      if (portal) setUsedProviders(asList(listing.providers).map((item) => stringValue(item)));
    } catch (cause) {
      if (request === search.current) setError(cause instanceof Error ? cause.message : "Requests could not load.");
    } finally {
      if (request === search.current) setBusy(false);
    }
  };
  const loadFailovers = async () => {
    if (portal) return;
    try { setFailovers(asList((await getJSON<JSONRecord>(mode, "/telemetry")).recent).map(asRecord)); }
    catch { setFailovers([]); }
  };
  useEffect(() => { void load(emptyRequestFilter); void loadFailovers(); }, [mode]);
  const update = (field: keyof RequestFilter) => (event: Event) => {
    const value = (event.currentTarget as HTMLInputElement | HTMLSelectElement).value;
    setDraft((current) => ({ ...current, [field]: value }));
  };
  const refresh = () => { void load(applied); void loadFailovers(); };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Traffic" title="Requests" detail={portal ? "Your requests the gateway recorded, newest first, kept for the usage retention window. Find one by the X-Request-Id its response carried." : "Requests the gateway recorded, newest first, kept for the usage retention window. Find one a client reports by the X-Request-Id its response carried."} actions={<button class="button button--secondary" type="button" onClick={refresh}><RefreshCw size={16} /> Refresh</button>} />
      <form class="usage-filter surface activity-filter" onSubmit={(event) => { event.preventDefault(); void load(draft); }}>
        <label>Status<select value={draft.status} onInput={update("status")}><option value="all">All statuses</option><option value="ok">Succeeded</option><option value="error">Failed</option></select></label>
        <label>Provider<select value={draft.provider} onInput={update("provider")}><option value="all">All providers</option>{providerIDs.map((id) => <option value={id} key={id}>{id}</option>)}</select></label>
        <label>Model<input value={draft.model} onInput={update("model")} placeholder="Exact routed model" /></label>
        <label>Key<select value={draft.keyID} onInput={update("keyID")}><option value="all">All keys</option>{keys.map((key) => <option value={stringValue(key.id)} key={stringValue(key.id)}>{stringValue(key.name, stringValue(key.prefix))}</option>)}</select></label>
        <label>Project<select value={draft.projectID} onInput={update("projectID")}><option value="all">All projects</option>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
        <label>Request ID<input value={draft.requestID} onInput={update("requestID")} placeholder="req_…" /></label>
        <button class="button button--primary" type="submit" disabled={busy}>Search</button>
      </form>
      {error ? <ErrorState title="Requests are unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={refresh}>Retry</button>} />
        : rows === null ? <LoadingState title="Loading requests" />
        : rows.length === 0 ? <EmptyState title="No recorded requests" detail="No request matches these filters in the retained history." />
        : <section class="surface table-wrap">
          <table>
            <thead><tr><th>Time</th><th>Request</th><th>Endpoint</th><th>Model</th><th>Provider</th><th>Caller</th><th>Status</th><th>Latency</th><th>Tokens</th></tr></thead>
            <tbody>{rows.map((row) => {
              const status = numberValue(row.status_code);
              const keyID = stringValue(row.key_id);
              const projectID = stringValue(row.project_id);
              return <tr key={String(row.id)}>
                <td class="technical">{new Date(numberValue(row.ts) * 1000).toLocaleString()}</td>
                <td class="technical" title={stringValue(row.request_id)}>{stringValue(row.request_id)}</td>
                <td>{stringValue(row.endpoint)}</td>
                <td class="technical">{modelSummary(row)}</td>
                <td>{stringValue(row.provider, "—")}</td>
                <td>{keyID ? stringValue(row.key_name) || (keyNames.get(keyID) ?? keyID) : projectID ? projectNames.get(projectID) ?? projectID : portal ? "Portal" : "Administrator or local"}</td>
                <td><span class={`status-pill status-pill--${statusTone(status)}`} title={stringValue(row.error_code) || undefined}>{status}{stringValue(row.error_code) ? ` ${errorCodeLabel(stringValue(row.error_code))}` : ""}</span></td>
                <td>{numberValue(row.latency_ms).toLocaleString()} ms</td>
                <td>{tokenSummary(row)}</td>
              </tr>;
            })}</tbody>
          </table>
          {cursor ? <footer class="activity-more"><button class="button button--secondary" type="button" disabled={busy} onClick={() => void load(applied, cursor)}>Load older requests</button></footer> : null}
        </section>}
      {portal ? null : <section class="surface table-wrap">
        <div class="section-heading"><div><p class="eyebrow">Failover</p><h2>Recent failover chains</h2></div><span>{failovers.length} kept</span></div>
        {failovers.length === 0 ? <p class="muted-copy">No request has needed more than one attempt or failed an attempt recently.</p> : <table>
          <thead><tr><th>Time</th><th>Requested</th><th>Served by</th><th>Attempts</th></tr></thead>
          <tbody>{failovers.map((chain, index) => <tr key={`${numberValue(chain.ts)}-${index}`}>
            <td class="technical">{new Date(numberValue(chain.ts) * 1000).toLocaleString()}</td>
            <td class="technical">{stringValue(chain.requested)}</td>
            <td class="technical">{stringValue(chain.served, "Nothing served")}</td>
            <td>{asList(chain.attempts).map(asRecord).map((attempt) => `${stringValue(attempt.provider)}/${stringValue(attempt.model)} ${attempt.ok === true ? "ok" : stringValue(attempt.error, "failed")}`).join("; ")}</td>
          </tr>)}</tbody>
        </table>}
      </section>}
    </div>
  );
}
