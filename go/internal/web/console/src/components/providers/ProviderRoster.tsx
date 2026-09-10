import { useEffect, useRef, useState } from "preact/hooks";
import { RefreshCw } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../../lib/api";
import type { ConsoleMode } from "../../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../../lib/records";
import { ProviderMark, hasProviderMark } from "../ProviderMark";
import { boolValue } from "./shared";

export const providerShelves = ["OpenAI-compatible", "Anthropic-compatible", "Google native & cloud", "Local & self-hosted", "Embeddings, speech & media", "Client setup", "Other candidates"];
export const discoveryFilters = [
  ["all", "All providers"], ["configured", "Your accounts"], ["free", "Free tiers"],
  ["no-key", "Free · no key"], ["trial", "Trials"], ["device", "Device sign-in"],
  ["local", "Local"], ["candidate", "New candidates"], ["unavailable", "Unavailable"],
];

export function shelfFor(entry: JSONRecord): string {
  const id = stringValue(entry.id);
  if (boolValue(entry.client_only)) return "Client setup";
  if (["ollama", "localai"].includes(id) || stringValue(entry.category) === "local") return "Local & self-hosted";
  if (["gemini", "ai_studio", "vertex_ai", "bedrock", "azure_openai"].includes(id)) return "Google native & cloud";
  if (id === "edge_tts" || ["embeddings", "speech", "images", "audio", "video"].includes(stringValue(entry.protocol))) return "Embeddings, speech & media";
  if (boolValue(entry.remote_roster) && !["openai", "anthropic"].includes(stringValue(entry.protocol))) return "Other candidates";
  return stringValue(entry.protocol) === "anthropic" ? "Anthropic-compatible" : "OpenAI-compatible";
}

export function safeRosterURL(value: unknown): string | undefined {
  try {
    const url = new URL(stringValue(value));
    return url.protocol === "https:" && !url.username && !url.password ? url.href : undefined;
  } catch { return undefined; }
}

function endpointIdentity(value: unknown): string {
  try {
    const url = new URL(stringValue(value));
    if (!["https:", "http:"].includes(url.protocol) || url.username || url.password) return "";
    url.pathname = url.pathname.replace(/%[0-9a-f]{2}/gi, (encoded) => {
      const character = String.fromCharCode(parseInt(encoded.slice(1), 16));
      return /[a-z0-9_~.-]/i.test(character) ? character : encoded.toUpperCase();
    });
    // Match producer canonicalization: doubled slashes and reserved escapes select routes.
    return url.href.endsWith("//") ? url.href : url.href.replace(/\/$/, "");
  } catch { return ""; }
}

export function rosterUnavailable(entry: JSONRecord): boolean {
  return ["quarantined", "withdrawn"].includes(stringValue(entry.state));
}

export function mergeProviderRoster(builtins: JSONRecord[], roster: JSONRecord[]): JSONRecord[] {
  const merged: (JSONRecord & { roster_entries: JSONRecord[] })[] = builtins.map((entry) => ({ ...entry, roster_entries: [] }));
  const candidates: JSONRecord[] = [];
  const seen = new Set<string>();
  for (const remote of roster) {
    const id = stringValue(remote.id);
    if (!id) continue;
    const endpoint = endpointIdentity(remote.base_url);
    const protocol = stringValue(remote.protocol);
    const identity = `${protocol}:${endpoint || id}:${stringValue(remote.state)}`;
    if (seen.has(identity)) continue;
    seen.add(identity);
    // Match shipped defaults, never rewrite an instance or follow roster URL drift.
    // A familiar ID with a different endpoint remains a separate discovery option.
    const builtin = merged.find((entry) => {
      const base = endpointIdentity(entry.default_base_url);
      return endpoint && base ? endpoint === base && (!protocol || protocol === stringValue(entry.protocol))
        : !endpoint && id === stringValue(entry.id);
    });
    if (builtin && !rosterUnavailable(remote)) builtin.roster_entries.push(remote);
    else candidates.push({
      ...remote, id: `roster:${id}`, roster_id: id, label: stringValue(remote.name, id),
      remote_roster: true, configured: false, roster_entries: [remote],
      auth_methods: [stringValue(remote.auth, "unknown")],
    });
  }
  return [...merged, ...candidates];
}

export function matchesDiscoveryFilter(entry: JSONRecord, filter: string): boolean {
  const remote = boolValue(entry.remote_roster);
  const unavailable = remote ? rosterUnavailable(entry) : !!stringValue(entry.availability) && stringValue(entry.availability) !== "available" && !boolValue(entry.client_only);
  if (filter === "unavailable") return unavailable;
  if (remote && unavailable) return false;
  const offers = asList(entry.roster_entries).map(asRecord).filter((offer) => {
    if (rosterUnavailable(offer)) return false;
    const expires = stringValue(offer.offer_expires_at);
    return !expires || (Number.isFinite(Date.parse(expires)) && Date.parse(expires) > Date.now());
  });
  switch (filter) {
    case "configured": return boolValue(entry.configured);
    case "free": return offers.some((offer) => ["free_tier", "recurring_credit"].includes(stringValue(offer.offer)));
    case "no-key": return offers.some((offer) => offer.auth === "none" && ["free_tier", "recurring_credit"].includes(stringValue(offer.offer)));
    case "trial": return offers.some((offer) => offer.offer === "trial");
    case "device": return asList(entry.auth_methods).includes("oauth_device");
    case "local": return shelfFor(entry) === "Local & self-hosted";
    case "candidate": return remote;
    default: return true;
  }
}

export function rosterSetupUnavailableReason(entry: JSONRecord, registry: JSONRecord[]): string {
  if (entry.protocol === "anthropic" && entry.auth === "none") return "Anonymous Anthropic setup is unavailable: the shipped custom Anthropic adapter requires an API key.";
  if (entry.setup !== "compatible" || entry.state !== "active" || !["api_key", "none"].includes(stringValue(entry.auth))) return "Quick setup requires an active, compatible entry with known authentication. Review the provider documentation.";
  const protocol = stringValue(entry.protocol);
  if (!["openai", "anthropic"].includes(protocol) || !safeRosterURL(entry.base_url)) return "Quick setup requires a supported OpenAI or Anthropic protocol and an HTTPS endpoint.";
  const adapter = registry.find((item) => item.id === `custom_${protocol}`);
  if (!adapter) return "The required custom adapter is not available in this gateway.";
  return "";
}

export function rosterSetupEntry(entry: JSONRecord, registry: JSONRecord[]): JSONRecord | null {
  if (rosterSetupUnavailableReason(entry, registry)) return null;
  const adapter = registry.find((item) => item.id === `custom_${stringValue(entry.protocol)}`);
  const slug = stringValue(entry.name, stringValue(entry.roster_id)).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 48) || "roster-provider";
  return {
    ...adapter, label: stringValue(entry.label, stringValue(entry.name)),
    default_provider_id: slug, default_base_url: safeRosterURL(entry.base_url),
    requires_api_key: entry.auth === "api_key", requires_base_url: true,
    auth_methods: [entry.auth], onboarding_fields: ["base_url", ...(entry.auth === "api_key" ? ["api_key"] : [])],
    provider_config: {}, configured: false, configured_provider_ids: [], instances: [],
  };
}

export function useProviderRoster(mode: ConsoleMode) {
  const [state, setState] = useState<JSONRecord>({});
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const generation = useRef(0);
  useEffect(() => {
    const current = ++generation.current;
    setState({}); setBusy(true); setError("");
    void getJSON<JSONRecord>(mode, "/provider-roster").then((payload) => {
      if (generation.current === current) setState(asRecord(payload));
    }).catch(() => {
      if (generation.current === current) setError("Remote roster unavailable. Built-in providers are ready to use.");
    }).finally(() => { if (generation.current === current) setBusy(false); });
    return () => { generation.current += 1; };
  }, [mode]);
  const refresh = async () => {
    if (mode !== "admin" || busy) return;
    const current = generation.current;
    setBusy(true); setError("");
    try {
      await sendJSON<JSONRecord>(mode, "/provider-roster/refresh", "POST", {});
      const payload = await getJSON<JSONRecord>(mode, "/provider-roster");
      if (generation.current === current) setState(asRecord(payload));
    } catch {
      if (generation.current === current) setError("Roster refresh failed. Keeping the last available discovery entries; try again.");
      // Refresh failures can update the server's stale/source status too.
      try {
        const payload = await getJSON<JSONRecord>(mode, "/provider-roster");
        if (generation.current === current) setState(asRecord(payload));
      } catch { /* Keep the last snapshot while the optional service is unavailable. */ }
    } finally { if (generation.current === current) setBusy(false); }
  };
  return { state, entries: asList(state.entries).map(asRecord), busy, error, refresh, canRefresh: mode === "admin" };
}

export function RosterStatus({ roster }: { roster: ReturnType<typeof useProviderRoster> }) {
  const { state, busy, error, refresh, canRefresh } = roster;
  const refreshFailed = !!stringValue(state.error);
  const degradedSources = asList(state.sources).map(asRecord).filter((source) => !!stringValue(source.status) && source.status !== "ok");
  const recovery = canRefresh ? "An administrator can refresh the roster here." : "Only an administrator can refresh the roster.";
  return <div class="provider-roster-status">
    <div><p role="status">{error || (busy ? "Updating remote roster…" : state.configured === false ? "Remote roster is not configured. Built-in providers are available." : `Remote roster · Revision ${numberValue(state.revision)} · Auto-refresh ${boolValue(state.auto_refresh) ? "on" : "off"}${boolValue(state.stale) ? " · Stale snapshot" : ""}${refreshFailed ? " · Last refresh failed" : ""}`)}</p>{degradedSources.length ? <p>{degradedSources.length} discovery source{degradedSources.length === 1 ? " is" : "s are"} degraded. Some entries may be out of date.</p> : null}{!canRefresh || refreshFailed || boolValue(state.stale) || degradedSources.length ? <p>{recovery}</p> : null}{stringValue(state.last_success) ? <p>Last updated {stringValue(state.last_success)}</p> : null}</div>
    {canRefresh ? <button class="button button--secondary" type="button" disabled={busy} onClick={() => void refresh()}><RefreshCw size={14} />{busy ? "Updating…" : "Refresh roster"}</button> : null}
  </div>;
}

export function rosterLogoSource(value: unknown): string | undefined {
  const logo = asRecord(value);
  const mime = stringValue(logo.mime);
  const data = stringValue(logo.data);
  // The backend validates the raster; still bound and whitelist browser input.
  if (!["image/png", "image/jpeg"].includes(mime) || !data || data.length > 87384 || data.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(data)) return undefined;
  if ((mime === "image/png" && !data.startsWith("iVBORw0KGgo")) || (mime === "image/jpeg" && !data.startsWith("/9j/"))) return undefined;
  return `data:${mime};base64,${data}`;
}

export function RosterMark({ entry }: { entry: JSONRecord }) {
  const source = rosterLogoSource(entry.logo);
  const [failed, setFailed] = useState("");
  const id = stringValue(entry.roster_id, stringValue(entry.id));
  const baseURL = stringValue(entry.base_url, stringValue(entry.default_base_url));
  const local = <ProviderMark id={id} label={stringValue(entry.label, stringValue(entry.name))} baseURL={baseURL} />;
  if (hasProviderMark(id, baseURL)) return local;
  return source && failed !== source ? <span class="provider-mark" aria-hidden="true"><img src={source} alt="" width="24" height="24" onError={() => setFailed(source)} /></span> : local;
}

export function rosterReportURL(entry: JSONRecord, revision: unknown): string {
  const issue = safeRosterURL(asRecord(entry.report).issue_url);
  if (issue) {
    const url = new URL(issue);
    if (url.hostname === "github.com" && !url.port && /^\/xibodev\/llm-gateway\/issues\/\d+\/?$/.test(url.pathname)) return `${url.origin}${url.pathname}`;
  }
  const params = new URLSearchParams({ title: `Provider roster report: ${stringValue(entry.id)}`, body: `### Roster entry ID\n\n${stringValue(entry.id)}\n\n### Roster revision\n\n${numberValue(revision)}\n\n### Problem\n\nDescribe the issue with this discovery entry.\n\n### Public evidence and reproduction\n\nInclude only public evidence. Reports are public: never include credentials, private endpoints, configuration, or personal information.` });
  return `https://github.com/xibodev/llm-gateway/issues/new?${params}`;
}

const offerLabels: Record<string, string> = { free_tier: "Free tier", recurring_credit: "Recurring credit", trial: "Trial", paid: "Paid", unknown: "Offer unknown" };

export function RosterMetadata({ entries, revision }: { entries: JSONRecord[]; revision: unknown }) {
  return <div class="provider-roster-metadata">{entries.map((entry) => {
    const signup = safeRosterURL(entry.signup_url);
    const docs = safeRosterURL(entry.docs_url);
    const probe = asRecord(entry.probe);
    const report = asRecord(entry.report);
    const requirements = asList(entry.requirements).map(String);
    const conflicts = asList(entry.conflicts).map(String);
    return <section key={stringValue(entry.id)} aria-label="Remote discovery metadata">
      <h3>Discovery information</h3>
      <p>{stringValue(entry.description, "Confirm current terms with the provider before connecting.")}</p>
      <dl>
        <div><dt>Offer</dt><dd>{offerLabels[stringValue(entry.offer)] || "Offer unknown"}{stringValue(entry.offer_expires_at) ? ` · Expires ${stringValue(entry.offer_expires_at)}` : ""}</dd></div>
        <div><dt>Authentication</dt><dd>{entry.auth === "none" ? "No key (source-reported)" : entry.auth === "api_key" ? "API key" : "Unknown; no anonymous access assumed"}</dd></div>
        <div><dt>Discovery state</dt><dd>{stringValue(entry.state, "unknown")} · {stringValue(entry.setup, "candidate")}</dd></div>
        <div><dt>Reachability</dt><dd>{stringValue(probe.status, "not_checked").replaceAll("_", " ")}{stringValue(probe.checked_at) ? ` · ${stringValue(probe.checked_at)}` : ""}</dd></div>
        <div><dt>Candidate endpoint</dt><dd class="technical">{stringValue(entry.base_url, "Not supplied")}</dd></div>
        <div><dt>Reports</dt><dd>{numberValue(report.confirmations)} confirmations{stringValue(report.reason) ? ` · ${stringValue(report.reason)}` : ""}</dd></div>
      </dl>
      {requirements.length ? <p><strong>Requirements:</strong> {requirements.join(" · ")}</p> : null}
      {conflicts.length ? <p class="form-error"><strong>Conflicting information:</strong> {conflicts.join(" · ")}</p> : null}
      <p class="form-help">Discovery and HTTP reachability do not establish inference access or zero cost. Configured endpoints stay unchanged.</p>
      <div class="provider-roster-links">{signup ? <a href={signup} target="_blank" rel="noopener noreferrer">Sign up / get a key ↗</a> : null}{docs ? <a href={docs} target="_blank" rel="noopener noreferrer">Documentation ↗</a> : null}<a href={rosterReportURL(entry, revision)} target="_blank" rel="noopener noreferrer">Report an issue ↗</a></div>
      <p class="form-help">Reports open a public GitHub issue. Include only public evidence; never include credentials, private endpoints, configuration, or personal information.</p>
    </section>;
  })}</div>;
}
