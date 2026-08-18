import { useState } from "preact/hooks";
import { LoaderCircle, ShieldCheck, UserPlus, Users, FolderPlus, X } from "lucide-preact";
import { sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, stringValue } from "../lib/records";
import { EmptyState, PageHeading } from "../components/PageState";
import { useDialogFocus } from "../components/useDialogFocus";

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

function AddMembershipDialog({ project, principals, onClose, onCreated }: { project: JSONRecord; principals: JSONRecord[]; onClose: () => void; onCreated: () => Promise<void> }) {
  const [principalID, setPrincipalID] = useState(stringValue(principals[0]?.id));
  const [role, setRole] = useState("member");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dialogRef = useDialogFocus(onClose);
  const projectID = stringValue(project.id);
  const submit = async (event: Event) => {
    event.preventDefault();
    if (!principalID) { setError("Select a principal."); return; }
    setBusy(true);
    setError("");
    try {
      await sendJSON<JSONRecord>("admin", "/memberships", "POST", { project_id: projectID, principal_id: principalID, role });
      await onCreated();
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Membership could not be saved.");
    } finally { setBusy(false); }
  };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="add-membership-title" tabIndex={-1}><header><div><p class="eyebrow">{stringValue(project.name, stringValue(project.slug))}</p><h2 id="add-membership-title">Add member</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><label>Principal<select value={principalID} onInput={(event) => setPrincipalID((event.currentTarget as HTMLSelectElement).value)}>{principals.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))} ({stringValue(principal.kind)})</option>)}</select></label><label>Role<select value={role} onInput={(event) => setRole((event.currentTarget as HTMLSelectElement).value)}><option value="owner">Owner</option><option value="admin">Admin</option><option value="member">Member</option><option value="viewer">Viewer</option></select></label>{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <ShieldCheck size={16} />} Save membership</button></footer></form></section></div>;
}

export function Access({ data, mode, onChanged }: { data: JSONRecord; mode: ConsoleMode; onChanged: () => Promise<void> }) {
  const [createPrincipal, setCreatePrincipal] = useState(false);
  const [createProject, setCreateProject] = useState(false);
  const [membershipProject, setMembershipProject] = useState<JSONRecord | null>(null);
  const [result, setResult] = useState<ActionResult>(null);
  const [busy, setBusy] = useState("");
  const principals = asList(data.principals).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const memberships = asList(data.memberships).map(asRecord);
  const activePrincipals = principals.filter((principal) => stringValue(principal.status, "active") === "active" && stringValue(principal.kind) !== "system");
  const principalName = (id: string) => {
    const principal = principals.find((candidate) => stringValue(candidate.id) === id);
    return principal ? stringValue(principal.display_name, id) : id;
  };

  const setStatus = async (target: "principals" | "projects", record: JSONRecord) => {
    const id = stringValue(record.id);
    const next = stringValue(record.status, "active") === "active" ? "disabled" : "active";
    setBusy(`${target}-${id}`);
    try {
      await sendJSON<JSONRecord>("admin", `/${target}/status`, "POST", { id, status: next });
      setResult({ title: next === "active" ? "Enabled" : "Disabled", success: true, detail: `${stringValue(record.display_name, stringValue(record.name, id))} is now ${next}.` });
      await onChanged();
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
      setResult({ title: "Membership removed", success: true, detail: `${principalName(principalID)} was removed from the project.` });
      await onChanged();
    } catch (cause) {
      setResult({ title: "Removal failed", success: false, detail: cause instanceof Error ? cause.message : "The membership could not be removed." });
    } finally { setBusy(""); }
  };

  if (mode !== "admin") {
    return <div class="page-stack"><PageHeading eyebrow="Access" title="Access" detail="Workspace access is administered from the admin console." /><EmptyState title="Administrator area" detail="Ask a gateway administrator to manage principals, projects, and memberships." /></div>;
  }

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Identity and projects" title="Access" detail="Create the humans and services that own catalogs and keys, group them into projects, and control membership." actions={<><button class="button button--secondary" type="button" onClick={() => setCreateProject(true)}><FolderPlus size={16} /> New project</button><button class="button button--primary" type="button" onClick={() => setCreatePrincipal(true)}><UserPlus size={16} /> New principal</button></>} />
      <ResultNotice result={result} />
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Principals</p><h2>Humans and services</h2></div><span>{principals.length} record{principals.length === 1 ? "" : "s"}</span></div>
        {principals.length === 0 ? <EmptyState title="No principals yet" detail="Create a human principal to own private catalogs, playground runs, and provider connections." /> : <div class="table-wrap"><table><thead><tr><th>Name</th><th>Kind</th><th>Email</th><th>Status</th><th>Actions</th></tr></thead><tbody>{principals.map((principal) => {
          const id = stringValue(principal.id);
          const status = stringValue(principal.status, "active");
          const system = stringValue(principal.kind) === "system";
          return <tr key={id}><td><strong>{stringValue(principal.display_name, id)}</strong><small class="table-subtitle technical">{id}</small></td><td>{stringValue(principal.kind)}</td><td>{stringValue(principal.email, "—")}</td><td><span class={`status-pill ${status === "active" ? "status-pill--ready" : "status-pill--muted"}`}>{status}</span></td><td>{system ? <span class="provider-card__meta">Built-in</span> : <button class="button button--secondary" type="button" disabled={busy === `principals-${id}`} onClick={() => void setStatus("principals", principal)}>{status === "active" ? "Disable" : "Enable"}</button>}</td></tr>;
        })}</tbody></table></div>}
      </section>
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Projects</p><h2>Key and budget scopes</h2></div><span>{projects.length} project{projects.length === 1 ? "" : "s"}</span></div>
        {projects.length === 0 ? <EmptyState title="No projects yet" detail="Create a project before minting API keys — every key is scoped to a project." /> : <div class="table-wrap"><table><thead><tr><th>Project</th><th>Slug</th><th>Status</th><th>Members</th><th>Actions</th></tr></thead><tbody>{projects.map((project) => {
          const id = stringValue(project.id);
          const status = stringValue(project.status, "active");
          const memberCount = memberships.filter((membership) => stringValue(membership.project_id) === id).length;
          return <tr key={id}><td><strong>{stringValue(project.name, stringValue(project.slug))}</strong><small class="table-subtitle technical">{id}</small></td><td class="technical">{stringValue(project.slug)}</td><td><span class={`status-pill ${status === "active" ? "status-pill--ready" : "status-pill--muted"}`}>{status}</span></td><td>{memberCount}</td><td><div class="provider-actions"><button class="button button--secondary" type="button" disabled={!activePrincipals.length} title={activePrincipals.length ? undefined : "Create an active principal first"} onClick={() => setMembershipProject(project)}><Users size={15} /> Add member</button><button class="button button--secondary" type="button" disabled={busy === `projects-${id}`} onClick={() => void setStatus("projects", project)}>{status === "active" ? "Disable" : "Enable"}</button></div></td></tr>;
        })}</tbody></table></div>}
      </section>
      <section class="surface">
        <div class="section-heading"><div><p class="eyebrow">Memberships</p><h2>Who can act in which project</h2></div><span>{memberships.length} membership{memberships.length === 1 ? "" : "s"}</span></div>
        {memberships.length === 0 ? <EmptyState title="No memberships yet" detail="Add a principal to a project so it can hold keys and run project-attributed requests." /> : <div class="table-wrap"><table><thead><tr><th>Principal</th><th>Project</th><th>Role</th><th>Actions</th></tr></thead><tbody>{memberships.map((membership) => {
          const projectID = stringValue(membership.project_id);
          const principalID = stringValue(membership.principal_id);
          const project = projects.find((candidate) => stringValue(candidate.id) === projectID);
          return <tr key={`${projectID}-${principalID}`}><td>{principalName(principalID)}</td><td>{project ? stringValue(project.name, stringValue(project.slug)) : projectID}</td><td>{stringValue(membership.role)}</td><td><button class="button button--secondary" type="button" disabled={busy === `member-${projectID}-${principalID}`} onClick={() => void removeMembership(membership)}>Remove</button></td></tr>;
        })}</tbody></table></div>}
      </section>
      {createPrincipal ? <CreatePrincipalDialog onClose={() => setCreatePrincipal(false)} onCreated={onChanged} /> : null}
      {createProject ? <CreateProjectDialog onClose={() => setCreateProject(false)} onCreated={onChanged} /> : null}
      {membershipProject ? <AddMembershipDialog project={membershipProject} principals={activePrincipals} onClose={() => setMembershipProject(null)} onCreated={onChanged} /> : null}
    </div>
  );
}
