import { useEffect, useRef, useState } from "preact/hooks";
import { RefreshCw } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import { auditActor, auditPath, auditTone, emptyAuditFilter, nextCursor, safeAuditDetail, type AuditFilter } from "../lib/activity";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

// AuditLog lists the retained audit history, newest first, filtered and
// paged, with each event's actor and secret-free detail.
export function AuditLog({ mode }: { mode: ConsoleMode }) {
  const [draft, setDraft] = useState<AuditFilter>(emptyAuditFilter);
  const [applied, setApplied] = useState<AuditFilter>(emptyAuditFilter);
  const [events, setEvents] = useState<JSONRecord[] | null>(null);
  const [cursor, setCursor] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // A page that arrives after a newer search must not mix into its events.
  const search = useRef(0);

  const load = async (filter: AuditFilter, beforeID = 0) => {
    const request = beforeID ? search.current : ++search.current;
    setBusy(true);
    setError("");
    try {
      const listing = await getJSON<JSONRecord>(mode, auditPath(filter, beforeID));
      if (request !== search.current) return;
      const page = asList(listing.events).map(asRecord);
      setEvents((current) => beforeID && current ? [...current, ...page] : page);
      setCursor(nextCursor(listing));
      setApplied(filter);
    } catch (cause) {
      if (request === search.current) setError(cause instanceof Error ? cause.message : "Audit history could not load.");
    } finally {
      if (request === search.current) setBusy(false);
    }
  };
  useEffect(() => { void load(emptyAuditFilter); }, [mode]);
  const update = (field: keyof AuditFilter) => (event: Event) => {
    const value = (event.currentTarget as HTMLInputElement | HTMLSelectElement).value;
    setDraft((current) => ({ ...current, [field]: value }));
  };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Governance" title="Audit log" detail="Administrative and connection changes from the retained audit history, newest first. Records never hold secret values." actions={<button class="button button--secondary" type="button" onClick={() => void load(applied)}><RefreshCw size={16} /> Refresh</button>} />
      <form class="usage-filter surface activity-filter" onSubmit={(event) => { event.preventDefault(); void load(draft); }}>
        <label>Action<input value={draft.action} onInput={update("action")} placeholder="Prefix, such as provider." /></label>
        <label>Actor<input value={draft.actor} onInput={update("actor")} placeholder="Principal or key ID" /></label>
        <label>Result<select value={draft.result} onInput={update("result")}><option value="all">All results</option><option value="success">Success</option><option value="failure">Failure</option><option value="denied">Denied</option></select></label>
        <label>Target type<input value={draft.targetType} onInput={update("targetType")} placeholder="provider, key, project…" /></label>
        <label>Target ID<input value={draft.targetID} onInput={update("targetID")} placeholder="Exact ID" /></label>
        <button class="button button--primary" type="submit" disabled={busy}>Search</button>
      </form>
      {error ? <ErrorState title="Audit history is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load(applied)}>Retry</button>} />
        : events === null ? <LoadingState title="Loading audit records" />
        : events.length === 0 ? <EmptyState title="No audit records" detail="No retained event matches these filters." />
        : <section class="surface table-wrap">
          <table>
            <thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Result</th><th>Detail</th></tr></thead>
            <tbody>{events.map((event) => {
              const result = stringValue(event.result, "success");
              const detail = safeAuditDetail(event.detail);
              return <tr key={String(event.id)}>
                <td class="technical">{new Date(numberValue(event.ts) * 1000).toLocaleString()}</td>
                <td class="technical">{auditActor(event)}</td>
                <td>{stringValue(event.action)}</td>
                <td class="technical">{[stringValue(event.target_type), stringValue(event.target_id)].filter(Boolean).join(" ")}</td>
                <td><span class={`status-pill status-pill--${auditTone(result)}`}>{result}</span></td>
                <td>{Object.keys(detail).length ? <details><summary>Detail</summary><pre class="technical">{JSON.stringify(detail, null, 2)}</pre></details> : "—"}</td>
              </tr>;
            })}</tbody>
          </table>
          {cursor ? <footer class="activity-more"><button class="button button--secondary" type="button" disabled={busy} onClick={() => void load(applied, cursor)}>Load older events</button></footer> : null}
        </section>}
    </div>
  );
}
