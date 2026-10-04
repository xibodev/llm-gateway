package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

var audioClient = &http.Client{
	Timeout:       300 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
}

const proxyErrorBodyLimit = 64 << 10

// resolveAudioTarget maps a request model (provider/model or category) to a
// single upstream (provider, model), applying key-policy allowlists.
func resolveAudioTarget(principal *config.Principal, model string, operation core.ModelOperation) (provider, upstreamModel string, status int, msg string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", 400, "'model' is required (e.g. localai/whisper-base)"
	}
	resolution, err := resolveModel(context.Background(), model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			return "", "", 404, err.Error()
		}
		return "", "", 500, "Gateway is not configured for the requested model."
	}
	targets, st, m := authorizeKeyPolicy(principal, model, resolution.Category, resolution.Targets)
	if st != 0 {
		return "", "", st, m
	}
	if len(targets) == 0 {
		return "", "", 404, "no routable target for '" + model + "'"
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

func audioSurface(operation core.ModelOperation) (core.ModelSurface, bool) {
	switch operation {
	case core.ModelOperationAudioIn:
		return core.ModelSurfaceAudioTranscriptions, true
	case core.ModelOperationAudioOut:
		return core.ModelSurfaceAudioSpeech, true
	default:
		return "", false
	}
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

// transcriptionFields are the form fields forwarded upstream besides the
// audio and the resolved model.
var transcriptionFields = []string{"language", "prompt", "response_format", "temperature"}

// transcriptionForm is a transcription request read part by part. The audio
// is copied straight into the form sent upstream, so the gateway holds it once
// rather than once parsed and again re-encoded.
type transcriptionForm struct {
	upstream *multipart.Writer
	body     bytes.Buffer
	hasFile  bool
	fields   map[string]string
	query    url.Values
}

func readTranscriptionForm(r *http.Request) (*transcriptionForm, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	form := &transcriptionForm{fields: map[string]string{}, query: r.URL.Query()}
	form.upstream = multipart.NewWriter(&form.body)
	for {
		part, err := reader.NextPart()
		// NextPart wraps io.EOF when the body ends before the closing
		// boundary; only the bare value marks a complete form.
		if err == io.EOF {
			return form, nil
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		// Parts that are not forwarded are skipped unread rather than held.
		switch {
		case part.FileName() != "":
			// As with r.FormFile, only the first audio part is sent.
			if name != "file" || form.hasFile {
				continue
			}
			audio, err := form.upstream.CreateFormFile("file", part.FileName())
			if err == nil {
				_, err = io.Copy(audio, part)
			}
			if err != nil {
				return nil, err
			}
			form.hasFile = true
		case name == "model" || slices.Contains(transcriptionFields, name):
			if _, seen := form.fields[name]; seen {
				continue
			}
			value, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			form.fields[name] = string(value)
		}
	}
}

// value keeps r.FormValue's precedence, a query parameter before the form's
// first field of that name, so reading the form as a stream changes nothing a
// client can see.
func (f *transcriptionForm) value(name string) string {
	if values := f.query[name]; len(values) > 0 {
		return values[0]
	}
	return f.fields[name]
}

// finish appends the resolved model and the forwarded fields after the audio
// and returns the upstream body.
func (f *transcriptionForm) finish(model string) (*bytes.Buffer, string) {
	_ = f.upstream.WriteField("model", model)
	for _, name := range transcriptionFields {
		if value := f.value(name); value != "" {
			_ = f.upstream.WriteField(name, value)
		}
	}
	_ = f.upstream.Close()
	return &f.body, f.upstream.FormDataContentType()
}

// POST /v1/audio/transcriptions — multipart audio -> text (STT). Reverse-proxied
// to the resolved OpenAI-compatible provider (e.g. LocalAI whisper).
func handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	form, err := readTranscriptionForm(r)
	if err != nil {
		status := writeBodyError(w, err, 400, "invalid multipart form")
		recordFailureUsage("openai.transcriptions", "", principal, status, "invalid_multipart", started)
		return
	}
	model := form.value("model")
	provider, upstreamModel, status, msg := resolveAudioTarget(principal, model, core.ModelOperationAudioIn)
	if status != 0 {
		recordFailureUsage("openai.transcriptions", model, principal, status, "policy_or_route", started)
		writeError(w, status, msg)
		return
	}
	if !form.hasFile {
		recordFailureUsage("openai.transcriptions", model, principal, 400, "missing_file", started)
		writeError(w, 400, "missing 'file' (the audio to transcribe)")
		return
	}
	if !admitRequest(w, "openai.transcriptions", model, principal, "policy_or_route", started) {
		return
	}
	buf, contentType := form.finish(upstreamModel)

	coreResponse, handled, coreErr := providers.InvokeCoreSurfaceForPrincipal(
		r.Context(), provider, callerOf(principal), core.Request{
			Surface: core.ModelSurfaceAudioTranscriptions, Model: upstreamModel,
			Body: buf.Bytes(), ContentType: contentType,
		},
	)
	if handled {
		status := http.StatusOK
		errorCode := ""
		if coreErr != nil {
			status, errorCode = upstreamErrorStatus(coreErr), "upstream"
		}
		router.RecordUsage(router.UsageRecord{
			RequestID: principal.RequestID, Endpoint: "openai.transcriptions", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
			Provider: provider, Project: principal.Project, Key: principal.Key,
			ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
			StatusCode: status, LatencyMS: time.Since(started).Milliseconds(),
			ErrorCode: errorCode, IsStub: isStub(provider),
		})
		if coreErr != nil {
			writeUpstreamError(w, coreErr)
			return
		}
		writeAPIProxySuccess(w, status, coreResponse.ContentType, coreResponse.Body)
		return
	}

	base, headers, okp := providers.ProviderHTTPTarget(provider, callerOf(principal))
	if !okp {
		recordFailureUsage("openai.transcriptions", model, principal, 400, "audio_unsupported", started)
		writeError(w, 400, "provider '"+provider+"' does not support audio (use an OpenAI-compatible provider such as LocalAI)")
		return
	}

	req, _ := http.NewRequestWithContext(r.Context(), "POST", base+"/audio/transcriptions", buf)
	copyAuthHeaders(req, headers, true)
	req.Header.Set("Content-Type", contentType)
	resp, err := audioClient.Do(req)
	if err != nil {
		recordFailureUsage("openai.transcriptions", model, principal, 502, "upstream", started)
		writeError(w, 502, "audio transcription upstream error")
		return
	}
	result := readAPIProxyResponse(resp, "audio transcription")
	router.RecordUsage(router.UsageRecord{
		RequestID: principal.RequestID, Endpoint: "openai.transcriptions", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
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
		status := writeBodyError(w, err, 422, "invalid request body")
		recordFailureUsage("openai.speech", "", principal, status, "invalid_body", started)
		return
	}
	reqModel, _ := body["model"].(string)
	provider, upstreamModel, status, msg := resolveAudioTarget(principal, reqModel, core.ModelOperationAudioOut)
	if status != 0 {
		recordFailureUsage("openai.speech", reqModel, principal, status, "policy_or_route", started)
		writeError(w, status, msg)
		return
	}
	if synthesizer, native := providers.SpeechSynthesizerForPrincipal(provider, callerOf(principal)); native {
		serveNativeSpeech(r.Context(), w, body, synthesizer, provider, upstreamModel, principal, started, reqModel)
		return
	}
	if !admitRequest(w, "openai.speech", reqModel, principal, "policy_or_route", started) {
		return
	}
	body["model"] = upstreamModel
	payload, _ := json.Marshal(body)
	coreResponse, handled, coreErr := providers.InvokeCoreSurfaceForPrincipal(
		r.Context(), provider, callerOf(principal), core.Request{
			Surface: core.ModelSurfaceAudioSpeech, Model: upstreamModel,
			Body: payload, ContentType: core.ContentTypeJSON,
		},
	)
	if handled {
		status := http.StatusOK
		errorCode := ""
		if coreErr != nil {
			status, errorCode = upstreamErrorStatus(coreErr), "upstream"
		}
		router.RecordUsage(router.UsageRecord{
			RequestID: principal.RequestID, Endpoint: "openai.speech", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
			Provider: provider, Project: principal.Project, Key: principal.Key,
			ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
			StatusCode: status, LatencyMS: time.Since(started).Milliseconds(),
			ErrorCode: errorCode, IsStub: isStub(provider),
		})
		if coreErr != nil {
			writeUpstreamError(w, coreErr)
			return
		}
		writeAPIProxySuccess(w, status, coreResponse.ContentType, coreResponse.Body)
		return
	}
	base, headers, okp := providers.ProviderHTTPTarget(provider, callerOf(principal))
	if !okp {
		recordFailureUsage("openai.speech", reqModel, principal, 400, "audio_unsupported", started)
		writeError(w, 400, "provider '"+provider+"' does not support audio (use an OpenAI-compatible provider such as LocalAI)")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", base+"/audio/speech", bytes.NewReader(payload))
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
		RequestID: principal.RequestID, Endpoint: "openai.speech", RequestedModel: provider + "/" + upstreamModel, RoutedModel: upstreamModel,
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
	ctx context.Context, w http.ResponseWriter, body map[string]any, synthesizer providers.SpeechSynthesizer,
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
	if principal != nil && principal.Token != "" {
		projectPolicy, err := iam.GetProjectPolicy(principal.ProjectID)
		if err != nil {
			recordFailureUsage("openai.speech", reqModel, principal, 500, "policy_or_route", started)
			writeError(w, 500, "Project policy store unavailable.")
			return
		}
		// Native voices are models. Only unscoped callers may override the
		// authorized target with the legacy OpenAI-shaped voice field.
		if principal.RoutesOnly || len(principal.AllowedRoutes) > 0 ||
			len(principal.AllowedModels) > 0 || len(principal.AllowedProviders) > 0 ||
			len(projectPolicy.AllowedModels) > 0 || len(projectPolicy.AllowedProviders) > 0 {
			voice = upstreamModel
		}
	}
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
	if !admitRequest(w, "openai.speech", reqModel, principal, "policy_or_route", started) {
		return
	}
	var audio []byte
	var err error
	if contextual, ok := synthesizer.(providers.ContextSpeechSynthesizer); ok {
		audio, _, err = contextual.SynthesizeContext(ctx, voice, input, speedToRate(speed))
	} else {
		audio, _, err = synthesizer.Synthesize(voice, input, speedToRate(speed))
	}
	if err != nil {
		recordFailureUsage("openai.speech", reqModel, principal, 502, "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	router.RecordUsage(router.UsageRecord{
		RequestID: principal.RequestID, Endpoint: "openai.speech", RequestedModel: provider + "/" + voice, RoutedModel: voice,
		Provider: provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		StatusCode: 200, LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(provider),
	})
	w.Header().Set("Content-Type", "audio/mpeg")
	w.WriteHeader(200)
	_, _ = w.Write(audio)
}
