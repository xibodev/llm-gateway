import { useEffect, useMemo, useState } from "preact/hooks";
import { ArrowDown, ArrowUp, Pencil, Plus, Save, Trash2, X } from "lucide-preact";
import { getJSON, sendJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, endpointsOf, stringValue } from "../lib/records";
import { EmptyState, ErrorState, PageHeading } from "../components/PageState";
import { RouteDetail } from "./RouteDetail";
import { ModelFilters, catalogModels, filterModels, useModelFilter } from "../components/ModelPicker";

type RouteMember = { provider: string; model: string };
type ModelChoice = RouteMember & { label: string };

function choicesFrom(payload: JSONRecord): ModelChoice[] {
  return asList(payload.data).map(asRecord).flatMap((row) => {
    const id = stringValue(row.id);
    const slash = id.indexOf("/");
    if (slash <= 0 || slash === id.length - 1 || stringValue(row.owned_by) === "endpoint") return [];
    const provider = id.slice(0, slash);
    const model = id.slice(slash + 1);
    return [{ provider, model, label: stringValue(row.display_name, id) }];
  });
}

export function Routes({ data, mode, detail, onChanged, onNavigate }: { data: JSONRecord; mode: ConsoleMode; detail: string; onChanged: () => Promise<void>; onNavigate: (page: "routes", detail?: string) => void }) {
  const [catalog, setCatalog] = useState<JSONRecord>({});
  const [catalogError, setCatalogError] = useState("");
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [members, setMembers] = useState<RouteMember[]>([]);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  // Route members are picked from a catalog of hundreds; narrow before listing.
  const [memberFilter, setMemberFilter] = useModelFilter();
  const owners = asList(data.principals).map(asRecord).filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const [ownerID, setOwnerID] = useState(stringValue(owners[0]?.id));
  const principalQuery = mode === "admin" && ownerID ? `?principal_id=${encodeURIComponent(ownerID)}` : "";
  const endpoints = endpointsOf(data);
  const routes = Object.entries(endpoints).map(([routeName, value]) => ({
    name: routeName,
    members: asList(asRecord(value).failover).map(asRecord).map((member) => ({ provider: stringValue(member.provider), model: stringValue(member.model) })),
  }));

  const loadCatalog = async () => {
    try {
      setCatalogError("");
      setCatalog(await getJSON<JSONRecord>(mode, `/models${principalQuery}`));
    } catch (cause) {
      setCatalogError(cause instanceof Error ? cause.message : "Model catalog could not load.");
    }
  };
  useEffect(() => { void loadCatalog(); }, [mode, ownerID]);
  const choices = useMemo(() => choicesFrom(catalog), [catalog]);
  const catalogRows = useMemo(() => catalogModels(catalog), [catalog]);
  // Endpoints cannot nest inside a route, so they never appear as members.
  const memberModels = useMemo(() => catalogRows.filter((row) => !row.isCategory), [catalogRows]);
  const visibleMemberIDs = useMemo(
    () => new Set(filterModels(memberModels, memberFilter).map((row) => row.id)),
    [memberModels, memberFilter],
  );
  const visibleChoices = useMemo(
    () => choices.filter((choice) => visibleMemberIDs.has(`${choice.provider}/${choice.model}`)),
    [choices, visibleMemberIDs],
  );

  const startCreate = () => {
    setEditing(true);
    setName("");
    const pick = visibleChoices[0] ?? choices[0];
    setMembers(pick ? [{ provider: pick.provider, model: pick.model }] : []);
    setMessage("");
  };
  const startEdit = (route: { name: string; members: RouteMember[] }) => {
    setEditing(true);
    setName(route.name);
    setMembers(route.members);
    setMessage("");
  };
  const updateMember = (index: number, value: string) => {
    const [provider, model] = value.split("::", 2);
    setMembers((current) => current.map((member, memberIndex) => memberIndex === index ? { provider, model } : member));
  };
  const moveMember = (index: number, direction: -1 | 1) => {
    setMembers((current) => {
      const target = index + direction;
      if (target < 0 || target >= current.length) return current;
      const next = [...current];
      [next[index], next[target]] = [next[target], next[index]];
      return next;
    });
  };
  const save = async (event: Event) => {
    event.preventDefault();
    if (!name.trim() || !members.length || members.some((member) => !member.provider || !member.model)) {
      setMessage("A route name and at least one catalog-backed provider/model member are required.");
      return;
    }
    setBusy(true);
    try {
      await sendJSON<JSONRecord>("admin", `/endpoints${principalQuery}`, "POST", { name: name.trim(), failover: members });
      await onChanged();
      setEditing(false);
      setMessage("Route saved in the selected failover order.");
    } catch (cause) {
      setMessage(cause instanceof Error ? cause.message : "Route could not be saved.");
    } finally { setBusy(false); }
  };
  const remove = async (routeName: string) => {
    if (!window.confirm(`Delete route ${routeName}?`)) return;
    setBusy(true);
    try {
      await sendJSON<JSONRecord>("admin", `/endpoints/${encodeURIComponent(routeName)}`, "DELETE");
      await onChanged();
      setMessage("Route deleted.");
    } catch (cause) {
      setMessage(cause instanceof Error ? cause.message : "Route could not be deleted.");
    } finally { setBusy(false); }
  };

  if (mode !== "admin") return <ErrorState title="Routes are managed by an administrator" detail="Owner portal mode uses approved routes but does not change shared failover policy." />;

  if (detail) {
    return <RouteDetail routeName={detail} data={data} mode={mode} onChanged={onChanged} onBack={() => onNavigate("routes")} />;
  }

  const openDetail = (event: Event, routeName: string) => {
    const origin = event.target as HTMLElement | null;
    if (origin?.closest("button, a, select, input, label")) return;
    onNavigate("routes", routeName);
  };

  return (
    <div class="page-stack">
      <PageHeading eyebrow="Routing policy" title="Routes" detail="Ordered members map directly to gateway failover: the first healthy provider/model is tried before the next." actions={<><label class="owner-select">Catalog owner<select value={ownerID} onInput={(event) => setOwnerID((event.currentTarget as HTMLSelectElement).value)}><option value="">Shared catalogs only</option>{owners.map((owner) => <option value={stringValue(owner.id)} key={stringValue(owner.id)}>{stringValue(owner.display_name, stringValue(owner.email, stringValue(owner.id)))}</option>)}</select></label><button class="button button--primary" type="button" disabled={!choices.length} onClick={startCreate}><Plus size={16} /> Create route</button></>} />
      {catalogError ? <ErrorState title="Model catalog is unavailable" detail={catalogError} action={<button class="button button--secondary" type="button" onClick={() => void loadCatalog()}>Retry catalog</button>} /> : null}
      {message ? <p class="route-message" role="status">{message}</p> : null}
      {editing ? <form class="surface route-editor" onSubmit={save}><header><div><p class="eyebrow">Route editor</p><h2>{name ? `Edit ${name}` : "Create route"}</h2></div><button class="icon-button" type="button" aria-label="Close route editor" onClick={() => setEditing(false)}><X size={17} /></button></header><label>Route name<input value={name} onInput={(event) => setName((event.currentTarget as HTMLInputElement).value)} placeholder="for example, coding" /></label><div class="route-editor__members"><ModelFilters models={memberModels} filter={memberFilter} onChange={setMemberFilter} includeCategories={false} /><div class="section-heading"><div><p class="eyebrow">Failover order</p><h3>Provider and model members</h3></div><button class="button button--secondary" type="button" disabled={!choices.length} onClick={() => setMembers((current) => { const pick = visibleChoices[0] ?? choices[0]; return [...current, pick ? { provider: pick.provider, model: pick.model } : { provider: "", model: "" }]; })}><Plus size={15} /> Add member</button></div>{members.map((member, index) => <div class="route-editor__member" key={`${index}-${member.provider}-${member.model}`}><span class="route-order">{index + 1}</span><select value={`${member.provider}::${member.model}`} onInput={(event) => updateMember(index, (event.currentTarget as HTMLSelectElement).value)}>{(visibleChoices.some((choice) => choice.provider === member.provider && choice.model === member.model) ? visibleChoices : [...visibleChoices, { provider: member.provider, model: member.model, label: `${member.provider}/${member.model}` }]).map((choice) => <option value={`${choice.provider}::${choice.model}`} key={`${choice.provider}::${choice.model}`}>{choice.label}</option>)}</select><div class="route-editor__member-actions"><button class="icon-button" type="button" disabled={index === 0} aria-label="Move member up" onClick={() => moveMember(index, -1)}><ArrowUp size={15} /></button><button class="icon-button" type="button" disabled={index === members.length - 1} aria-label="Move member down" onClick={() => moveMember(index, 1)}><ArrowDown size={15} /></button><button class="icon-button" type="button" aria-label="Remove member" onClick={() => setMembers((current) => current.filter((_, memberIndex) => memberIndex !== index))}><Trash2 size={15} /></button></div></div>)}</div><footer><button class="button button--secondary" type="button" onClick={() => setEditing(false)}>Cancel</button><button class="button button--primary" type="submit" disabled={busy}><Save size={16} /> Save ordered route</button></footer></form> : null}
      {routes.length === 0 ? <EmptyState title="No fallback routes yet" detail="Create a route after provider catalogs contain the models you want to order." action={!editing ? <button class="button button--primary" type="button" disabled={!choices.length} onClick={startCreate}>Create route</button> : undefined} /> : null}
      <section class="route-list route-list--grid">
        {routes.map((route) => <article class="route-card provider-card--clickable" key={route.name} role="link" tabIndex={0} aria-label={`Open ${route.name} route details`} onClick={(event) => openDetail(event, route.name)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onNavigate("routes", route.name); } }}><header><div><p class="eyebrow">Endpoint route</p><h2>{route.name}</h2></div><div class="route-card__actions"><span class="route-count">{route.members.length} member{route.members.length === 1 ? "" : "s"}</span><button class="icon-button" type="button" aria-label={`Edit ${route.name}`} onClick={() => startEdit(route)}><Pencil size={16} /></button><button class="icon-button" type="button" aria-label={`Delete ${route.name}`} onClick={() => void remove(route.name)}><Trash2 size={16} /></button></div></header><ol>{route.members.map((member, index) => <li key={`${route.name}-${index}`}><span class="route-order">{index + 1}</span><span class="technical">{member.provider}/{member.model}</span>{index < route.members.length - 1 ? <ArrowDown size={16} aria-label="then" /> : <span class="route-terminal">served</span>}</li>)}</ol></article>)}
      </section>
    </div>
  );
}
