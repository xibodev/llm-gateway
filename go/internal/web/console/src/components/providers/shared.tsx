import { useState } from "preact/hooks";
import { sendJSON, type JSONRecord } from "../../lib/api";
import { asList, asRecord, stringValue } from "../../lib/records";

export type ActionResult = { title: string; success: boolean; detail: string } | null;

export function boolValue(value: unknown): boolean { return value === true; }

export function groupFor(entry: JSONRecord): string {
  if (boolValue(entry.client_only)) return "Gateway clients";
  const methods = asList(entry.auth_methods).map(String);
  if (methods.some((method) => method.startsWith("oauth"))) return "Official OAuth";
  if (methods.includes("none")) return "Local and self-hosted";
  return "API key connections";
}

// Status vocabulary is deliberately honest: "configured" means only that a
// credential exists; nothing is called healthy until a check has proven it.
export function statusLabel(status: string): string {
  return {
    configured: "Configured · unverified",
    misconfigured: "Setup incomplete",
    catalog_synced: "Catalog synced",
    verified: "Verified",
    check_failed: "Check failed",
    disabled: "Disabled",
    needs_credentials: "Needs credentials",
    not_configured: "Not configured", client_setup: "Client setup", unavailable: "Unavailable",
  }[status] ?? status.replaceAll("_", " ");
}

export function StatusBadge({ status }: { status: string }) {
  const ready = status === "verified" || status === "catalog_synced";
  const muted = status === "not_configured" || status === "unavailable" || status === "client_setup" || status === "configured" || status === "disabled";
  return <span class={`status-pill ${ready ? "status-pill--ready" : muted ? "status-pill--muted" : "status-pill--attention"}`}>{statusLabel(status)}</span>;
}

export function ResultNotice({ result }: { result: ActionResult }) {
  if (!result) return null;
  return <section class={`action-notice ${result.success ? "action-notice--success" : "action-notice--warning"}`} role="status"><strong>{result.title}</strong><span>{result.detail}</span></section>;
}

export function configuredProviderIDs(entry: JSONRecord): string[] {
  const ids = asList(entry.configured_provider_ids).map(String).filter(Boolean);
  if (!ids.length && boolValue(entry.configured)) {
    const id = stringValue(entry.id);
    if (id) ids.push(id);
  }
  return ids;
}

export function configuredProviderConfig(entry: JSONRecord, data: JSONRecord): JSONRecord {
  const providerIDs = configuredProviderIDs(entry);
  if (!providerIDs.length) return {};
  return asList(data.providers).map(asRecord)
    .find((provider) => providerIDs.includes(stringValue(provider.id))) ?? {};
}

export type LifecycleOperation = "test" | "repair" | "refresh" | "verify" | "delete";

// Honest human-readable operation names. The HTTP routes keep their historical
// names; the console describes what each action actually does.
export const operationLabels: Record<LifecycleOperation, string> = {
  test: "reachability check",
  refresh: "catalog sync",
  repair: "cache reset",
  verify: "test completion",
  delete: "removal",
};

// useProviderLifecycle centralizes the provider lifecycle calls shared by the
// hub grid and the provider detail page, keyed by `${providerID}-${operation}`.
export function useProviderLifecycle(ownerID: string, onChanged: () => Promise<void>) {
  const [busy, setBusy] = useState("");
  const [result, setResult] = useState<ActionResult>(null);

  const runLifecycle = async (entry: JSONRecord, operation: LifecycleOperation, providerIDOverride?: string, model?: string) => {
    const providerIDs = configuredProviderIDs(entry);
    const providerID = providerIDOverride ?? providerIDs[0] ?? stringValue(entry.id);
    if (!providerID) return;
    const query = new URLSearchParams();
    if (ownerID) query.set("principal_id", ownerID);
    if (operation === "verify" && model) query.set("model", model);
    const queryText = query.toString() ? `?${query.toString()}` : "";
    if (operation === "delete" && !window.confirm(`Revoke and remove ${stringValue(entry.label, providerID)}? Existing routes may need repair.`)) return;
    setBusy(`${providerID}-${operation}`);
    try {
      const payload = operation === "delete"
        ? await sendJSON<JSONRecord>("admin", `/providers/${encodeURIComponent(providerID)}`, "DELETE")
        : await sendJSON<JSONRecord>("admin", `/providers/${encodeURIComponent(providerID)}/${operation}${queryText}`, "POST", {});
      const success = operation === "delete" ? boolValue(payload.ok) : boolValue(payload.success);
      setResult({ title: `${stringValue(entry.label, providerID)} ${operationLabels[operation]}`, success, detail: stringValue(payload.details, success ? "Operation completed." : "Operation did not complete.") });
      await onChanged();
    } catch (cause) {
      setResult({ title: `${stringValue(entry.label, providerID)} ${operationLabels[operation]}`, success: false, detail: cause instanceof Error ? cause.message : "Operation failed." });
    } finally {
      setBusy("");
    }
  };

  return { busy, result, setResult, runLifecycle };
}
