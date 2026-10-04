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
