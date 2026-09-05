package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

var audioClient = &http.Client{
	Timeout:       300 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
}

const proxyErrorBodyLimit = 64 << 10

// resolveAudioTarget maps a request model (provider/model or category) to a
// single upstream (provider, model), applying key-policy allowlists.
func resolveAudioTarget(principal *config.Principal, model string) (provider, upstreamModel string, status int, msg string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", 400, "'model' is required (e.g. localai/whisper-base)"
	}
	resolution, err := router.ResolveForPrincipal(model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			return "", "", 404, err.Error()
		}
		return "", "", 500, "Gateway is not configured for the requested model."
	}
	targets, st, m := enforceKeyPolicy(principal, model, resolution.Category, resolution.Targets)
	if st != 0 {
		return "", "", st, m
	}
	if len(targets) == 0 {
		return "", "", 404, "no routable target for '" + model + "'"
	}
	return targets[0].Provider, targets[0].Model, 0, ""
}

func copyAuthHeaders(dst *http.Request, headers http.Header, skipContentType bool) {
	for k, vs := range headers {
		if skipContentType && strings.EqualFold(k, "content-type") {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

type apiProxyResponse struct {
	body        []byte
	contentType string
	status      int
	errorCode   string
	err         error
}

func readAPIProxyResponse(resp *http.Response, prefix string) apiProxyResponse {
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return apiProxyResponse{
				status:    http.StatusBadGateway,
				errorCode: "upstream_body_read",
				err:       providers.HTTPInvocationError(prefix, http.StatusBadGateway, []byte("response body could not be read")),
			}
		}
		return apiProxyResponse{body: body, contentType: resp.Header.Get("Content-Type"), status: resp.StatusCode}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, proxyErrorBodyLimit+1))
	if readErr != nil {
		body = []byte("response body could not be read")
	} else if len(body) > proxyErrorBodyLimit {
		// Never sanitize a prefix cut at an arbitrary byte: it could contain only
		// part of a credential and evade a whole-token redaction rule.
		body = []byte("response body exceeded diagnostic limit")
	}
	return apiProxyResponse{
		status:    resp.StatusCode,
		errorCode: "upstream_http_error",
		err:       providers.HTTPInvocationError(prefix, resp.StatusCode, body),
	}
}

func writeAPIProxySuccess(w http.ResponseWriter, status int, contentType string, body []byte) {
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// POST /v1/audio/transcriptions — multipart audio -> text (STT). Reverse-proxied
// to the resolved OpenAI-compatible provider (e.g. LocalAI whisper).
func handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		recordFailureUsage("openai.transcriptions", "", principal, 400, "invalid_multipart", started)
		writeError(w, 400, "invalid multipart form")
		return
	}
	provider, upstreamModel, status, msg := resolveAudioTarget(principal, r.FormValue("model"))
	if status != 0 {
		recordFailureUsage("openai.transcriptions", r.FormValue("model"), principal, status, "policy_or_route", started)
		writeError(w, status, msg)
		return
	}
	base, headers, okp := providers.ProviderHTTPTarget(provider, principal)
	if !okp {
		recordFailureUsage("openai.transcriptions", r.FormValue("model"), principal, 400, "audio_unsupported", started)
		writeError(w, 400, "provider '"+provider+"' does not support audio (use an OpenAI-compatible provider such as LocalAI)")
		return
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		recordFailureUsage("openai.transcriptions", r.FormValue("model"), principal, 400, "missing_file", started)
		writeError(w, 400, "missing 'file' (the audio to transcribe)")
		return
	}
	defer file.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", fh.Filename)
	_, _ = io.Copy(fw, file)
	_ = mw.WriteField("model", upstreamModel)
	for _, f := range []string{"language", "prompt", "response_format", "temperature"} {
		if v := r.FormValue(f); v != "" {
			_ = mw.WriteField(f, v)
		}
	}
	_ = mw.Close()

	req, _ := http.NewRequest("POST", base+"/audio/transcriptions", &buf)
	copyAuthHeaders(req, headers, true)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := audioClient.Do(req)
	if err != nil {
		recordFailureUsage("openai.transcriptions", r.FormValue("model"), principal, 502, "upstream", started)
		writeError(w, 502, "audio transcription upstream error")
		return
	}
	result := readAPIProxyResponse(resp, "audio transcription")
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.transcriptions", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
		Provider: provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		StatusCode: result.status, LatencyMS: time.Since(started).Milliseconds(),
		ErrorCode: result.errorCode, IsStub: isStub(provider),
	})
	if result.err != nil {
		writeUpstreamError(w, result.err)
		return
	}
	writeAPIProxySuccess(w, result.status, result.contentType, result.body)
}

// POST /v1/audio/speech — text -> audio (TTS). Reverse-proxied to the resolved
// OpenAI-compatible provider (e.g. LocalAI piper voices).
func handleSpeech(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		recordFailureUsage("openai.speech", "", principal, 422, "invalid_body", started)
		writeError(w, 422, "invalid request body")
		return
	}
	reqModel, _ := body["model"].(string)
	provider, upstreamModel, status, msg := resolveAudioTarget(principal, reqModel)
	if status != 0 {
		recordFailureUsage("openai.speech", reqModel, principal, status, "policy_or_route", started)
		writeError(w, status, msg)
		return
	}
	if synthesizer, native := providers.SpeechSynthesizerForPrincipal(provider, principal); native {
		serveNativeSpeech(w, body, synthesizer, provider, upstreamModel, principal, started, reqModel)
		return
	}
	base, headers, okp := providers.ProviderHTTPTarget(provider, principal)
	if !okp {
		recordFailureUsage("openai.speech", reqModel, principal, 400, "audio_unsupported", started)
		writeError(w, 400, "provider '"+provider+"' does not support audio (use an OpenAI-compatible provider such as LocalAI)")
		return
	}
	body["model"] = upstreamModel
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+"/audio/speech", bytes.NewReader(payload))
	copyAuthHeaders(req, headers, false)
	req.Header.Set("Content-Type", "application/json")
	resp, err := audioClient.Do(req)
	if err != nil {
		recordFailureUsage("openai.speech", reqModel, principal, 502, "upstream", started)
		writeError(w, 502, "audio speech upstream error")
		return
	}
	result := readAPIProxyResponse(resp, "audio speech")
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.speech", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
		Provider: provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		StatusCode: result.status, LatencyMS: time.Since(started).Milliseconds(),
		ErrorCode: result.errorCode, IsStub: isStub(provider),
	})
	if result.err != nil {
		writeUpstreamError(w, result.err)
		return
	}
	writeAPIProxySuccess(w, result.status, result.contentType, result.body)
}

func audioErrorCode(status int) string {
	if status >= 300 {
		return "upstream_http_error"
	}
	return ""
}

// speedToRate maps the OpenAI speech `speed` field (1.0 = normal) onto the
// prosody rate percentage the synthesis service expects (e.g. "+20%", "-50%").
func speedToRate(speed float64) string {
	if speed <= 0 {
		return "+0%"
	}
	percent := int(math.Round((speed - 1) * 100))
	if percent >= 0 {
		return fmt.Sprintf("+%d%%", percent)
	}
	return fmt.Sprintf("%d%%", percent)
}

// serveNativeSpeech renders TTS through a provider that synthesizes audio
// in-process (e.g. edge_tts) instead of proxying an OpenAI-compatible HTTP API.
func serveNativeSpeech(
	w http.ResponseWriter, body map[string]any, synthesizer providers.SpeechSynthesizer,
	provider, upstreamModel string, principal *config.Principal, started time.Time, reqModel string,
) {
	input, _ := body["input"].(string)
	if strings.TrimSpace(input) == "" {
		recordFailureUsage("openai.speech", reqModel, principal, 422, "missing_input", started)
		writeError(w, 422, "'input' is required")
		return
	}
	if format, _ := body["response_format"].(string); format != "" && !strings.EqualFold(format, "mp3") {
		recordFailureUsage("openai.speech", reqModel, principal, 400, "unsupported_format", started)
		writeError(w, 400, "provider '"+provider+"' produces mp3 audio; set response_format to \"mp3\" or omit it")
		return
	}
	voice, _ := body["voice"].(string)
	if strings.TrimSpace(voice) == "" {
		voice = upstreamModel
	}
	if strings.TrimSpace(voice) == "" || strings.EqualFold(voice, "default") {
		voice = synthesizer.DefaultVoice()
	}
	speed := 1.0
	if raw, ok := body["speed"].(float64); ok {
		speed = raw
	}
	audio, _, err := synthesizer.Synthesize(voice, input, speedToRate(speed))
	if err != nil {
		recordFailureUsage("openai.speech", reqModel, principal, 502, "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.speech", RequestedModel: provider + "/" + voice, RoutedModel: voice,
		Provider: provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		StatusCode: 200, LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(provider),
	})
	w.Header().Set("Content-Type", "audio/mpeg")
	w.WriteHeader(200)
	_, _ = w.Write(audio)
}
