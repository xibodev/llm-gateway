package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

const videoOperationHandlePrefix = "llmgw.video.v1."

// Image and video generation reuse the gateway's normal routing, key policy and
// usage accounting. Google reports image cost as modality-tagged tokens, so no
// separate cost model is needed; video is long-running and therefore exposed as
// start-then-poll rather than a request that pretends to be synchronous.

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n"`
	ResponseFormat string `json:"response_format"`
}

type videoRequest struct {
	Model      string         `json:"model"`
	Prompt     string         `json:"prompt"`
	Parameters map[string]any `json:"parameters"`
	Operation  string         `json:"operation"`
}

func validateVideoOperation(operation, model string) (int, string) {
	if strings.HasPrefix(operation, "http://") || strings.HasPrefix(operation, "https://") {
		return http.StatusBadRequest, "operation must not be a full URL"
	}
	opModel := ""
	const marker = "models/"
	if idx := strings.Index(operation, marker); idx >= 0 {
		rest := operation[idx+len(marker):]
		if end := strings.Index(rest, "/operations/"); end >= 0 {
			opModel = rest[:end]
		}
	}
	if opModel == "" {
		return http.StatusBadRequest, "could not derive model from operation"
	}
	if opModel != model {
		return http.StatusForbidden, "operation does not belong to authorized model"
	}
	return 0, ""
}

type videoOperationHandle struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Operation string `json:"operation"`
}

func encodeVideoOperation(provider, model, operation string) string {
	raw, _ := json.Marshal(videoOperationHandle{Provider: provider, Model: model, Operation: operation})
	return videoOperationHandlePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeVideoOperation(value string) (videoOperationHandle, bool) {
	if !strings.HasPrefix(value, videoOperationHandlePrefix) {
		return videoOperationHandle{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, videoOperationHandlePrefix))
	if err != nil {
		return videoOperationHandle{}, false
	}
	var handle videoOperationHandle
	if json.Unmarshal(raw, &handle) != nil || strings.TrimSpace(handle.Provider) == "" || strings.TrimSpace(handle.Model) == "" || strings.TrimSpace(handle.Operation) == "" {
		return videoOperationHandle{}, false
	}
	return handle, true
}

func resolveVideoOperation(value, provider, model string) (string, int, string) {
	if strings.HasPrefix(value, videoOperationHandlePrefix) {
		handle, ok := decodeVideoOperation(value)
		if !ok {
			return "", http.StatusBadRequest, "operation handle is invalid"
		}
		if handle.Provider != provider || handle.Model != model {
			return "", http.StatusForbidden, "operation does not belong to authorized provider and model"
		}
		value = handle.Operation
	}
	if status, message := validateVideoOperation(value, model); status != 0 {
		return "", status, message
	}
	return value, 0, ""
}

// resolveMediaTarget maps a requested model to one provider/model pair under
// the caller's key policy, mirroring resolveAudioTarget.
func resolveMediaTarget(principal *config.Principal, model string, operation core.ModelOperation) (string, string, int, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", http.StatusBadRequest, "'model' is required"
	}
	resolution, err := resolveModel(context.Background(), model, principal)
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
	for _, target := range targets {
		row, found := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, callerOf(principal))
		if found && !providers.ModelSupportsOperation(row, operation) {
			continue
		}
		switch operation {
		case core.ModelOperationImage:
			if _, supported := imageGeneratorFor(target.Provider, principal); supported {
				return target.Provider, target.Model, 0, ""
			}
		case core.ModelOperationVideo:
			if _, supported := videoGeneratorFor(target.Provider, principal); supported {
				return target.Provider, target.Model, 0, ""
			}
		}
	}
	return "", "", http.StatusBadRequest, "no route member supports the requested media operation"
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
	count := 1
	if body.N != nil {
		if *body.N <= 0 {
			recordFailureUsage("openai.images", body.Model, principal, http.StatusBadRequest, "invalid_count", started)
			writeError(w, http.StatusBadRequest, "'n' must be greater than zero")
			return
		}
		count = *body.N
	}
	providerID, upstreamModel, status, message := resolveMediaTarget(principal, body.Model, core.ModelOperationImage)
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
	var images []providers.GeneratedImage
	var usage map[string]any
	var err error
	if contextual, ok := generator.(providers.ContextImageGenerator); ok {
		images, usage, err = contextual.GenerateImagesContext(r.Context(), upstreamModel, body.Prompt, count)
	} else {
		images, usage, err = generator.GenerateImages(upstreamModel, body.Prompt, count)
	}
	latency := time.Since(started).Milliseconds()
	if err != nil {
		recordFailureUsage("openai.images", body.Model, principal, upstreamErrorStatus(err), "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	if err := validateGeneratedImages(images); err != nil {
		recordFailureUsage("openai.images", body.Model, principal, http.StatusBadGateway, "invalid_upstream_response", started)
		writeError(w, http.StatusBadGateway, err.Error())
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

func validateGeneratedImages(images []providers.GeneratedImage) error {
	if len(images) == 0 {
		return fmt.Errorf("upstream provider returned no image data")
	}
	for _, image := range images {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(image.MimeType))
		if err != nil || len(image.Data) == 0 {
			return fmt.Errorf("upstream provider returned invalid image data")
		}
		switch strings.ToLower(mediaType) {
		case "image/png", "image/jpeg", "image/webp":
		default:
			return fmt.Errorf("upstream provider returned an unsupported image type")
		}
	}
	return nil
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
	providerID, upstreamModel, status, message := resolveMediaTarget(principal, body.Model, core.ModelOperationVideo)
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
		operation, status, message = resolveVideoOperation(operation, providerID, upstreamModel)
		if status != 0 {
			recordFailureUsage("openai.videos", body.Model, principal, status, "invalid_operation", started)
			writeError(w, status, message)
			return
		}
		var job providers.VideoJob
		var err error
		if contextual, ok := generator.(providers.ContextVideoGenerator); ok {
			job, err = contextual.PollVideoContext(r.Context(), operation)
		} else {
			job, err = generator.PollVideo(operation)
		}
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
	var job providers.VideoJob
	var err error
	if contextual, ok := generator.(providers.ContextVideoGenerator); ok {
		job, err = contextual.StartVideoContext(r.Context(), upstreamModel, body.Prompt, body.Parameters)
	} else {
		job, err = generator.StartVideo(upstreamModel, body.Prompt, body.Parameters)
	}
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
		"operation": encodeVideoOperation(providerID, model, job.Operation),
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
	provider, err := providers.GetProviderForPrincipal(providerID, callerOf(principal))
	if err != nil {
		return nil, false
	}
	return providers.AsImageGenerator(provider)
}

func videoGeneratorFor(providerID string, principal *config.Principal) (providers.VideoGenerator, bool) {
	provider, err := providers.GetProviderForPrincipal(providerID, callerOf(principal))
	if err != nil {
		return nil, false
	}
	return providers.AsVideoGenerator(provider)
}
