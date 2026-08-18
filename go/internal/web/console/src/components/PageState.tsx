import type { ComponentChildren } from "preact";
import { AlertCircle, Inbox, LoaderCircle } from "lucide-preact";

type StatePanelProps = {
  title: string;
  detail: string;
  action?: ComponentChildren;
};

export function LoadingState({ title = "Loading workspace" }: { title?: string }) {
  return (
    <section class="state-panel" aria-live="polite">
      <LoaderCircle class="spin" size={22} aria-hidden="true" />
      <div><h2>{title}</h2><p>Requesting the current gateway state.</p></div>
    </section>
  );
}

export function EmptyState({ title, detail, action }: StatePanelProps) {
  return (
    <section class="state-panel">
      <Inbox size={22} aria-hidden="true" />
      <div><h2>{title}</h2><p>{detail}</p>{action}</div>
    </section>
  );
}

export function ErrorState({ title, detail, action }: StatePanelProps) {
  return (
    <section class="state-panel state-panel--error" role="alert">
      <AlertCircle size={22} aria-hidden="true" />
      <div><h2>{title}</h2><p>{detail}</p>{action}</div>
    </section>
  );
}

export function PageHeading({ eyebrow, title, detail, actions }: { eyebrow: string; title: string; detail: string; actions?: ComponentChildren }) {
  return (
    <header class="page-heading">
      <div><p class="eyebrow">{eyebrow}</p><h1>{title}</h1><p>{detail}</p></div>
      {actions ? <div class="page-heading__actions">{actions}</div> : null}
    </header>
  );
}
