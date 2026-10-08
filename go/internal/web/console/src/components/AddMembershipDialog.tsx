import { useState } from "preact/hooks";
import { LoaderCircle, ShieldCheck, X } from "lucide-preact";
import { sendJSON, type JSONRecord } from "../lib/api";
import { principalSearch } from "../lib/directory";
import { stringValue } from "../lib/records";
import { RemoteSearchSelect } from "./DataTable";
import { useDialogFocus } from "./useDialogFocus";

// AddMembershipDialog adds a principal to a project, or changes the role of
// one already in it. The principal is found on the server among the active
// people and service principals.
export function AddMembershipDialog({ project, onClose, onCreated }: { project: JSONRecord; onClose: () => void; onCreated: () => Promise<void> }) {
  const [principalID, setPrincipalID] = useState("");
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
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog" role="dialog" aria-modal="true" aria-labelledby="add-membership-title" tabIndex={-1}><header><div><p class="eyebrow">{stringValue(project.name, stringValue(project.slug))}</p><h2 id="add-membership-title">Add member</h2></div><button class="icon-button" type="button" aria-label="Close dialog" onClick={onClose}><X size={18} /></button></header><form class="form-stack" onSubmit={submit}><RemoteSearchSelect label="Principal" noun="principals" value={principalID} emptyLabel="Select a principal" {...principalSearch("admin", { kinds: ["human", "service"], status: "active" }, { showKind: true })} onChange={setPrincipalID} /><label>Role<select value={role} onInput={(event) => setRole((event.currentTarget as HTMLSelectElement).value)}><option value="owner">Owner</option><option value="admin">Admin</option><option value="member">Member</option><option value="viewer">Viewer</option></select></label>{error ? <p class="form-error" role="alert">{error}</p> : null}<footer><button class="button button--secondary" type="button" onClick={onClose}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}>{busy ? <LoaderCircle class="spin" size={16} /> : <ShieldCheck size={16} />} Save membership</button></footer></form></section></div>;
}
