import { useEffect, useMemo, useState } from "preact/hooks";
import { AlertCircle, BarChart3, RefreshCw, ShieldQuestion } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

function dateLabel(timestamp: number, bucket: string): string {
  const date = new Date(timestamp * 1000);
  return bucket === "hour" ? date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : date.toLocaleDateString([], { month: "short", day: "numeric" });
}

export function UsageQuotas({ data, mode }: { data: JSONRecord; mode: ConsoleMode }) {
  const [bucket, setBucket] = useState("day");
  const [rangeDays, setRangeDays] = useState("30");
  const [provider, setProvider] = useState("all");
  const [model, setModel] = useState("");
  const [keyID, setKeyID] = useState("all");
  const [projectID, setProjectID] = useState("all");
  const [report, setReport] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const providers = asList(data.providers).map(asRecord);
  const keys = asList(data.keys).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const load = async () => {
    const to = Math.floor(Date.now() / 1000) + 1;
    const from = to - Number(rangeDays) * 24 * 60 * 60;
    const query = new URLSearchParams({ bucket, from: String(from), to: String(to) });
    if (provider !== "all") query.set("provider", provider);
    if (model.trim()) query.set("model", model.trim());
    if (keyID !== "all") query.set("key_id", keyID);
    if (projectID !== "all") query.set("project_id", projectID);
    try { setError(""); setReport(await getJSON<JSONRecord>(mode, `/usage?${query.toString()}`)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Usage report could not load."); }
  };
  useEffect(() => { void load(); }, [mode]);
  const series = asList(report?.series).map(asRecord);
  const advisories = asList(report?.quota_advisories).map(asRecord);
  const maxRequests = useMemo(() => Math.max(1, ...series.map((item) => numberValue(item.requests))), [report]);
  const totals = asRecord(asRecord(report?.control_plane).totals);
  const portalTotals = asRecord(report?.totals);
  const totalRequests = numberValue(totals.requests, numberValue(portalTotals.requests));

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Consumption and limits" title="Usage & quotas" detail="Recorded gateway usage is bucketed in UTC. Provider subscription limits remain unknown unless a verified adapter supplies them." actions={<button class="button button--secondary" type="button" onClick={() => void load()}><RefreshCw size={16} /> Refresh report</button>} />
      <section class="usage-filter surface"><label>Range<select value={rangeDays} onInput={(event) => setRangeDays((event.currentTarget as HTMLSelectElement).value)}><option value="7">Last 7 days</option><option value="30">Last 30 days</option><option value="90">Last 90 days</option></select></label><label>Bucket<select value={bucket} onInput={(event) => setBucket((event.currentTarget as HTMLSelectElement).value)}><option value="hour">Hour</option><option value="day">Day</option><option value="week">Week</option></select></label><label>Provider<select value={provider} onInput={(event) => setProvider((event.currentTarget as HTMLSelectElement).value)}><option value="all">All providers</option>{providers.map((item) => <option value={stringValue(item.id)} key={stringValue(item.id)}>{stringValue(item.id)}</option>)}</select></label><label>Model<input value={model} onInput={(event) => setModel((event.currentTarget as HTMLInputElement).value)} placeholder="Exact routed model" /></label><label>Key<select value={keyID} onInput={(event) => setKeyID((event.currentTarget as HTMLSelectElement).value)}><option value="all">All keys</option>{keys.map((key) => <option value={stringValue(key.id)} key={stringValue(key.id)}>{stringValue(key.name, stringValue(key.prefix))}</option>)}</select></label><label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}><option value="all">All projects</option>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label><button class="button button--primary" type="button" onClick={() => void load()}>Apply filters</button></section>
      {error ? <ErrorState title="Usage report is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load()}>Retry</button>} /> : report === null ? <LoadingState title="Loading usage report" /> : <><section class="metric-grid"><article class="metric-card"><span class="metric-card__icon"><BarChart3 size={19} /></span><div><p>Recorded requests</p><strong>{totalRequests}</strong><small>Filtered gateway events</small></div></article><article class="metric-card"><span class="metric-card__icon"><ShieldQuestion size={19} /></span><div><p>Provider quota</p><strong>Unknown</strong><small>Never inferred from traffic</small></div></article><article class="metric-card"><span class="metric-card__icon"><AlertCircle size={19} /></span><div><p>Failed requests</p><strong>{numberValue(totals.errors, numberValue(portalTotals.errors))}</strong><small>Recorded status failures</small></div></article></section><section class="surface"><div class="section-heading"><div><p class="eyebrow">UTC time series</p><h2>Requests by {bucket}</h2></div><span>{series.length} bucket{series.length === 1 ? "" : "s"}</span></div>{series.length === 0 ? <EmptyState title="No usage in this range" detail="Adjust filters or run a routed request to create recorded usage." /> : <div class="usage-bars">{series.map((item) => <div class="usage-bar" key={String(item.start)}><div class="usage-bar__track"><span style={{ height: `${Math.max(2, Math.round(numberValue(item.requests) / maxRequests * 100))}%` }} title={`${numberValue(item.requests)} requests`} /></div><strong>{numberValue(item.requests)}</strong><small>{dateLabel(numberValue(item.start), bucket)}</small></div>)}</div>}</section><section class="surface table-wrap"><div class="section-heading"><div><p class="eyebrow">Provider quota advisory</p><h2>Verified limits only</h2></div><span class="status-pill status-pill--muted">No routing impact</span></div><table><thead><tr><th>Provider</th><th>State</th><th>Source</th><th>Refreshed</th></tr></thead><tbody>{advisories.map((advisory) => <tr key={stringValue(advisory.provider_id)}><td>{stringValue(advisory.provider_id)}</td><td><span class="status-pill status-pill--muted">{stringValue(advisory.status, "unknown")}</span></td><td>{stringValue(advisory.source, "Not supplied")}</td><td class="technical">{stringValue(advisory.refreshed_at, "—")}</td></tr>)}{advisories.length === 0 ? <tr><td colSpan={4}>No configured provider has quota data.</td></tr> : null}</tbody></table></section></>}
    </div>
  );
}
