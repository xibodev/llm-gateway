import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { ArrowLeft, CheckCircle2, ExternalLink, Plug, Play, Power, RefreshCw, Search, ShieldCheck, Trash2, Wrench } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { ProviderMark } from "../components/ProviderMark";
import { EmptyState, PageHeading } from "../components/PageState";
import { OAuthConnectDialog } from "../components/providers/OAuthConnectDialog";
import { Pager, defaultPageSize } from "../components/ModelPicker";
import { ConnectDialog, PrivateAPIKeyDialog } from "../components/providers/ProviderHub";
import {
  ResultNotice,
  StatusBadge,
  boolValue,
  configuredProviderConfig,
  configuredProviderIDs,
  connectionChoices,
  evidenceLabel,
  groupFor,
  tileStatus,
  useProviderLifecycle,
} from "../components/providers/shared";

function formatEpoch(value: unknown): string {
  const seconds = numberValue(value);
  if (!seconds) return "—";
  return new Date(seconds * 1000).toLocaleString();
}

export function ProviderDetail({ entryID, data, mode, onChanged, onBack, onOpenPlayground }: {
  entryID: string;
  data: JSONRecord;
  mode: ConsoleMode;
  onChanged: () => Promise<void>;
  onBack: () => void;
  onOpenPlayground?: (modelID: string, ownerID: string) => void;
}) {
  const registry = asList(data.provider_registry).map(asRecord);
  const statuses = asList(data.provider_statuses).map(asRecord);
  let registryEntry = registry.find((candidate) => stringValue(candidate.id) === entryID || asList(candidate.aliases).includes(entryID));
  let statusEntry = statuses.find((candidate) => stringValue(candidate.id) === entryID && !(registryEntry && boolValue(candidate.custom)));

  // If entryID did not match a top-level tile directly, check whether it is a configured instance of a multi-instance tile
  if (!registryEntry && !statusEntry) {
    for (const status of statuses) {
      const match = asList(status.instances).map(asRecord).find((inst) => stringValue(inst.id) === entryID);
      if (match) {
        statusEntry = {
          ...status,
          ...match,
          id: entryID,
          label: stringValue(match.label, entryID),
          instances: [match],
          configured_provider_ids: [entryID],
        };
        const regID = stringValue(match.registry_id);
        if (regID) {
          registryEntry = registry.find((c) => stringValue(c.id) === regID || asList(c.aliases).includes(regID));
        }
        break;
      }
    }
  }

  // If entryID is a remote roster candidate, resolve its candidate metadata:
  if (!registryEntry && !statusEntry && entryID.startsWith("roster:")) {
    const rawRosterID = entryID.replace(/^roster:/, "");
    const rosterEntries = asList(asRecord(data.provider_roster)?.entries).map(asRecord);
    const candidate = rosterEntries.find((c) => stringValue(c.id) === rawRosterID);
    if (candidate) {
      statusEntry = {
        id: entryID,
        label: stringValue(candidate.name, rawRosterID),
        description: stringValue(candidate.description, "Community discovery candidate."),
        protocol: stringValue(candidate.protocol, "openai"),
        default_base_url: stringValue(candidate.base_url),
        base_url: stringValue(candidate.base_url),
        configured: false,
        instances: [],
        configured_provider_ids: [],
        remote_roster: true,
        roster_entries: [candidate],
      };
    }
  }

  const entry = useMemo(() => ({ ...(registryEntry ?? {}), ...(statusEntry ?? {}) }), [registryEntry, statusEntry]);

  const owners = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const [ownerID, setOwnerID] = useState(stringValue(owners[0]?.id));
  const { busy, result, runLifecycle } = useProviderLifecycle(ownerID, onChanged);
  const [connectOpen, setConnectOpen] = useState(false);
  const [zenAPIKey, setZenAPIKey] = useState(false);
  const [privateKeyOpen, setPrivateKeyOpen] = useState(false);
  const [oauthOpen, setOAuthOpen] = useState(false);
  const [verifyModel, setVerifyModel] = useState("");
  const [modelSearch, setModelSearch] = useState("");
  const [modelPage, setModelPage] = useState(0);

  const providerIDs = configuredProviderIDs(entry);
  // The upsert route is keyed on the provider id alone: a suggested id must
  // avoid every configured provider, not only the ones under this tile.
  const allProviderIDs = asList(data.providers).map(asRecord).map((provider) => stringValue(provider.id)).filter(Boolean);
  // A multi-instance tile has no per-instance OAuth binder yet: "" here disables
  // the Add account action rather than guessing which instance to bind.
  const oauthProviderID = providerIDs.length > 1 ? "" : (providerIDs[0] ?? stringValue(entry.id));
  const instances = asList(entry.instances).map(asRecord);
  // The tile's configuration_issue is a space-join of every instance's message —
  // the unreadable concatenation the per-instance field exists to replace. Keep
  // it only where it is one instance's own words; rows carry the rest.
  const tileConfigurationIssue = instances.length > 1 ? "" : stringValue(entry.configuration_issue);
  const providerConfig = configuredProviderConfig(entry, data);
  const disabledProviderIDs = new Set(asList(entry.disabled_provider_ids).map(String));
  const [toggleBusy, setToggleBusy] = useState("");
  const [connectionBusy, setConnectionBusy] = useState("");
  const configured = providerIDs.length > 0;
  const isClient = boolValue(entry.client_only);
  const methods = asList(entry.auth_methods).map(String);
  const supportsOAuth = methods.some((method) => method.startsWith("oauth"));
  const label = stringValue(entry.label, entryID);
  const unavailable = stringValue(entry.availability, "available") !== "available" && !isClient;
  const isCodex = stringValue(entry.id) === "openai_codex";
  const isZen = stringValue(entry.id) === "opencode_zen";
  const isOllama = stringValue(entry.id) === "ollama";

  const connections = asList(data.provider_connections).map(asRecord)
    .filter((connection) => providerIDs.includes(stringValue(connection.provider_id)));
  const principalName = (id: string) => {
    const principal = asList(data.principals).map(asRecord).find((candidate) => stringValue(candidate.id) === id);
    return principal ? stringValue(principal.display_name, id) : id;
  };

  // Per-provider model catalog, scoped to the selected owner when OAuth/private credentials apply.
  const [catalog, setCatalog] = useState<JSONRecord | null>(null);
  const [catalogError, setCatalogError] = useState("");
  const catalogRequest = useRef(0);
  const catalogPath = mode === "admin"
    ? (ownerID && supportsOAuth ? `/models?principal_id=${encodeURIComponent(ownerID)}` : "/models")
    : "/models";
  useEffect(() => {
    const request = ++catalogRequest.current;
    setCatalog(null);
    setCatalogError("");
    if (!configured) return;

    const fetchCatalog = (path: string) => {
      getJSON<JSONRecord>(mode, path)
        .then((payload) => {
          if (request !== catalogRequest.current) return;
          setCatalog(payload);
        })
        .catch((cause) => {
          if (request === catalogRequest.current) {
            setCatalogError(cause instanceof Error ? cause.message : "Model catalog could not load.");
          }
        });
    };

    fetchCatalog(catalogPath);
  }, [mode, catalogPath, configured, stringValue(entry.catalog_refreshed)]);
  const models = asList(catalog?.data).map(asRecord)
    .filter((row) => providerIDs.includes(stringValue(row.owned_by)) || providerIDs.some((providerID) => stringValue(row.id).startsWith(`${providerID}/`)));
  const matchedModels = useMemo(() => {
    const needle = modelSearch.trim().toLowerCase();
    if (!needle) return models;
    return models.filter((row) => `${stringValue(row.id)} ${stringValue(row.display_name)}`.toLowerCase().includes(needle));
  }, [models, modelSearch]);
  const pagedModels = matchedModels.slice(modelPage * defaultPageSize, (modelPage + 1) * defaultPageSize);

  useEffect(() => { setModelPage(0); }, [modelSearch, catalogPath]);

  if (!registryEntry && !statusEntry) {
    return <div class="page-stack"><PageHeading eyebrow="Provider" title="Unknown integration" detail="This integration is not present in the registry or the configured providers." /><button class="button button--secondary" type="button" onClick={onBack}><ArrowLeft size={16} /> Back to providers</button></div>;
  }

  const active = (providerID: string, operation: string) => busy === `${providerID}-${operation}`;
  const docsURL = stringValue(entry.docs_url);

  const toggleEnabled = async (providerID: string) => {
    const enable = disabledProviderIDs.has(providerID);
    setToggleBusy(providerID);
    try {
      await sendJSON<JSONRecord>("admin", `/providers/${encodeURIComponent(providerID)}/enabled`, "POST", { enabled: enable });
      await onChanged();
    } finally {
      setToggleBusy("");
    }
  };

  const disconnect = async (connection: JSONRecord) => {
    const connectionID = stringValue(connection.id);
    const principalID = stringValue(connection.principal_id);
    if (!connectionID || !window.confirm(`Disconnect ${stringValue(connection.connection_name, "this account")}?`)) return;
    setConnectionBusy(connectionID);
    try {
      const path = mode === "portal"
        ? `/connections/${encodeURIComponent(connectionID)}`
        : `/principals/${encodeURIComponent(principalID)}/connections/${encodeURIComponent(connectionID)}`;
      await sendJSON<JSONRecord>(mode, path, "DELETE");
      await onChanged();
    } finally { setConnectionBusy(""); }
  };

  return (
    <div class="page-stack">
      <nav class="detail-breadcrumb"><button class="button button--secondary" type="button" onClick={onBack}><ArrowLeft size={16} /> Providers</button></nav>
      <header class="detail-heading surface">
        <ProviderMark id={entryID} label={label} />
        <div class="detail-heading__body">
          <div class="detail-heading__title"><h1>{label}</h1><StatusBadge status={tileStatus(entry)} /></div>
          <p>{stringValue(entry.description, "Gateway integration.")}</p>
          <dl class="compact-facts">
            <div><dt>Group</dt><dd>{groupFor(entry)}</dd></div>
            <div><dt>Protocol</dt><dd>{stringValue(entry.protocol, "gateway")}</dd></div>
            <div><dt>Connection choices</dt><dd>{connectionChoices(entry).join(" · ") || "Gateway credential"}</dd></div>
            <div><dt>Scope</dt><dd>{stringValue(entry.connection_scope, "—")}</dd></div>
            {stringValue(providerConfig.project) ? <div><dt>Project</dt><dd>{stringValue(providerConfig.project)}</dd></div> : null}
            {stringValue(providerConfig.location) ? <div><dt>Location</dt><dd>{stringValue(providerConfig.location)}</dd></div> : null}
            {docsURL ? <div><dt>Docs</dt><dd><a href={docsURL} target="_blank" rel="noreferrer noopener">Provider documentation <ExternalLink size={13} /></a></dd></div> : null}
          </dl>
          {stringValue(entry.risk_notice) ? <p class="form-help">{stringValue(entry.risk_notice)}</p> : null}
          {isCodex ? <p class="form-help"><strong>Personal subscription only.</strong> Codex connects a selected human owner through device OAuth. It is never anonymous or a free gateway connection.</p> : null}
          {isZen ? <p class="form-help"><strong>Choose anonymous or personal.</strong> Anonymous access needs no key; add an optional private API key when you want account-backed access.</p> : null}
          {isOllama ? <p class="form-help"><strong>Native Ollama root:</strong> use the server root such as <span class="technical">http://127.0.0.1:11434</span>, not an OpenAI <span class="technical">/v1</span> path. Run Detect local, then Check reachability to confirm this gateway process can reach it.</p> : null}
          {tileConfigurationIssue ? <p class="form-error" role="alert">{tileConfigurationIssue}</p> : null}
        </div>
        {mode === "admin" && !isClient && !unavailable ? <div class="detail-heading__actions">
          {supportsOAuth || configured ? <label class="owner-select">Catalog owner<select value={ownerID} onInput={(event) => setOwnerID((event.currentTarget as HTMLSelectElement).value)}><option value="">No private owner selected</option>{owners.map((owner) => <option value={stringValue(owner.id)} key={stringValue(owner.id)}>{stringValue(owner.display_name, stringValue(owner.id))}</option>)}</select></label> : null}
          {supportsOAuth ? <button class="button button--primary" type="button" disabled={!ownerID || !oauthProviderID} title={!oauthProviderID ? "Multiple instances are configured for this integration; resolve to a single instance before adding an OAuth account." : undefined} onClick={() => setOAuthOpen(true)}><Plug size={15} /> Add account</button> : isZen ? <><button class="button button--primary" type="button" onClick={() => { setZenAPIKey(false); setConnectOpen(true); }}><Plug size={15} /> {configured ? "Edit anonymous connection" : "Connect anonymously"}</button><button class="button button--secondary" type="button" onClick={() => { setZenAPIKey(true); setConnectOpen(true); }}><ShieldCheck size={15} /> Connect with API key</button></> : <button class="button button--primary" type="button" onClick={() => setConnectOpen(true)}><Plug size={15} /> {configured ? "Edit configuration" : "Connect"}</button>}
        </div> : mode === "portal" && !isClient && !unavailable ? <div class="detail-heading__actions">
          {supportsOAuth || configured ? <button class="button button--primary" type="button" disabled={supportsOAuth ? !oauthProviderID : providerIDs.length !== 1} title={supportsOAuth ? (!oauthProviderID ? "Multiple instances are configured for this integration; resolve to a single instance before adding an OAuth account." : undefined) : (providerIDs.length > 1 ? "Multiple instances are configured for this integration; ask an administrator to add a private connection for a specific instance." : undefined)} onClick={() => (supportsOAuth ? setOAuthOpen(true) : setPrivateKeyOpen(true))}><Plug size={15} /> {connections.length ? "Add or replace account" : "Connect"}</button> : <span class="provider-card__meta">Administrator setup required</span>}
        </div> : null}
      </header>
      <ResultNotice result={result} />
      {isClient ? <section class="surface"><div class="section-heading"><div><p class="eyebrow">Gateway client</p><h2>Client setup only</h2></div></div><p class="muted-copy">This integration is a client of the gateway, not an upstream provider. Point the client at the gateway base URL with a minted gateway key; no provider connection exists to manage here.</p></section> : null}
      {unavailable ? <section class="surface"><EmptyState title="Integration not available" detail="This registry entry is planned but not yet connectable." /></section> : null}
      {configured && mode === "admin" ? <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Configured gateway providers</p><h2>Checks and lifecycle</h2></div><span>{providerIDs.length} provider{providerIDs.length === 1 ? "" : "s"}</span></div>
        <p class="muted-copy">Each action states exactly what it proves. A reachability check exercises only the catalog API; only a test completion verifies that inference works end to end. Clear cache &amp; retry resets local provider state — it cannot fix a wrong credential or endpoint.</p>
        <div class="table-wrap"><table><thead><tr><th>Provider ID</th><th>Status</th><th>Evidence</th><th>Catalog</th><th>Freshness</th><th>Actions</th></tr></thead><tbody>
          {instances.map((instance) => {
            const providerID = stringValue(instance.id);
            return <tr key={providerID}>
            <td><strong class="technical">{providerID}</strong>{boolValue(instance.disabled) ? <small class="table-subtitle">Disabled — requests 404 until re-enabled</small> : null}</td>
            <td><StatusBadge status={stringValue(instance.status, "configured")} />{stringValue(instance.configuration_issue) ? <small class="table-subtitle">{stringValue(instance.configuration_issue)}</small> : null}</td>
            <td><dl class="evidence-list"><div><dt>Auth</dt><dd>{evidenceLabel("authentication", instance.authentication_state)}</dd></div><div><dt>Catalog</dt><dd>{evidenceLabel("catalog", instance.catalog_evidence)}</dd></div><div><dt>Completion</dt><dd>{evidenceLabel("completion", instance.completion_evidence)}</dd></div></dl></td>
            <td>{stringValue(instance.catalog_state) === "not_discoverable" ? "Catalog not discoverable" : `${numberValue(instance.model_count)} model${numberValue(instance.model_count) === 1 ? "" : "s"} · ${stringValue(instance.catalog_state, "unknown")}`}</td>
            <td class="technical">{stringValue(instance.catalog_refreshed, "Never synced")}</td>
            <td><div class="provider-actions">
              <button class="button button--secondary" type="button" title="Take the provider in or out of service without deleting its configuration or credentials" disabled={toggleBusy === providerID} onClick={() => void toggleEnabled(providerID)}><Power size={15} /> {disabledProviderIDs.has(providerID) ? "Enable" : "Disable"}</button>
              <button class="button button--secondary" type="button" title="Confirm the endpoint answers the catalog API — does not run a completion" disabled={active(providerID, "test") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "test", providerID)}><Plug size={15} /> Check reachability</button>
              <button class="button button--secondary" type="button" title="Refresh this provider's model catalog" disabled={active(providerID, "refresh") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "refresh", providerID)}><RefreshCw size={15} /> Sync catalog</button>
              <button class="button button--primary" type="button" title="Run one real minimal completion — the only check that verifies inference" disabled={active(providerID, "verify") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "verify", providerID, verifyModel)}><CheckCircle2 size={15} /> Test inference</button>
              <button class="button button--secondary" type="button" title="Reset cached provider and catalog state, then refetch — cannot repair credentials or endpoints" disabled={active(providerID, "repair") || (supportsOAuth && !ownerID)} onClick={() => void runLifecycle(entry, "repair", providerID)}><Wrench size={15} /> Clear cache &amp; retry</button>
              <button class="button button--danger" type="button" disabled={active(providerID, "delete")} onClick={() => void runLifecycle(entry, "delete", providerID)}><Trash2 size={15} /> Remove</button>
            </div></td>
            </tr>;
          })}
        </tbody></table></div>
        {models.length ? <label class="verify-model-select">Test completion model<select value={verifyModel} onInput={(event) => setVerifyModel((event.currentTarget as HTMLSelectElement).value)}><option value="">Automatic (recommended model)</option>{models.map((row) => <option value={stringValue(row.id).split("/").pop()} key={stringValue(row.id)}>{stringValue(row.id)}</option>)}</select></label> : null}
        <dl class="compact-facts provider-check-facts">
          <div><dt>Last check</dt><dd>{stringValue(entry.last_check_operation) ? `${stringValue(entry.last_check_operation).replaceAll("_", " ")} · ${entry.last_check_success === true ? "passed" : "failed"} · ${stringValue(entry.last_checked_at)}` : "No check recorded yet"}</dd></div>
          <div><dt>Last verified</dt><dd>{stringValue(entry.last_verified_at) ? `${stringValue(entry.last_verified_at)} (${stringValue(entry.verified_model, "model unknown")})` : "Never — run a test completion"}</dd></div>
          {stringValue(entry.last_check_detail) ? <div class="provider-check-facts__detail"><dt>Detail</dt><dd>{stringValue(entry.last_check_detail)}</dd></div> : null}
        </dl>
      </section> : null}
      {configured ? <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Catalog</p><h2>Models from this provider</h2></div><span>{matchedModels.length === models.length ? `${models.length} model${models.length === 1 ? "" : "s"}` : `${matchedModels.length} of ${models.length} models`}</span></div>
        {models.length > defaultPageSize ? <label class="search-field catalog-search"><Search size={17} /><span class="sr-only">Search this provider's models</span><input value={modelSearch} onInput={(event) => setModelSearch((event.currentTarget as HTMLInputElement).value)} placeholder="Filter by model id or name" /></label> : null}
        {mode === "admin" && !ownerID ? <EmptyState title="Select a catalog owner" detail="Model catalogs are scoped to a human principal. Pick one above, or create one on the Access page." /> : catalogError ? <EmptyState title="Model catalog is unavailable" detail={catalogError} /> : catalog === null ? <p class="muted-copy">Loading model catalog…</p> : models.length === 0 ? <EmptyState title="No models synced yet" detail="Run Sync to refresh this provider's catalog, then models appear here and in route building." /> : <div class="table-wrap"><table><thead><tr><th>Model</th><th>Capabilities</th><th>Supported surfaces</th><th>Try</th></tr></thead><tbody>
          {pagedModels.map((row) => <tr key={stringValue(row.id)}><td><strong class="technical">{stringValue(row.id)}</strong>{stringValue(row.display_name) ? <small class="table-subtitle">{stringValue(row.display_name)}</small> : null}</td><td>{Object.keys(asRecord(row.capabilities)).join(" · ") || "Not supplied"}</td><td class="technical">{asList(row.supported_surfaces ?? row.supported_endpoints).map(String).join(" · ") || "Catalog did not declare"}</td><td>{onOpenPlayground ? <button class="button button--secondary" type="button" title="Add this model to the playground" onClick={() => onOpenPlayground(stringValue(row.id), ownerID)}><Play size={14} /> Try in Playground</button> : null}</td></tr>)}
        </tbody></table><Pager total={matchedModels.length} page={modelPage} pageSize={defaultPageSize} onPage={setModelPage} /></div>}
      </section> : null}
      {configured ? <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Private connections</p><h2>Per-principal credentials</h2></div><span>{connections.length} record{connections.length === 1 ? "" : "s"}</span></div>
        {connections.length === 0 ? <EmptyState title="No private connections" detail="Private connections override the system credential for one principal only." /> : <div class="table-wrap"><table><thead><tr><th>Name</th><th>Human owner</th><th>Connected account</th><th>Kind</th><th>Status</th><th>Last used</th><th>Action</th></tr></thead><tbody>
          {connections.map((connection) => <tr key={stringValue(connection.id)}><td><strong>{stringValue(connection.connection_name, "connection")}</strong></td><td>{principalName(stringValue(connection.principal_id))}</td><td>{stringValue(connection.oauth_account_label, stringValue(connection.oauth_account_id, "Not reported"))}</td><td class="technical">{stringValue(connection.credential_kind)}</td><td><span class={`status-pill ${stringValue(connection.status, "active") === "active" ? "status-pill--ready" : "status-pill--muted"}`}>{stringValue(connection.status, "active")}</span></td><td class="technical">{formatEpoch(connection.last_used_at)}</td><td>{stringValue(connection.status, "active") === "active" ? <button class="button button--danger" type="button" disabled={connectionBusy === stringValue(connection.id)} onClick={() => void disconnect(connection)}><Trash2 size={14} /> Disconnect</button> : "—"}</td></tr>)}
        </tbody></table></div>}
      </section> : null}
      {!configured && !isClient && !unavailable ? <section class="surface"><EmptyState title="Not connected yet" detail={supportsOAuth ? "Add an account with the official OAuth flow to configure this integration." : "Connect this integration to sync its catalog and route requests through it."} action={mode === "admin" && !supportsOAuth ? <button class="button button--primary" type="button" onClick={() => setConnectOpen(true)}><Plug size={16} /> Connect</button> : undefined} /></section> : null}
      {connectOpen ? <ConnectDialog entry={{ ...entry, requires_api_key: zenAPIKey || entry.requires_api_key, provider_config: providerConfig }} mode={configured ? "edit" : "create"} takenIDs={allProviderIDs} onClose={() => setConnectOpen(false)} onConfigured={onChanged} /> : null}
      {privateKeyOpen ? <PrivateAPIKeyDialog entry={entry} providerID={providerIDs.length === 1 ? providerIDs[0] : ""} onClose={() => setPrivateKeyOpen(false)} onConfigured={onChanged} /> : null}
      {oauthOpen ? <OAuthConnectDialog entry={entry} providerID={oauthProviderID} data={data} mode={mode} onClose={() => setOAuthOpen(false)} onComplete={onChanged} /> : null}
    </div>
  );
}
