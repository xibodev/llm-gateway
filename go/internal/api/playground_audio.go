package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

// The playground exercises audio models through the same project attribution,
// policy enforcement and quota consumption as chat, so an operator can verify a
// speech or transcription model without minting a key or leaving the console.

type playgroundSpeechBody struct {
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Model       string `json:"model"`
	Voice       string `json:"voice,omitempty"`
	Input       string `json:"input"`
	Speed       any    `json:"speed"`
}

type playgroundEmbeddingsBody struct {
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Model       string `json:"model"`
	Input       any    `json:"input"`
}

// resolvePlaygroundActor mirrors the chat playground's admin principal
// resolution: an explicit human principal, or the acting admin's own.
func resolvePlaygroundActor(r *http.Request, principalID, projectID string) (*config.Principal, iam.Project, int, string) {
	principalID = strings.TrimSpace(principalID)
	if principalID == "" {
		principalID = getAdminActor(r).PrincipalID
	}
	if principalID == "" {
		return nil, iam.Project{}, http.StatusBadRequest, "principal_id is required for static-admin playground requests"
	}
	owner, found, err := iam.PrincipalByID(principalID)
	if err != nil {
		return nil, iam.Project{}, http.StatusInternalServerError, "Identity store unavailable."
	}
	if !found || owner.Kind != "human" {
		return nil, iam.Project{}, http.StatusBadRequest, "playground requires an active human principal"
	}
	return resolvePlaygroundPrincipal(owner, projectID)
}

// audioPlaygroundTarget resolves a requested model to one provider/model pair
// under the project's policy, exactly as the audio endpoints do.
func audioPlaygroundTarget(principal *config.Principal, model string, operation core.ModelOperation) (string, string, int, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", http.StatusBadRequest, "model is required"
	}
	resolution, err := resolveModel(context.Background(), model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			return "", "", http.StatusNotFound, err.Error()
		}
		return "", "", http.StatusInternalServerError, "Gateway route configuration is unavailable."
	}
	targets, status, message := enforcePlaygroundPolicy(principal, model, resolution.Category, resolution.Targets)
	if status != 0 {
		return "", "", status, message
	}
	if len(targets) == 0 {
		return "", "", http.StatusNotFound, "no routable target for '" + model + "'"
	}
	for _, target := range targets {
		row, found := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, callerOf(principal))
		if found && providers.ModelOperationSupport(row, operation) == core.SupportUnsupported {
			continue
		}
		if operation == core.ModelOperationAudioOut {
			if _, native := providers.SpeechSynthesizerForPrincipal(target.Provider, callerOf(principal)); native {
				return target.Provider, target.Model, 0, ""
			}
		}
		if operation == core.ModelOperationEmbeddings {
			if instance, err := providers.GetProviderForPrincipal(target.Provider, callerOf(principal)); err == nil {
				if _, native := providers.AsEmbeddingProvider(instance); native {
					return target.Provider, target.Model, 0, ""
				}
			}
		}
		if surface, ok := audioSurface(operation); ok && providers.CoreServesSurfaceForPrincipal(
			target.Provider, target.Model, surface, callerOf(principal),
		) {
			return target.Provider, target.Model, 0, ""
		}
		if _, _, ok := providers.ProviderHTTPTarget(target.Provider, callerOf(principal)); ok {
			return target.Provider, target.Model, 0, ""
		}
	}
	return "", "", http.StatusBadRequest, "no route member supports the requested operation"
}

func mediaPlaygroundTarget(principal *config.Principal, model string, operation core.ModelOperation) (string, string, int, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", http.StatusBadRequest, "model is required"
	}
	resolution, err := resolveModel(context.Background(), model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			return "", "", http.StatusNotFound, err.Error()
		}
		return "", "", http.StatusInternalServerError, "Gateway route configuration is unavailable."
	}
	targets, status, message := enforcePlaygroundPolicy(principal, model, resolution.Category, resolution.Targets)
	if status != 0 {
		return "", "", status, message
	}
	for _, target := range targets {
		row, found := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, callerOf(principal))
		if found && !providers.ModelSupportsOperation(row, operation) {
			continue
		}
		if operation == core.ModelOperationImage {
			if _, ok := imageGeneratorFor(target.Provider, principal); ok {
				return target.Provider, target.Model, 0, ""
			}
		} else if _, ok := videoGeneratorFor(target.Provider, principal); ok {
			return target.Provider, target.Model, 0, ""
		}
	}
	return "", "", http.StatusBadRequest, "no route member supports the requested media operation"
}

// POST /admin/api/playground/speech — synthesize audio and return it inline so
// the console can play it back without a second authenticated fetch.
func handleAdminPlaygroundSpeech(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body playgroundSpeechBody
	if !decodeBody(r, &body) {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return
	}
	principal, project, status, message := resolvePlaygroundActor(r, body.PrincipalID, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if strings.TrimSpace(body.Input) == "" {
		writeError(w, http.StatusBadRequest, "input text is required")
		return
	}
	providerID, voice, status, message := audioPlaygroundTarget(principal, body.Model, core.ModelOperationAudioOut)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	speed := 1.0
	switch typed := body.Speed.(type) {
	case float64:
		speed = typed
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			speed = parsed
		}
	}

	started := time.Now()
	audio, format, contentType, upstreamStatus, err := playgroundSpeechAudio(
		r.Context(), providerID, voice, body.Voice, body.Input, speed, principal,
	)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		if upstreamStatus == 0 {
			upstreamStatus = http.StatusBadGateway
		}
		router.RecordUsage(router.UsageRecord{
			Endpoint: "playground.speech", RequestedModel: body.Model, Provider: providerID,
			Project: project.Slug, Key: "playground", ProjectID: project.ID,
			PrincipalID: principal.PrincipalID, StatusCode: upstreamStatus,
			LatencyMS: latency, ErrorCode: "upstream",
		})
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.speech", TargetType: "project", TargetID: project.ID, Result: "failure", Detail: map[string]any{"model": body.Model}})
		if upstreamStatus >= http.StatusBadRequest &&
			upstreamStatus < http.StatusInternalServerError {
			writeError(w, upstreamStatus, "speech provider rejected the request")
		} else {
			writeError(w, http.StatusBadGateway, "speech provider request failed")
		}
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.speech", RequestedModel: body.Model, RoutedModel: voice,
		Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
		PrincipalID: principal.PrincipalID, StatusCode: http.StatusOK, LatencyMS: latency,
	})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.speech", TargetType: "project", TargetID: project.ID, Result: "success", Detail: map[string]any{"model": body.Model, "voice": voice}})
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":       map[string]any{"provider": providerID, "model": voice},
		"latency_ms":   latency,
		"audio_format": format,
		"content_type": contentType,
		"audio_base64": base64.StdEncoding.EncodeToString(audio),
		"audio_bytes":  len(audio),
	})
}

func handleAdminPlaygroundEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body playgroundEmbeddingsBody
	if !decodeBody(r, &body) {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return
	}
	principal, project, status, message := resolvePlaygroundActor(r, body.PrincipalID, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if inputEmpty(body.Input) {
		writeError(w, http.StatusBadRequest, "embedding input is required")
		return
	}
	providerID, upstreamModel, status, message := audioPlaygroundTarget(principal, body.Model, core.ModelOperationEmbeddings)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	if instance, providerErr := providers.GetProviderForPrincipal(providerID, callerOf(principal)); providerErr == nil {
		if embedder, supported := providers.AsEmbeddingProvider(instance); supported {
			if !nativeEmbeddingInputValid(body.Input) {
				writeError(w, http.StatusBadRequest, "native embedding input must be a string or array of strings")
				return
			}
			started := time.Now()
			result, embedErr := embedder.Embed(r.Context(), upstreamModel, body.Input)
			latency := time.Since(started).Milliseconds()
			status := http.StatusOK
			errorCode := ""
			if embedErr != nil {
				status, errorCode = upstreamErrorStatus(embedErr), "upstream"
			}
			recordPlaygroundEmbeddingUsage(body.Model, providerID, upstreamModel, principal, project, status, latency, errorCode)
			if embedErr != nil {
				writeUpstreamError(w, embedErr)
				return
			}
			writePlaygroundEmbeddingResult(w, r, result, body.Model, providerID, upstreamModel, principal, project, latency)
			return
		}
	}
	base, headers, ok := providers.ProviderHTTPTarget(providerID, callerOf(principal))
	if !ok {
		writeError(w, http.StatusBadRequest, "provider does not expose an OpenAI-compatible embeddings endpoint")
		return
	}
	payload, _ := json.Marshal(embeddingsRequest{Model: upstreamModel, Input: body.Input})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base, "/")+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build the upstream request")
		return
	}
	copyAuthHeaders(request, headers, false)
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := embeddingsClient.Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		recordPlaygroundEmbeddingUsage(body.Model, providerID, upstreamModel, principal, project, http.StatusBadGateway, latency, "upstream")
		writeError(w, http.StatusBadGateway, "embeddings provider request failed")
		return
	}
	result := readAPIProxyResponse(response, "embeddings")
	recordPlaygroundEmbeddingUsage(body.Model, providerID, upstreamModel, principal, project, result.status, latency, result.errorCode)
	if result.err != nil {
		writeUpstreamError(w, result.err)
		return
	}
	decoded := decodeJSONObject(result.body)
	writePlaygroundEmbeddingResult(w, r, decoded, body.Model, providerID, upstreamModel, principal, project, latency)
}

func writePlaygroundEmbeddingResult(w http.ResponseWriter, r *http.Request, decoded map[string]any, requestedModel, providerID, upstreamModel string, principal *config.Principal, project iam.Project, latency int64) {
	data, _ := decoded["data"].([]any)
	dimensions := 0
	if len(data) > 0 {
		if first, ok := data[0].(map[string]any); ok {
			if vector, ok := first["embedding"].([]any); ok {
				dimensions = len(vector)
			}
		}
	}
	auditAdmin(r, "playground.embeddings", "project", project.ID, map[string]any{"model": requestedModel, "principal_id": principal.PrincipalID})
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":     map[string]any{"provider": providerID, "model": upstreamModel},
		"latency_ms": latency, "vectors": len(data), "dimensions": dimensions,
		"usage": safePlaygroundValue(decoded["usage"]), "raw_response": safePlaygroundValue(decoded),
	})
}

func recordPlaygroundEmbeddingUsage(requestedModel, providerID, upstreamModel string, principal *config.Principal, project iam.Project, status int, latency int64, errorCode string) {
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.embeddings", RequestedModel: requestedModel, RoutedModel: upstreamModel,
		Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
		PrincipalID: principal.PrincipalID, StatusCode: status, LatencyMS: latency,
		ErrorCode: errorCode, CreditsMilli: embeddingsCreditsMilli,
	})
}

func playgroundSpeechAudio(
	ctx context.Context,
	providerID, model, voice, input string, speed float64,
	principal *config.Principal,
) ([]byte, string, string, int, error) {
	if synthesizer, native := providers.SpeechSynthesizerForPrincipal(
		providerID, callerOf(principal),
	); native {
		var audio []byte
		var format string
		var err error
		if contextual, ok := synthesizer.(providers.ContextSpeechSynthesizer); ok {
			audio, format, err = contextual.SynthesizeContext(ctx, model, input, speedToRate(speed))
		} else {
			audio, format, err = synthesizer.Synthesize(model, input, speedToRate(speed))
		}
		return audio, format, "audio/mpeg", 0, err
	}
	if strings.TrimSpace(voice) == "" {
		voice = model
	}
	payload, err := json.Marshal(map[string]any{
		"model": model, "voice": voice, "input": input, "speed": speed,
	})
	if err != nil {
		return nil, "", "", 0, err
	}
	coreResponse, handled, coreErr := providers.InvokeCoreSurfaceForPrincipal(
		ctx, providerID, callerOf(principal), core.Request{
			Surface: core.ModelSurfaceAudioSpeech, Model: model,
			Body: payload, ContentType: core.ContentTypeJSON,
		},
	)
	if handled {
		if coreErr != nil {
			return nil, "", "", upstreamErrorStatus(coreErr), coreErr
		}
		return coreResponse.Body, audioFormat(coreResponse.ContentType), coreResponse.ContentType, http.StatusOK, nil
	}
	base, headers, ok := providers.ProviderHTTPTarget(providerID, callerOf(principal))
	if !ok {
		return nil, "", "", http.StatusBadRequest, fmt.Errorf(
			"provider does not expose an OpenAI-compatible speech endpoint",
		)
	}
	payload, err = json.Marshal(map[string]any{
		"model": model, "voice": voice, "input": input,
		"speed": speed, "response_format": "mp3",
	})
	if err != nil {
		return nil, "", "", 0, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(base, "/")+"/audio/speech",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, "", "", 0, err
	}
	copyAuthHeaders(request, headers, false)
	request.Header.Set("Content-Type", "application/json")
	response, err := audioClient.Do(request)
	if err != nil {
		return nil, "", "", 0, err
	}
	defer response.Body.Close()
	audio, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if readErr != nil {
		return nil, "", "", response.StatusCode, readErr
	}
	if response.StatusCode >= http.StatusBadRequest {
		return nil, "", "", response.StatusCode, fmt.Errorf(
			"speech provider returned HTTP %d", response.StatusCode,
		)
	}
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	return audio, "mp3", contentType, response.StatusCode, nil
}

func audioFormat(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/l16":
		return "pcm16"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	default:
		return "audio"
	}
}

// POST /admin/api/playground/transcription — multipart audio in, text out.
func handleAdminPlaygroundTranscription(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	principal, project, status, message := resolvePlaygroundActor(r, r.FormValue("principal_id"), r.FormValue("project_id"))
	if status != 0 {
		writeError(w, status, message)
		return
	}
	providerID, upstreamModel, status, message := audioPlaygroundTarget(principal, r.FormValue("model"), core.ModelOperationAudioIn)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "an audio 'file' is required")
		return
	}
	defer file.Close()

	started := time.Now()
	body, contentType, err := multipartAudioRequest(file, header.Filename, upstreamModel, r.FormValue("language"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build the upstream request")
		return
	}
	coreResponse, handled, coreErr := providers.InvokeCoreSurfaceForPrincipal(
		r.Context(), providerID, callerOf(principal), core.Request{
			Surface: core.ModelSurfaceAudioTranscriptions, Model: upstreamModel,
			Body: body, ContentType: contentType,
		},
	)
	if handled {
		latency := time.Since(started).Milliseconds()
		status := http.StatusOK
		errorCode := ""
		if coreErr != nil {
			status, errorCode = upstreamErrorStatus(coreErr), "upstream"
		}
		router.RecordUsage(router.UsageRecord{
			Endpoint: "playground.transcription", RequestedModel: r.FormValue("model"), RoutedModel: upstreamModel,
			Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
			PrincipalID: principal.PrincipalID, StatusCode: status, LatencyMS: latency, ErrorCode: errorCode,
		})
		result := "success"
		if coreErr != nil {
			result = "failure"
		}
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.transcription", TargetType: "project", TargetID: project.ID, Result: result, Detail: map[string]any{"model": r.FormValue("model")}})
		if coreErr != nil {
			writeUpstreamError(w, coreErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"project_id": project.ID, "principal_id": principal.PrincipalID,
			"served":     map[string]any{"provider": providerID, "model": upstreamModel},
			"latency_ms": latency,
			"text":       transcriptionText(coreResponse.Body),
			"raw":        safePlaygroundValue(decodeJSONObject(coreResponse.Body)),
		})
		return
	}
	base, headers, ok := providers.ProviderHTTPTarget(providerID, callerOf(principal))
	if !ok {
		writeError(w, http.StatusBadRequest, "provider '"+providerID+"' does not expose an OpenAI-compatible transcription endpoint")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build the upstream request")
		return
	}
	copyAuthHeaders(request, headers, true)
	request.Header.Set("Content-Type", contentType)
	response, err := audioClient.Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		router.RecordUsage(router.UsageRecord{
			Endpoint: "playground.transcription", RequestedModel: r.FormValue("model"), Provider: providerID,
			Project: project.Slug, Key: "playground", ProjectID: project.ID,
			PrincipalID: principal.PrincipalID, StatusCode: http.StatusBadGateway, LatencyMS: latency, ErrorCode: "upstream",
		})
		writeError(w, http.StatusBadGateway, "transcription upstream error")
		return
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.transcription", RequestedModel: r.FormValue("model"), RoutedModel: upstreamModel,
		Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
		PrincipalID: principal.PrincipalID, StatusCode: response.StatusCode, LatencyMS: latency,
		ErrorCode: audioErrorCode(response.StatusCode),
	})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.transcription", TargetType: "project", TargetID: project.ID, Result: map[bool]string{true: "success", false: "failure"}[response.StatusCode < 400], Detail: map[string]any{"model": r.FormValue("model")}})
	if response.StatusCode >= 400 {
		writeError(w, response.StatusCode, fmt.Sprintf("transcription failed: %s", strings.TrimSpace(string(payload))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":     map[string]any{"provider": providerID, "model": upstreamModel},
		"latency_ms": latency,
		"text":       transcriptionText(payload),
		"raw":        safePlaygroundValue(decodeJSONObject(payload)),
	})
}

// multipartAudioRequest rebuilds the uploaded audio as an upstream multipart
// body with the resolved model name.
func multipartAudioRequest(file io.Reader, filename, model, language string) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, "", err
	}
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(language) != "" {
		if err := writer.WriteField("language", language); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

func decodeJSONObject(payload []byte) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// transcriptionText pulls the transcript out of an OpenAI-shaped response,
// falling back to the raw body when a provider returns plain text.
func transcriptionText(payload []byte) string {
	if decoded := decodeJSONObject(payload); len(decoded) > 0 {
		if text, ok := decoded["text"].(string); ok {
			return text
		}
	}
	return strings.TrimSpace(string(payload))
}

type playgroundMediaBody struct {
	ProjectID   string         `json:"project_id"`
	PrincipalID string         `json:"principal_id"`
	Model       string         `json:"model"`
	Prompt      string         `json:"prompt"`
	Parameters  map[string]any `json:"parameters"`
	Operation   string         `json:"operation"`
}

// POST /admin/api/playground/image generates an image under project
// attribution and returns it inline for the console to display.
func handleAdminPlaygroundImage(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body playgroundMediaBody
	if !decodeBody(r, &body) {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return
	}
	principal, project, status, message := resolvePlaygroundActor(r, body.PrincipalID, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	executePlaygroundImage(w, r, body, principal, project)
}

func executePlaygroundImage(w http.ResponseWriter, r *http.Request, body playgroundMediaBody, principal *config.Principal, project iam.Project) {
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "a prompt is required")
		return
	}
	providerID, model, status, message := mediaPlaygroundTarget(principal, body.Model, core.ModelOperationImage)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	generator, ok := imageGeneratorFor(providerID, principal)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider '"+providerID+"' does not generate images")
		return
	}
	started := time.Now()
	var images []providers.GeneratedImage
	var usage map[string]any
	var err error
	if contextual, ok := generator.(providers.ContextImageGenerator); ok {
		images, usage, err = contextual.GenerateImagesContext(r.Context(), model, body.Prompt, 1)
	} else {
		images, usage, err = generator.GenerateImages(model, body.Prompt, 1)
	}
	latency := time.Since(started).Milliseconds()
	if err != nil {
		recordPlaygroundMediaFailure("playground.image", "playground.image", body.Model, providerID, model, principal, project, started, err)
		writeUpstreamError(w, err)
		return
	}
	if err := validateGeneratedImages(images); err != nil {
		recordPlaygroundMediaFailure("playground.image", "playground.image", body.Model, providerID, model, principal, project, started, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	inputTokens, outputTokens := googleModalityUsage(usage)
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.image", RequestedModel: body.Model, RoutedModel: model,
		Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
		PrincipalID: principal.PrincipalID, InputTokens: inputTokens, OutputTokens: outputTokens,
		StatusCode: http.StatusOK, LatencyMS: latency,
	})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.image", TargetType: "project", TargetID: project.ID, Result: "success", Detail: map[string]any{"model": body.Model}})
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": project.ID, "principal_id": principal.PrincipalID,
		"served":       map[string]any{"provider": providerID, "model": model},
		"latency_ms":   latency,
		"content_type": images[0].MimeType,
		"image_base64": base64.StdEncoding.EncodeToString(images[0].Data),
		"image_bytes":  len(images[0].Data),
		"usage":        usage,
	})
}

// POST /admin/api/playground/video — start a generation, or poll one when the
// request carries an operation name.
func handleAdminPlaygroundVideo(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body playgroundMediaBody
	if !decodeBody(r, &body) {
		writeError(w, http.StatusBadRequest, "invalid playground request")
		return
	}
	principal, project, status, message := resolvePlaygroundActor(r, body.PrincipalID, body.ProjectID)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	providerID, model, status, message := mediaPlaygroundTarget(principal, body.Model, core.ModelOperationVideo)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	generator, ok := videoGeneratorFor(providerID, principal)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider '"+providerID+"' does not generate video")
		return
	}
	if operation := strings.TrimSpace(body.Operation); operation != "" {
		operation, status, message = resolveVideoOperation(operation, providerID, model)
		if status != 0 {
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
			recordPlaygroundMediaFailure("playground.video", "playground.video", body.Model, providerID, model, principal, project, time.Now(), err)
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, videoJobPayload(providerID, model, job))
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "a prompt is required to start a generation")
		return
	}
	started := time.Now()
	var job providers.VideoJob
	var err error
	if contextual, ok := generator.(providers.ContextVideoGenerator); ok {
		job, err = contextual.StartVideoContext(r.Context(), model, body.Prompt, body.Parameters)
	} else {
		job, err = generator.StartVideo(model, body.Prompt, body.Parameters)
	}
	if err != nil {
		recordPlaygroundMediaFailure("playground.video", "playground.video", body.Model, providerID, model, principal, project, started, err)
		writeUpstreamError(w, err)
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "playground.video", RequestedModel: body.Model, RoutedModel: model,
		Provider: providerID, Project: project.Slug, Key: "playground", ProjectID: project.ID,
		PrincipalID: principal.PrincipalID, StatusCode: http.StatusAccepted,
		LatencyMS: time.Since(started).Milliseconds(),
	})
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.video", TargetType: "project", TargetID: project.ID, Result: "success", Detail: map[string]any{"model": body.Model, "operation": job.Operation}})
	writeJSON(w, http.StatusAccepted, videoJobPayload(providerID, model, job))
}

func recordPlaygroundMediaFailure(endpoint, action, requested, providerID, model string, principal *config.Principal, project iam.Project, started time.Time, err error) {
	router.RecordUsage(router.UsageRecord{
		Endpoint: endpoint, RequestedModel: requested, RoutedModel: model, Provider: providerID,
		Project: project.Slug, Key: "playground", ProjectID: project.ID, PrincipalID: principal.PrincipalID,
		StatusCode: upstreamErrorStatus(err), LatencyMS: time.Since(started).Milliseconds(), ErrorCode: "upstream",
	})
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.PrincipalID, Action: action, TargetType: "project", TargetID: project.ID,
		Result: "failure", Detail: map[string]any{"model": requested},
	})
}
