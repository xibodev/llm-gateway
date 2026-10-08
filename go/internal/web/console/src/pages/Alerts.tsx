import { useEffect, useRef, useState } from "preact/hooks";
import {
  BellRing,
  CirclePlay,
  Plus,
  Power,
  RefreshCw,
  Trash2,
  X,
} from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import { principalSearch } from "../lib/directory";
import { deliveriesPath, deliveryKindLabel, deliveryKinds, deliveryState, emptyDeliveryFilter, nextAttemptLabel, nextCursor, type DeliveryFilter } from "../lib/activity";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { RemoteSearchSelect, SearchSelect, dataTable, useTableView, type TableColumn } from "../components/DataTable";
import {
  EmptyState,
  ErrorState,
  LoadingState,
  PageHeading,
} from "../components/PageState";

const metricOptions = [
  ["requests", "Requests"],
  ["input_tokens", "Input tokens"],
  ["output_tokens", "Output tokens"],
  ["total_tokens", "Total tokens"],
  ["cost_microusd", "Estimated cost"],
  ["credits_milli", "Model credits"],
];

function kindLabel(kind: string): string {
  return kind === "key_expiry" ? "Key expiry" : "Quota usage";
}

function metricLabel(metric: string): string {
  return metricOptions.find(([value]) => value === metric)?.[1] ?? metric.replaceAll("_", " ");
}

export function Alerts({ data }: { data: JSONRecord }) {
  const projects = asList(data.projects).map(asRecord);
  const [rules, setRules] = useState<JSONRecord[] | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState("");
  const [creating, setCreating] = useState(false);
  const [kind, setKind] = useState("quota_usage");
  const [metric, setMetric] = useState("total_tokens");
  const [threshold, setThreshold] = useState("80");
  const [period, setPeriod] = useState("month");
  const [projectID, setProjectID] = useState("");
  const [principalID, setPrincipalID] = useState("");
  const [deliveryDraft, setDeliveryDraft] = useState<DeliveryFilter>(emptyDeliveryFilter);
  const [deliveryFilter, setDeliveryFilter] = useState<DeliveryFilter>(emptyDeliveryFilter);
  const [deliveries, setDeliveries] = useState<JSONRecord[] | null>(null);
  const [deliveryCursor, setDeliveryCursor] = useState(0);
  const [deliveryError, setDeliveryError] = useState("");
  const [deliveryBusy, setDeliveryBusy] = useState(false);
  // A page that arrives after a newer search must not mix into its rows.
  const deliverySearch = useRef(0);

  const load = async () => {
    try {
      setError("");
      const response = await getJSON<JSONRecord>("admin", "/alerts");
      setRules(asList(response.rules).map(asRecord));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert rules could not load.");
    }
  };

  const loadDeliveries = async (filter: DeliveryFilter, beforeID = 0) => {
    const search = beforeID ? deliverySearch.current : ++deliverySearch.current;
    setDeliveryBusy(true);
    setDeliveryError("");
    try {
      const listing = await getJSON<JSONRecord>("admin", deliveriesPath(filter, beforeID));
      if (search !== deliverySearch.current) return;
      const page = asList(listing.deliveries).map(asRecord);
      setDeliveries((current) => beforeID && current ? [...current, ...page] : page);
      setDeliveryCursor(nextCursor(listing));
      setDeliveryFilter(filter);
    } catch (cause) {
      if (search === deliverySearch.current) setDeliveryError(cause instanceof Error ? cause.message : "Deliveries could not load.");
    } finally {
      if (search === deliverySearch.current) setDeliveryBusy(false);
    }
  };

  useEffect(() => { void load(); void loadDeliveries(emptyDeliveryFilter); }, []);

  const create = async (event: Event) => {
    event.preventDefault();
    const thresholdValue = Number(threshold);
    if (!Number.isInteger(thresholdValue) || thresholdValue <= 0) {
      setError(kind === "key_expiry" ? "Expiry warning days must be a positive whole number." : "Quota threshold must be a whole percentage from 1 to 100.");
      return;
    }
    if (kind === "quota_usage" && thresholdValue > 100) {
      setError("Quota threshold must be a whole percentage from 1 to 100.");
      return;
    }
    setBusy("create");
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/alerts", "POST", {
        kind,
        metric: kind === "key_expiry" ? "days" : metric,
        threshold: thresholdValue,
        period: kind === "key_expiry" ? "day" : period,
        project_id: projectID || undefined,
        principal_id: principalID || undefined,
      });
      setCreating(false);
      setMessage("Alert rule created.");
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert rule could not be created.");
    } finally {
      setBusy("");
    }
  };

  const setEnabled = async (rule: JSONRecord, enabled: boolean) => {
    const id = stringValue(rule.id);
    if (!id) return;
    setBusy(id);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/alerts/status", "POST", { id, enabled });
      setMessage(`Alert rule ${enabled ? "enabled" : "disabled"}.`);
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert status could not be updated.");
    } finally {
      setBusy("");
    }
  };

  const remove = async (rule: JSONRecord) => {
    const id = stringValue(rule.id);
    if (!id || !window.confirm("Delete this alert rule? Existing outbox events are retained.")) return;
    setBusy(id);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", `/alerts/${encodeURIComponent(id)}`, "DELETE");
      setMessage("Alert rule deleted.");
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert rule could not be deleted.");
    } finally {
      setBusy("");
    }
  };

  const evaluate = async () => {
    setBusy("evaluate");
    setError("");
    try {
      const response = await sendJSON<JSONRecord>("admin", "/alerts/evaluate", "POST", {});
      const enqueued = numberValue(response.enqueued);
      setMessage(`Evaluation completed. ${enqueued} notification${enqueued === 1 ? "" : "s"} enqueued.`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert evaluation failed.");
    } finally {
      setBusy("");
    }
  };

  const projectNames = new Map(projects.map((project) => [
    stringValue(project.id),
    stringValue(project.name, stringValue(project.slug, stringValue(project.id))),
  ]));

  const ruleScope = (rule: JSONRecord) => {
    const project = stringValue(rule.project_id);
    const principal = stringValue(rule.principal_id);
    return [project ? projectNames.get(project) : "", principal ? stringValue(rule.principal_name, principal) : ""].filter(Boolean).join(" · ") || "All eligible keys";
  };
  const ruleCondition = (rule: JSONRecord) => stringValue(rule.kind) === "key_expiry"
    ? `${numberValue(rule.threshold)} day${numberValue(rule.threshold) === 1 ? "" : "s"} before expiry`
    : `${numberValue(rule.threshold)}% of ${stringValue(rule.period)} ${metricLabel(stringValue(rule.metric))}`;
  const ruleColumns: TableColumn<JSONRecord>[] = [
    { id: "type", header: "Type", sortValue: (rule) => kindLabel(stringValue(rule.kind)), cell: (rule) => kindLabel(stringValue(rule.kind)) },
    { id: "condition", header: "Condition", sortValue: ruleCondition, cell: ruleCondition },
    { id: "scope", header: "Scope", sortValue: ruleScope, cell: ruleScope },
    { id: "status", header: "Status", sortValue: (rule) => (rule.enabled === true ? "enabled" : "disabled"), cell: (rule) => <span class={`status-pill ${rule.enabled === true ? "status-pill--ready" : "status-pill--muted"}`}>{rule.enabled === true ? "enabled" : "disabled"}</span> },
    {
      id: "actions", header: "Actions", cell: (rule) => {
        const id = stringValue(rule.id);
        const enabled = rule.enabled === true;
        const label = `${kindLabel(stringValue(rule.kind))} alert`;
        return <div class="row-actions"><button class="icon-button icon-button--compact" type="button" aria-label={`${enabled ? "Disable" : "Enable"} ${label}`} title={enabled ? "Disable" : "Enable"} disabled={busy === id} onClick={() => void setEnabled(rule, !enabled)}><Power size={14} /></button><button class="icon-button icon-button--compact" type="button" aria-label={`Delete ${label}`} title="Delete" disabled={busy === id} onClick={() => void remove(rule)}><Trash2 size={14} /></button></div>;
      },
    },
  ];
  const ruleView = useTableView(rules ?? [], ruleColumns);

  return (
    <div class="page-stack">
      <PageHeading
        eyebrow="Operational notifications"
        title="Alerts"
        detail="Create quota and key-expiry rules, control delivery eligibility, and run scheduled evaluation without exposing notification credentials."
        actions={(
          <>
            <button class="button button--secondary" type="button" disabled={busy === "evaluate"} onClick={() => void evaluate()}>
              <CirclePlay size={16} /> Evaluate now
            </button>
            <button class="button button--secondary" type="button" onClick={() => { void load(); void loadDeliveries(deliveryFilter); }}>
              <RefreshCw size={16} /> Refresh
            </button>
            <button class="button button--primary" type="button" onClick={() => { setCreating(true); setError(""); }}>
              <Plus size={16} /> New rule
            </button>
          </>
        )}
      />
      {message ? <p class="route-message" role="status">{message}</p> : null}
      {error ? <ErrorState title="Alert action could not complete" detail={error} /> : null}
      {creating ? (
        <form class="surface alert-editor" onSubmit={create}>
          <header>
            <div><p class="eyebrow">New alert</p><h2>Define an operational threshold</h2></div>
            <button class="icon-button" type="button" aria-label="Close alert editor" onClick={() => setCreating(false)}><X size={17} /></button>
          </header>
          <div class="alert-editor__grid">
            <label>Rule type<select value={kind} onInput={(event) => setKind((event.currentTarget as HTMLSelectElement).value)}><option value="quota_usage">Quota usage</option><option value="key_expiry">Key expiry</option></select></label>
            {kind === "quota_usage" ? <label>Metric<select value={metric} onInput={(event) => setMetric((event.currentTarget as HTMLSelectElement).value)}>{metricOptions.map(([value, label]) => <option value={value} key={value}>{label}</option>)}</select></label> : null}
            <label>{kind === "key_expiry" ? "Warning days" : "Threshold percent"}<input inputMode="numeric" value={threshold} onInput={(event) => setThreshold((event.currentTarget as HTMLInputElement).value)} /></label>
            {kind === "quota_usage" ? <label>Period<select value={period} onInput={(event) => setPeriod((event.currentTarget as HTMLSelectElement).value)}><option value="day">Daily</option><option value="month">Monthly</option></select></label> : null}
            <SearchSelect label="Project scope" noun="projects" value={projectID} options={[{ value: "", label: "All projects" }, ...projects.map((project) => ({ value: stringValue(project.id), label: stringValue(project.name, stringValue(project.slug)) }))]} onChange={setProjectID} />
            <RemoteSearchSelect label="Principal scope" noun="principals" value={principalID} emptyLabel="All principals" {...principalSearch("admin", {})} onChange={setPrincipalID} />
          </div>
          <p class="form-help">{kind === "key_expiry" ? "The scheduled evaluator enqueues one notification per matching active key before it expires." : "Quota rules trigger against configured key or project limits; a missing limit cannot produce an alert."}</p>
          <footer><button class="button button--secondary" type="button" onClick={() => setCreating(false)}>Cancel</button><button class="button button--primary" type="submit" disabled={busy === "create"}><BellRing size={16} /> Create rule</button></footer>
        </form>
      ) : null}
      {rules === null && !error ? <LoadingState title="Loading alert rules" /> : null}
      {rules?.length === 0 ? <EmptyState title="No alert rules configured" detail="Create a quota warning or key-expiry rule to populate the notification outbox." /> : null}
      {rules?.length ? (
        <section class="surface">
          <div class="section-heading"><div><p class="eyebrow">Configured rules</p><h2>Notification eligibility</h2></div><span>{rules.length} rule{rules.length === 1 ? "" : "s"}</span></div>
          {dataTable(ruleView, ruleColumns, { label: "Alert rules", rowKey: (rule) => stringValue(rule.id) })}
        </section>
      ) : null}
      <section class="surface table-wrap">
        <div class="section-heading"><div><p class="eyebrow">Outbox</p><h2>Deliveries</h2></div><span>Delivered by an external worker, which claims each notification and retries a failure up to its attempt limit.</span></div>
        <form class="usage-filter activity-filter" onSubmit={(event) => { event.preventDefault(); void loadDeliveries(deliveryDraft); }}>
          <label>Status<select value={deliveryDraft.status} onInput={(event) => setDeliveryDraft((current) => ({ ...current, status: (event.currentTarget as HTMLSelectElement).value }))}><option value="all">All statuses</option><option value="pending">Pending</option><option value="failed">Failed, retrying</option><option value="exhausted">Failed, out of attempts</option><option value="delivered">Delivered</option></select></label>
          <label>Kind<select value={deliveryDraft.kind} onInput={(event) => setDeliveryDraft((current) => ({ ...current, kind: (event.currentTarget as HTMLSelectElement).value }))}><option value="all">All kinds</option>{deliveryKinds.map(([value, label]) => <option value={value} key={value}>{label}</option>)}</select></label>
          <button class="button button--primary" type="submit" disabled={deliveryBusy}>Search</button>
        </form>
        {deliveryError ? <ErrorState title="Deliveries are unavailable" detail={deliveryError} />
          : deliveries === null ? <LoadingState title="Loading deliveries" />
          : deliveries.length === 0 ? <EmptyState title="No deliveries" detail="No alert has been queued for delivery with these filters." />
          : <>
            <table>
              <thead><tr><th>Queued</th><th>Kind</th><th>Scope</th><th>Status</th><th>Attempts</th><th>Next attempt</th><th>Delivered</th><th>Last error</th></tr></thead>
              <tbody>{deliveries.map((delivery) => {
                const state = deliveryState(delivery);
                const project = stringValue(delivery.project_id);
                const principal = stringValue(delivery.principal_id);
                const scope = [project ? projectNames.get(project) ?? project : "", principal ? stringValue(delivery.principal_name, principal) : ""].filter(Boolean).join(" · ") || "—";
                const deliveredAt = numberValue(delivery.delivered_at);
                const lastError = stringValue(delivery.last_error);
                return <tr key={String(delivery.id)}>
                  <td class="technical">{new Date(numberValue(delivery.ts) * 1000).toLocaleString()}</td>
                  <td>{deliveryKindLabel(stringValue(delivery.kind))}</td>
                  <td>{scope}</td>
                  <td><span class={`status-pill status-pill--${state.tone}`}>{state.label}</span></td>
                  <td>{numberValue(delivery.attempts)} of {numberValue(delivery.max_attempts)}</td>
                  <td class="technical">{nextAttemptLabel(delivery)}</td>
                  <td class="technical">{deliveredAt ? new Date(deliveredAt * 1000).toLocaleString() : "—"}</td>
                  <td class="delivery-error" title={lastError || undefined}>{lastError || "—"}</td>
                </tr>;
              })}</tbody>
            </table>
            {deliveryCursor ? <footer class="activity-more"><button class="button button--secondary" type="button" disabled={deliveryBusy} onClick={() => void loadDeliveries(deliveryFilter, deliveryCursor)}>Load older deliveries</button></footer> : null}
          </>}
      </section>
    </div>
  );
}
