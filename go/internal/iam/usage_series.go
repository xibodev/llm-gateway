package iam

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type UsageTimeSeriesFilter struct {
	From        int64
	To          int64
	Bucket      string
	Provider    string
	Model       string
	KeyID       string
	ProjectID   string
	PrincipalID string
}

type UsageBucket struct {
	Start        int64 `json:"start"`
	End          int64 `json:"end"`
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CostMicroUSD int64 `json:"cost_microusd"`
	CreditsMilli int64 `json:"credits_milli"`
	Errors       int64 `json:"errors"`
}

func UsageTimeSeries(filter UsageTimeSeriesFilter) ([]UsageBucket, error) {
	filter.Bucket = strings.ToLower(strings.TrimSpace(filter.Bucket))
	if filter.Bucket == "" {
		filter.Bucket = "day"
	}
	if filter.Bucket != "hour" && filter.Bucket != "day" && filter.Bucket != "week" {
		return nil, fmt.Errorf("bucket must be hour, day, or week")
	}
	now := time.Now().UTC().Unix()
	if filter.To == 0 {
		filter.To = now + 1
	}
	if filter.From == 0 {
		filter.From = filter.To - int64(30*24*time.Hour/time.Second)
	}
	if filter.From < 0 || filter.To <= filter.From {
		return nil, fmt.Errorf("time range is invalid")
	}
	from := time.Unix(filter.From, 0).UTC()
	to := time.Unix(filter.To, 0).UTC()
	start := bucketStartUTC(from, filter.Bucket)
	end := bucketStartUTC(to, filter.Bucket)
	if end.Before(to) {
		end = nextBucketUTC(end, filter.Bucket)
	}
	if bucketCount(start, end, filter.Bucket) > 1000 {
		return nil, fmt.Errorf("time range contains too many buckets")
	}

	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `SELECT ts,status_code,input_tokens,output_tokens,cost_microusd,credits_milli FROM usage_events WHERE ts>=? AND ts<? AND is_stub=0`
	args := []any{filter.From, filter.To}
	for _, item := range []struct {
		column string
		value  string
	}{
		{"provider", filter.Provider}, {"routed_model", filter.Model}, {"key_id", filter.KeyID}, {"project_id", filter.ProjectID}, {"principal_id", filter.PrincipalID},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			query += " AND " + item.column + "=?"
			args = append(args, value)
		}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[int64]UsageBucket{}
	for rows.Next() {
		var timestamp int64
		var status int
		var input, output, cost, credits int64
		if err := rows.Scan(&timestamp, &status, &input, &output, &cost, &credits); err != nil {
			return nil, err
		}
		bucket := bucketStartUTC(time.Unix(timestamp, 0).UTC(), filter.Bucket).Unix()
		item := values[bucket]
		item.Start = bucket
		item.End = nextBucketUTC(time.Unix(bucket, 0).UTC(), filter.Bucket).Unix()
		item.Requests++
		item.InputTokens += input
		item.OutputTokens += output
		item.CostMicroUSD += cost
		item.CreditsMilli += credits
		if status >= 400 {
			item.Errors++
		}
		values[bucket] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []UsageBucket{}
	for cursor := start; cursor.Before(end); cursor = nextBucketUTC(cursor, filter.Bucket) {
		item, ok := values[cursor.Unix()]
		if !ok {
			item = UsageBucket{Start: cursor.Unix(), End: nextBucketUTC(cursor, filter.Bucket).Unix()}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

func bucketStartUTC(value time.Time, bucket string) time.Time {
	value = value.UTC()
	switch bucket {
	case "hour":
		return value.Truncate(time.Hour)
	case "week":
		day := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	default:
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	}
}

func nextBucketUTC(value time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		return value.Add(time.Hour)
	case "week":
		return value.AddDate(0, 0, 7)
	default:
		return value.AddDate(0, 0, 1)
	}
}

func bucketCount(start, end time.Time, bucket string) int {
	count := 0
	for cursor := start; cursor.Before(end) && count <= 1000; cursor = nextBucketUTC(cursor, bucket) {
		count++
	}
	return count
}
