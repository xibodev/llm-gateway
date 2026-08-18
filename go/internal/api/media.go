package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// Image and video generation reuse the gateway's normal routing, key policy and
// usage accounting. Google reports image cost as modality-tagged tokens, so no
// separate cost model is needed; video is long-running and therefore exposed as
// start-then-poll rather than a request that pretends to be synchronous.

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
}

type videoRequest struct {
	Model      string         `json:"model"`
	Prompt     string         `json:"prompt"`
	Parameters map[string]any `json:"parameters"`
	Operation  string         `json:"operation"`
}

// resolveMediaTarget maps a requested model to one provider/model pair under
// the caller's key policy, mirroring resolveAudioTarget.
func resolveMediaTarget(principal *config.Principal, model string) (string, string, int, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", http.StatusBadRequest, "'model' is required"
	}
	resolution, err := router.ResolveForPrincipal(model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			return "", "", http.StatusNotFound, err.Error()
		}
		return "", "", http.StatusInternalServerError, "Gateway is not configured for the requested model."
	}
	targets, status, message := enforceKeyPolicy(principal, model, resolution.Category, resolution.Targets)
	if status != 0 {
		return "", "", status, message
	}
	if len(targets) == 0 {
		return "", "", http.StatusNotFound, "no routable target for '" + model + "'"
	}
	return targets[0].Provider, targets[0].Model, 0, ""
}

// POST /v1/images/generations — OpenAI-shaped image generation.
func handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var body imageRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		recordFailureUsage("openai.images", "", principal, 422, "invalid_body", started)
		writeError(w, 422, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		recordFailureUsage("openai.images", body.Model, principal, 400, "missing_prompt", started)
		writeError(w, 400, "'prompt' is required")
		return
	}
	if format := strings.TrimSpace(body.ResponseFormat); format != "" && format != "b64_json" {
		recordFailureUsage("openai.images", body.Model, principal, 400, "unsupported_format", started)
		writeError(w, 400, "only response_format 'b64_json' is supported; generated images are returned inline")
		return
	}
	providerID, upstreamModel, status, message := resolveMediaTarget(principal, body.Model)
	if status != 0 {
		recordFailureUsage("openai.images", body.Model, principal, status, "policy_or_route", started)
		writeError(w, status, message)
		return
	}
	generator, supported := imageGeneratorFor(providerID, principal)
	if !supported {
		recordFailureUsage("openai.images", body.Model, principal, 400, "image_unsupported", started)
		writeError(w, 400, "provider '"+providerID+"' does not generate images")
		return
	}
	count := body.N
	if count <= 0 {
		count = 1
	}
	images, usage, err := generator.GenerateImages(upstreamModel, body.Prompt, count)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		recordFailureUsage("openai.images", body.Model, principal, upstreamErrorStatus(err), "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	inputTokens, outputTokens := googleModalityUsage(usage)
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.images", RequestedModel: body.Model, RoutedModel: upstreamModel,
		Provider: providerID, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		InputTokens: inputTokens, OutputTokens: outputTokens,
		StatusCode: http.StatusOK, LatencyMS: latency, IsStub: isStub(providerID),
	})
	data := make([]any, 0, len(images))
	for _, image := range images {
		data = append(data, map[string]any{
			"b64_json":  base64.StdEncoding.EncodeToString(image.Data),
			"mime_type": image.MimeType,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(), "model": upstreamModel, "data": data,
	})
}

// POST /v1/videos/generations — start a generation, or poll one by passing
// "operation". Video takes minutes, so the gateway never blocks on it.
func handleVideoGenerations(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var body videoRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		recordFailureUsage("openai.videos", "", principal, 422, "invalid_body", started)
		writeError(w, 422, "invalid request body")
		return
	}
	providerID, upstreamModel, status, message := resolveMediaTarget(principal, body.Model)
	if status != 0 {
		recordFailureUsage("openai.videos", body.Model, principal, status, "policy_or_route", started)
		writeError(w, status, message)
		return
	}
	generator, supported := videoGeneratorFor(providerID, principal)
	if !supported {
		recordFailureUsage("openai.videos", body.Model, principal, 400, "video_unsupported", started)
		writeError(w, 400, "provider '"+providerID+"' does not generate video")
		return
	}

	// A request carrying an operation is a poll, not a new generation.
	if operation := strings.TrimSpace(body.Operation); operation != "" {
		job, err := generator.PollVideo(operation)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, videoJobPayload(providerID, upstreamModel, job))
		return
	}

	if strings.TrimSpace(body.Prompt) == "" {
		recordFailureUsage("openai.videos", body.Model, principal, 400, "missing_prompt", started)
		writeError(w, 400, "'prompt' is required to start a generation, or pass 'operation' to poll one")
		return
	}
	job, err := generator.StartVideo(upstreamModel, body.Prompt, body.Parameters)
	if err != nil {
		recordFailureUsage("openai.videos", body.Model, principal, upstreamErrorStatus(err), "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.videos", RequestedModel: body.Model, RoutedModel: upstreamModel,
		Provider: providerID, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		StatusCode: http.StatusAccepted, LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(providerID),
	})
	writeJSON(w, http.StatusAccepted, videoJobPayload(providerID, upstreamModel, job))
}

func videoJobPayload(providerID, model string, job providers.VideoJob) map[string]any {
	payload := map[string]any{
		"operation": job.Operation,
		"status":    map[bool]string{true: "completed", false: "running"}[job.Done],
		"provider":  providerID,
		"model":     model,
	}
	if job.VideoURI != "" {
		payload["video_uri"] = job.VideoURI
	}
	if len(job.Data) > 0 {
		payload["video_base64"] = base64.StdEncoding.EncodeToString(job.Data)
		payload["video_bytes"] = len(job.Data)
	}
	if job.MimeType != "" {
		payload["mime_type"] = job.MimeType
	}
	return payload
}

// googleModalityUsage flattens Google's usageMetadata into the gateway's token
// counters. Image output arrives as modality-tagged tokens.
func googleModalityUsage(usage map[string]any) (int, int) {
	number := func(key string) int {
		if value, ok := usage[key].(float64); ok {
			return int(value)
		}
		return 0
	}
	return number("promptTokenCount"), number("candidatesTokenCount")
}

func imageGeneratorFor(providerID string, principal *config.Principal) (providers.ImageGenerator, bool) {
	provider, err := providers.GetProviderForPrincipal(providerID, principal)
	if err != nil {
		return nil, false
	}
	return providers.AsImageGenerator(provider)
}

func videoGeneratorFor(providerID string, principal *config.Principal) (providers.VideoGenerator, bool) {
	provider, err := providers.GetProviderForPrincipal(providerID, principal)
	if err != nil {
		return nil, false
	}
	return providers.AsVideoGenerator(provider)
}
