package api

import (
	"bytes"
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
)

// The playground exercises audio models through the same project attribution,
// policy enforcement and quota consumption as chat, so an operator can verify a
// speech or transcription model without minting a key or leaving the console.

type playgroundSpeechBody struct {
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Model       string `json:"model"`
	Input       string `json:"input"`
	Speed       any    `json:"speed"`
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
func audioPlaygroundTarget(principal *config.Principal, model string) (string, string, int, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", http.StatusBadRequest, "model is required"
	}
	resolution, err := router.ResolveForPrincipal(model, principal)
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
	return targets[0].Provider, targets[0].Model, 0, ""
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
	providerID, voice, status, message := audioPlaygroundTarget(principal, body.Model)
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
		providerID, voice, body.Input, speed, principal,
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

func playgroundSpeechAudio(
	providerID, model, input string, speed float64,
	principal *config.Principal,
) ([]byte, string, string, int, error) {
	if synthesizer, native := providers.SpeechSynthesizerForPrincipal(
		providerID, principal,
	); native {
		audio, format, err := synthesizer.Synthesize(
			model, input, speedToRate(speed),
		)
		return audio, format, "audio/mpeg", 0, err
	}
	base, headers, ok := providers.ProviderHTTPTarget(providerID, principal)
	if !ok {
		return nil, "", "", http.StatusBadRequest, fmt.Errorf(
			"provider does not expose an OpenAI-compatible speech endpoint",
		)
	}
	payload, err := json.Marshal(map[string]any{
		"model": model, "voice": model, "input": input,
		"speed": speed, "response_format": "mp3",
	})
	if err != nil {
		return nil, "", "", 0, err
	}
	request, err := http.NewRequest(
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

// POST /admin/api/playground/transcription — multipart audio in, text out,
// proxied to the resolved OpenAI-compatible transcription provider.
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
	providerID, upstreamModel, status, message := audioPlaygroundTarget(principal, r.FormValue("model"))
	if status != 0 {
		writeError(w, status, message)
		return
	}
	base, headers, ok := providers.ProviderHTTPTarget(providerID, principal)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider '"+providerID+"' does not expose an OpenAI-compatible transcription endpoint")
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
	request, err := http.NewRequest(http.MethodPost, base+"/audio/transcriptions", body)
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
func multipartAudioRequest(file io.Reader, filename, model, language string) (io.Reader, string, error) {
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
	return &buf, writer.FormDataContentType(), nil
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

// POST /admin/api/playground/image — generate an image under project
// attribution and return it inline for the console to display.
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
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "a prompt is required")
		return
	}
	providerID, model, status, message := audioPlaygroundTarget(principal, body.Model)
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
	images, usage, err := generator.GenerateImages(model, body.Prompt, 1)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.PrincipalID, Action: "playground.image", TargetType: "project", TargetID: project.ID, Result: "failure", Detail: map[string]any{"model": body.Model}})
		writeUpstreamError(w, err)
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
	providerID, model, status, message := audioPlaygroundTarget(principal, body.Model)
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
		job, err := generator.PollVideo(operation)
		if err != nil {
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
	job, err := generator.StartVideo(model, body.Prompt, body.Parameters)
	if err != nil {
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
