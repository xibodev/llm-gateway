import { useState } from "preact/hooks";
import { ArrowRight, CheckCircle2, Circle, X } from "lucide-preact";
import type { JSONRecord } from "../lib/api";
import type { PageID } from "../lib/navigation";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";

const dismissKey = "llmgw.console.setup-guide-dismissed";

type GuideStep = {
  title: string;
  detail: string;
  action: string;
  page: PageID;
  pageDetail?: string;
  done: boolean;
};

function guideStorage(): Storage | null {
  if (typeof window === "undefined") return null;
  try { return window.localStorage; } catch { return null; }
}

// buildGuideSteps derives the first-run journey from live workspace state.
// Every "done" flag is computed from evidence, mirroring the honest provider
// status ladder: a step is complete when the thing it teaches has actually
// happened, not when the user has visited the page.
export function buildGuideSteps(data: JSONRecord): GuideStep[] {
  const principals = asList(data.principals).map(asRecord);
  const humans = principals.filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active");
  const projects = asList(data.projects).map(asRecord).filter((project) => stringValue(project.status, "active") === "active");
  const memberships = asList(data.memberships).map(asRecord);
  const providers = asList(data.providers);
  const statuses = asList(data.provider_statuses).map(asRecord);
  const keys = asList(data.keys);
  const routes = Object.keys(asRecord(data.categories));
  const activeHumanIDs = new Set(humans.map((principal) => stringValue(principal.id)));
  const activeProjectIDs = new Set(projects.map((project) => stringValue(project.id)));
  const hasProjectOwner = memberships.some((membership) => {
    const role = stringValue(membership.role);
    return activeHumanIDs.has(stringValue(membership.principal_id)) &&
      activeProjectIDs.has(stringValue(membership.project_id)) &&
      (role === "owner" || role === "admin");
  });
  const configured = statuses.filter((status) => status.configured === true);
  const catalogSynced = statuses.some((status) => numberValue(status.model_count) > 0);
  const verified = statuses.some((status) => stringValue(status.last_verified_at) !== "");
  // Point each step at the provider that still needs it, rather than whichever
  // configured provider happens to sort first.
  const needsCatalog = configured.find((status) => numberValue(status.model_count) === 0) ?? configured[0];
  const needsVerify = configured.find((status) => stringValue(status.last_verified_at) === "") ?? configured[0];

  return [
    {
      title: "Create a human owner",
      detail: "A human principal owns private catalogs, OAuth accounts, and playground runs.",
      action: "Open Access", page: "access", done: humans.length > 0,
    },
    {
      title: "Create a project and add the owner",
      detail: "Add an active human as project owner or administrator so it can use private catalogs and the playground.",
      action: "Open Access", page: "access", done: hasProjectOwner,
    },
    {
      title: "Connect a provider",
      detail: "Add an API-key, local, or official-OAuth integration.",
      action: "Open Providers", page: "providers", done: providers.length > 0,
    },
    {
      title: "Sync its model catalog",
      detail: "Sync pulls the provider's real model list — routes and the playground build on it.",
      action: "Open provider", page: "providers", pageDetail: stringValue(needsCatalog?.id), done: catalogSynced,
    },
    {
      title: "Verify inference with a test completion",
      detail: "The only proof requests actually work end to end — reachability alone is not it.",
      action: "Open provider", page: "providers", pageDetail: stringValue(needsVerify?.id), done: verified,
    },
    {
      title: "Create a failover route",
      detail: "Order catalog-backed models into a named chain that survives 429s, 5xx, and timeouts.",
      action: "Open Routes", page: "routes", done: routes.length > 0,
    },
    {
      title: "Mint a project API key",
      detail: "Shown once, stored hashed. This is what your coding CLI authenticates with.",
      action: "Open API keys", page: "keys", done: keys.length > 0,
    },
    {
      title: "Wire up your CLI",
      detail: "Point Claude Code, Codex, or Copilot CLI at the gateway with the copy-ready snippets.",
      action: "Open Models & endpoints", page: "models", done: false,
    },
  ];
}

export function GetStartedGuide({ data, onNavigate }: { data: JSONRecord; onNavigate: (page: PageID, detail?: string) => void }) {
  const [dismissed, setDismissed] = useState(() => guideStorage()?.getItem(dismissKey) === "1");
  const steps = buildGuideSteps(data);
  const doneCount = steps.filter((step) => step.done).length;
  const finished = steps.slice(0, -1).every((step) => step.done);
  if (dismissed || (finished && doneCount >= steps.length - 1)) return null;
  const currentIndex = steps.findIndex((step) => !step.done);

  const dismiss = () => {
    guideStorage()?.setItem(dismissKey, "1");
    setDismissed(true);
  };

  return (
    <section class="surface get-started" aria-label="First configuration guide">
      <div class="section-heading">
        <div><p class="eyebrow">Get started</p><h2>First configuration</h2></div>
        <div class="get-started__meta"><span>{doneCount} of {steps.length} done</span><button class="icon-button" type="button" aria-label="Hide the setup guide" title="Hide this guide permanently" onClick={dismiss}><X size={16} /></button></div>
      </div>
      <ol class="get-started__steps">
        {steps.map((step, index) => {
          const current = index === currentIndex;
          return <li key={step.title} class={step.done ? "is-done" : current ? "is-current" : ""}>
            {step.done ? <CheckCircle2 size={18} aria-label="Done" /> : <Circle size={18} aria-hidden="true" />}
            <div class="get-started__body">
              <strong>{step.title}</strong>
              <small>{step.detail}</small>
            </div>
            {!step.done ? <button class={`button ${current ? "button--primary" : "button--secondary"}`} type="button" onClick={() => onNavigate(step.page, step.pageDetail || undefined)}>{step.action} <ArrowRight size={14} /></button> : null}
          </li>;
        })}
      </ol>
    </section>
  );
}
