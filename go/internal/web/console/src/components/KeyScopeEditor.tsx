import type { JSONRecord } from "../lib/api";
import { asList, asRecord, endpointsOf, numberValue, stringValue } from "../lib/records";

export const keyQuotaLabels: Record<string, string> = {
  rpm: "requests/minute", daily_requests: "daily requests", monthly_requests: "monthly requests",
  daily_input_tokens: "daily input tokens", daily_output_tokens: "daily output tokens",
  monthly_total_tokens: "monthly total tokens", daily_cost_microusd: "daily cost (micro-USD)",
  monthly_cost_microusd: "monthly cost (micro-USD)", daily_credits_milli: "daily milli-credits",
  monthly_credits_milli: "monthly milli-credits",
};

export function keyPolicySummary(policy: JSONRecord): string {
  const parts: string[] = [];
  const routes = asList(policy.allowed_routes).map(String).map((value) => value.trim()).filter(Boolean);
  if (policy.routes_only === true) parts.push(`Routes only: ${routes.join(", ") || "none (blocked)"}`);
  else if (routes.length) parts.push(`Routes: ${routes.join(", ")} (direct models also allowed)`);
  const providers = asList(policy.allowed_providers).map(String).map((value) => value.trim()).filter(Boolean);
  const models = asList(policy.allowed_models).map(String).map((value) => value.trim()).filter(Boolean);
  if (providers.length) parts.push(`Providers: ${providers.join(", ")}`);
  if (models.length) parts.push(`Model selectors: ${models.join(", ")}`);
  const quotas = Object.entries(keyQuotaLabels).filter(([field]) => numberValue(policy[field]) > 0);
  if (quotas.length) parts.push(`${quotas.length} key usage limit${quotas.length === 1 ? "" : "s"}: ${quotas.map(([field, label]) => `${policy[field]} ${label}`).join(", ")}`);
  return parts.length ? `${parts.join("; ")}. Project limits also apply.` : "Inherits project access and limits";
}

export function KeyScopeEditor({ data, policy, onChange }: {
  data: JSONRecord; policy: JSONRecord; onChange: (policy: JSONRecord) => void;
}) {
  const endpoints = endpointsOf(data);
  const routeDraft = asList(policy.allowed_routes).map(String);
  const providerDraft = asList(policy.allowed_providers).map(String);
  const selectedRoutes = routeDraft.map((value) => value.trim()).filter(Boolean);
  const selectedProviders = providerDraft.map((value) => value.trim()).filter(Boolean);
  const routeNames = [...new Set([...Object.keys(endpoints), ...selectedRoutes])].sort();
  const catalogProviderIDs = [
    ...asList(data.providers).map((value) => stringValue(asRecord(value).id)),
    ...asList(data.connection_providers).map((value) => stringValue(asRecord(value).id)),
    ...asList(data.provider_statuses).map(asRecord).flatMap((status) => [
      ...asList(status.configured_provider_ids).map((value) => stringValue(value)),
      ...asList(status.instances).map((value) => stringValue(asRecord(value).id)),
    ]),
  ].filter(Boolean);
  const providerIDs = [...new Set([...catalogProviderIDs, ...selectedProviders])].sort();
  const changeList = (field: string, values: string[], value: string, checked: boolean) => {
    onChange({ ...policy, [field]: checked ? [...values, value] : values.filter((item) => item !== value) });
  };
  const advanced = policy.routes_only === true || selectedRoutes.length > 0 || selectedProviders.length > 0 || asList(policy.allowed_models).length > 0;
  return <section class="key-scope">
    <h3>What can this key call?</h3>
    <p class="form-help">Project rules always apply. A key can narrow access, never expand it.</p>
    <details open={advanced}>
      <summary>Advanced: restrict routes, providers or models</summary>
      <label class="key-scope__check"><input type="checkbox" checked={policy.routes_only === true} onChange={(event) => onChange({ ...policy, routes_only: event.currentTarget.checked })} /><span>Selected routes only <small>Block direct model calls, including aliases.</small></span></label>
      <fieldset><legend>Allowed routes</legend>
        {routeNames.length ? <div class="key-scope__choices">{routeNames.map((route) => <label class="key-scope__check" key={route}><input type="checkbox" checked={selectedRoutes.includes(route)} onChange={(event) => changeList("allowed_routes", selectedRoutes, route, event.currentTarget.checked)} /><span>{route}</span></label>)}</div> : null}
        {!Object.keys(endpoints).length ? <label>Route names (comma-separated)<input name="allowed_routes" value={routeDraft.join(",")} onInput={(event) => onChange({ ...policy, allowed_routes: event.currentTarget.value.split(",") })} placeholder="coding, research" /><small>Enter canonical names provided by your administrator. This view does not expose the shared route catalog.</small></label> : null}
        <p class="form-help">{policy.routes_only === true ? "Select at least one route. Direct model calls are blocked." : "Empty means all project-permitted routes. Direct model access is controlled separately below."} Grants follow edits to the named routes.</p>
      </fieldset>
      <fieldset><legend>Allowed provider instances</legend>
        <div class="key-scope__choices">{providerIDs.map((provider) => <label class="key-scope__check" key={provider}><input type="checkbox" checked={selectedProviders.includes(provider)} onChange={(event) => changeList("allowed_providers", selectedProviders, provider, event.currentTarget.checked)} /><span>{provider}</span></label>)}</div>
        {!catalogProviderIDs.length ? <label>Provider IDs (comma-separated)<input name="allowed_providers" value={providerDraft.join(",")} onInput={(event) => onChange({ ...policy, allowed_providers: event.currentTarget.value.split(",") })} /></label> : null}
        <p class="form-help">Empty means all project-permitted providers. Restrictions apply to every route target, not just direct calls. This does not pin a credential account.</p>
      </fieldset>
      <label>Additional model selectors (comma-separated)<input name="allowed_models" value={asList(policy.allowed_models).map(String).join(",")} onInput={(event) => onChange({ ...policy, allowed_models: event.currentTarget.value.split(",") })} placeholder="provider/model or canonical route name" /></label>
      <p class="form-help">Empty adds no model restriction. For a routed call, this list must include the route name, not just its member models.</p>
    </details>
    <div class="key-scope__preview" aria-live="polite"><h3>Access summary</h3><p>{keyPolicySummary(policy)}</p>
      {selectedRoutes.map((route) => {
        const members = asList(asRecord(endpoints[route]).failover).map(asRecord);
        return <div key={route}><strong>{route}</strong>{members.length ? <ol>{members.map((member, index) => {
          const provider = stringValue(member.provider);
          const excluded = selectedProviders.length > 0 && !selectedProviders.includes(provider);
          return <li key={`${index}-${provider}`}><span class="technical">{provider}/{stringValue(member.model)}</span>{excluded ? " - excluded by key provider rule" : " - subject to project rules and credentials"}</li>;
        })}</ol> : <p class="form-help">Route members unavailable in this view. An unknown or deleted route cannot be called.</p>}</div>;
      })}
      <p class="form-help">This is a policy summary, not an inference test. Connection availability and project policy are checked on each request. Paid fallback remains possible.</p>
      {Object.entries(keyQuotaLabels).filter(([field]) => !["rpm", "daily_requests"].includes(field) && numberValue(policy[field]) > 0).map(([field, label]) => <p class="form-help" key={field}>{label}: {String(policy[field])} (preserved)</p>)}
    </div>
  </section>;
}
