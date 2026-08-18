package api

import (
	"time"

	"llmgw/internal/config"
	"llmgw/internal/router"
)

func recordFailureUsage(
	endpoint, requestedModel string, principal *config.Principal,
	status int, errorCode string, started time.Time,
) {
	if principal == nil {
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: endpoint, RequestedModel: requestedModel,
		Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID,
		KeyID: principal.KeyID, StatusCode: status,
		LatencyMS: time.Since(started).Milliseconds(), ErrorCode: errorCode,
	})
}
