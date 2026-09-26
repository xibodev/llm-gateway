import type { JSONRecord } from "./api";
import { numberValue } from "./records";

export const keyQuotaLabels: Record<string, string> = {
  rpm: "Requests per minute",
  daily_requests: "Daily requests",
  monthly_requests: "Monthly requests",
  daily_input_tokens: "Daily input tokens",
  daily_output_tokens: "Daily output tokens",
  monthly_total_tokens: "Monthly total tokens",
  daily_cost_microusd: "Daily cost (micro-USD)",
  monthly_cost_microusd: "Monthly cost (micro-USD)",
  daily_credits_milli: "Daily model credits (milli-credits)",
  monthly_credits_milli: "Monthly model credits (milli-credits)",
};

export const keyQuotaFields = Object.keys(keyQuotaLabels);

export function keyQuotaDraftsFor(policy: JSONRecord): Record<string, string> {
  return Object.fromEntries(keyQuotaFields.map((field) => [field, String(numberValue(policy[field]))]));
}

export function keyQuotaPolicyFromDrafts(drafts: Record<string, string>): {
  policy: JSONRecord | null;
  error: string;
} {
  const policy: JSONRecord = {};
  for (const field of keyQuotaFields) {
    const raw = (drafts[field] ?? "").trim() || "0";
    const value = Number(raw);
    if (!/^\d+$/.test(raw) || !Number.isSafeInteger(value)) {
      return { policy: null, error: `${keyQuotaLabels[field]} must be a nonnegative whole number.` };
    }
    policy[field] = value;
  }
  return { policy, error: "" };
}
