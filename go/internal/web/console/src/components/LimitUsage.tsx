import { useEffect, useState } from "preact/hooks";
import { RefreshCw, X } from "lucide-preact";
import { getJSON, type JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../lib/records";
import { formatLimitAmount, limitLabel, limitScopeLabel, limitShare, limitState } from "../lib/limits";
import { useDialogFocus } from "./useDialogFocus";

// LimitUsageTable lists limits, each with what its current window has used
// and when that window ends, and names the one closest to refusing requests.
export function LimitUsageTable({ limits, empty }: { limits: JSONRecord[]; empty: string }) {
  if (!limits.length) return <p class="muted-copy">{empty}</p>;
  return <div class="table-wrap"><table class="limit-usage-table">
    <thead><tr><th>Limit</th><th>Set by</th><th>Used in this window</th><th>Window ends</th></tr></thead>
    <tbody>{limits.map((limit) => {
      const metric = stringValue(limit.metric);
      const share = limitShare(limit);
      const state = limitState(limit);
      const closest = limit.closest === true;
      return <tr key={`${stringValue(limit.scope)}:${stringValue(limit.field)}`} class={closest ? "limit-usage--closest" : undefined}>
        <td>{limitLabel(stringValue(limit.field))}{closest ? <small class="table-subtitle">Closest to refusing requests</small> : null}</td>
        <td>{limitScopeLabel(stringValue(limit.scope))}</td>
        <td>
          <span class="limit-bar" aria-hidden="true"><span class={`limit-bar__fill limit-bar__fill--${state.tone}`} style={{ width: `${Math.round(share * 100)}%` }} /></span>
          {formatLimitAmount(numberValue(limit.used), metric)} of {formatLimitAmount(numberValue(limit.limit), metric)} ({Math.round(share * 100)}%) <span class={`status-pill status-pill--${state.tone}`}>{state.label}</span>
        </td>
        <td class="technical">{new Date(numberValue(limit.resets_at) * 1000).toLocaleString()}</td>
      </tr>;
    })}</tbody>
  </table></div>;
}

// KeyLimitsDialog shows the limits a key's requests are admitted under, with
// their usage as the gateway reports it on opening and on each refresh.
export function KeyLimitsDialog({ mode, apiKey, onClose, returnFocus }: {
  mode: ConsoleMode; apiKey: JSONRecord; onClose: () => void; returnFocus: HTMLElement | null;
}) {
  const [limits, setLimits] = useState<JSONRecord[] | null>(null);
  const [error, setError] = useState("");
  const dialogRef = useDialogFocus(onClose, returnFocus);
  const id = stringValue(apiKey.id);
  const load = async () => {
    setError("");
    try {
      const report = await getJSON<JSONRecord>(mode, `/keys/${encodeURIComponent(id)}/limits`);
      setLimits(asList(report.limits).map(asRecord));
    } catch (cause) { setError(cause instanceof Error ? cause.message : "The key's limits could not load."); }
  };
  useEffect(() => { void load(); }, [id]);
  const perCaller = (limits ?? []).some((limit) => stringValue(limit.scope) === "caller");
  return <div class="dialog-backdrop" role="presentation"><section ref={dialogRef} class="dialog key-limits-dialog" role="dialog" aria-modal="true" aria-labelledby="key-limits-title" tabIndex={-1}>
    <header><div><p class="eyebrow">Limits and usage</p><h2 id="key-limits-title">{stringValue(apiKey.name, "API key")}</h2></div><button class="icon-button" type="button" aria-label="Close limits" onClick={onClose}><X size={18} /></button></header>
    {error ? <p class="form-error" role="alert">{error}</p>
      : limits === null ? <p class="muted-copy">Loading limits…</p>
      : <LimitUsageTable limits={limits} empty="No limit applies to this key: neither the key, its project nor the gateway sets one." />}
    <p class="form-help">Windows are UTC minutes, days and months. A request counts when it is admitted; its tokens, cost and credits when its response completes.{perCaller ? " The per-caller limit counts on the gateway process that answered." : ""}</p>
    <footer><button class="button button--secondary" type="button" onClick={() => void load()}><RefreshCw size={16} /> Refresh</button><button class="button button--primary" type="button" data-dialog-initial-focus onClick={onClose}>Close</button></footer>
  </section></div>;
}
