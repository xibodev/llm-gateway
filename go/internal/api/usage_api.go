package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func usageFilterFromRequest(r *http.Request, principalID string) (iam.UsageTimeSeriesFilter, error) {
	query := r.URL.Query()
	filter := iam.UsageTimeSeriesFilter{
		Bucket: query.Get("bucket"), Provider: query.Get("provider"), Model: query.Get("model"),
		KeyID: query.Get("key_id"), ProjectID: query.Get("project_id"), PrincipalID: principalID,
	}
	parse := func(name string) (int64, error) {
		raw := strings.TrimSpace(query.Get(name))
		if raw == "" {
			return 0, nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("%s must be a non-negative unix timestamp", name)
		}
		return value, nil
	}
	var err error
	if filter.From, err = parse("from"); err != nil {
		return filter, err
	}
	if filter.To, err = parse("to"); err != nil {
		return filter, err
	}
	if filter.From == 0 {
		if filter.From, err = parse("since"); err != nil {
			return filter, err
		}
	}
	if filter.To == 0 {
		filter.To = time.Now().UTC().Unix() + 1
	}
	if filter.From == 0 {
		filter.From = filter.To - int64(30*24*time.Hour/time.Second)
	}
	if filter.To <= filter.From {
		return filter, fmt.Errorf("to must be later than from")
	}
	return filter, nil
}

func providerQuotaAdvisories(principalID string) ([]map[string]any, error) {
	ids := make([]string, 0, len(config.Get().Providers))
	for id := range config.Get().Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	now := time.Now().Unix()
	for _, id := range ids {
		snapshots, err := iam.ListProviderQuotaSnapshots(iam.ProviderQuotaFilter{
			ProviderID: id, PrincipalID: strings.TrimSpace(principalID),
		})
		if err != nil {
			return nil, err
		}
		if len(snapshots) == 0 {
			out = append(out, map[string]any{
				"provider_id": id, "status": "unknown", "source": "no_verified_quota_adapter",
			})
			continue
		}

		fresh := make([]iam.ProviderQuotaSnapshot, 0, len(snapshots))
		latestRefresh := int64(0)
		for _, snapshot := range snapshots {
			if snapshot.RefreshedAt > latestRefresh {
				latestRefresh = snapshot.RefreshedAt
			}
			if iam.ProviderQuotaSnapshotIsFresh(snapshot, now) {
				fresh = append(fresh, snapshot)
			}
		}
		if len(fresh) == 0 {
			out = append(out, map[string]any{
				"provider_id": id, "status": "stale", "source": "stale_quota_snapshot",
				"refreshed_at": time.Unix(latestRefresh, 0).UTC().Format(time.RFC3339),
			})
			continue
		}

		usable := false
		for _, snapshot := range fresh {
			if iam.ProviderQuotaSnapshotHasUsableValue(snapshot) {
				usable = true
				break
			}
		}
		status := "unknown"
		source := "adapter_reported_unknown"
		if usable {
			status = "available"
			source = quotaAdvisorySummaryValue(fresh, func(snapshot iam.ProviderQuotaSnapshot) string {
				return snapshot.Source
			})
		}
		out = append(out, map[string]any{
			"provider_id": id, "status": status, "source": source,
			"confidence": quotaAdvisorySummaryValue(fresh, func(snapshot iam.ProviderQuotaSnapshot) string {
				return snapshot.Confidence
			}),
			"account_count": quotaAdvisoryAccountCount(fresh),
			"refreshed_at":  time.Unix(latestProviderQuotaRefresh(fresh), 0).UTC().Format(time.RFC3339),
			"dimensions":    providerQuotaDimensions(fresh),
		})
	}
	return out, nil
}

func providerQuotaDimensions(snapshots []iam.ProviderQuotaSnapshot) []map[string]any {
	out := make([]map[string]any, 0, len(snapshots))
	for _, snapshot := range snapshots {
		dimension := map[string]any{
			"connection_id": snapshot.ConnectionID,
			"metric":        snapshot.Metric, "unit": snapshot.Unit, "window": snapshot.Window,
			"source": snapshot.Source, "confidence": snapshot.Confidence,
			"refreshed_at": time.Unix(snapshot.RefreshedAt, 0).UTC().Format(time.RFC3339),
		}
		if snapshot.Label != "" {
			dimension["label"] = snapshot.Label
		}
		if snapshot.LimitValue != nil {
			dimension["limit"] = *snapshot.LimitValue
		}
		if snapshot.UsedValue != nil {
			dimension["used"] = *snapshot.UsedValue
		}
		if snapshot.RemainingValue != nil {
			dimension["remaining"] = *snapshot.RemainingValue
		}
		if snapshot.ResetAt > 0 {
			dimension["reset_at"] = time.Unix(snapshot.ResetAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, dimension)
	}
	return out
}

func quotaAdvisorySummaryValue(
	snapshots []iam.ProviderQuotaSnapshot, value func(iam.ProviderQuotaSnapshot) string,
) string {
	values := map[string]bool{}
	for _, snapshot := range snapshots {
		if current := strings.TrimSpace(value(snapshot)); current != "" {
			values[current] = true
		}
	}
	if len(values) != 1 {
		return "mixed"
	}
	for current := range values {
		return current
	}
	return "unknown"
}

func quotaAdvisoryAccountCount(snapshots []iam.ProviderQuotaSnapshot) int {
	connections := map[string]bool{}
	for _, snapshot := range snapshots {
		connections[snapshot.ConnectionID] = true
	}
	return len(connections)
}

func latestProviderQuotaRefresh(snapshots []iam.ProviderQuotaSnapshot) int64 {
	latest := int64(0)
	for _, snapshot := range snapshots {
		if snapshot.RefreshedAt > latest {
			latest = snapshot.RefreshedAt
		}
	}
	return latest
}
