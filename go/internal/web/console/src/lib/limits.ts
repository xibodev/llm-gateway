import type { JSONRecord } from "./api";
import { asList, asRecord, numberValue, stringValue } from "./records";
import { formatUsageMetric } from "./usage";

// The limits as the usage view names them. Their amounts carry units, so the
// names carry none, unlike the editor's fields, which take raw amounts.
const limitLabels: Record<string, string> = {
  rpm: "Requests per minute",
  daily_requests: "Requests per day",
  monthly_requests: "Requests per month",
  daily_input_tokens: "Input tokens per day",
  daily_output_tokens: "Output tokens per day",
  monthly_total_tokens: "Tokens per month",
  daily_cost_microusd: "Estimated cost per day",
  monthly_cost_microusd: "Estimated cost per month",
  daily_credits_milli: "Model credits per day",
  monthly_credits_milli: "Model credits per month",
  rate_limit_per_minute: "Requests per minute",
};

const scopeLabels: Record<string, string> = { key: "Key", project: "Project", caller: "Gateway, per caller" };

export function limitLabel(field: string): string {
  return limitLabels[field] ?? field.replaceAll("_", " ");
}

export function limitScopeLabel(scope: string): string {
  return scopeLabels[scope] ?? scope;
}

// formatLimitAmount shows an amount of what a limit counts: cost in dollars,
// credits in credits, and anything else as a count.
export function formatLimitAmount(value: number, metric: string): string {
  if (metric === "cost_microusd") return formatUsageMetric(value, "cost");
  if (metric === "credits_milli") return `${(value / 1000).toLocaleString(undefined, { maximumFractionDigits: 3 })} credits`;
  return value.toLocaleString();
}

// limitShare is how much of a limit its window has used, from 0 to 1. A
// limit counted past its end, as a token limit can be, is used up.
export function limitShare(limit: JSONRecord): number {
  const max = numberValue(limit.limit);
  return max > 0 ? Math.min(numberValue(limit.used) / max, 1) : 0;
}

// limitState says whether a limit refuses requests until its window ends,
// is close to it, or has room.
export function limitState(limit: JSONRecord): { label: string; tone: "ready" | "attention" } {
  const share = limitShare(limit);
  if (share >= 1) return { label: "Used up", tone: "attention" };
  if (share >= 0.8) return { label: "Near its limit", tone: "attention" };
  return { label: "Has room", tone: "ready" };
}

// errorCodeLabel names what refused a request by the error code its usage
// records: a limit by whose it is and what it counts. Other codes read as
// recorded.
export function errorCodeLabel(code: string): string {
  if (code === "rate_limit") return "Gateway limit: requests per minute per caller";
  const [kind, scope, field] = code.split(":");
  if (kind === "quota" && field) return `${limitScopeLabel(scope)} limit: ${limitLabel(field).toLowerCase()}`;
  return code;
}

// openCircuitsOf lists the open circuits of provider instances.
export function openCircuitsOf(instances: JSONRecord[]): JSONRecord[] {
  return instances.flatMap((instance) => asList(instance.open_circuits).map(asRecord));
}

// circuitSummary says whose requests open circuits refuse and when they
// admit requests again. name names a principal; requests without one use
// the gateway's credential.
export function circuitSummary(circuits: JSONRecord[], name: (principalID: string) => string): string {
  if (!circuits.length) return "";
  const latest = Math.max(...circuits.map((circuit) => Date.parse(stringValue(circuit.open_until)) || 0));
  const until = latest > 0 ? new Date(latest).toLocaleTimeString() : "the end of its cooldown";
  if (circuits.length > 1) return `Refusing the requests of ${circuits.length} callers until ${until} at the latest.`;
  const principal = stringValue(circuits[0].principal_id);
  const failures = numberValue(circuits[0].failures);
  return `Refusing ${principal ? name(principal) : "requests on the gateway's credential"} until ${until}, after ${failures} failure${failures === 1 ? "" : "s"} in a row.`;
}
