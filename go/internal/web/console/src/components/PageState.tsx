import { Component, type ComponentChildren } from "preact";
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

// RenderBoundary turns a render error into a panel instead of an empty
// console. Around a page it keeps the shell and navigation usable, and a new
// resetKey (another page or detail) renders the children again.
export class RenderBoundary extends Component<{ resetKey?: string }, { failed: boolean }> {
  state = { failed: false };

  componentDidCatch(error: unknown) {
    console.error(error);
    this.setState({ failed: true });
  }

  componentDidUpdate(previous: Readonly<{ resetKey?: string }>) {
    if (this.state.failed && previous.resetKey !== this.props.resetKey) this.setState({ failed: false });
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return <ErrorState title="This view could not be displayed" detail="An unexpected error stopped this view from rendering. Open another page or reload the console." action={<button class="button button--primary" type="button" onClick={() => window.location.reload()}>Reload console</button>} />;
  }
}

export function PageHeading({ eyebrow, title, detail, actions }: { eyebrow: string; title: string; detail: string; actions?: ComponentChildren }) {
  return (
    <header class="page-heading">
      <div><p class="eyebrow">{eyebrow}</p><h1>{title}</h1><p>{detail}</p></div>
      {actions ? <div class="page-heading__actions">{actions}</div> : null}
    </header>
  );
}
