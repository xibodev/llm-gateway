import type { JSONRecord } from "./api";
import { numberValue } from "./records";

// The server refuses a usage series that spans more than this many buckets.
export const maxUsageBuckets = 1000;

const bucketSeconds: Record<string, number> = { hour: 3600, day: 86400, week: 604800 };

// usageBucketAllowed reports whether the server will chart a range of this
// length in this bucket size. A range that does not start on a bucket boundary
// touches a partial bucket at each end, so the count is taken at that worst case.
export function usageBucketAllowed(rangeSeconds: number, bucket: string): boolean {
  const size = bucketSeconds[bucket];
  return size > 0 && Math.ceil(rangeSeconds / size) + 1 <= maxUsageBuckets;
}

// usageTotals sums the series, which honours every filter. The report's own
// totals cover the time range only, so they cannot stand for filtered usage.
export function usageTotals(series: JSONRecord[]): { requests: number; errors: number } {
  return series.reduce<{ requests: number; errors: number }>((totals, item) => ({
    requests: totals.requests + numberValue(item.requests),
    errors: totals.errors + numberValue(item.errors),
  }), { requests: 0, errors: 0 });
}

// Buckets start on UTC boundaries, so they are labelled in UTC: a local label
// would file a day's usage under the neighbouring date west or east of UTC.
export function usageBucketLabel(start: number, bucket: string): string {
  const date = new Date(start * 1000);
  return bucket === "hour"
    ? date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", timeZone: "UTC" })
    : date.toLocaleDateString([], { month: "short", day: "numeric", timeZone: "UTC" });
}

// A quantity the usage chart can show for each bucket.
export type UsageMetric = "requests" | "tokens" | "errors" | "cost";

export const usageMetricLabels: Record<UsageMetric, string> = {
  requests: "Requests",
  tokens: "Tokens",
  errors: "Failed requests",
  cost: "Estimated cost",
};

// usageMetricValue reads one metric of a series bucket or a breakdown row.
export function usageMetricValue(item: JSONRecord, metric: UsageMetric): number {
  switch (metric) {
    case "tokens": return numberValue(item.input_tokens) + numberValue(item.output_tokens);
    case "errors": return numberValue(item.errors);
    case "cost": return numberValue(item.cost_microusd);
    default: return numberValue(item.requests);
  }
}

// usageMetricTotal sums one metric over a filtered series.
export function usageMetricTotal(series: JSONRecord[], metric: UsageMetric): number {
  return series.reduce((total, item) => total + usageMetricValue(item, metric), 0);
}

// formatUsageMetric shows a metric's value; cost is kept in micro-USD and
// shown in dollars.
export function formatUsageMetric(value: number, metric: UsageMetric): string {
  if (metric !== "cost") return value.toLocaleString();
  const dollars = value / 1_000_000;
  return `$${dollars.toFixed(dollars >= 10 ? 2 : 4)}`;
}
