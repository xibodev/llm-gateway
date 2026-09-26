import { useEffect, useState } from "preact/hooks";
import {
  BellRing,
  CirclePlay,
  Plus,
  RefreshCw,
  Trash2,
  X,
} from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
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
  const principals = asList(data.principals).map(asRecord);
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

  const load = async () => {
    try {
      setError("");
      const response = await getJSON<JSONRecord>("admin", "/alerts");
      setRules(asList(response.rules).map(asRecord));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert rules could not load.");
    }
  };

  useEffect(() => { void load(); }, []);

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
  const principalNames = new Map(principals.map((principal) => [
    stringValue(principal.id),
    stringValue(principal.display_name, stringValue(principal.email, stringValue(principal.id))),
  ]));

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
            <button class="button button--secondary" type="button" onClick={() => void load()}>
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
            <label>Project scope<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}><option value="">All projects</option>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
            <label>Principal scope<select value={principalID} onInput={(event) => setPrincipalID((event.currentTarget as HTMLSelectElement).value)}><option value="">All principals</option>{principals.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.email, stringValue(principal.id)))}</option>)}</select></label>
          </div>
          <p class="form-help">{kind === "key_expiry" ? "The scheduled evaluator enqueues one notification per matching active key before it expires." : "Quota rules trigger against configured key or project limits; a missing limit cannot produce an alert."}</p>
          <footer><button class="button button--secondary" type="button" onClick={() => setCreating(false)}>Cancel</button><button class="button button--primary" type="submit" disabled={busy === "create"}><BellRing size={16} /> Create rule</button></footer>
        </form>
      ) : null}
      {rules === null && !error ? <LoadingState title="Loading alert rules" /> : null}
      {rules?.length === 0 ? <EmptyState title="No alert rules configured" detail="Create a quota warning or key-expiry rule to populate the notification outbox." /> : null}
      {rules?.length ? (
        <section class="surface table-wrap">
          <div class="section-heading"><div><p class="eyebrow">Configured rules</p><h2>Notification eligibility</h2></div><span>{rules.length} rule{rules.length === 1 ? "" : "s"}</span></div>
          <table>
            <thead><tr><th>Type</th><th>Condition</th><th>Scope</th><th>Status</th><th>Actions</th></tr></thead>
            <tbody>{rules.map((rule) => {
              const id = stringValue(rule.id);
              const enabled = rule.enabled === true;
              const ruleKind = stringValue(rule.kind);
              const project = stringValue(rule.project_id);
              const principal = stringValue(rule.principal_id);
              const scope = [project ? projectNames.get(project) : "", principal ? principalNames.get(principal) : ""].filter(Boolean).join(" · ") || "All eligible keys";
              const condition = ruleKind === "key_expiry"
                ? `${numberValue(rule.threshold)} day${numberValue(rule.threshold) === 1 ? "" : "s"} before expiry`
                : `${numberValue(rule.threshold)}% of ${stringValue(rule.period)} ${metricLabel(stringValue(rule.metric))}`;
              return <tr key={id}><td>{kindLabel(ruleKind)}</td><td>{condition}</td><td>{scope}</td><td><span class={`status-pill ${enabled ? "status-pill--ready" : "status-pill--muted"}`}>{enabled ? "enabled" : "disabled"}</span></td><td><div class="table-actions"><button class="button button--secondary" type="button" disabled={busy === id} onClick={() => void setEnabled(rule, !enabled)}>{enabled ? "Disable" : "Enable"}</button><button class="icon-button" type="button" aria-label={`Delete ${kindLabel(ruleKind)} alert`} disabled={busy === id} onClick={() => void remove(rule)}><Trash2 size={15} /></button></div></td></tr>;
            })}</tbody>
          </table>
        </section>
      ) : null}
    </div>
  );
}
