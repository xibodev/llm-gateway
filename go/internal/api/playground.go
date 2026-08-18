package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

type playgroundBody struct {
	ProjectID       string           `json:"project_id"`
	PrincipalID     string           `json:"principal_id"`
	Model           string           `json:"model"`
	Messages        []map[string]any `json:"messages"`
	Stream          bool             `json:"stream"`
	Temperature     any              `json:"temperature"`
	MaxTokens       any              `json:"max_tokens"`
	ReasoningEffort any              `json:"reasoning_effort"`
}

func handleUserPlayground(w http.ResponseWriter, r *http.Request) {
	owner, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	var body playgroundBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid playground request")
		return
	}
	principal, project, status, message := resolvePlaygroundPrincipal(owner, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	executePlayground(w, r, body, principal, project, "self-service")
}

func handleAdminPlayground(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body playgroundBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid playground request")
		return
	}
	principalID := strings.TrimSpace(body.PrincipalID)
	if principalID == "" {
		principalID = getAdminActor(r).PrincipalID
	}
	if principalID == "" {
		writeError(w, 400, "principal_id is required for static-admin playground requests")
		return
	}
	owner, found, err := iam.PrincipalByID(principalID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found || owner.Kind != "human" {
		writeError(w, 400, "playground requires an active human principal")
		return
	}
	principal, project, status, message := resolvePlaygroundPrincipal(owner, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	executePlayground(w, r, body, principal, project, "admin")
}

func resolvePlaygroundPrincipal(owner iam.Principal, projectID string) (*config.Principal, iam.Project, int, string) {
	if owner.Kind != "human" || owner.Status != "active" {
		return nil, iam.Project{}, http.StatusForbidden, "An active human principal is required."
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, iam.Project{}, http.StatusBadRequest, "project_id is required for playground attribution."
	}
	project, found, err := iam.ProjectByID(projectID)
	if err != nil {
		return nil, iam.Project{}, http.StatusInternalServerError, "Identity store unavailable."
	}
	if !found || project.Status != "active" {
		return nil, iam.Project{}, http.StatusNotFound, "unknown active project"
	}
	role, member, err := iam.MembershipRole(project.ID, owner.ID)
	if err != nil {
		return nil, iam.Project{}, http.StatusInternalServerError, "Membership store unavailable."
	}
	if !member || (role != "owner" && role != "admin") {
		return nil, iam.Project{}, http.StatusForbidden, "Project owner or admin membership is required for playground use."
	}
	return &config.Principal{
		PrincipalID: owner.ID, PrincipalKind: owner.Kind, ProjectID: project.ID, Project: project.Slug, Key: "playground", Role: role,
	}, project, 0, ""
}

func executePlayground(w http.ResponseWriter, r *http.Request, body playgroundBody, principal *config.Principal, project iam.Project, source string) {
	if body.Stream {
		writeError(w, http.StatusBadRequest, "Streaming is not available in the playground yet. Use a non-streaming request.")
		return
	}
	if strings.TrimSpace(body.Model) == "" || len(body.Messages) == 0 {
		writeError(w, 400, "model and at least one message are required")
		return
	}
	started := time.Now()
	resolution, err := router.ResolveForPrincipal(body.Model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 500, "Gateway route configuration is unavailable.")
		}
		return
	}
	targets, status, message := enforcePlaygroundPolicy(principal, body.Model, resolution.Category, resolution.Targets)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	request := chatRequest{Model: body.Model, Messages: body.Messages, Temperature: body.Temperature, MaxTokens: body.MaxTokens, ReasoningEffort: body.ReasoningEffort}
	response, served, trace, err := router.ExecuteCompleteWithTrace(targets, providerMessages(body.Messages), body.Model, principal, chatKwargs(&request))
	latency := time.Since(started).Milliseconds()
	if err != nil {
		router.RecordUsage(router.UsageRecord{
			Endpoint: "playground.chat", RequestedModel: body.Model, Project: project.Slug, Key: "playground",
			ProjectID: project.ID, PrincipalID: principal.PrincipalID, StatusCode: upstreamErrorStatus(err), LatencyMS: latency, ErrorCode: "upstream",
		})
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.execute", TargetType: "project", TargetID: project.ID, Result: "failure", Detail: map[string]any{"model": body.Model, "source": source}})
		writeUpstreamError(w, err)
		return
	}
	inputTokens, outputTokens := responseUsage(response)
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.chat", RequestedModel: body.Model, RoutedModel: served.Model, Provider: served.Provider,
		Project: project.Slug, Key: "playground", ProjectID: project.ID, PrincipalID: principal.PrincipalID,
		InputTokens: inputTokens, OutputTokens: outputTokens, StatusCode: http.StatusOK, LatencyMS: latency, IsStub: playgroundStub(served.Provider, principal),
	})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.execute", TargetType: "project", TargetID: project.ID, Result: "success", Detail: map[string]any{"model": body.Model, "served_provider": served.Provider, "served_model": served.Model, "source": source}})
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":     map[string]any{"provider": served.Provider, "model": served.Model},
		"latency_ms": latency, "usage": safePlaygroundValue(response["usage"]),
		"fallback_trace": trace, "raw_response": safePlaygroundValue(response),
	})
}

func enforcePlaygroundPolicy(principal *config.Principal, requestedModel, resolvedCategory string, targets []router.Target) ([]router.Target, int, string) {
	policy, err := iam.GetProjectPolicy(principal.ProjectID)
	if err != nil {
		return nil, 500, "Project policy store unavailable."
	}
	if !modelPolicyAllows(policy.AllowedModels, requestedModel, resolvedCategory, targets) {
		return nil, 403, "This project is not allowed to use the requested model or route."
	}
	if len(policy.AllowedProviders) > 0 {
		filtered := make([]router.Target, 0, len(targets))
		for _, target := range targets {
			if containsStr(policy.AllowedProviders, target.Provider) {
				filtered = append(filtered, target)
			}
		}
		if len(filtered) == 0 {
			return nil, 403, "This project is not allowed to use any provider in the requested route."
		}
		targets = filtered
	}
	if err := iam.CheckAndConsumeProjectRequest(principal.ProjectID, time.Now()); err != nil {
		var exceeded *iam.QuotaExceeded
		if errors.As(err, &exceeded) {
			return nil, 429, exceeded.Error()
		}
		return nil, 500, "Project quota store unavailable."
	}
	return targets, 0, ""
}

func responseUsage(response map[string]any) (int, int) {
	usage, _ := response["usage"].(map[string]any)
	return firstInt(usage, "prompt_tokens", "input_tokens"), firstInt(usage, "completion_tokens", "output_tokens")
}

func playgroundStub(providerID string, principal *config.Principal) bool {
	provider, err := providers.GetProviderForPrincipal(providerID, principal)
	return err == nil && provider.IsStub()
}

func safePlaygroundValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, child := range typed {
			if playgroundCredentialKey(key) {
				continue
			}
			out[key] = safePlaygroundValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = safePlaygroundValue(child)
		}
		return out
	default:
		return value
	}
}

func playgroundCredentialKey(key string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(key)))
	if playgroundUsageCounterKey(normalized) {
		return false
	}
	for _, marker := range []string{"authorization", "bearer", "session", "credential", "secret", "password", "cookie", "token", "key"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func playgroundUsageCounterKey(key string) bool {
	switch key {
	case "prompt_tokens", "completion_tokens", "input_tokens", "output_tokens", "total_tokens", "token_count",
		"cached_tokens", "reasoning_tokens", "audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens",
		"prompt_tokens_details", "completion_tokens_details", "input_tokens_details", "output_tokens_details":
		return true
	}
	return false
}
