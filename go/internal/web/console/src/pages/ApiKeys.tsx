import { useRef, useState } from "preact/hooks";
import { Check, Copy, Eye, EyeOff, KeyRound, Pencil, Plus, Save, Trash2, X } from "lucide-preact";
import { sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { EmptyState, PageHeading } from "../components/PageState";
import { useDialogFocus } from "../components/useDialogFocus";

function policyFor(key: JSONRecord): JSONRecord { return asRecord(key.policy); }

function KeySecret({ token, onDismiss, returnFocus }: { token: string; onDismiss: () => void; returnFocus: HTMLElement | null }) {
  const [copied, setCopied] = useState(false);
  const dialogRef = useDialogFocus(onDismiss, returnFocus);
  const copy = async () => { await navigator.clipboard?.writeText(token); setCopied(true); };
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog key-secret-dialog" role="dialog" aria-modal="true" aria-labelledby="key-secret-title" tabIndex={-1}><header><div><p class="eyebrow">Gateway credential</p><h2 id="key-secret-title">API key</h2></div><button class="icon-button" type="button" aria-label="Close key secret" onClick={onDismiss}><X size={18} /></button></header><pre class="technical key-secret-value">{token}</pre><footer><button class="button button--secondary" type="button" data-dialog-initial-focus onClick={() => void copy()}>{copied ? <Check size={16} /> : <Copy size={16} />}{copied ? "Copied" : "Copy key"}</button><button class="button button--primary" type="button" onClick={onDismiss}>Close</button></footer></section></div>;
}

export function ApiKeys({ data, mode, onChanged }: { data: JSONRecord; mode: ConsoleMode; onChanged: () => Promise<void> }) {
  const keys = asList(data.keys).map(asRecord);
  const projects = asList(data.projects).map(asRecord);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<JSONRecord | null>(null);
  const [name, setName] = useState("Gateway key");
  const [projectID, setProjectID] = useState(stringValue(projects[0]?.id));
  const humans = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  // A key inherits its principal's private provider connections. Keys bound to
  // an auto-created service principal cannot use a human's OAuth subscription
  // (Copilot, Codex) — so the owner is an explicit, visible choice.
  const [principalID, setPrincipalID] = useState("");
  const [rpm, setRPM] = useState("0");
  const [dailyRequests, setDailyRequests] = useState("0");
  const [secret, setSecret] = useState("");
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [revealingID, setRevealingID] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const createButtonRef = useRef<HTMLButtonElement | null>(null);
  const create = async (event: Event) => {
    event.preventDefault();
    if (!projectID || !name.trim()) { setMessage("Project and key name are required."); return; }
    setBusy(true);
    try {
      const payload: JSONRecord = { project_id: projectID, name: name.trim(), rpm: Number(rpm) || 0, daily_requests: Number(dailyRequests) || 0 };
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
    if (!id) return;
    if (stringValue(key.status) === "revoked" && disabled !== undefined) { setMessage("Revoked keys are permanently disabled."); return; }
    setBusy(true);
    try {
      const policy = policyFor(key);
      const payload: JSONRecord = disabled === undefined ? { rpm: Number(rpm) || 0, daily_requests: Number(dailyRequests) || 0 } : { disabled };
      if (mode === "admin") payload.id = id;
      await sendJSON<JSONRecord>(mode, mode === "admin" ? "/keys/update" : `/keys/${encodeURIComponent(id)}/update`, "POST", payload);
      setEditing(null);
      setMessage(disabled === undefined ? "Key policy updated." : "Key status updated.");
      await onChanged();
      void policy;
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be updated."); }
    finally { setBusy(false); }
  };
  const revoke = async (key: JSONRecord) => {
    const id = stringValue(key.id);
    if (!id || !window.confirm(`Revoke ${stringValue(key.name, "this key")}? Existing clients will stop working.`)) return;
    setBusy(true);
    try {
      await sendJSON<JSONRecord>(mode, mode === "admin" ? `/keys?id=${encodeURIComponent(id)}` : `/keys/${encodeURIComponent(id)}`, "DELETE");
      setMessage("Key revoked.");
      await onChanged();
    } catch (cause) { setMessage(cause instanceof Error ? cause.message : "Key could not be revoked."); }
    finally { setBusy(false); }
  };
  const edit = (key: JSONRecord) => {
    if (stringValue(key.status) === "revoked") return;
    const policy = policyFor(key);
    setRPM(String(numberValue(policy.rpm, numberValue(key.rpm))));
    setDailyRequests(String(numberValue(policy.daily_requests, numberValue(key.daily_requests))));
    setEditing(key);
  };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Credential governance" title="API keys" detail="Create scoped gateway keys, reveal their encrypted values, edit status and policy, and revoke with confirmation." actions={<button ref={createButtonRef} class="button button--primary" type="button" onClick={() => { setCreating(true); setMessage(""); }}><Plus size={16} /> Create key</button>} />
      {message ? <p class="route-message" role="status">{message}</p> : null}
      {creating ? <form class="surface key-editor" onSubmit={create}><header><div><p class="eyebrow">New key</p><h2>Issue credential</h2></div><button class="icon-button" type="button" aria-label="Close key editor" onClick={() => setCreating(false)}><X size={17} /></button></header><label>Project<select value={projectID} onInput={(event) => setProjectID((event.currentTarget as HTMLSelectElement).value)}><option value="">Select project</option>{projects.map((project) => <option value={stringValue(project.id)} key={stringValue(project.id)}>{stringValue(project.name, stringValue(project.slug))}</option>)}</select></label><label>Name<input value={name} onInput={(event) => setName((event.currentTarget as HTMLInputElement).value)} /></label>{mode === "admin" ? <><label>Acts as<select value={principalID} onInput={(event) => setPrincipalID((event.currentTarget as HTMLSelectElement).value)}><option value="">New service identity (shared credentials only)</option>{humans.map((principal) => <option value={stringValue(principal.id)} key={stringValue(principal.id)}>{stringValue(principal.display_name, stringValue(principal.id))} (human)</option>)}</select></label><p class="form-help">{principalID ? "This key inherits that human's private provider connections, including OAuth subscriptions such as GitHub Copilot." : "A service identity can only use shared system credentials. Pick a human owner if this key must reach a Copilot or Codex OAuth subscription."}</p></> : null}<div class="key-editor__limits"><label>Requests per minute<input inputMode="numeric" value={rpm} onInput={(event) => setRPM((event.currentTarget as HTMLInputElement).value)} /></label><label>Daily requests<input inputMode="numeric" value={dailyRequests} onInput={(event) => setDailyRequests((event.currentTarget as HTMLInputElement).value)} /></label></div><footer><button class="button button--secondary" type="button" onClick={() => setCreating(false)}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}><KeyRound size={16} /> Issue key</button></footer></form> : null}
      {editing ? <section class="surface key-editor"><header><div><p class="eyebrow">Policy edit</p><h2>{stringValue(editing.name, "Gateway key")}</h2></div><button class="icon-button" type="button" aria-label="Close policy editor" onClick={() => setEditing(null)}><X size={17} /></button></header><div class="key-editor__limits"><label>Requests per minute<input inputMode="numeric" value={rpm} onInput={(event) => setRPM((event.currentTarget as HTMLInputElement).value)} /></label><label>Daily requests<input inputMode="numeric" value={dailyRequests} onInput={(event) => setDailyRequests((event.currentTarget as HTMLInputElement).value)} /></label></div><footer><button class="button button--secondary" type="button" onClick={() => setEditing(null)}>Cancel</button><button class="button button--primary" type="button" disabled={busy} onClick={() => void update(editing)}><Save size={16} /> Save policy</button></footer></section> : null}
      {keys.length === 0 ? <EmptyState title="No API keys in this workspace" detail={mode === "portal" ? "Create a key for a project where you have key-management permission." : "Create a service key after setting a project and principal."} /> : null}
      {keys.length ? <section class="surface table-wrap"><table><thead><tr><th>Name</th><th>Prefix</th><th>Project</th><th>Principal</th><th>Policy</th><th>Status</th><th>Actions</th></tr></thead><tbody>{keys.map((key) => {
        const policy = policyFor(key);
        const status = stringValue(key.status, "active");
        const active = status === "active";
        const revoked = status === "revoked";
        const statusClass = active ? "status-pill--ready" : revoked ? "status-pill--attention" : "status-pill--muted";
        const id = stringValue(key.id);
        const visible = Boolean(revealed[id]);
        const revealable = key.revealable === true;
        return <tr key={id}><td><KeyRound size={15} /> {stringValue(key.name, "Gateway key")}</td><td><span class="key-value"><span class="technical">{visible ? revealed[id] : stringValue(key.prefix, "Hidden")}</span>{revealable ? <button class="icon-button" type="button" aria-label={`${visible ? "Hide" : "Reveal"} ${stringValue(key.name, "API key")}`} title={visible ? "Hide key" : "Reveal key"} disabled={busy || Boolean(revealingID)} onClick={() => void reveal(key)}>{visible ? <EyeOff size={15} /> : <Eye size={15} />}</button> : null}</span></td><td>{stringValue(key.project, stringValue(key.project_id))}</td><td>{stringValue(key.principal, stringValue(key.principal_id))}</td><td>{numberValue(policy.rpm, numberValue(key.rpm)) || numberValue(policy.daily_requests, numberValue(key.daily_requests)) ? `${numberValue(policy.rpm, numberValue(key.rpm)) || "—"} rpm · ${numberValue(policy.daily_requests, numberValue(key.daily_requests)) || "—"} daily` : "Unrestricted"}</td><td><span class={`status-pill ${statusClass}`} title={revoked ? "Permanently revoked" : undefined}>{status}</span></td><td><div class="table-actions"><button class="icon-button" type="button" aria-label={`Edit ${stringValue(key.name)}`} disabled={busy || revoked} onClick={() => edit(key)}><Pencil size={15} /></button>{!revoked ? <button class="button button--secondary" type="button" disabled={busy} onClick={() => void update(key, active)}>{active ? "Disable" : "Enable"}</button> : null}<button class="icon-button" type="button" aria-label={`Revoke ${stringValue(key.name)}`} disabled={busy || revoked} onClick={() => void revoke(key)}><Trash2 size={15} /></button></div></td></tr>;
      })}</tbody></table></section> : null}
      {secret ? <KeySecret token={secret} onDismiss={() => setSecret("")} returnFocus={createButtonRef.current} /> : null}
    </div>
  );
}
