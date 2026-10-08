import { useEffect, useRef, useState } from "preact/hooks";
import { Ban, Check, Copy, Eye, EyeOff, Gauge, KeyRound, Pencil, Plus, Save, Trash2, X } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";
import { KeyScopeEditor, keyPolicySummary, keyQuotaLabels } from "../components/KeyScopeEditor";
import { KeyLimitsDialog } from "../components/LimitUsage";
import { RemoteSearchSelect, SearchSelect, dataTable, serverTableView, tablePageSizes, type ServerTableState, type TableColumn } from "../components/DataTable";
import { keyCount, principalSearch } from "../lib/directory";
import { keyQuotaDraftsFor, keyQuotaFields, keyQuotaPolicyFromDrafts } from "../lib/key-policy";
import { formatKeyTime, keyExpiryFromInput, keyExpiryInputValue, keyTimes } from "../lib/key-dates";
import { useDialogFocus } from "../components/useDialogFocus";
import "../styles/keys.css";

const ownerDecisionRequired = "__select_owner__";

// keyExpired reports a key whose expiry has passed. It stays expired: its
// expiry can no longer change, and a new key replaces it.
function keyExpired(key: JSONRecord): boolean {
  const expires = keyTimes(key).expires;
  return key.expired === true || (expires > 0 && expires * 1000 <= Date.now());
}

// keyStatus is the status the list shows: revoked, expired, disabled or active.
function keyStatus(key: JSONRecord): string {
  if (stringValue(key.status) === "revoked") return "revoked";
  return keyExpired(key) ? "expired" : stringValue(key.status, "active");
}

// keyDeletable reports a key that no longer works, revoked or expired, which
// is the only kind deleting removes from the list. Its usage and audit
// history keep its name.
function keyDeletable(key: JSONRecord): boolean {
  return ["revoked", "expired"].includes(keyStatus(key));
}

// The status filters of the list, which the server applies. The first, the
// default, hides the keys that no longer work.
const statusFilters: { id: string; label: string }[] = [
  { id: "usable", label: "Active and disabled" },
  { id: "ended", label: "Revoked or expired" },
  { id: "active", label: "Active" },
  { id: "disabled", label: "Disabled" },
  { id: "expired", label: "Expired" },
  { id: "revoked", label: "Revoked" },
  { id: "all", label: "All statuses" },
];

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
  const projects = asList(data.projects).map(asRecord);
  const memberships = asList(data.memberships).map(asRecord);
  const self = asRecord(data.principal);
  // A project's eligible owners are its active people and service principals,
  // whom its memberships name. The portal acts as its signed-in user.
  const eligibleOwners = (id: string) => mode === "portal" ? [] : memberships.filter((membership) =>
    stringValue(membership.project_id) === id &&
    ["human", "service"].includes(stringValue(membership.principal_kind)) && stringValue(membership.principal_status) === "active" &&
    ["owner", "admin", "member", "viewer"].includes(stringValue(membership.role)) && stringValue(membership.status, "active") === "active")
    .map((membership) => ({ id: stringValue(membership.principal_id), display_name: stringValue(membership.principal_name, stringValue(membership.principal_id)), kind: stringValue(membership.principal_kind) }));
  const creatableProjects = projects.filter((project) => stringValue(project.status, "active") === "active" &&
    (mode === "admin" || memberships.some((membership) => stringValue(membership.project_id) === stringValue(project.id) &&
      stringValue(membership.principal_id) === stringValue(self.id) && ["owner", "admin", "member"].includes(stringValue(membership.role)))));
  const initialProjectID = initialContext?.projectID ?? stringValue(creatableProjects.find((project) =>
    !initialContext?.ownerID || eligibleOwners(stringValue(project.id)).some((owner) => owner.id === initialContext.ownerID))?.id);
  const [creating, setCreating] = useState(Boolean(initialContext));
  const [editing, setEditing] = useState<JSONRecord | null>(null);
  const [name, setName] = useState("Gateway key");
  const [expiryDraft, setExpiryDraft] = useState("");
  const [projectID, setProjectID] = useState(initialProjectID);
  const [principalID, setPrincipalID] = useState(initialContext?.ownerID
    ? eligibleOwners(initialProjectID).some((owner) => owner.id === initialContext.ownerID) ? initialContext.ownerID : ownerDecisionRequired
    : "");
  const [ownerFilter, setOwnerFilter] = useState(mode === "admin" ? initialContext?.ownerID ?? "" : "");
  const [projectFilter, setProjectFilter] = useState(initialContext?.projectID ?? "");
  const [search, setSearch] = useState("");
  const [routeFilter, setRouteFilter] = useState(initialContext?.routeName ?? "");
  const [statusFilter, setStatusFilter] = useState(statusFilters[0].id);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const initialScope = (): JSONRecord => ({ allowed_routes: initialContext?.routeName ? [initialContext.routeName] : [], routes_only: Boolean(initialContext?.routeName), allowed_providers: [], allowed_models: [] });
  const [scope, setScope] = useState<JSONRecord>(initialScope);
  const [quotaDrafts, setQuotaDrafts] = useState<Record<string, string>>(() => keyQuotaDraftsFor({}));
  const [secret, setSecret] = useState("");
  // The key whose limits are open, and the button that opened them.
  const [limitsOf, setLimitsOf] = useState<{ key: JSONRecord; opener: HTMLElement | null } | null>(null);
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [revealingID, setRevealingID] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const createButtonRef = useRef<HTMLButtonElement | null>(null);
  const editorFormRef = useRef<HTMLFormElement | null>(null);
  // The server pages, sorts and filters the list; listing is its answer
  // for the page shown.
  const [listing, setListing] = useState<{ keys: JSONRecord[]; total: number } | null>(null);
  const [listError, setListError] = useState("");
  const [table, setTable] = useState<ServerTableState>({ page: 0, pageSize: tablePageSizes[0], sort: null });
  const [reload, setReload] = useState(0);
  const listRequest = useRef(0);
  const listQuery = new URLSearchParams({ status: statusFilter, limit: String(table.pageSize), offset: String(table.page * table.pageSize) });
  if (mode === "admin" && ownerFilter) listQuery.set("principal_id", ownerFilter);
  if (projectFilter) listQuery.set("project_id", projectFilter);
  if (routeFilter) listQuery.set("route", routeFilter);
  if (search.trim()) listQuery.set("q", search.trim());
  if (table.sort) {
    listQuery.set("sort", table.sort.id);
    listQuery.set("order", table.sort.descending ? "desc" : "asc");
  }
  const listPath = `/keys?${listQuery.toString()}`;
  useEffect(() => {
    // A page that arrives after a newer request must not replace its answer.
    const request = ++listRequest.current;
    setListError("");
    getJSON<JSONRecord>(mode, listPath).then((payload) => {
      if (request === listRequest.current) setListing({ keys: asList(payload.keys).map(asRecord), total: numberValue(payload.total) });
    }).catch((cause) => {
      if (request === listRequest.current) setListError(cause instanceof Error ? cause.message : "Keys could not load.");
    });
  }, [mode, listPath, reload]);
  // A page emptied, as deleting the last keys of the last page leaves it,
  // gives way to the last page that has keys.
  useEffect(() => {
    if (listing && !listing.keys.length && listing.total > 0 && table.page > 0) {
      setTable((current) => ({ ...current, page: Math.max(0, Math.ceil(listing.total / current.pageSize) - 1) }));
    }
  }, [listing]);
  const keys = listing?.keys ?? [];
  // filtered changes a filter of the list, which then shows its first page
  // and nothing selected.
  const filtered = (change: () => void) => {
    change();
    setTable((current) => ({ ...current, page: 0 }));
    setSelected({});
  };
  // changed reloads the list and the console's state after a change.
  const changed = async () => {
    setReload((current) => current + 1);
    await onChanged();
  };
  const owners = eligibleOwners(projectID);
  const selectedOwner = owners.find((owner) => owner.id === principalID);
  // Only the deletable keys the list shows can be selected, so a filter never
  // hides a key it would delete.
  const deletableShown = keys.filter(keyDeletable).map((key) => stringValue(key.id));
  const selectedIDs = deletableShown.filter((id) => selected[id]);
  const editablePolicy = (): JSONRecord | null => {
    // Read text drafts from the form too: Enter can submit before Preact renders input state.
    const form = editorFormRef.current ? new FormData(editorFormRef.current) : null;
    const submittedQuotaDrafts = Object.fromEntries(keyQuotaFields.map((field) => {
      const draft = form?.get(field);
      return [field, typeof draft === "string" ? draft : quotaDrafts[field] ?? "0"];
    }));
    const quotas = keyQuotaPolicyFromDrafts(submittedQuotaDrafts);
    if (!quotas.policy) {
      setMessage(quotas.error);
      return null;
    }
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
    return { ...quotas.policy, allowed_routes: allowedRoutes,
      routes_only: scope.routes_only === true, allowed_providers: lists.allowed_providers, allowed_models: lists.allowed_models };
  };
  const create = async (event: Event) => {
    event.preventDefault();
    if (busy) return;
    if (!creatableProjects.some((project) => project.id === projectID) || !name.trim()) { setMessage("An active project with key-management permission and a key name are required."); return; }
    if (mode === "admin" && principalID && !selectedOwner) { setMessage("Choose an active project member or explicitly choose to create or reuse a service identity."); return; }
    const policy = editablePolicy();
    if (!policy) return;
    // Like the limits, read the field from the form: Enter can submit first.
    const expiryField = editorFormRef.current ? new FormData(editorFormRef.current).get("expires_at") : null;
    const expiry = keyExpiryFromInput(typeof expiryField === "string" ? expiryField : expiryDraft);
    if (expiry.error) { setMessage(expiry.error); return; }
    setBusy(true);
    setMessage("");
    try {
      const payload: JSONRecord = { project_id: projectID, name: name.trim(), ...policy };
      if (expiry.expiresAt) payload.expires_at = expiry.expiresAt;
      if (mode === "admin" && principalID) payload.principal_id = principalID;
      const response = await sendJSON<JSONRecord>(mode, "/keys", "POST", payload);
      const token = stringValue(response.token);
      if (!token) throw new Error("The server did not return the key value.");
      setSecret(token);
      setCreating(false);
      await changed();
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
    if (disabled === undefined) {
      const expiry = editedExpiry(key);
      if (expiry.error) { setMessage(expiry.error); return; }
      if (expiry.changed) payload.expires_at = expiry.expiresAt;
    }
    setBusy(true);
    try {
      if (mode === "admin") payload.id = id;
      await sendJSON<JSONRecord>(mode, mode === "admin" ? "/keys/update" : `/keys/${encodeURIComponent(id)}/update`, "POST", payload);
      setEditing(null);
      setMessage(disabled === undefined ? "Key updated." : "Key status updated.");
      await changed();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be updated."); }
    finally { setBusy(false); }
  };
  const revoke = async (key: JSONRecord) => {
    const id = stringValue(key.id);
    if (!id || !window.confirm(`Revoke ${stringValue(key.name, "this key")}? Existing clients will stop working, and the key cannot be enabled again.`)) return;
    setBusy(true);
    try {
      await sendJSON<JSONRecord>(mode, mode === "admin" ? `/keys?id=${encodeURIComponent(id)}` : `/keys/${encodeURIComponent(id)}`, "DELETE");
      setRevealed((current) => { const next = { ...current }; delete next[id]; return next; });
      if (editing?.id === id) setEditing(null);
      setMessage("Key revoked.");
      await changed();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be revoked."); }
    finally { setBusy(false); }
  };
  // remove deletes keys that no longer work. Deleting takes them out of the
  // list; their usage and audit history keep their names.
  const remove = async (ids: string[]) => {
    if (!ids.length || busy) return;
    const subject = ids.length === 1 ? stringValue(keys.find((key) => key.id === ids[0])?.name, "this key") : `${ids.length} keys`;
    if (!window.confirm(`Delete ${subject}? Deleting removes a revoked or expired key from this list; its usage and audit history keep its name.`)) return;
    setBusy(true);
    try {
      const response = await sendJSON<JSONRecord>(mode, "/keys/delete", "POST", { ids });
      const deleted = asList(response.deleted).length;
      const refused = asList(response.refused).map(asRecord);
      setSelected({});
      if (editing && ids.includes(stringValue(editing.id))) setEditing(null);
      setMessage(`${deleted === 1 ? "Key" : `${deleted} keys`} deleted.${refused.length ? ` ${refused.length} not deleted: ${stringValue(refused[0].error)}` : ""}`);
      await changed();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Keys could not be deleted."); }
    finally { setBusy(false); }
  };
  const toggleSelected = (ids: string[], value: boolean) => setSelected((current) => {
    const next = { ...current };
    for (const id of ids) next[id] = value;
    return next;
  });
  const edit = (key: JSONRecord) => {
    const policy = policyFor(key);
    if (stringValue(key.status) === "revoked" || (mode === "portal" && policy.admin_managed === true)) return;
    setQuotaDrafts(keyQuotaDraftsFor(policy));
    setScope(policy);
    setExpiryDraft(keyExpiryInputValue(keyTimes(key).expires));
    setCreating(false);
    setEditing(key);
    setMessage("");
  };
  // The expiry the editor asks of key, sent only when the field changed: an
  // untouched one would drop the seconds of the stored expiry, and an expired
  // key keeps its own.
  const editedExpiry = (key: JSONRecord): { changed: boolean; expiresAt: number; error: string } => {
    const field = editorFormRef.current ? new FormData(editorFormRef.current).get("expires_at") : null;
    const draft = typeof field === "string" ? field : expiryDraft;
    if (keyExpired(key) || draft.trim() === keyExpiryInputValue(keyTimes(key).expires)) return { changed: false, expiresAt: 0, error: "" };
    return { changed: true, ...keyExpiryFromInput(draft) };
  };
  const startCreate = () => {
    const nextProject = projectFilter || (ownerFilter ? stringValue(creatableProjects.find((project) =>
      eligibleOwners(stringValue(project.id)).some((owner) => owner.id === ownerFilter))?.id) : initialProjectID);
    setProjectID(nextProject);
    setPrincipalID(ownerFilter ? eligibleOwners(nextProject).some((owner) => owner.id === ownerFilter) ? ownerFilter : ownerDecisionRequired : "");
    setScope(initialScope());
    setQuotaDrafts(keyQuotaDraftsFor({}));
    setExpiryDraft("");
    setEditing(null);
    setCreating(true);
    setMessage("");
  };

  // Whether any key exists, which tells an empty filtered list from an empty
  // workspace.
  const anyKeys = keyCount(data) > 0;
  const columns: TableColumn<JSONRecord>[] = [
    {
      id: "select", header: "Select",
      headerCell: <input type="checkbox" aria-label="Select every revoked or expired key shown" disabled={busy || !deletableShown.length} checked={deletableShown.length > 0 && selectedIDs.length === deletableShown.length} onChange={(event) => toggleSelected(deletableShown, event.currentTarget.checked)} />,
      cell: (key) => {
        const deletable = keyDeletable(key);
        const id = stringValue(key.id);
        return <input type="checkbox" aria-label={`Select ${stringValue(key.name, "API key")}`} title={deletable ? undefined : "Only a revoked or expired key can be deleted"} disabled={busy || !deletable} checked={deletable && Boolean(selected[id])} onChange={(event) => toggleSelected([id], event.currentTarget.checked)} />;
      },
    },
    { id: "name", header: "Name", sortable: true, cell: (key) => <><KeyRound size={15} /> {stringValue(key.name, "Gateway key")}</> },
    {
      id: "prefix", header: "Prefix", cell: (key) => {
        const id = stringValue(key.id);
        const visible = Boolean(revealed[id]);
        return <span class="key-value"><span class="technical">{visible ? revealed[id] : stringValue(key.prefix, "Hidden")}</span>{key.revealable === true ? <button class="icon-button" type="button" aria-label={`${visible ? "Hide" : "Reveal"} ${stringValue(key.name, "API key")}`} title={visible ? "Hide key" : "Reveal key"} disabled={busy || Boolean(revealingID)} onClick={() => void reveal(key)}>{visible ? <EyeOff size={15} /> : <Eye size={15} />}</button> : null}</span>;
      },
    },
    { id: "project", header: "Project", sortable: true, cell: (key) => stringValue(key.project, stringValue(key.project_id)) },
    { id: "owner", header: "Owner", sortable: true, cell: (key) => stringValue(key.principal, stringValue(key.principal_id)) },
    {
      id: "policy", header: "Policy", class: "key-policy-summary", cell: (key) => {
        const policy = policyFor(key);
        const managed = policy.admin_managed === true;
        const locked = mode === "portal" && managed;
        return <>{keyPolicySummary(policy)}{managed ? <small>Admin-managed{locked ? ": ask an administrator to change policy or status." : ""}</small> : null}</>;
      },
    },
    {
      id: "status", header: "Status", sortable: true, cell: (key) => {
        const revoked = stringValue(key.status) === "revoked";
        const expired = keyExpired(key);
        const active = stringValue(key.status, "active") === "active";
        return <span class={`status-pill ${revoked || expired ? "status-pill--attention" : active ? "status-pill--ready" : "status-pill--muted"}`} title={revoked ? "Permanently revoked" : expired ? "This key has expired and stays expired; create a new key to replace it." : undefined}>{keyStatus(key)}</span>;
      },
    },
    { id: "created", header: "Created", sortable: true, class: "technical", cell: (key) => formatKeyTime(keyTimes(key).created, "—") },
    { id: "expires", header: "Expires", sortable: true, class: "technical", cell: (key) => formatKeyTime(keyTimes(key).expires, "Never") },
    { id: "last_used", header: "Last used", sortable: true, class: "technical", cell: (key) => formatKeyTime(keyTimes(key).lastUsed, "Never") },
    {
      id: "actions", header: "Actions", cell: (key) => {
        const active = stringValue(key.status, "active") === "active";
        const revoked = stringValue(key.status) === "revoked";
        const deletable = keyDeletable(key);
        const locked = mode === "portal" && policyFor(key).admin_managed === true;
        const id = stringValue(key.id);
        return <div class="table-actions"><button class="icon-button" type="button" aria-label={`Limits of ${stringValue(key.name)}`} title="Limits and usage" onClick={(event) => setLimitsOf({ key, opener: event.currentTarget })}><Gauge size={15} /></button><button class="icon-button" type="button" aria-label={`Edit ${stringValue(key.name)}`} title={locked ? "Only administrators can edit this key" : "Edit key"} disabled={busy || revoked || locked} onClick={() => edit(key)}><Pencil size={15} /></button>{!deletable ? <button class="button button--secondary" type="button" disabled={busy || locked} title={locked ? "Only administrators can change this key's status" : undefined} onClick={() => void update(key, active)}>{active ? "Disable" : "Enable"}</button> : null}{deletable
          ? <button class="icon-button" type="button" aria-label={`Delete ${stringValue(key.name)}`} title="Delete: remove this key from the list; its usage and audit history keep its name" disabled={busy} onClick={() => void remove([id])}><Trash2 size={15} /></button>
          : <button class="icon-button" type="button" aria-label={`Revoke ${stringValue(key.name)}`} title="Revoke: stop this key for good" disabled={busy} onClick={() => void revoke(key)}><Ban size={15} /></button>}</div>;
      },
    },
  ];

  return <div class="page-stack">
    <PageHeading eyebrow="Credential governance" title="API keys" detail="Scope gateway keys by owner and project, inspect policy, revoke a key that should stop working, and delete keys that no longer work." actions={<button ref={createButtonRef} class="button button--primary" type="button" disabled={busy} onClick={startCreate}><Plus size={16} /> Create key</button>} />
    {message ? <p class="route-message" role="status">{message}</p> : null}
    <section class="surface key-list-filters" aria-label="Filter API keys">
      {mode === "admin" ? <RemoteSearchSelect label="Owner" noun="owners" value={ownerFilter} emptyLabel="All owners" {...principalSearch(mode, {})} onChange={(value) => filtered(() => setOwnerFilter(value))} /> : <p class="form-help">Owner: {stringValue(self.display_name, "you")}. Only your keys are shown.</p>}
      <SearchSelect label="Project" noun="projects" value={projectFilter} options={[{ value: "", label: "All projects" }, ...projects.map((project) => ({ value: stringValue(project.id), label: stringValue(project.name, stringValue(project.slug, stringValue(project.id))) }))]} onChange={(value) => filtered(() => setProjectFilter(value))} />
      <label>Status<select value={statusFilter} onChange={(event) => { const value = event.currentTarget.value; filtered(() => setStatusFilter(value)); }}>{statusFilters.map((filter) => <option key={filter.id} value={filter.id}>{filter.label}</option>)}</select></label>
      <label>Search keys<input type="search" value={search} onInput={(event) => { const value = event.currentTarget.value; filtered(() => setSearch(value)); }} placeholder="Name, prefix, owner or project" /></label>
      {selectedIDs.length ? <button class="button button--secondary" type="button" disabled={busy} onClick={() => void remove(selectedIDs)}><Trash2 size={15} /> Delete {selectedIDs.length} selected</button> : null}
      {routeFilter ? <button class="button button--secondary" type="button" onClick={() => filtered(() => setRouteFilter(""))}>Explicit route grant: {routeFilter} <X size={14} aria-label="Clear route filter" /></button> : null}
    </section>
    {creating || editing ? <form ref={editorFormRef} class="surface key-editor" onSubmit={(event) => { if (editing) { event.preventDefault(); void update(editing); } else void create(event); }}>
      <header><div><p class="eyebrow">{editing ? "Key edit" : "New key"}</p><h2>{editing ? stringValue(editing.name, "Gateway key") : "Issue credential"}</h2></div><button class="icon-button" type="button" disabled={busy} aria-label="Close key editor" onClick={() => { setCreating(false); setEditing(null); }}><X size={17} /></button></header>
      {creating ? <>
        <SearchSelect label="Project" noun="projects" value={projectID} disabled={busy} options={[{ value: "", label: "Select project" }, ...creatableProjects.map((project) => ({ value: stringValue(project.id), label: stringValue(project.name, stringValue(project.slug)) }))]} onChange={(id) => { setProjectID(id); if (principalID && !eligibleOwners(id).some((owner) => owner.id === principalID)) setPrincipalID(ownerDecisionRequired); }} />
        <label>Name<input value={name} disabled={busy} onInput={(event) => setName(event.currentTarget.value)} /></label>
        <label>Expires (optional)<input type="datetime-local" name="expires_at" value={expiryDraft} disabled={busy} onInput={(event) => setExpiryDraft(event.currentTarget.value)} /></label>
        <p class="form-help">Leave blank for a key that does not expire. The time is in this browser's time zone, and an expired key stays expired.</p>
        {mode === "admin" ? <>
          <SearchSelect label="Acts as" noun="owners" value={principalID} disabled={busy} options={[
            { value: ownerDecisionRequired, label: "Choose an owner for this project", disabled: true },
            { value: "", label: "Create or reuse a service identity for this project and key name" },
            ...owners.map((principal) => ({ value: stringValue(principal.id), label: `${stringValue(principal.display_name, stringValue(principal.id))} (${stringValue(principal.kind)})` })),
          ]} onChange={setPrincipalID} />
          <p class="form-help">Administrators can issue keys for active human and service members, including viewers. Portal self-service creation excludes viewers. Selecting the service default can reuse an identity created for the same project and key name.</p>
          {principalID === ownerDecisionRequired ? <p class="form-error" role="alert">The requested owner is unavailable in this project. Choose an owner or explicitly select the service identity option before creating a key.</p> : null}
        </> : <p class="form-help">Acts as {stringValue(self.display_name, "your signed-in identity")}.</p>}
      </> : <>
        <p class="form-help">Owner: {stringValue(editing?.principal, stringValue(editing?.principal_id))}. Project: {stringValue(editing?.project, stringValue(editing?.project_id))}. All key quota limits can be edited below.</p>
        {editing && keyExpired(editing)
          ? <p class="form-help">This key expired {formatKeyTime(keyTimes(editing).expires, "")}. An expired key stays expired; create a new key to replace it.</p>
          : <>
            <label>Expires (optional)<input type="datetime-local" name="expires_at" value={expiryDraft} disabled={busy} onInput={(event) => setExpiryDraft(event.currentTarget.value)} /></label>
            <p class="form-help">Leave blank for a key that does not expire. The time is in this browser's time zone.</p>
          </>}
      </>}
      <p class="form-help">{mode === "admin" ? "Keys created or updated here are admin-managed. Their owners may reveal or revoke them in the portal, but cannot change policy or status." : "Admin-managed keys can be revealed or revoked here; policy and status changes require an administrator."}</p>
      <p class="form-help">{(creating ? mode === "portal" || selectedOwner?.kind === "human" : editing?.principal_kind === "human") ? "Human keys use the owner's eligible private connections first, with provider-specific fallback to shared or legacy credentials." : "Service keys use eligible shared or legacy credentials, not a human's private subscriptions."} Copilot service access requires an active project binding; Codex OAuth is human-private. Scope does not pin a credential account, and paid fallback may occur.</p>
      <div class="key-editor__limits">{keyQuotaFields.map((field) => <label key={field}>{keyQuotaLabels[field]}<input name={field} inputMode="numeric" value={quotaDrafts[field] ?? "0"} disabled={busy} onInput={(event) => setQuotaDrafts((current) => ({ ...current, [field]: event.currentTarget.value }))} /></label>)}</div>
      <p class="form-help">Use 0 for no additional key limit. Project access rules and usage ceilings still apply; key settings cannot raise them. These are configured limits; a key's limits action in the list shows how much of each its current window has used.</p>
      <KeyScopeEditor data={data} policy={{ ...scope, ...keyQuotaPolicyFromDrafts(quotaDrafts).policy }} onChange={setScope} />
      <footer><button class="button button--secondary" type="button" disabled={busy} onClick={() => { setCreating(false); setEditing(null); }}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{editing ? <Save size={16} /> : <Plus size={16} />}{busy ? "Saving..." : editing ? "Save key" : "Create key"}</button></footer>
    </form> : null}
    {listError ? <ErrorState title="Keys are unavailable" detail={listError} action={<button class="button button--secondary" type="button" onClick={() => setReload((current) => current + 1)}>Retry</button>} />
      : listing === null ? <LoadingState title="Loading keys" />
      : listing.total === 0 ? <EmptyState title={anyKeys ? "No keys match these filters" : "No API keys in this workspace"} detail={anyKeys ? "Clear or change the status, owner, project, route or search filter. Revoked and expired keys are hidden unless the status filter shows them." : "Create a key for an active project with key-management permission."} />
      : <section class="surface">{dataTable(serverTableView(keys, listing.total, table, setTable), columns, { label: "API keys", rowKey: (key) => stringValue(key.id), class: "key-list-table" })}</section>}
    {secret ? <KeySecret token={secret} onDismiss={() => setSecret("")} returnFocus={createButtonRef.current} /> : null}
    {limitsOf ? <KeyLimitsDialog mode={mode} apiKey={limitsOf.key} onClose={() => setLimitsOf(null)} returnFocus={limitsOf.opener} /> : null}
  </div>;
}
