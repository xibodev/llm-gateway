import { useEffect, useRef, useState } from "preact/hooks";
import { FolderOpen, KeyRound, LoaderCircle, PencilLine, Power, Trash2, UserPlus, Users, FolderPlus, X } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import { principalsPath } from "../lib/directory";
import type { ConsoleMode } from "../lib/mode";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { AddMembershipDialog } from "../components/AddMembershipDialog";
import { ShortID, dataTable, serverTableView, tablePageSizes, useTableView, type ServerTableState, type TableColumn } from "../components/DataTable";
import { EmptyState, ErrorState, LoadingState, PageHeading } from "../components/PageState";
import { useDialogFocus } from "../components/useDialogFocus";
import { ProjectDetail } from "./ProjectDetail";

type ActionResult = { title: string; success: boolean; detail: string } | null;

function ResultNotice({ result }: { result: ActionResult }) {
  if (!result) return null;
  return <section class={`action-notice ${result.success ? "action-notice--success" : "action-notice--warning"}`} role="status"><strong>{result.title}</strong><span>{result.detail}</span></section>;
}

function CreatePrincipalDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => Promise<void> }) {
  const [displayName, setDisplayName] = useState("");
  const [kind, setKind] = useState("human");
  const [email, setEmail] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dialogRef = useDialogFocus(onClose);
  const submit = async (event: Event) => {
    event.preventDefault();
    if (!displayName.trim()) { setError("A display name is required."); return; }
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/principals", "POST", { kind, display_name: displayName, email });
      await onCreated();
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Principal could not be created.");
    } finally { setBusy(false); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="create-principal-title" tabIndex={-1}><header><div><p class="eyebrow">Access</p><h2 id="create-principal-title">New principal</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><p class="muted-copy">Human principals can own private provider connections and catalogs. Service principals identify automated callers and cannot hold OAuth subscriptions.</p><label>Display name<input value={displayName} onInput={(event) => setDisplayName((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label><label>Kind<select value={kind} onInput={(event) => setKind((event.currentTarget as HTMLSelectElement).value)}><option value="human">Human</option><option value="service">Service</option></select></label><label>Email (optional)<input type="email" value={email} onInput={(event) => setEmail((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <UserPlus size={16} />} Create principal</button></footer></form></section></div>;
}

function RenamePrincipalDialog({ principal, onClose, onRenamed }: { principal: JSONRecord; onClose: () => void; onRenamed: (name: string) => Promise<void> }) {
  const [displayName, setDisplayName] = useState(stringValue(principal.display_name));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dialogRef = useDialogFocus(onClose);
  const submit = async (event: Event) => {
    event.preventDefault();
    const name = displayName.trim();
    if (!name) { setError("A display name is required."); return; }
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", `/principals/${encodeURIComponent(stringValue(principal.id))}/rename`, "POST", { display_name: name });
      await onRenamed(name);
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Principal could not be renamed.");
    } finally { setBusy(false); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="rename-principal-title" tabIndex={-1}><header><div><p class="eyebrow">Access</p><h2 id="rename-principal-title">Rename principal</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><p class="muted-copy">The name you choose sticks: signing in no longer replaces it with the name the identity provider reports.</p><label>Display name<input value={displayName} maxLength={200} onInput={(event) => setDisplayName((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <PencilLine size={16} />} Save name</button></footer></form></section></div>;
}

function CreateProjectDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => Promise<void> }) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dialogRef = useDialogFocus(onClose);
  const submit = async (event: Event) => {
    event.preventDefault();
    if (!slug.trim()) { setError("A project slug is required."); return; }
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/projects", "POST", { slug, name });
      await onCreated();
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Project could not be created.");
    } finally { setBusy(false); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="create-project-title" tabIndex={-1}><header><div><p class="eyebrow">Access</p><h2 id="create-project-title">New project</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><p class="muted-copy">Projects scope API keys, budgets, and memberships. Keys are always minted inside a project.</p><label>Slug<input value={slug} onInput={(event) => setSlug((event.currentTarget as HTMLInputElement).value)} placeholder="my-team" autoComplete="off" /></label><label>Name (optional)<input value={name} onInput={(event) => setName((event.currentTarget as HTMLInputElement).value)} placeholder="My Team" autoComplete="off" /></label>{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <FolderPlus size={16} />} Create project</button></footer></form></section></div>;
}

export function Access({ data, mode, detail = "", onChanged, onNavigate }: { data: JSONRecord; mode: ConsoleMode; detail?: string; onChanged: () => Promise<void>; onNavigate?: (page: PageID, detail?: string) => void }) {
  const [createPrincipal, setCreatePrincipal] = useState(false);
  const [renamePrincipal, setRenamePrincipal] = useState<JSONRecord | null>(null);
  const [createProject, setCreateProject] = useState(false);
  const [membershipProject, setMembershipProject] = useState<JSONRecord | null>(null);
  const [result, setResult] = useState<ActionResult>(null);
  const [busy, setBusy] = useState("");
  const projects = asList(data.projects).map(asRecord);
  const memberships = asList(data.memberships).map(asRecord);
  // The state counts the active people and service principals a project can
  // take as members, and memberships name their principal.
  const activePrincipals = numberValue(asRecord(data.counts).active_principals);
  const memberName = (membership: JSONRecord) => stringValue(membership.principal_name, stringValue(membership.principal_id));
  // The server pages, sorts and searches the principals; listing is its
  // answer for the page shown.
  const [listing, setListing] = useState<{ principals: JSONRecord[]; total: number } | null>(null);
  const [listError, setListError] = useState("");
  const [search, setSearch] = useState("");
  const [table, setTable] = useState<ServerTableState>({ page: 0, pageSize: tablePageSizes[0], sort: null });
  const [reload, setReload] = useState(0);
  const listRequest = useRef(0);
  const listPath = principalsPath({ q: search, limit: table.pageSize, offset: table.page * table.pageSize, sort: table.sort?.id, descending: table.sort?.descending });
  useEffect(() => {
    if (mode !== "admin" || detail) return;
    // A page that arrives after a newer request must not replace its answer.
    const request = ++listRequest.current;
    setListError("");
    getJSON<JSONRecord>("admin", listPath).then((payload) => {
      if (request === listRequest.current) setListing({ principals: asList(payload.principals).map(asRecord), total: numberValue(payload.total) });
    }).catch((cause) => {
      if (request === listRequest.current) setListError(cause instanceof Error ? cause.message : "Principals could not load.");
    });
  }, [mode, detail, listPath, reload]);
  const principals = listing?.principals ?? [];
  // changed reloads the principals and the console's state after a change.
  const changed = async () => {
    setReload((current) => current + 1);
    await onChanged();
  };
  const memberCount = (projectID: string) => memberships.filter((membership) => stringValue(membership.project_id) === projectID).length;

  const setStatus = async (target: "principals" | "projects", record: JSONRecord) => {
    const id = stringValue(record.id);
    const next = stringValue(record.status, "active") === "active" ? "disabled" : "active";
    setBusy(`${target}-${id}`);
    try {
      await sendJSON<JSONRecord>("admin", `/${target}/status`, "POST", { id, status: next });
      setResult({ title: next === "active" ? "Enabled" : "Disabled", success: true, detail: `${stringValue(record.display_name, stringValue(record.name, id))} is now ${next}.` });
      await changed();
    } catch (cause) {
      setResult({ title: "Status change failed", success: false, detail: cause instanceof Error ? cause.message : "The status could not be changed." });
    } finally { setBusy(""); }
  };

  const removeMembership = async (membership: JSONRecord) => {
    const projectID = stringValue(membership.project_id);
    const principalID = stringValue(membership.principal_id);
    setBusy(`member-${projectID}-${principalID}`);
    try {
      await sendJSON<JSONRecord>("admin", `/memberships?project_id=${encodeURIComponent(projectID)}&principal_id=${encodeURIComponent(principalID)}`, "DELETE");
      setResult({ title: "Membership removed", success: true, detail: `${memberName(membership)} was removed from the project.` });
      await onChanged();
    } catch (cause) {
      setResult({ title: "Removal failed", success: false, detail: cause instanceof Error ? cause.message : "The membership could not be removed." });
    } finally { setBusy(""); }
  };

  const statusPill = (status: string) => <span class={`status-pill ${status === "active" ? "status-pill--ready" : "status-pill--muted"}`}>{status}</span>;
  const projectName = (project: JSONRecord) => stringValue(project.name, stringValue(project.slug, stringValue(project.id)));
  const principalColumns: TableColumn<JSONRecord>[] = [
    {
      id: "name", header: "Name", sortable: true,
      cell: (principal) => <><strong>{stringValue(principal.display_name, stringValue(principal.id))}</strong>{principal.name_set_by_admin === true && stringValue(principal.external_subject) ? <small class="table-subtitle">Named by an administrator</small> : null}</>,
    },
    { id: "id", header: "ID", cell: (principal) => <ShortID id={stringValue(principal.id)} label="principal ID" /> },
    { id: "kind", header: "Kind", sortable: true, cell: (principal) => stringValue(principal.kind) },
    { id: "email", header: "Email", sortable: true, cell: (principal) => stringValue(principal.email, "—") },
    { id: "status", header: "Status", sortable: true, cell: (principal) => statusPill(stringValue(principal.status, "active")) },
    {
      id: "actions", header: "Actions", cell: (principal) => {
        const id = stringValue(principal.id);
        const name = stringValue(principal.display_name, id);
        const active = stringValue(principal.status, "active") === "active";
        if (stringValue(principal.kind) === "system") return <span class="provider-card__meta">Built-in</span>;
        return <div class="row-actions">
          {onNavigate ? <button class="icon-button icon-button--compact" type="button" aria-label={`Keys of ${name}`} title="Keys" onClick={() => onNavigate("keys", `owner=${encodeURIComponent(id)}`)}><KeyRound size={14} /></button> : null}
          <button class="icon-button icon-button--compact" type="button" aria-label={`Rename ${name}`} title="Rename" onClick={() => setRenamePrincipal(principal)}><PencilLine size={14} /></button>
          <button class="icon-button icon-button--compact" type="button" aria-label={`${active ? "Disable" : "Enable"} ${name}`} title={active ? "Disable" : "Enable"} disabled={busy === `principals-${id}`} onClick={() => void setStatus("principals", principal)}><Power size={14} /></button>
        </div>;
      },
    },
  ];
  const projectColumns: TableColumn<JSONRecord>[] = [
    { id: "project", header: "Project", sortValue: projectName, cell: (project) => <strong>{projectName(project)}</strong> },
    { id: "id", header: "ID", cell: (project) => <ShortID id={stringValue(project.id)} label="project ID" /> },
    { id: "slug", header: "Slug", class: "technical", sortValue: (project) => stringValue(project.slug), cell: (project) => stringValue(project.slug) },
    { id: "status", header: "Status", sortValue: (project) => stringValue(project.status, "active"), cell: (project) => statusPill(stringValue(project.status, "active")) },
    { id: "members", header: "Members", sortValue: (project) => memberCount(stringValue(project.id)), cell: (project) => memberCount(stringValue(project.id)) },
    {
      id: "actions", header: "Actions", cell: (project) => {
        const id = stringValue(project.id);
        const name = projectName(project);
        const active = stringValue(project.status, "active") === "active";
        return <div class="row-actions">
          {onNavigate ? <button class="icon-button icon-button--compact" type="button" aria-label={`Open ${name}`} title="Its limits, keys, members and usage" onClick={() => onNavigate("access", id)}><FolderOpen size={14} /></button> : null}
          {onNavigate ? <button class="icon-button icon-button--compact" type="button" aria-label={`Keys of ${name}`} title="Keys" onClick={() => onNavigate("keys", `project=${encodeURIComponent(id)}`)}><KeyRound size={14} /></button> : null}
          <button class="icon-button icon-button--compact" type="button" aria-label={`Add a member to ${name}`} title={activePrincipals ? "Add member" : "Create an active principal first"} disabled={!activePrincipals} onClick={() => setMembershipProject(project)}><Users size={14} /></button>
          <button class="icon-button icon-button--compact" type="button" aria-label={`${active ? "Disable" : "Enable"} ${name}`} title={active ? "Disable" : "Enable"} disabled={busy === `projects-${id}`} onClick={() => void setStatus("projects", project)}><Power size={14} /></button>
        </div>;
      },
    },
  ];
  const membershipProjectName = (membership: JSONRecord) => {
    const project = projects.find((candidate) => stringValue(candidate.id) === stringValue(membership.project_id));
    return project ? projectName(project) : stringValue(membership.project_id);
  };
  const membershipColumns: TableColumn<JSONRecord>[] = [
    { id: "principal", header: "Principal", sortValue: memberName, cell: memberName },
    { id: "project", header: "Project", sortValue: membershipProjectName, cell: membershipProjectName },
    { id: "role", header: "Role", sortValue: (membership) => stringValue(membership.role), cell: (membership) => stringValue(membership.role) },
    {
      id: "actions", header: "Actions", cell: (membership) => {
        const projectID = stringValue(membership.project_id);
        const principalID = stringValue(membership.principal_id);
        return <div class="row-actions"><button class="icon-button icon-button--compact" type="button" aria-label={`Remove ${memberName(membership)} from ${membershipProjectName(membership)}`} title="Remove" disabled={busy === `member-${projectID}-${principalID}`} onClick={() => void removeMembership(membership)}><Trash2 size={14} /></button></div>;
      },
    },
  ];
  const principalView = serverTableView(principals, listing?.total ?? 0, table, setTable);
  const projectView = useTableView(projects, projectColumns);
  const membershipView = useTableView(memberships, membershipColumns);

  if (mode !== "admin") {
    return <div class="page-stack"><PageHeading eyebrow="Access" title="Access" detail="Workspace access is administered from the admin console." /><EmptyState title="Administrator area" detail="Ask a gateway administrator to manage principals, projects, and memberships." /></div>;
  }
  // A detail names the project whose page is open.
  if (detail) {
    return <ProjectDetail projectID={detail} data={data} onChanged={onChanged} onNavigate={(page, next) => onNavigate?.(page, next)} onBack={() => onNavigate?.("access")} />;
  }

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Identity and projects" title="Access" detail="Create the humans and services that own catalogs and keys, group them into projects, and control membership." actions={<><button class="button button--secondary" type="button" onClick={() => setCreateProject(true)}><FolderPlus size={16} /> New project</button><button class="button button--primary" type="button" onClick={() => setCreatePrincipal(true)}><UserPlus size={16} /> New principal</button></>} />
      <ResultNotice result={result} />
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Principals</p><h2>Humans and services</h2></div><span>{principalView.total} record{principalView.total === 1 ? "" : "s"}</span></div>
        <label>Search principals<input type="search" value={search} onInput={(event) => { const value = event.currentTarget.value; setSearch(value); setTable((current) => ({ ...current, page: 0 })); }} placeholder="Name, email or ID" /></label>
        {listError ? <ErrorState title="Principals could not load" detail={listError} action={<button class="button button--secondary" type="button" onClick={() => setReload((current) => current + 1)}>Retry</button>} />
          : listing === null ? <LoadingState title="Loading principals" />
          : listing.total === 0 ? <EmptyState title={search ? "No principals match this search" : "No principals yet"} detail={search ? "Search by another name, email or ID." : "Create a human principal to own private catalogs, playground runs, and provider connections."} />
          : dataTable(principalView, principalColumns, { label: "Principals", rowKey: (principal) => stringValue(principal.id) })}
      </section>
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Projects</p><h2>Key and budget scopes</h2></div><span>{projects.length} project{projects.length === 1 ? "" : "s"}</span></div>
        <p class="muted-copy">Open a project for its limits and the budgets and allowlists that set them, its keys, its members and its usage.</p>
        {projects.length === 0 ? <EmptyState title="No projects yet" detail="Create a project before minting API keys — every key is scoped to a project." /> : dataTable(projectView, projectColumns, { label: "Projects", rowKey: (project) => stringValue(project.id) })}
      </section>
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Memberships</p><h2>Who can act in which project</h2></div><span>{memberships.length} membership{memberships.length === 1 ? "" : "s"}</span></div>
        {memberships.length === 0 ? <EmptyState title="No memberships yet" detail="Add a principal to a project so it can hold keys and run project-attributed requests." /> : dataTable(membershipView, membershipColumns, { label: "Memberships", rowKey: (membership) => `${stringValue(membership.project_id)}-${stringValue(membership.principal_id)}` })}
      </section>
      {createPrincipal ? <CreatePrincipalDialog onClose={() => setCreatePrincipal(false)} onCreated={changed} /> : null}
      {renamePrincipal ? <RenamePrincipalDialog principal={renamePrincipal} onClose={() => setRenamePrincipal(null)} onRenamed={async (name) => { setResult({ title: "Renamed", success: true, detail: `${stringValue(renamePrincipal.display_name, stringValue(renamePrincipal.id))} is now ${name}.` }); await changed(); }} /> : null}
      {createProject ? <CreateProjectDialog onClose={() => setCreateProject(false)} onCreated={onChanged} /> : null}
      {membershipProject ? <AddMembershipDialog project={membershipProject} onClose={() => setMembershipProject(null)} onCreated={onChanged} /> : null}
    </div>
  );
}
