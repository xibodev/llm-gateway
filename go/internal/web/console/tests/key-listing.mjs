// fakeKeyListing answers the key listing as the server does, from keys in
// the order given, which the listing treats as newest first. It applies the
// status, owner, project, route and search filters and pages.
export function fakeKeyListing(keys, now = Date.now()) {
  const expired = (key) => key.status !== "revoked" && Number(key.expires_at ?? 0) > 0 && Number(key.expires_at) * 1000 <= now;
  const status = (key) => key.status ?? "active";
  const keeps = {
    usable: (key) => ["active", "disabled"].includes(status(key)) && !expired(key),
    ended: (key) => status(key) === "revoked" || expired(key),
    active: (key) => status(key) === "active" && !expired(key),
    disabled: (key) => status(key) === "disabled" && !expired(key),
    expired,
    revoked: (key) => status(key) === "revoked",
    all: () => true,
  };
  const requests = [];
  const answer = (path) => {
    requests.push(path);
    const query = new URLSearchParams(path.split("?")[1] ?? "");
    const value = (name) => query.get(name) ?? "";
    const selected = keys.filter(keeps[value("status") || "usable"])
      .filter((key) => !value("principal_id") || key.principal_id === value("principal_id"))
      .filter((key) => !value("project_id") || key.project_id === value("project_id"))
      .filter((key) => !value("route") || (key.allowed_routes ?? key.policy?.allowed_routes ?? []).includes(value("route")))
      .filter((key) => !value("q") || [key.name, key.prefix, key.id].join(" ").toLowerCase().includes(value("q").toLowerCase()));
    const offset = Number(value("offset") || 0);
    const limit = Number(value("limit") || 50);
    return { keys: selected.slice(offset, offset + limit), total: selected.length };
  };
  return { answer, requests };
}
