import { Activity, ArrowRight, KeyRound, Plug, Route } from "lucide-preact";
import type { JSONRecord } from "../lib/api";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, endpointsOf, numberValue, stringValue } from "../lib/records";
import { PageHeading } from "../components/PageState";
import { GetStartedGuide } from "../components/GetStartedGuide";

function Metric({ icon, label, value, detail, onOpen }: { icon: preact.ComponentChildren; label: string; value: string; detail: string; onOpen?: () => void }) {
  if (!onOpen) {
    return <article class="metric-card"><span class="metric-card__icon">{icon}</span><div><p>{label}</p><strong>{value}</strong><small>{detail}</small></div></article>;
  }
  return <button class="metric-card metric-card--clickable" type="button" aria-label={`Open ${label}`} onClick={onOpen}><span class="metric-card__icon">{icon}</span><div><p>{label}</p><strong>{value}</strong><small>{detail}</small></div></button>;
}

type NextStep = {
  title: string;
  detail: string;
  action: string;
  page: PageID;
};

function nextStepFor(data: JSONRecord, mode: "admin" | "portal"): NextStep {
  const providers = asList(data.providers).map(asRecord);
  const connections = asList(data.provider_connections);
  const keys = asList(data.keys);
  const projects = asList(data.projects);
  const routes = Object.keys(endpointsOf(data));

  if (mode === "portal") {
    if (!projects.length) {
      return {
        title: "Review workspace access",
        detail: "You need an active project membership before issuing a key or running a request.",
        action: "Review settings",
        page: "settings",
      };
    }
    if (!keys.length) {
      return {
        title: "Create an API key",
        detail: "Issue a project-scoped gateway key for your coding CLI.",
        action: "Create API key",
        page: "keys",
      };
    }
    if (!connections.length) {
      return {
        title: "Try the shared gateway",
        detail: "A private provider is optional. Run a project-attributed request using the configured gateway routes.",
        action: "Open playground",
        page: "playground",
      };
    }
    return {
      title: "Run a gateway request",
      detail: "Verify your selected model or route through the project-attributed playground.",
      action: "Open playground",
      page: "playground",
    };
  }

  if (!providers.length) {
    return {
      title: "Connect a provider",
      detail: "Add a supported API-key, local, or private OAuth provider before creating a route.",
      action: "Connect provider",
      page: "providers",
    };
  }

  const catalogReady = providers.some((provider) => numberValue(provider.models) > 0);
  if (!catalogReady) {
    return {
      title: "Review provider readiness",
      detail: "Test the connection and sync its model catalog before routing coding CLI requests.",
      action: "Review providers",
      page: "providers",
    };
  }
  if (!routes.length) {
    return {
      title: "Create a fallback route",
      detail: "Order catalog-backed models so coding CLI requests can fail over between providers.",
      action: "Create route",
      page: "routes",
    };
  }
  if (!keys.length) {
    return {
      title: "Create an API key",
      detail: "Issue a project-scoped gateway key before connecting a coding CLI.",
      action: "Create API key",
      page: "keys",
    };
  }
  return {
    title: "Run a gateway request",
    detail: "Exercise the configured route and inspect its provider, latency, usage, and fallback trace.",
    action: "Open playground",
    page: "playground",
  };
}

export function Overview({ data, mode, onNavigate }: { data: JSONRecord; mode: "admin" | "portal"; onNavigate: (page: PageID, detail?: string) => void }) {
  const providers = asList(data.providers);
  const keys = asList(data.keys);
  const connections = asList(data.provider_connections);
  const endpoints = endpointsOf(data);
  const projects = asList(data.projects);
  const isPortal = mode === "portal";
  const totalProviders = isPortal ? connections.length : providers.length;
  const routes = isPortal ? projects.length : Object.keys(endpoints).length;
  const nextStep = nextStepFor(data, mode);

  return (
    <div class="page-stack">
      <PageHeading eyebrow={isPortal ? "Private workspace" : "Gateway operations"} title="Overview" detail="A concise view of configured access, routing, and operational state." />
      {!isPortal ? <GetStartedGuide data={data} onNavigate={onNavigate} /> : null}
      <section class="metric-grid">
        <Metric icon={<Plug size={19} />} label={isPortal ? "Private connections" : "Configured providers"} value={String(totalProviders)} detail="Current connection records" onOpen={() => onNavigate("providers")} />
        <Metric icon={<Route size={19} />} label={isPortal ? "Projects" : "Fallback routes"} value={String(routes)} detail="Available routing scopes" onOpen={() => onNavigate(isPortal ? "settings" : "routes")} />
        <Metric icon={<KeyRound size={19} />} label="API keys" value={String(keys.length)} detail="Lifecycle managed keys" onOpen={() => onNavigate("keys")} />
        <Metric icon={<Activity size={19} />} label="Gateway status" value="Ready" detail="Local console is connected" onOpen={() => onNavigate("usage")} />
      </section>
      <section class="surface surface--split">
        <div><p class="eyebrow">Next step</p><h2>{nextStep.title}</h2><p>{nextStep.detail}</p></div>
        <div class="next-step-side">
          <dl class="compact-facts"><div><dt>Mode</dt><dd>{isPortal ? "Owner portal" : "Administrator"}</dd></div><div><dt>Auth</dt><dd>{stringValue(asRecord(data.sso).enabled) === "true" ? "SSO enabled" : "Gateway policy"}</dd></div></dl>
          <button class="button button--primary" type="button" onClick={() => onNavigate(nextStep.page)}>
            {nextStep.action} <ArrowRight size={16} />
          </button>
        </div>
      </section>
    </div>
  );
}
