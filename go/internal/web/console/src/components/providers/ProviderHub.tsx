import { useMemo, useState } from "preact/hooks";
import {
  Compass,
  LoaderCircle,
  Plug,
  RefreshCw,
  Search,
  ShieldCheck,
  Trash2,
  X,
} from "lucide-preact";
import { sendJSON, type JSONRecord } from "../../lib/api";
import type { ConsoleMode } from "../../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../../lib/records";
import { ProviderMark } from "../ProviderMark";
import { EmptyState, PageHeading } from "../PageState";
import { useDialogFocus } from "../useDialogFocus";
import { OAuthConnectDialog } from "./OAuthConnectDialog";
import {
  type ActionResult,
  ResultNotice,
  StatusBadge,
  boolValue,
  configuredProviderConfig,
  configuredProviderIDs,
  groupFor,
  useProviderLifecycle,
} from "./shared";

export function ConnectDialog({ entry, onClose, onConfigured, mode = "create" }: { entry: JSONRecord; onClose: () => void; onConfigured: () => Promise<void>; mode?: "create" | "edit" }) {
  const providerConfig = asRecord(entry.provider_config);
  const existingIDs = configuredProviderIDs(entry);
  const suggestedID = (() => {
    const base = stringValue(entry.default_provider_id, stringValue(entry.id));
    if (!existingIDs.includes(base)) return base;
    for (let n = 2; ; n += 1) {
      const candidate = `${base}-${n}`;
      if (!existingIDs.includes(candidate)) return candidate;
    }
  })();
  const [providerID, setProviderID] = useState(mode === "edit" ? stringValue(providerConfig.id) : suggestedID);
  const [apiKey, setAPIKey] = useState("");
  const [baseURL, setBaseURL] = useState(stringValue(providerConfig.base_url) || stringValue(entry.default_base_url));
  const [region, setRegion] = useState(stringValue(providerConfig.region) || stringValue(entry.default_region));
  const [project, setProject] = useState(stringValue(providerConfig.project));
  const [location, setLocation] = useState(stringValue(providerConfig.location) || stringValue(entry.default_location));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const requiresKey = boolValue(entry.requires_api_key);
  const apiKeySet = boolValue(providerConfig.api_key_set);
  const takesServiceAccount = new Set(asList(entry.auth_methods).map(String)).has(SERVICE_ACCOUNT_KIND);
  const fields = new Set(asList(entry.onboarding_fields).map(String));
  const label = stringValue(entry.label, "Provider");
  const dialogRef = useDialogFocus(onClose);

  const submit = async (event: Event) => {
    event.preventDefault();
    if (!providerID.trim()) { setError("Provider ID is required."); return; }
    if (requiresKey && !apiKey.trim() && !apiKeySet) { setError("An API key is required for this integration."); return; }
    if (fields.has("base_url") && boolValue(entry.requires_base_url) && !baseURL.trim()) { setError("A base URL is required for this integration."); return; }
    if (fields.has("project") && !project.trim()) { setError("A Google Cloud project ID is required."); return; }
    if (fields.has("location") && !location.trim()) { setError("A provider location is required."); return; }
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/providers", "POST", {
        registry_id: stringValue(entry.id),
        id: providerID.trim(),
        api_key: apiKey,
        base_url: baseURL.trim(),
        region: region.trim(),
        project: project.trim(),
        location: location.trim(),
      });
      setAPIKey("");
      await onConfigured();
      onClose();
    } catch (cause) {
      setAPIKey("");
      setError(cause instanceof Error ? cause.message : "Provider setup failed.");
    } finally {
      setBusy(false);
    }
  };

  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="connect-provider-title" tabIndex={-1}><header><div><p class="eyebrow">Guided onboarding</p><h2 id="connect-provider-title">{mode === "edit" ? "Edit" : "Connect"} {label}</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><p class="muted-copy">Credential input is write-only. The console does not display or retrieve the value after setup.</p><label>Gateway provider ID<input value={providerID} disabled={mode === "edit"} onInput={(event) => setProviderID((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>{requiresKey ? <><label>{takesServiceAccount ? "API key or service account JSON" : "API key"}{apiKeySet ? " (leave blank to keep the current key)" : ""}{takesServiceAccount ? <textarea value={apiKey} rows={6} spellcheck={false} onInput={(event) => setAPIKey((event.currentTarget as HTMLTextAreaElement).value)} autoComplete="off" /> : <input type="password" value={apiKey} onInput={(event) => setAPIKey((event.currentTarget as HTMLInputElement).value)} autoComplete="off" />}</label>{takesServiceAccount ? <p class="form-help">Paste an API key, or the whole service account JSON file. The key type is detected from what you paste.</p> : null}</> : <p class="form-help">This integration does not require an API key. Confirm the local or self-hosted endpoint is reachable before continuing.</p>}{fields.has("base_url") ? <label>Base URL<input value={baseURL} onInput={(event) => setBaseURL((event.currentTarget as HTMLInputElement).value)} autoComplete="url" /></label> : null}{fields.has("region") ? <label>Region<input value={region} onInput={(event) => setRegion((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label> : null}{fields.has("project") ? <label>Google Cloud project ID<input value={project} onInput={(event) => setProject((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label> : null}{fields.has("location") ? <label>Location<input value={location} onInput={(event) => setLocation((event.currentTarget as HTMLInputElement).value)} placeholder="global" autoComplete="off" /></label> : null}{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <ShieldCheck size={16} />} {mode === "edit" ? "Save configuration" : "Connect provider"}</button></footer></form></section></div>;
}

// SERVICE_ACCOUNT_KIND matches gcpauth.CredentialKind on the server.
const SERVICE_ACCOUNT_KIND = "gcp_service_account";

// isServiceAccountJSON catches the common paste mistakes (an OAuth client file,
// a fragment, a path) in the browser, so the key is never sent to be rejected.
function isServiceAccountJSON(raw: string): boolean {
  try {
    const parsed = JSON.parse(raw);
    return !!parsed && typeof parsed === "object" && (parsed as JSONRecord).type === "service_account";
  } catch {
    return false;
  }
}

export function PrivateAPIKeyDialog({ entry, providerID, onClose, onConfigured }: { entry: JSONRecord; providerID: string; onClose: () => void; onConfigured: () => Promise<void> }) {
  const authMethods = new Set(asList(entry.auth_methods).map(String));
  const allowsServiceAccount = authMethods.has(SERVICE_ACCOUNT_KIND);
  const allowsAPIKey = authMethods.has("api_key");
  const [name, setName] = useState("personal");
  const [kind, setKind] = useState(allowsAPIKey ? "api_key" : SERVICE_ACCOUNT_KIND);
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const usingServiceAccount = kind === SERVICE_ACCOUNT_KIND;
  const label = stringValue(entry.label, providerID);
  const dialogRef = useDialogFocus(onClose);
  const submit = async (event: Event) => {
    event.preventDefault();
    if (!providerID) { setError("An administrator must configure this provider before you add a private credential."); return; }
    if (!secret.trim()) { setError(usingServiceAccount ? "A service account key is required." : "An API key is required."); return; }
    if (usingServiceAccount && !isServiceAccountJSON(secret)) {
      setError("That does not look like a service account key. Paste the whole JSON file, including its \"type\": \"service_account\" field.");
      return;
    }
    setBusy(true);
    try {
      await sendJSON<JSONRecord>("portal", "/connections", "POST", {
        provider_id: providerID, connection_name: name, credential_kind: kind, secret, make_default: true,
      });
      setSecret("");
      await onConfigured();
      onClose();
    } catch (cause) {
      setSecret("");
      setError(cause instanceof Error ? cause.message : "Private connection could not be stored.");
    } finally { setBusy(false); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="private-key-title" tabIndex={-1}><header><div><p class="eyebrow">Private API key</p><h2 id="private-key-title">Connect {label}</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><p class="muted-copy">This credential is encrypted for the signed-in human. It is write-only and is never returned to browser state.</p><label>Connection name<input value={name} onInput={(event) => setName((event.currentTarget as HTMLInputElement).value)} /></label>{allowsServiceAccount && allowsAPIKey ? <label>Credential type<select value={kind} onChange={(event) => { setKind((event.currentTarget as HTMLSelectElement).value); setSecret(""); setError(""); }}><option value="api_key">API key</option><option value={SERVICE_ACCOUNT_KIND}>Google Cloud service account key</option></select></label> : null}{usingServiceAccount ? <label>Service account key (JSON)<textarea value={secret} rows={8} spellcheck={false} onInput={(event) => setSecret((event.currentTarget as HTMLTextAreaElement).value)} placeholder={'{\n  "type": "service_account",\n  ...\n}'} autoComplete="off" /></label> : <label>API key<input type="password" value={secret} onInput={(event) => setSecret((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>}{usingServiceAccount ? <p class="form-help">The key is exchanged for short-lived access tokens. Its project is read from the key itself.</p> : null}{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <ShieldCheck size={16} />} Save private connection</button></footer></form></section></div>;
}

export function ProviderHub({ data, mode, onChanged, onOpenDetail }: { data: JSONRecord; mode: ConsoleMode; onChanged: () => Promise<void>; onOpenDetail: (entryID: string) => void }) {
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [connectEntry, setConnectEntry] = useState<{ entry: JSONRecord; mode: "create" | "edit" } | null>(null);
  const [privateKeyEntry, setPrivateKeyEntry] = useState<{ entry: JSONRecord; providerID: string } | null>(null);
  const [oauthEntry, setOAuthEntry] = useState<{ entry: JSONRecord; providerID: string } | null>(null);
  const [detectBusy, setDetectBusy] = useState(false);
  const [detectResult, setDetectResult] = useState<ActionResult>(null);
  const [localCandidates, setLocalCandidates] = useState<JSONRecord[]>([]);
  const owners = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const [ownerID, setOwnerID] = useState(stringValue(owners[0]?.id));
  const { busy, result, setResult, runLifecycle } = useProviderLifecycle(ownerID, onChanged);
  const registry = asList(data.provider_registry).map(asRecord);
  const statuses = asList(data.provider_statuses).map(asRecord);
  const statusByID = new Map(statuses.map((status) => [stringValue(status.id), status]));
  const privateProviderIDs = new Set(asList(data.provider_connections).map(asRecord)
    .filter((connection) => stringValue(connection.status, "active") === "active")
    .map((connection) => stringValue(connection.provider_id))
    .filter(Boolean));

  const entries = useMemo(() => {
    const registryIDs = new Set(registry.map((entry) => stringValue(entry.id)));
    const curated = registry.map((entry) => ({
      ...entry,
      ...statusByID.get(stringValue(entry.id)),
    }));
    // A tile owns every configured instance that claims its registry id. Keying
    // "custom" on the provider id instead orphaned any instance not named after
    // its registry entry — which is every second instance an operator adds.
    const custom = statuses.filter((status) => {
      const claimed = stringValue(status.registry_id, stringValue(status.id));
      return !registryIDs.has(claimed);
    });
    return [...curated, ...custom];
  }, [data.provider_registry, data.provider_statuses]);
  const filtered = entries.filter((entry) => {
    const haystack = `${stringValue(entry.label)} ${stringValue(entry.description)} ${stringValue(entry.id)}`.toLowerCase();
    if (search && !haystack.includes(search.toLowerCase())) return false;
    return filter === "all" || groupFor(entry) === filter;
  });
  const groups = ["Official OAuth", "API key connections", "Local and self-hosted", "Gateway clients"];

  const detectLocal = async () => {
    setDetectBusy(true);
    try {
      const payload = await sendJSON<JSONRecord>("admin", "/detect", "POST", {});
      const candidates = asList(payload.candidates).map(asRecord);
      setLocalCandidates(candidates);
      setDetectResult({ title: "Local detection", success: true, detail: candidates.length ? `${candidates.length} candidate${candidates.length === 1 ? "" : "s"} found. Nothing was configured automatically.` : "No local providers were found. Nothing was configured automatically." });
    } catch (cause) {
      setDetectResult({ title: "Local detection", success: false, detail: cause instanceof Error ? cause.message : "Local detection failed." });
    } finally { setDetectBusy(false); }
  };

  const connect = (entry: JSONRecord, connectMode: "create" | "edit" = "create") => {
    if (boolValue(entry.client_only)) {
      setResult({ title: "Gateway client setup", success: true, detail: "Claude Code uses the documented Anthropic gateway protocol. Configure an Anthropic API key or supported gateway credential; no personal Claude OAuth is offered." });
      return;
    }
    const methods = asList(entry.auth_methods).map(String);
    if (methods.some((method) => method.startsWith("oauth"))) {
      const oauthProviderIDs = configuredProviderIDs(entry);
      // A multi-instance tile has no per-instance OAuth binder yet; refuse
      // rather than silently bind the new account to instance [0].
      if (oauthProviderIDs.length > 1) {
        setResult({ title: "Official OAuth", success: false, detail: "Multiple instances are configured for this integration. Resolve to a single instance before adding an OAuth account." });
        return;
      }
      setOAuthEntry({ entry, providerID: oauthProviderIDs[0] ?? stringValue(entry.id) });
      return;
    }
    if (mode === "portal") {
      const providerIDs = configuredProviderIDs(entry);
      if (!providerIDs.length) {
        setResult({ title: "Private connection", success: false, detail: "Administrator setup is required before a private API key can be added." });
        return;
      }
      // A tile with several instances has no per-instance picker here yet; send
      // the operator to the detail page rather than guessing which instance the
      // key belongs to.
      if (providerIDs.length > 1) {
        onOpenDetail(stringValue(entry.id));
        return;
      }
      setPrivateKeyEntry({ entry, providerID: providerIDs[0] });
      return;
    }
    setConnectEntry({ entry: { ...entry, provider_config: configuredProviderConfig(entry, data) }, mode: connectMode });
  };

  const openDetail = (event: Event, entryID: string) => {
    const origin = event.target as HTMLElement | null;
    if (origin?.closest("button, a, select, input, label")) return;
    onOpenDetail(entryID);
  };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Provider hub" title="Providers" detail="Registry-driven integrations show safe connection state, catalog freshness, and explicit lifecycle outcomes. Open a card for models, connections, and checks." actions={mode === "admin" ? <><label class="owner-select">Catalog owner<select value={ownerID} onInput={(event) => setOwnerID((event.currentTarget as HTMLSelectElement).value)}><option value="">No private owner selected</option>{owners.map((owner) => <option value={stringValue(owner.id)} key={stringValue(owner.id)}>{stringValue(owner.display_name, stringValue(owner.email, stringValue(owner.id)))}</option>)}</select></label><button class="button button--secondary" type="button" disabled={detectBusy} onClick={() => void detectLocal()}><Compass size={16} /> Detect local</button></> : undefined} />
      <section class="provider-toolbar surface"><label class="search-field"><Search size={17} /><span class="sr-only">Search providers</span><input value={search} onInput={(event) => setSearch((event.currentTarget as HTMLInputElement).value)} placeholder="Search integrations" /></label><div class="filter-row"><button class={`filter-chip ${filter === "all" ? "filter-chip--active" : ""}`} type="button" onClick={() => setFilter("all")}>All</button>{groups.map((group) => <button class={`filter-chip ${filter === group ? "filter-chip--active" : ""}`} type="button" key={group} onClick={() => setFilter(group)}>{group}</button>)}</div></section>
      <ResultNotice result={result ?? detectResult} />
      {localCandidates.length ? <section class="surface"><div class="section-heading"><div><p class="eyebrow">Local discovery</p><h2>Detected candidates</h2></div><span class="status-pill status-pill--muted">Confirmation required</span></div><div class="candidate-list">{localCandidates.map((candidate, index) => <div key={`${stringValue(candidate.id)}-${index}`}><strong>{stringValue(candidate.label, stringValue(candidate.id, "Local provider"))}</strong><span class="technical">{stringValue(candidate.base_url, stringValue(candidate.url))}</span><small>Detection did not change configuration.</small></div>)}</div></section> : null}
      {filtered.length === 0 ? <EmptyState title="No matching provider" detail="Adjust the search or filter to inspect another registry integration." /> : null}
      {groups.map((group) => {
        const groupEntries = filtered.filter((entry) => groupFor(entry) === group);
        if (!groupEntries.length) return null;
        return <section class="provider-group" key={group}><header><p class="eyebrow">{group}</p><span>{groupEntries.length} integration{groupEntries.length === 1 ? "" : "s"}</span></header><div class="provider-card-grid">{groupEntries.map((entry) => {
          const id = stringValue(entry.id);
          const label = stringValue(entry.label, id);
          const status = stringValue(entry.status, "not_configured");
          const configured = boolValue(entry.configured);
          const isClient = boolValue(entry.client_only);
          const unavailable = stringValue(entry.availability) !== "available" && !isClient;
          const methods = asList(entry.auth_methods).map(String);
          const supportsOAuth = methods.some((method) => method.startsWith("oauth"));
          const providerIDs = configuredProviderIDs(entry);
          const hasPrivateConnection = mode === "portal" && providerIDs.some((providerID) => privateProviderIDs.has(providerID));
          const portalRequiresAdminSetup = mode === "portal" && !isClient && !supportsOAuth && providerIDs.length === 0;
          const active = (operation: string) => busy === `${providerIDs[0] ?? id}-${operation}`;
          const statusCounts = asRecord(entry.instance_status_counts);
          const instanceCount = asList(entry.instances).length;
          // Composition, not a rollup: a tile is an aggregator, so it reports what
          // its instances are rather than adopting one instance's status.
          const compositionText = Object.entries(statusCounts)
            .map(([state, count]) => `${numberValue(count)} ${state}`)
            .join(" · ");
          return <article class="provider-card provider-card--rich provider-card--clickable" key={id} role="link" tabIndex={0} aria-label={`Open ${label} details`} onClick={(event) => openDetail(event, id)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onOpenDetail(id); } }}><header><ProviderMark id={id} label={label} /><div><h2>{label}</h2><p>{stringValue(entry.description, "Gateway integration.")}</p></div><StatusBadge status={status} /></header><dl><div><dt>Catalog</dt><dd>{numberValue(entry.model_count)} model{numberValue(entry.model_count) === 1 ? "" : "s"} · {stringValue(entry.catalog_state, "unknown")}</dd></div><div><dt>Connection</dt><dd>{numberValue(entry.connection_count)} private record{numberValue(entry.connection_count) === 1 ? "" : "s"}</dd></div><div><dt>Instances</dt><dd>{instanceCount} instance{instanceCount === 1 ? "" : "s"}{compositionText ? ` · ${compositionText}` : ""}</dd></div><div><dt>Protocol</dt><dd>{stringValue(entry.protocol, "gateway")}</dd></div><div><dt>Auth</dt><dd>{methods.join(" · ") || "Gateway credential"}</dd></div></dl>{stringValue(entry.configuration_issue) ? <p class="form-error" role="alert">{stringValue(entry.configuration_issue)}</p> : null}{isClient ? <div class="provider-client-note"><ShieldCheck size={16} /><span>Client setup only. No provider OAuth action is available.</span></div> : null}<footer>{configured && mode === "admin" ? <div class="provider-actions"><button class="button button--secondary" type="button" title="Configure another instance of this integration" onClick={() => connect(entry, "create")}><Plug size={15} /> Add instance</button>{supportsOAuth ? <button class="button button--primary" type="button" disabled={!ownerID || instanceCount > 1} title={instanceCount > 1 ? "Multiple instances are configured; add an OAuth account from an unambiguous single-instance tile" : undefined} onClick={() => connect(entry)}><Plug size={15} /> Add account</button> : null}{instanceCount > 1 ? <button class="button button--secondary" type="button" title="Grid actions are ambiguous across instances; manage each instance from its own row" onClick={() => onOpenDetail(id)}><Plug size={15} /> Manage {instanceCount} instances</button> : <><button class="button button--secondary" type="button" title="Confirm the endpoint answers the catalog API — does not run a completion" disabled={active("test") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "test")}><Plug size={15} /> Check reachability</button><button class="button button--secondary" type="button" title="Refresh this provider's model catalog" disabled={active("refresh") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "refresh")}><RefreshCw size={15} /> Sync catalog</button><button class="button button--danger" type="button" disabled={active("delete")} title="Remove the configured provider and its system connection" onClick={() => void runLifecycle(entry, "delete")}><Trash2 size={15} /> Remove</button></>}</div> : mode === "portal" && !isClient ? unavailable ? <span class="provider-card__meta">Integration not available</span> : portalRequiresAdminSetup ? <span class="provider-card__meta">Administrator setup required</span> : <button class="button button--primary" type="button" disabled={supportsOAuth && instanceCount > 1} title={supportsOAuth && instanceCount > 1 ? "Multiple instances are configured; add an OAuth account from an unambiguous single-instance tile" : undefined} onClick={() => connect(entry)}><Plug size={16} /> {hasPrivateConnection ? "Add or replace account" : "Connect"}</button> : !configured && !unavailable ? <button class="button button--primary" type="button" onClick={() => connect(entry)}><Plug size={16} /> {isClient ? "View setup" : "Connect"}</button> : <span class="provider-card__meta">{unavailable ? "Integration not available" : "Connection managed privately"}</span>}<span class="technical provider-card__freshness">{stringValue(entry.catalog_refreshed, "Catalog not synced")}</span></footer></article>;
        })}</div></section>;
      })}
      {connectEntry ? <ConnectDialog entry={connectEntry.entry} mode={connectEntry.mode} onClose={() => setConnectEntry(null)} onConfigured={onChanged} /> : null}
      {privateKeyEntry ? <PrivateAPIKeyDialog entry={privateKeyEntry.entry} providerID={privateKeyEntry.providerID} onClose={() => setPrivateKeyEntry(null)} onConfigured={onChanged} /> : null}
      {oauthEntry ? <OAuthConnectDialog entry={oauthEntry.entry} providerID={oauthEntry.providerID} data={data} mode={mode} onClose={() => setOAuthEntry(null)} onComplete={onChanged} /> : null}
    </div>
  );
}
