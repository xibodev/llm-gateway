import { useRef, useState } from "preact/hooks";
import { Check, Copy, Eye, EyeOff, KeyRound, Pencil, Plus, Save, Trash2, X } from "lucide-preact";
import { sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, PageHeading } from "../components/PageState";
import { KeyScopeEditor, keyPolicySummary, keyQuotaLabels } from "../components/KeyScopeEditor";
import { useDialogFocus } from "../components/useDialogFocus";
import "../styles/keys.css";

const ownerDecisionRequired = "__select_owner__";

function policyFor(key: JSONRecord): JSONRecord {
  // Admin state flattens policy; portal keys nest it. Never treat timestamps as quotas.
  const source = key.policy ? asRecord(key.policy) : key;
  return Object.fromEntries([
    "allowed_routes", "routes_only", "allowed_providers", "allowed_models", "admin_managed",
    ...Object.keys(keyQuotaLabels),
  ].filter((field) => source[field] !== undefined).map((field) => [field, source[field]]));
}

function KeySecret({ token, onDismiss, returnFocus }: { token: string; onDismiss: () => void; returnFocus: HTMLElement | null }) {
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");
  const dialogRef = useDialogFocus(onDismiss, returnFocus);
  const copy = async () => {
    try {
      if (!navigator.clipboard) throw new Error("Clipboard unavailable. Select the key and copy it manually.");
      await navigator.clipboard.writeText(token);
      setCopied(true);
      setError("");
    } catch { setError("Could not copy. Select the key and copy it manually."); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog key-secret-dialog" role="dialog" aria-modal="true" aria-labelledby="key-secret-title" tabIndex={-1}>
    <header><div><p class="eyebrow">Gateway credential</p><h2 id="key-secret-title">API key</h2></div><button class="icon-button" type="button" aria-label="Close key secret" onClick={onDismiss}><X size={18} /></button></header>
    <pre class="technical key-secret-value">{token}</pre>
    {error ? <p class="form-error" role="alert">{error}</p> : null}
    <footer><button class="button button--secondary" type="button" data-dialog-initial-focus onClick={() => void copy()}>{copied ? <Check size={16} /> : <Copy size={16} />}{copied ? "Copied" : "Copy key"}</button><button class="button button--primary" type="button" onClick={onDismiss}>Close</button></footer>
  </section></div>;
}

export function ApiKeys({ data, mode, onChanged, initialContext }: {
  data: JSONRecord; mode: ConsoleMode; onChanged: () => Promise<void>;
  initialContext?: { ownerID?: string; projectID?: string; routeName?: string };
}) {
  const keys = asList(data.keys).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const principals = asList(data.principals).map(asRecord);
  const memberships = asList(data.memberships).map(asRecord);
  const self = asRecord(data.principal);
  const eligibleOwners = (id: string) => principals.filter((principal) =>
    ["human", "service"].includes(stringValue(principal.kind)) && stringValue(principal.status) === "active" &&
    memberships.some((membership) => stringValue(membership.project_id) === id &&
      stringValue(membership.principal_id) === stringValue(principal.id) &&
      ["owner", "admin", "member", "viewer"].includes(stringValue(membership.role)) && stringValue(membership.status, "active") === "active"));
  const creatableProjects = projects.filter((project) => stringValue(project.status, "active") === "active" &&
    (mode === "admin" || memberships.some((membership) => stringValue(membership.project_id) === stringValue(project.id) &&
      stringValue(membership.principal_id) === stringValue(self.id) && ["owner", "admin", "member"].includes(stringValue(membership.role)))));
  const initialProjectID = initialContext?.projectID ?? stringValue(creatableProjects.find((project) =>
    !initialContext?.ownerID || eligibleOwners(stringValue(project.id)).some((owner) => owner.id === initialContext.ownerID))?.id);
  const [creating, setCreating] = useState(Boolean(initialContext));
  const [editing, setEditing] = useState<JSONRecord | null>(null);
  const [name, setName] = useState("Gateway key");
  const [projectID, setProjectID] = useState(initialProjectID);
  const [principalID, setPrincipalID] = useState(initialContext?.ownerID
    ? eligibleOwners(initialProjectID).some((owner) => owner.id === initialContext.ownerID) ? initialContext.ownerID : ownerDecisionRequired
    : "");
  const [ownerFilter, setOwnerFilter] = useState(mode === "admin" ? initialContext?.ownerID ?? "" : "");
  const [projectFilter, setProjectFilter] = useState(initialContext?.projectID ?? "");
  const [search, setSearch] = useState("");
  const [routeFilter, setRouteFilter] = useState(initialContext?.routeName ?? "");
  const initialScope = (): JSONRecord => ({ allowed_routes: initialContext?.routeName ? [initialContext.routeName] : [], routes_only: Boolean(initialContext?.routeName), allowed_providers: [], allowed_models: [] });
  const [scope, setScope] = useState<JSONRecord>(initialScope);
  const [rpm, setRPM] = useState("0");
  const [dailyRequests, setDailyRequests] = useState("0");
  const [secret, setSecret] = useState("");
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [revealingID, setRevealingID] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const createButtonRef = useRef<HTMLButtonElement | null>(null);
  const editorFormRef = useRef<HTMLFormElement | null>(null);
  const owners = eligibleOwners(projectID);
  const selectedOwner = owners.find((owner) => owner.id === principalID);
  const ownerNames = new Map(principals.map((principal) => [stringValue(principal.id), stringValue(principal.display_name, stringValue(principal.id))]));
  for (const key of keys) if (!ownerNames.has(stringValue(key.principal_id))) ownerNames.set(stringValue(key.principal_id), stringValue(key.principal, stringValue(key.principal_id)));
  if (ownerFilter && !ownerNames.has(ownerFilter)) ownerNames.set(ownerFilter, `${ownerFilter} (unavailable owner)`);
  const filteredKeys = keys.filter((key) => {
    if (mode === "admin" && ownerFilter && key.principal_id !== ownerFilter) return false;
    if (projectFilter && key.project_id !== projectFilter) return false;
    if (routeFilter && !asList(policyFor(key).allowed_routes).includes(routeFilter)) return false;
    return [key.name, key.prefix, key.id, key.project, key.project_id, key.principal, key.principal_id, ownerNames.get(stringValue(key.principal_id))]
      .map((value) => stringValue(value)).join(" ").toLowerCase().includes(search.trim().toLowerCase());
  });
  const editablePolicy = (): JSONRecord | null => {
    if (![rpm, dailyRequests].every((value) => /^\d+$/.test(value.trim()) && Number.isSafeInteger(Number(value)))) {
      setMessage("Requests per minute and daily requests must be nonnegative whole numbers.");
      return null;
    }
    // Read text drafts from the form too: Enter can submit before Preact renders input state.
    const form = editorFormRef.current ? new FormData(editorFormRef.current) : null;
    const lists = Object.fromEntries(["allowed_routes", "allowed_providers", "allowed_models"].map((field) => {
      const draft = form?.get(field);
      const values = typeof draft === "string" ? draft.split(",") : asList(scope[field]).map(String);
      return [field, [...new Set(values.map((value) => value.trim()).filter(Boolean))]];
    }));
    const allowedRoutes = lists.allowed_routes;
    if (scope.routes_only === true && !allowedRoutes.length) {
      setMessage("Select at least one allowed route for a routes-only key.");
      return null;
    }
    // Only send editable fields; the server preserves all other quota limits.
    return { rpm: Number(rpm), daily_requests: Number(dailyRequests), allowed_routes: allowedRoutes,
      routes_only: scope.routes_only === true, allowed_providers: lists.allowed_providers, allowed_models: lists.allowed_models };
  };
  const create = async (event: Event) => {
    event.preventDefault();
    if (busy) return;
    if (!creatableProjects.some((project) => project.id === projectID) || !name.trim()) { setMessage("An active project with key-management permission and a key name are required."); return; }
    if (mode === "admin" && principalID && !selectedOwner) { setMessage("Choose an active project member or explicitly choose to create or reuse a service identity."); return; }
    const policy = editablePolicy();
    if (!policy) return;
    setBusy(true);
    setMessage("");
    try {
      const payload: JSONRecord = { project_id: projectID, name: name.trim(), ...policy };
      if (mode === "admin" && principalID) payload.principal_id = principalID;
      const response = await sendJSON<JSONRecord>(mode, "/keys", "POST", payload);
      const token = stringValue(response.token);
      if (!token) throw new Error("The server did not return the key value.");
      setSecret(token);
      setCreating(false);
      await onChanged();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be created."); }
    finally { setBusy(false); }
  };
  const reveal = async (key: JSONRecord) => {
    const id = stringValue(key.id);
    if (!id) return;
    if (revealed[id]) {
      setRevealed((current) => { const next = { ...current }; delete next[id]; return next; });
      return;
    }
    setRevealingID(id);
    setMessage("");
    try {
      const response = await sendJSON<JSONRecord>(mode, `/keys/${encodeURIComponent(id)}/reveal`, "POST");
      const token = stringValue(response.token);
      if (!token) throw new Error("The server did not return the key value.");
      setRevealed((current) => ({ ...current, [id]: token }));
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be revealed."); }
    finally { setRevealingID(""); }
  };
  const update = async (key: JSONRecord, disabled?: boolean) => {
    const id = stringValue(key.id);
    if (!id || busy) return;
    if (stringValue(key.status) === "revoked") { setMessage("Revoked keys are permanently disabled."); return; }
    if (mode === "portal" && policyFor(key).admin_managed === true) { setMessage("Only an administrator can edit this key. You can still reveal or revoke it."); return; }
    const payload: JSONRecord | null = disabled === undefined ? editablePolicy() : { disabled };
    if (!payload) return;
    setBusy(true);
    try {
      if (mode === "admin") payload.id = id;
      await sendJSON<JSONRecord>(mode, mode === "admin" ? "/keys/update" : `/keys/${encodeURIComponent(id)}/update`, "POST", payload);
      setEditing(null);
      setMessage(disabled === undefined ? "Key policy updated." : "Key status updated.");
      await onChanged();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be updated."); }
    finally { setBusy(false); }
  };
  const revoke = async (key: JSONRecord) => {
    const id = stringValue(key.id);
    if (!id || !window.confirm(`Revoke ${stringValue(key.name, "this key")}? Existing clients will stop working.`)) return;
    setBusy(true);
    try {
      await sendJSON<JSONRecord>(mode, mode === "admin" ? `/keys?id=${encodeURIComponent(id)}` : `/keys/${encodeURIComponent(id)}`, "DELETE");
      setRevealed((current) => { const next = { ...current }; delete next[id]; return next; });
      if (editing?.id === id) setEditing(null);
      setMessage("Key revoked.");
      await onChanged();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be revoked."); }
    finally { setBusy(false); }
  };
  const edit = (key: JSONRecord) => {
    const policy = policyFor(key);
    if (stringValue(key.status) === "revoked" || (mode === "portal" && policy.admin_managed === true)) return;
    setRPM(String(numberValue(policy.rpm)));
    setDailyRequests(String(numberValue(policy.daily_requests)));
    setScope(policy);
    setCreating(false);
    setEditing(key);
    setMessage("");
  };
  const startCreate = () => {
    const nextProject = projectFilter || (ownerFilter ? stringValue(creatableProjects.find((project) =>
      eligibleOwners(stringValue(project.id)).some((owner) => owner.id === ownerFilter))?.id) : initialProjectID);
    setProjectID(nextProject);
    setPrincipalID(ownerFilter ? eligibleOwners(nextProject).some((owner) => owner.id === ownerFilter) ? ownerFilter : ownerDecisionRequired : "");
    setScope(initialScope());
    setRPM("0");
    setDailyRequests("0");
    setEditing(null);
    setCreating(true);
    setMessage("");
  };

  return <div class="page-stack">
    <PageHeading eyebrow="Credential governance" title="API keys" detail="Scope gateway keys by owner and project, inspect policy, and revoke with confirmation." actions={<button ref={createButtonRef} class="button button--primary" type="button" disabled={busy} onClick={startCreate}><Plus size={16} /> Create key</button>} />
    {message ? <p class="route-message" role="status">{message}</p> : null}
    <section class="surface key-list-filters" aria-label="Filter API keys">
      {mode === "admin" ? <label>Owner<select value={ownerFilter} onChange={(event) => setOwnerFilter(event.currentTarget.value)}><option value="">All owners</option>{[...ownerNames].filter(([id]) => id).map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label> : <p class="form-help">Owner: {stringValue(self.display_name, "you")}. Only your keys are shown.</p>}
      <label>Project<select value={projectFilter} onChange={(event) => setProjectFilter(event.currentTarget.value)}><option value="">All projects</option>{projects.map((project) => <option key={stringValue(project.id)} value={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug, stringValue(project.id)))}</option>)}</select></label>
      <label>Search keys<input type="search" value={search} onInput={(event) => setSearch(event.currentTarget.value)} placeholder="Name, prefix, owner or project" /></label>
      {routeFilter ? <button class="button button--secondary" type="button" onClick={() => setRouteFilter("")}>Explicit route grant: {routeFilter} <X size={14} aria-label="Clear route filter" /></button> : null}
    </section>
    {creating || editing ? <form ref={editorFormRef} class="surface key-editor" onSubmit={(event) => { if (editing) { event.preventDefault(); void update(editing); } else void create(event); }}>
      <header><div><p class="eyebrow">{editing ? "Policy edit" : "New key"}</p><h2>{editing ? stringValue(editing.name, "Gateway key") : "Issue credential"}</h2></div><button class="icon-button" type="button" disabled={busy} aria-label="Close key editor" onClick={() => { setCreating(false); setEditing(null); }}><X size={17} /></button></header>
      {creating ? <>
        <label>Project<select value={projectID} disabled={busy} onChange={(event) => { const id = event.currentTarget.value; setProjectID(id); if (principalID && !eligibleOwners(id).some((owner) => owner.id === principalID)) setPrincipalID(ownerDecisionRequired); }}><option value="">Select project</option>{creatableProjects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label>
        <label>Name<input value={name} disabled={busy} onInput={(event) => setName(event.currentTarget.value)} /></label>
        {mode === "admin" ? <>
          <label>Acts as<select value={principalID} disabled={busy} onChange={(event) => setPrincipalID(event.currentTarget.value)}><option value={ownerDecisionRequired} disabled>Choose an owner for this project</option><option value="">Create or reuse a service identity for this project and key name</option>{owners.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))} ({stringValue(principal.kind)})</option>)}</select></label>
          <p class="form-help">Administrators can issue keys for active human and service members, including viewers. Portal self-service creation excludes viewers. Selecting the service default can reuse an identity created for the same project and key name.</p>
          {principalID === ownerDecisionRequired ? <p class="form-error" role="alert">The requested owner is unavailable in this project. Choose an owner or explicitly select the service identity option before creating a key.</p> : null}
        </> : <p class="form-help">Acts as {stringValue(self.display_name, "your signed-in identity")}.</p>}
      </> : <p class="form-help">Owner: {stringValue(editing?.principal, stringValue(editing?.principal_id))}. Project: {stringValue(editing?.project, stringValue(editing?.project_id))}. Other quota limits are preserved.</p>}
      <p class="form-help">{mode === "admin" ? "Keys created or updated here are admin-managed. Their owners may reveal or revoke them in the portal, but cannot change policy or status." : "Admin-managed keys can be revealed or revoked here; policy and status changes require an administrator."}</p>
      <p class="form-help">{(creating ? mode === "portal" || selectedOwner?.kind === "human" : editing?.principal_kind === "human") ? "Human keys use the owner's eligible private connections first, with provider-specific fallback to shared or legacy credentials." : "Service keys use eligible shared or legacy credentials, not a human's private subscriptions."} Copilot service access requires an active project binding; Codex OAuth is human-private. Scope does not pin a credential account, and paid fallback may occur.</p>
      <div class="key-editor__limits"><label>Requests per minute<input inputMode="numeric" value={rpm} disabled={busy} onInput={(event) => setRPM(event.currentTarget.value)} /></label><label>Daily requests<input inputMode="numeric" value={dailyRequests} disabled={busy} onInput={(event) => setDailyRequests(event.currentTarget.value)} /></label></div>
      <p class="form-help">Use 0 for no additional key limit. Project access rules and usage ceilings still apply; key settings cannot raise them. These are configured limits, not remaining balances.</p>
      <KeyScopeEditor data={data} policy={{ ...scope, rpm: Number(rpm), daily_requests: Number(dailyRequests) }} onChange={setScope} />
      <footer><button class="button button--secondary" type="button" disabled={busy} onClick={() => { setCreating(false); setEditing(null); }}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{editing ? <Save size={16} /> : <Plus size={16} />}{busy ? "Saving..." : editing ? "Save policy" : "Create key"}</button></footer>
    </form> : null}
    {!filteredKeys.length ? <EmptyState title={keys.length ? "No keys match these filters" : "No API keys in this workspace"} detail={keys.length ? "Clear or change the owner, project, route or search filter." : "Create a key for an active project with key-management permission."} /> : <section class="surface table-wrap"><table><thead><tr><th>Name</th><th>Prefix</th><th>Project</th><th>Owner</th><th>Policy</th><th>Status</th><th>Actions</th></tr></thead><tbody>{filteredKeys.map((key) => {
      const policy = policyFor(key);
      const active = stringValue(key.status, "active") === "active";
      const revoked = stringValue(key.status) === "revoked";
      const expiresAt = numberValue(key.expires_at);
      const expired = key.expired === true || (expiresAt > 0 && expiresAt * 1000 <= Date.now());
      const status = revoked ? "revoked" : expired ? "expired" : stringValue(key.status, "active");
      const managed = policy.admin_managed === true;
      const locked = mode === "portal" && managed;
      const id = stringValue(key.id);
      const visible = Boolean(revealed[id]);
      return <tr key={id}>
        <td><KeyRound size={15} /> {stringValue(key.name, "Gateway key")}</td>
        <td><span class="key-value"><span class="technical">{visible ? revealed[id] : stringValue(key.prefix, "Hidden")}</span>{key.revealable === true ? <button class="icon-button" type="button" aria-label={`${visible ? "Hide" : "Reveal"} ${stringValue(key.name, "API key")}`} title={visible ? "Hide key" : "Reveal key"} disabled={busy || Boolean(revealingID)} onClick={() => void reveal(key)}>{visible ? <EyeOff size={15} /> : <Eye size={15} />}</button> : null}</span></td>
        <td>{stringValue(key.project, stringValue(key.project_id))}</td><td>{stringValue(key.principal, ownerNames.get(stringValue(key.principal_id)) ?? stringValue(key.principal_id))}</td>
        <td class="key-policy-summary">{keyPolicySummary(policy)}{managed ? <small>Admin-managed{locked ? ": ask an administrator to change policy or status." : ""}</small> : null}</td>
        <td><span class={`status-pill ${revoked || expired ? "status-pill--attention" : active ? "status-pill--ready" : "status-pill--muted"}`} title={revoked ? "Permanently revoked" : expired ? "This key has expired; enabling it does not renew its expiry." : undefined}>{status}</span></td>
        <td><div class="table-actions"><button class="icon-button" type="button" aria-label={`Edit ${stringValue(key.name)}`} title={locked ? "Only administrators can edit this key" : "Edit policy"} disabled={busy || revoked || locked} onClick={() => edit(key)}><Pencil size={15} /></button>{!revoked ? <button class="button button--secondary" type="button" disabled={busy || locked || expired} title={expired ? "Expired keys cannot be re-enabled here" : locked ? "Only administrators can change this key's status" : undefined} onClick={() => void update(key, active)}>{active ? "Disable" : "Enable"}</button> : null}<button class="icon-button" type="button" aria-label={`Revoke ${stringValue(key.name)}`} disabled={busy || revoked} onClick={() => void revoke(key)}><Trash2 size={15} /></button></div></td>
      </tr>;
    })}</tbody></table></section>}
    {secret ? <KeySecret token={secret} onDismiss={() => setSecret("")} returnFocus={createButtonRef.current} /> : null}
  </div>;
}
