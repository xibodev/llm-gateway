import { useEffect, useState } from "preact/hooks";
import { RefreshCw, Server } from "lucide-preact";
import { getJSON, type JSONRecord } from "../../lib/api";
import { asList, asRecord, numberValue, stringValue } from "../../lib/records";

// companionDaemonState names whether the companion daemon answered the
// gateway's probe.
export function companionDaemonState(report: JSONRecord): { label: string; tone: "ready" | "attention" | "muted" } {
  if (report.probed !== true) return { label: "Not checked", tone: "muted" };
  return report.reachable === true ? { label: "Reachable", tone: "ready" } : { label: "Unreachable", tone: "attention" };
}

// CompanionDaemonPanel shows the optional companion daemon as the gateway
// sees it: where it is, whether they share a secret, which providers depend
// on it, whether it answers and what it serves, and what keeps a provider
// from working. It shows nothing while no provider depends on the daemon and
// no address names it.
export function CompanionDaemonPanel() {
  const [report, setReport] = useState<JSONRecord | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const load = async () => {
    setBusy(true);
    try {
      setReport(await getJSON<JSONRecord>("admin", "/companion-daemon"));
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "The companion daemon's status could not load.");
    } finally { setBusy(false); }
  };
  useEffect(() => { void load(); }, []);
  if (error) return <section class="surface companion-daemon" aria-label="Companion daemon"><p class="form-error" role="alert">{error}</p></section>;
  if (!report) return null;
  const dependents = asList(report.dependent_providers).map(asRecord);
  if (!dependents.length && report.address_configured !== true) return null;
  const state = companionDaemonState(report);
  const served = asList(report.served).map(asRecord);
  const warnings = asList(report.warnings).map((warning) => stringValue(warning));
  return (
    <section class="surface companion-daemon" aria-label="Companion daemon">
      <div class="section-heading">
        <div><p class="eyebrow"><Server size={13} /> Optional companion daemon</p><h2>Companion daemon</h2></div>
        <div class="table-actions">
          <span class={`status-pill status-pill--${state.tone}`}>{state.label}</span>
          <button class="icon-button" type="button" aria-label="Check the companion daemon again" disabled={busy} onClick={() => void load()}><RefreshCw size={15} /></button>
        </div>
      </div>
      <dl class="companion-daemon__facts">
        <div><dt>Address</dt><dd class="technical">{stringValue(report.address)}{report.address_configured === true ? "" : " (default)"}</dd></div>
        <div><dt>Shared secret</dt><dd>{report.secret_set === true ? "Set" : "Not set"}</dd></div>
        <div><dt>Version</dt><dd>{stringValue(report.version, "—")}</dd></div>
        {report.probed === true ? <div><dt>Answered in</dt><dd>{report.reachable === true ? `${numberValue(report.latency_ms).toLocaleString()} ms` : "No answer"}</dd></div> : null}
      </dl>
      {stringValue(report.error) ? <p class="form-help technical">{stringValue(report.error)}</p> : null}
      <div class="companion-daemon__lists">
        <div>
          <h3>Providers that depend on it</h3>
          {dependents.length ? <ul>{dependents.map((dependent) => <li key={stringValue(dependent.id)}><strong>{stringValue(dependent.id)}</strong> <span class="technical">{stringValue(dependent.type)}</span>{dependent.disabled === true ? " (disabled)" : ""}</li>)}</ul> : <p class="muted-copy">No configured provider uses the daemon.</p>}
        </div>
        <div>
          <h3>Provider types it serves</h3>
          {served.length ? <ul>{served.map((provider) => <li key={stringValue(provider.id)}><strong>{stringValue(provider.name, stringValue(provider.id))}</strong> <span class="technical">{stringValue(provider.id)}</span></li>)}</ul> : <p class="muted-copy">{report.reachable === true ? "The daemon reports no provider types." : "Unknown until the daemon answers."}</p>}
        </div>
      </div>
      {warnings.length ? <section class="action-notice action-notice--warning" role="alert"><strong>{warnings.length === 1 ? "Warning" : `${warnings.length} warnings`}</strong><ul>{warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul></section> : null}
    </section>
  );
}
