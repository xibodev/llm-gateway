import { useEffect, useMemo, useState } from "preact/hooks";
import { AlertCircle, BarChart3, Coins, RefreshCw, ShieldQuestion } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { formatUsageMetric, usageBucketAllowed, usageBucketLabel, usageMetricLabels, usageMetricTotal, usageMetricValue, usageTotals, type UsageMetric } from "../lib/usage";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";

const daySeconds = 24 * 60 * 60;

// The breakdowns the report's control plane groups usage by.
const breakdowns: { id: string; label: string; field: string }[] = [
  { id: "provider", label: "Provider", field: "provider" },
  { id: "model", label: "Model", field: "model" },
  { id: "key", label: "Key", field: "key_id" },
  { id: "project", label: "Project", field: "project_id" },
];

export function UsageQuotas({ data, mode }: { data: JSONRecord; mode: ConsoleMode }) {
  const [bucket, setBucket] = useState("day");
  const [rangeDays, setRangeDays] = useState("30");
  const [provider, setProvider] = useState("all");
  const [model, setModel] = useState("");
  const [keyID, setKeyID] = useState("all");
  const [projectID, setProjectID] = useState("all");
  const [metric, setMetric] = useState<UsageMetric>("requests");
  const [breakdown, setBreakdown] = useState("provider");
  const [report, setReport] = useState<JSONRecord | null>(null);
  // The bucket the shown report was requested with; the selector may differ until applied.
  const [reportBucket, setReportBucket] = useState(bucket);
  const [error, setError] = useState("");
  const keys = asList(data.keys).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const names = useMemo(() => ({
    key: new Map(keys.map((key) => [stringValue(key.id), stringValue(key.name, stringValue(key.prefix))])),
    project: new Map(projects.map((project) => [stringValue(project.id), stringValue(project.name, stringValue(project.slug))])),
  }), [data]);
  const load = async () => {
    const to = Math.floor(Date.now() / 1000) + 1;
    const from = to - Number(rangeDays) * daySeconds;
    const requestedBucket = bucket;
    const query = new URLSearchParams({ bucket: requestedBucket, from: String(from), to: String(to) });
    if (provider !== "all") query.set("provider", provider);
    if (model.trim()) query.set("model", model.trim());
    if (keyID !== "all") query.set("key_id", keyID);
    if (projectID !== "all") query.set("project_id", projectID);
    try {
      setError("");
      setReport(await getJSON<JSONRecord>(mode, `/usage?${query.toString()}`));
      setReportBucket(requestedBucket);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Usage report could not load."); }
  };
  useEffect(() => { void load(); }, [mode]);
  const hourAllowed = usageBucketAllowed(Number(rangeDays) * daySeconds, "hour");
  const changeRange = (value: string) => {
    setRangeDays(value);
    if (!usageBucketAllowed(Number(value) * daySeconds, bucket)) setBucket("day");
  };
  const series = asList(report?.series).map(asRecord);
  const advisories = asList(report?.quota_advisories).map(asRecord);
  // The administrator filters by the configured providers, a user by those
  // their usage in the range names; the chosen one stays listed either way.
  const providerIDs = mode === "admin"
    ? asList(data.providers).map((item) => stringValue(asRecord(item).id))
    : asList(report?.providers).map((item) => stringValue(item));
  if (provider !== "all" && !providerIDs.includes(provider)) providerIDs.push(provider);
  const maxValue = useMemo(() => Math.max(1, ...series.map((item) => usageMetricValue(item, metric))), [report, metric]);
  const totals = usageTotals(series);
  const tokens = usageMetricTotal(series, "tokens");
  const groupBy = breakdowns.find((item) => item.id === breakdown) ?? breakdowns[0];
  const groupRows = asList(asRecord(asRecord(report?.control_plane).groups)[groupBy.id]).map(asRecord);
  const groupName = (row: JSONRecord) => {
    const value = stringValue(row[groupBy.field]);
    if (!value) return groupBy.id === "key" || groupBy.id === "project" ? "Administrator or local" : "Not routed";
    // A key group names its key, a deleted one included, which no key list does.
    if (groupBy.id === "key") return stringValue(row.key_name) || (names.key.get(value) ?? value);
    if (groupBy.id === "project") return names.project.get(value) ?? value;
    return value;
  };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Consumption and limits" title="Usage & quotas" detail="Recorded gateway usage is bucketed in UTC. Provider subscription limits remain unknown unless a verified adapter supplies them." actions={<button class="button button--secondary" type="button" onClick={() => void load()}><RefreshCw size={16} /> Refresh report</button>} />
      <section class="usage-filter surface"><label>Range<select value={rangeDays} onInput={(event) => changeRange((event.currentTarget as HTMLSelectElement).value)}><option value="7">Last 7 days</option><option value="30">Last 30 days</option><option value="90">Last 90 days</option></select></label><label>Bucket<select value={bucket} onInput={(event) => setBucket((event.currentTarget as HTMLSelectElement).value)}><option value="hour" disabled={!hourAllowed}>{hourAllowed ? "Hour" : "Hour (shorter ranges only)"}</option><option value="day">Day</option><option value="week">Week</option></select></label><label>Provider<select value={provider} onInput={(event) => setProvider((event.currentTarget as HTMLSelectElement).value)}><option value="all">All providers</option>{providerIDs.map((id) => <option value={id} key={id}>{id}</option>)}</select></label><label>Model<input value={model} onInput={(event) => setModel((event.currentTarget as HTMLInputElement).value)} placeholder="Exact routed model" /></label><label>Key<select value={keyID} onInput={(event) => setKeyID((event.currentTarget as HTMLSelectElement).value)}><option value="all">All keys</option>{keys.map((key) => <option value={stringValue(key.id)} key={stringValue(key.id)}>{stringValue(key.name, stringValue(key.prefix))}</option>)}</select></label><label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}><option value="all">All projects</option>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label><button class="button button--primary" type="button" onClick={() => void load()}>Apply filters</button></section>
      {error ? <ErrorState title="Usage report is unavailable" detail={error} action={<button class="button button--secondary" type="button" onClick={() => void load()}>Retry</button>} /> : report === null ? <LoadingState title="Loading usage report" /> : <>
        <section class="metric-grid"><article class="metric-card"><span class="metric-card__icon"><BarChart3 size={19} /></span><div><p>Recorded requests</p><strong>{totals.requests}</strong><small>Filtered gateway events</small></div></article><article class="metric-card"><span class="metric-card__icon"><Coins size={19} /></span><div><p>Tokens</p><strong>{tokens.toLocaleString()}</strong><small>Input and output reported</small></div></article><article class="metric-card"><span class="metric-card__icon"><ShieldQuestion size={19} /></span><div><p>Provider quota</p><strong>Unknown</strong><small>Never inferred from traffic</small></div></article><article class="metric-card"><span class="metric-card__icon"><AlertCircle size={19} /></span><div><p>Failed requests</p><strong>{totals.errors}</strong><small>Recorded status failures</small></div></article></section>
        <section class="surface"><div class="section-heading"><div><p class="eyebrow">UTC time series</p><h2>{usageMetricLabels[metric]} by {reportBucket}</h2></div><label class="inline-select">Chart<select value={metric} onInput={(event) => setMetric((event.currentTarget as HTMLSelectElement).value as UsageMetric)}>{(Object.keys(usageMetricLabels) as UsageMetric[]).map((item) => <option value={item} key={item}>{usageMetricLabels[item]}</option>)}</select></label></div>{series.length === 0 ? <EmptyState title="No usage in this range" detail="Adjust filters or run a routed request to create recorded usage." /> : <div class="usage-bars">{series.map((item) => { const value = usageMetricValue(item, metric); return <div class="usage-bar" key={String(item.start)}><div class="usage-bar__track"><span style={{ height: `${Math.max(2, Math.round(value / maxValue * 100))}%` }} title={`${formatUsageMetric(value, metric)} ${usageMetricLabels[metric].toLowerCase()}`} /></div><strong>{formatUsageMetric(value, metric)}</strong><small>{usageBucketLabel(numberValue(item.start), reportBucket)}</small></div>; })}</div>}</section>
        <section class="surface table-wrap"><div class="section-heading"><div><p class="eyebrow">Breakdown</p><h2>Top {groupBy.label.toLowerCase()}s in this range</h2></div><label class="inline-select">Group by<select value={breakdown} onInput={(event) => setBreakdown((event.currentTarget as HTMLSelectElement).value)}>{breakdowns.map((item) => <option value={item.id} key={item.id}>{item.label}</option>)}</select></label></div>{groupRows.length === 0 ? <EmptyState title="Nothing to break down" detail="No recorded usage matches these filters." /> : <table><thead><tr><th>{groupBy.label}</th><th>Requests</th><th>Failed</th><th>Input tokens</th><th>Output tokens</th><th>Estimated cost</th><th>Average latency</th></tr></thead><tbody>{groupRows.map((row, index) => <tr key={`${groupName(row)}-${index}`}><td class="technical">{groupName(row)}</td><td>{numberValue(row.requests).toLocaleString()}</td><td>{numberValue(row.errors).toLocaleString()}</td><td>{numberValue(row.input_tokens).toLocaleString()}</td><td>{numberValue(row.output_tokens).toLocaleString()}</td><td>{formatUsageMetric(numberValue(row.cost_microusd), "cost")}</td><td>{numberValue(row.average_latency_ms).toLocaleString()} ms</td></tr>)}</tbody></table>}</section>
        <section class="surface table-wrap"><div class="section-heading"><div><p class="eyebrow">Provider quota advisory</p><h2>Verified limits only</h2></div><span class="status-pill status-pill--muted">No routing impact</span></div><table><thead><tr><th>Provider</th><th>State</th><th>Source</th><th>Refreshed</th></tr></thead><tbody>{advisories.map((advisory) => <tr key={stringValue(advisory.provider_id)}><td>{stringValue(advisory.provider_id)}</td><td><span class="status-pill status-pill--muted">{stringValue(advisory.status, "unknown")}</span></td><td>{stringValue(advisory.source, "Not supplied")}</td><td class="technical">{stringValue(advisory.refreshed_at, "—")}</td></tr>)}{advisories.length === 0 ? <tr><td colSpan={4}>No configured provider has quota data.</td></tr> : null}</tbody></table></section></>}
    </div>
  );
}