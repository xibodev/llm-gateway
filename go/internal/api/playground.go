package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
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
	Tools           any              `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
}

func handleUserPlaygroundChat(w http.ResponseWriter, r *http.Request) {
	handleUserPlaygroundSurface(w, r, core.ModelSurfaceChatCompletions)
}

func handleUserPlaygroundResponses(w http.ResponseWriter, r *http.Request) {
	handleUserPlaygroundSurface(w, r, core.ModelSurfaceResponses)
}

func handleUserPlaygroundMessages(w http.ResponseWriter, r *http.Request) {
	handleUserPlaygroundSurface(w, r, core.ModelSurfaceMessages)
}

func handleAdminPlaygroundChat(w http.ResponseWriter, r *http.Request) {
	handleAdminPlaygroundSurface(w, r, core.ModelSurfaceChatCompletions)
}

func handleAdminPlaygroundResponses(w http.ResponseWriter, r *http.Request) {
	handleAdminPlaygroundSurface(w, r, core.ModelSurfaceResponses)
}

func handleAdminPlaygroundMessages(w http.ResponseWriter, r *http.Request) {
	handleAdminPlaygroundSurface(w, r, core.ModelSurfaceMessages)
}

func decodePlaygroundPayload(w http.ResponseWriter, r *http.Request) (map[string]any, playgroundBody, bool) {
	var payload map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return nil, playgroundBody{}, false
	}
	raw, _ := json.Marshal(payload)
	var body playgroundBody
	if json.Unmarshal(raw, &body) != nil {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return nil, playgroundBody{}, false
	}
	return payload, body, true
}

func handleUserPlaygroundSurface(w http.ResponseWriter, r *http.Request, surface core.ModelSurface) {
	owner, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	payload, body, ok := decodePlaygroundPayload(w, r)
	if !ok {
		return
	}
	principal, project, status, message := resolvePlaygroundPrincipal(owner, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	executePlaygroundSurface(w, r, payload, body, principal, project, "self-service", surface)
}

func handleAdminPlaygroundSurface(w http.ResponseWriter, r *http.Request, surface core.ModelSurface) {
	if !adminAuthed(w, r) {
		return
	}
	payload, body, ok := decodePlaygroundPayload(w, r)
	if !ok {
		return
	}
	principalID := strings.TrimSpace(body.PrincipalID)
	if principalID == "" {
		principalID = getAdminActor(r).PrincipalID
	}
	if principalID == "" {
		writeError(w, http.StatusBadRequest, "principal_id is required for static-admin playground requests")
		return
	}
	owner, found, err := iam.PrincipalByID(principalID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Identity store unavailable.")
		return
	}
	if !found || owner.Kind != "human" {
		writeError(w, http.StatusBadRequest, "playground requires an active human principal")
		return
	}
	principal, project, status, message := resolvePlaygroundPrincipal(owner, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	executePlaygroundSurface(w, r, payload, body, principal, project, "admin", surface)
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
	resolution, err := resolveModel(r.Context(), body.Model, principal)
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
	targets, err = router.FilterCompatibleTargets(targets, callerOf(principal), router.CompatibilityRequest{
		Surface: core.ModelSurfaceChatCompletions, Tools: requestHasTools(body.Tools), Vision: requestIsMultimodal(body.Messages),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request := chatRequest{Model: body.Model, Messages: body.Messages, Temperature: body.Temperature, MaxTokens: body.MaxTokens, ReasoningEffort: body.ReasoningEffort, Tools: body.Tools, ToolChoice: body.ToolChoice}
	response, served, trace, err := router.ExecuteCompleteWithTraceContext(governed(r.Context(), principal), targets, providerMessages(body.Messages), body.Model, callerOf(principal), chatKwargs(&request))
	latency := time.Since(started).Milliseconds()
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
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
		"transport_mode": targetTransportMode(*served, callerOf(principal), "/v1/chat/completions"),
	})
}

func executePlaygroundSurface(w http.ResponseWriter, r *http.Request, payload map[string]any, body playgroundBody, principal *config.Principal, project iam.Project, source string, surface core.ModelSurface) {
	if body.Stream {
		writeError(w, http.StatusBadRequest, "Streaming is not available in the playground yet. Use a non-streaming request.")
		return
	}
	if strings.TrimSpace(body.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if surface == core.ModelSurfaceMessages && payload["max_tokens"] == nil {
		writeError(w, http.StatusBadRequest, "max_tokens is required for Anthropic Messages")
		return
	}
	started := time.Now()
	resolution, err := resolveModel(r.Context(), body.Model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "Gateway route configuration is unavailable.")
		}
		return
	}
	targets, status, message := enforcePlaygroundPolicy(principal, body.Model, resolution.Category, resolution.Targets)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if surface == core.ModelSurfaceResponses && responsesRequestHasProviderState(payload) {
		if !isExactProviderModelResolution(body.Model, resolution) {
			writeError(w, http.StatusBadRequest, "Stateful Responses requests require an exact provider/model target.")
			return
		}
		private, privateErr := iam.HasResolvablePrivateProviderConnection(principal.PrincipalID, resolution.Targets[0].Provider)
		if privateErr != nil || !private {
			writeError(w, http.StatusForbidden, "Stateful Responses requests require a private human provider connection.")
			return
		}
	}
	targets, err = router.FilterCompatibleTargets(targets, callerOf(principal), router.CompatibilityRequest{
		Surface: surface, Tools: requestHasTools(payload["tools"]), Vision: playgroundPayloadIsMultimodal(surface, payload),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	delete(payload, "project_id")
	delete(payload, "principal_id")
	delete(payload, "stream")

	var response map[string]any
	var served *router.Target
	var trace []router.AttemptTrace
	switch surface {
	case core.ModelSurfaceResponses:
		response, served, err = router.ExecuteResponsesContext(governed(r.Context(), principal), targets, payload, body.Model, callerOf(principal))
	case core.ModelSurfaceMessages:
		response, served, err = router.ExecuteAnthropicMessagesContext(governed(r.Context(), principal), targets, payload, body.Model, callerOf(principal))
	default:
		var request chatRequest
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &request)
		if len(request.Messages) == 0 {
			writeError(w, http.StatusBadRequest, "at least one message is required")
			return
		}
		response, served, trace, err = router.ExecuteCompleteWithTraceContext(governed(r.Context(), principal), targets, providerMessages(request.Messages), body.Model, callerOf(principal), chatKwargs(&request))
		if err == nil {
			response["model"] = served.Model
			normalizeChatResponseEnvelope(response)
		}
	}
	latency := time.Since(started).Milliseconds()
	endpoint := "playground." + playgroundSurfaceName(surface)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		router.RecordUsage(router.UsageRecord{Endpoint: endpoint, RequestedModel: body.Model, Project: project.Slug, Key: "playground", ProjectID: project.ID, PrincipalID: principal.PrincipalID, StatusCode: upstreamErrorStatus(err), LatencyMS: latency, ErrorCode: "upstream"})
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.execute", TargetType: "project", TargetID: project.ID, Result: "failure", Detail: map[string]any{"model": body.Model, "surface": playgroundSurfacePath(surface), "source": source}})
		writeUpstreamError(w, err)
		return
	}
	inputTokens, outputTokens := responseUsage(response)
	router.RecordUsage(router.UsageRecord{Endpoint: endpoint, RequestedModel: body.Model, RoutedModel: served.Model, Provider: served.Provider, Project: project.Slug, Key: "playground", ProjectID: project.ID, PrincipalID: principal.PrincipalID, InputTokens: inputTokens, OutputTokens: outputTokens, StatusCode: http.StatusOK, LatencyMS: latency, IsStub: playgroundStub(served.Provider, principal)})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.execute", TargetType: "project", TargetID: project.ID, Result: "success", Detail: map[string]any{"model": body.Model, "surface": playgroundSurfacePath(surface), "served_provider": served.Provider, "served_model": served.Model, "source": source}})
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":     map[string]any{"provider": served.Provider, "model": served.Model},
		"latency_ms": latency, "usage": safePlaygroundValue(response["usage"]),
		"fallback_trace": trace, "raw_response": safePlaygroundValue(response),
		"transport_mode": targetTransportMode(*served, callerOf(principal), playgroundSurfacePath(surface)),
		"surface":        playgroundSurfacePath(surface),
	})
}

func playgroundPayloadIsMultimodal(surface core.ModelSurface, payload map[string]any) bool {
	if surface == core.ModelSurfaceResponses {
		return responsesRequestIsMultimodal(payload)
	}
	messages, _ := payload["messages"].([]any)
	converted := make([]map[string]any, 0, len(messages))
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok {
			converted = append(converted, message)
		}
	}
	return requestIsMultimodal(converted)
}

func playgroundSurfaceName(surface core.ModelSurface) string {
	switch surface {
	case core.ModelSurfaceResponses:
		return "responses"
	case core.ModelSurfaceMessages:
		return "messages"
	default:
		return "chat"
	}
}

func playgroundSurfacePath(surface core.ModelSurface) string {
	return map[core.ModelSurface]string{
		core.ModelSurfaceChatCompletions: "/v1/chat/completions",
		core.ModelSurfaceResponses:       "/v1/responses",
		core.ModelSurfaceMessages:        "/v1/messages",
	}[surface]
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
	targets, status, message := availableRouteTargets(targets)
	if status != 0 {
		return nil, status, message
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
	provider, err := providers.GetProviderForPrincipal(providerID, callerOf(principal))
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
