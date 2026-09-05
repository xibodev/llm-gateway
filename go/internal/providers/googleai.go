package providers

// Google's generative models are reachable through two surfaces that share a
// request grammar but almost nothing else:
//
//	ai_studio  generativelanguage.googleapis.com/v1beta/models/{model}
//	           billed against Gemini API prepay credits
//	vertex_ai  {region-}aiplatform.googleapis.com/v1/projects/{p}/locations/{l}/
//	           publishers/google/models/{model}
//	           billed against the Cloud billing account
//
// They are separate providers rather than one with a flag because their model
// catalogues genuinely differ: the same video model is
// veo-3.1-fast-generate-preview on AI Studio and veo-3.1-lite-generate-001 on
// Vertex, and a model's available locations differ per model (gemini-3.5-flash
// answers at "global" but 404s at us-central1, while Veo answers only at
// us-central1). Sharing one catalogue would silently mis-route.
//
// Long-running operations also differ: AI Studio polls with GET on the
// operation resource, Vertex with POST to the model's fetchPredictOperation.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"llmgw/internal/iam"
)

const (
	aiStudioDefaultBase = "https://generativelanguage.googleapis.com/v1beta"
	vertexDefaultHost   = "aiplatform.googleapis.com"
	vertexDefaultAPI    = "v1"
	// Vertex exposes a multi-region "global" endpoint that carries the widest
	// model selection; regional endpoints are opt-in per model.
	vertexDefaultLocation = "global"
)

// GoogleAISurface distinguishes the two deployments.
type GoogleAISurface string

const (
	SurfaceAIStudio GoogleAISurface = "ai_studio"
	SurfaceVertex   GoogleAISurface = "vertex_ai"
)

// GoogleAIProvider speaks the Gemini generateContent grammar against either
// surface. Requests carry the key in x-goog-api-key; the value never appears in
// a URL, so it cannot leak through logs or referrers.
type GoogleAIProvider struct {
	surface GoogleAISurface
	apiKey  string
	// bearerToken carries a minted OAuth2 access token. When set it replaces
	// the x-goog-api-key header: service-account auth is a Bearer credential,
	// and Vertex rejects a request that presents both.
	bearerToken string
	baseURL     string // ai_studio only
	project     string // vertex only
	location    string // vertex only
	timeout     time.Duration
}

// NewAIStudio builds the generativelanguage.googleapis.com provider.
func NewAIStudio(baseURL, apiKey string, timeoutSeconds float64) GoogleAIProvider {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = aiStudioDefaultBase
	}
	return GoogleAIProvider{
		surface: SurfaceAIStudio, apiKey: strings.TrimSpace(apiKey),
		baseURL: base, timeout: googleTimeout(timeoutSeconds),
	}
}

// NewVertexAI builds the aiplatform.googleapis.com provider. An empty location
// means the global endpoint.
func NewVertexAI(baseURL, apiKey, project, location string, timeoutSeconds float64) GoogleAIProvider {
	location = strings.TrimSpace(location)
	if location == "" {
		location = vertexDefaultLocation
	}
	return GoogleAIProvider{
		surface: SurfaceVertex, apiKey: strings.TrimSpace(apiKey),
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		project:  strings.TrimSpace(project),
		location: location,
		timeout:  googleTimeout(timeoutSeconds),
	}
}

// NewVertexAIWithAccessToken builds the Vertex provider for a service-account
// credential. The caller mints the token and this provider only presents it, so
// token lifetime and refresh stay in one place (internal/gcpauth).
func NewVertexAIWithAccessToken(
	baseURL, accessToken, project, location string, timeoutSeconds float64,
) GoogleAIProvider {
	provider := NewVertexAI(baseURL, "", project, location, timeoutSeconds)
	provider.bearerToken = strings.TrimSpace(accessToken)
	return provider
}

func googleTimeout(seconds float64) time.Duration {
	if seconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

func (p GoogleAIProvider) IsStub() bool { return false }

// vertexHost applies the one region rule every Vertex endpoint in this file
// shares: the global endpoint has no region prefix in the host, regional ones
// do. modelURL, operationURL and vertexPublisherModels each need a host built
// this way but disagree on API version and path shape, so only the host rule
// lives here rather than a full URL builder.
func vertexHost(location string) string {
	if location != vertexDefaultLocation {
		return location + "-" + vertexDefaultHost
	}
	return vertexDefaultHost
}

// modelURL builds the fully-qualified endpoint for one model action.
// Vertex needs a project and encodes the location twice: once in the host for
// regional endpoints and once in the resource path.
func (p GoogleAIProvider) modelURL(model, action string) (string, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return "", &ConfigError{Msg: "a model name is required"}
	}
	if p.surface == SurfaceAIStudio {
		return fmt.Sprintf("%s/models/%s:%s", p.baseURL, model, action), nil
	}
	if p.project == "" {
		return "", &ConfigError{Msg: "vertex_ai: project is required (set 'project' on the provider)"}
	}
	base := p.baseURL
	if base == "" {
		base = "https://" + vertexHost(p.location) + "/" + vertexDefaultAPI
	}
	return fmt.Sprintf("%s/projects/%s/locations/%s/publishers/google/models/%s:%s",
		base, p.project, p.location, model, action), nil
}

func (p GoogleAIProvider) do(method, url string, body any) (map[string]any, int, error) {
	return p.doContext(context.Background(), method, url, body)
}

func (p GoogleAIProvider) doContext(ctx context.Context, method, url string, body any) (map[string]any, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	switch {
	case p.bearerToken != "":
		request.Header.Set("Authorization", "Bearer "+p.bearerToken)
	case p.apiKey != "":
		request.Header.Set("x-goog-api-key", p.apiKey)
	}
	response, err := (&http.Client{Timeout: p.timeout}).Do(request)
	if err != nil {
		return nil, 0, &InvocationError{Msg: p.label() + ": " + err.Error()}
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if response.StatusCode >= 400 {
		return decoded, response.StatusCode, p.upstreamError(decoded, raw, response.StatusCode)
	}
	return decoded, response.StatusCode, nil
}

// upstreamError turns Google's error envelope into a gateway error that names
// the real cause. Billing exhaustion and missing model access are distinct
// operator problems and must not both surface as "upstream error".
func (p GoogleAIProvider) upstreamError(decoded map[string]any, raw []byte, status int) error {
	message := strings.TrimSpace(string(raw))
	googleStatus := ""
	if envelope, ok := decoded["error"].(map[string]any); ok {
		if text, ok := envelope["message"].(string); ok && text != "" {
			message = text
		}
		if text, ok := envelope["status"].(string); ok {
			googleStatus = text
		}
	}
	switch {
	case googleStatus == "RESOURCE_EXHAUSTED" && strings.Contains(strings.ToLower(message), "credit"):
		return invocationStatus(p.label()+": provider billing exhausted — "+message, status)
	case status == http.StatusNotFound:
		return invocationStatus(p.label()+": model not available to this project or location — "+message, status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return invocationStatus(p.label()+": credential rejected — "+message, status)
	}
	return invocationStatus(p.label()+": "+message, status)
}

func (p GoogleAIProvider) label() string {
	if p.surface == SurfaceVertex {
		return "vertex_ai"
	}
	return "ai_studio"
}

// ---- chat ---------------------------------------------------------------- //

// Complete translates OpenAI-shaped messages into Gemini contents and maps the
// reply back, so the gateway's routing and translation layers are unchanged.
func (p GoogleAIProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p GoogleAIProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	url, err := p.modelURL(model, "generateContent")
	if err != nil {
		return nil, err
	}
	decoded, _, err := p.doContext(ctx, http.MethodPost, url, googleContentRequest(messages, kw, nil))
	if err != nil {
		return nil, err
	}
	if err := googleEmptyReplyError(p.label(), decoded); err != nil {
		return nil, err
	}
	return googleToOpenAIChat(model, decoded), nil
}

// googleEmptyReplyError explains a reply that contains no text. Gemini's
// thinking models spend maxOutputTokens on reasoning before writing an answer,
// so a small max_tokens yields a successful HTTP 200 carrying an empty string —
// a silent failure the caller cannot diagnose. Name the cause instead.
func googleEmptyReplyError(label string, decoded map[string]any) error {
	for _, part := range googleParts(decoded) {
		if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
			return nil
		}
		if _, ok := part["inlineData"]; ok {
			return nil
		}
	}
	usage, _ := decoded["usageMetadata"].(map[string]any)
	thoughts, _ := usage["thoughtsTokenCount"].(float64)
	finish := ""
	if candidates, ok := decoded["candidates"].([]any); ok && len(candidates) > 0 {
		if first, ok := candidates[0].(map[string]any); ok {
			finish, _ = first["finishReason"].(string)
		}
	}
	if thoughts > 0 {
		return &InvocationError{Msg: fmt.Sprintf(
			"%s: the model spent its entire output budget on reasoning (%d thinking tokens) and returned no text — raise max_tokens or omit it",
			label, int(thoughts))}
	}
	if finish != "" && finish != "STOP" {
		return &InvocationError{Msg: fmt.Sprintf("%s: the model returned no text (finish reason %s)", label, finish)}
	}
	return &InvocationError{Msg: label + ": the model returned no text"}
}

func (p GoogleAIProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: p.label() + ": streaming is not implemented for this provider yet; use a non-streaming request"}
}

// googleContentRequest maps chat messages to Gemini's contents/parts grammar.
// Gemini has no "system" role: system text becomes systemInstruction.
func googleContentRequest(messages []Message, kw Kwargs, modalities []string) map[string]any {
	contents := make([]map[string]any, 0, len(messages))
	var systemParts []map[string]any
	for _, message := range messages {
		role, _ := message["role"].(string)
		text, _ := message["content"].(string)
		if role == "system" || role == "developer" {
			systemParts = append(systemParts, map[string]any{"text": text})
			continue
		}
		if role == "assistant" {
			role = "model"
		}
		if role == "" {
			role = "user"
		}
		contents = append(contents, map[string]any{
			"role": role, "parts": []map[string]any{{"text": text}},
		})
	}
	request := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		request["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	generation := map[string]any{}
	if value, ok := kw["max_tokens"]; ok && value != nil {
		generation["maxOutputTokens"] = value
	} else if value, ok := kw["_max_output_tokens"]; ok && value != nil {
		generation["maxOutputTokens"] = value
	}
	if value, ok := kw["temperature"]; ok && value != nil {
		generation["temperature"] = value
	}
	if len(modalities) > 0 {
		generation["responseModalities"] = modalities
	}
	if len(generation) > 0 {
		request["generationConfig"] = generation
	}
	return request
}

// googleToOpenAIChat reshapes a generateContent reply into the Chat Completions
// envelope the gateway already understands.
func googleToOpenAIChat(model string, decoded map[string]any) map[string]any {
	text := strings.Builder{}
	for _, part := range googleParts(decoded) {
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	inputTokens, outputTokens, totalTokens := googleUsage(decoded)
	served := model
	if value, ok := decoded["modelVersion"].(string); ok && value != "" {
		served = value
	}
	return map[string]any{
		"id":     "chatcmpl-google",
		"object": "chat.completion",
		"model":  served,
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": text.String()},
		}},
		"usage": map[string]any{
			"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": totalTokens,
		},
	}
}

func googleParts(decoded map[string]any) []map[string]any {
	candidates, _ := decoded["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	first, _ := candidates[0].(map[string]any)
	content, _ := first["content"].(map[string]any)
	rawParts, _ := content["parts"].([]any)
	parts := make([]map[string]any, 0, len(rawParts))
	for _, raw := range rawParts {
		if part, ok := raw.(map[string]any); ok {
			parts = append(parts, part)
		}
	}
	return parts
}

// googleUsage reads usageMetadata. Google reports image cost as token counts
// tagged with modality, so image and text accounting stay uniform.
func googleUsage(decoded map[string]any) (int, int, int) {
	usage, _ := decoded["usageMetadata"].(map[string]any)
	number := func(key string) int {
		if value, ok := usage[key].(float64); ok {
			return int(value)
		}
		return 0
	}
	input := number("promptTokenCount")
	output := number("candidatesTokenCount") + number("thoughtsTokenCount")
	total := number("totalTokenCount")
	if total == 0 {
		total = input + output
	}
	return input, output, total
}

// ---- image --------------------------------------------------------------- //

// GeneratedImage is one decoded image returned by an image-capable model.
type GeneratedImage struct {
	Data     []byte
	MimeType string
}

// ImageGenerator is implemented by providers that synthesize images.
type ImageGenerator interface {
	GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error)
}

// GenerateImages asks an image-capable Gemini model for inline image bytes.
func (p GoogleAIProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, &InvocationError{Msg: p.label() + ": a prompt is required"}
	}
	url, err := p.modelURL(model, "generateContent")
	if err != nil {
		return nil, nil, err
	}
	messages := []Message{{"role": "user", "content": prompt}}
	body := googleContentRequest(messages, Kwargs{}, []string{"TEXT", "IMAGE"})
	decoded, _, err := p.do(http.MethodPost, url, body)
	if err != nil {
		return nil, nil, err
	}
	images := make([]GeneratedImage, 0, 1)
	for _, part := range googleParts(decoded) {
		inline, ok := part["inlineData"].(map[string]any)
		if !ok {
			continue
		}
		encoded, _ := inline["data"].(string)
		mime, _ := inline["mimeType"].(string)
		raw, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil || len(raw) == 0 {
			continue
		}
		images = append(images, GeneratedImage{Data: raw, MimeType: mime})
		if count > 0 && len(images) >= count {
			break
		}
	}
	if len(images) == 0 {
		return nil, nil, &InvocationError{Msg: p.label() + ": the model returned no image data — it may be a text-only model"}
	}
	usage, _ := decoded["usageMetadata"].(map[string]any)
	return images, usage, nil
}

// ---- video --------------------------------------------------------------- //

// VideoJob identifies an in-flight long-running generation.
type VideoJob struct {
	Operation string `json:"operation"`
	Done      bool   `json:"done"`
	VideoURI  string `json:"video_uri,omitempty"`
	Data      []byte `json:"-"`
	MimeType  string `json:"mime_type,omitempty"`
}

// VideoGenerator is implemented by providers that synthesize video. Video is
// inherently long-running, so the contract is start-then-poll rather than a
// blocking call pretending to be synchronous.
type VideoGenerator interface {
	StartVideo(model, prompt string, parameters map[string]any) (VideoJob, error)
	PollVideo(operation string) (VideoJob, error)
}

// StartVideo begins a Veo generation and returns its operation name.
// storageUri is deliberately optional: without it the service returns inline
// bytes, which keeps the gateway free of a Cloud Storage dependency.
func (p GoogleAIProvider) StartVideo(model, prompt string, parameters map[string]any) (VideoJob, error) {
	if strings.TrimSpace(prompt) == "" {
		return VideoJob{}, &InvocationError{Msg: p.label() + ": a prompt is required"}
	}
	url, err := p.modelURL(model, "predictLongRunning")
	if err != nil {
		return VideoJob{}, err
	}
	if parameters == nil {
		parameters = map[string]any{}
	}
	if _, ok := parameters["sampleCount"]; !ok {
		parameters["sampleCount"] = 1
	}
	body := map[string]any{
		"instances":  []any{map[string]any{"prompt": prompt}},
		"parameters": parameters,
	}
	decoded, _, err := p.do(http.MethodPost, url, body)
	if err != nil {
		return VideoJob{}, err
	}
	name, _ := decoded["name"].(string)
	if name == "" {
		return VideoJob{}, &InvocationError{Msg: p.label() + ": the service did not return an operation name"}
	}
	return VideoJob{Operation: name}, nil
}

// PollVideo checks a long-running operation and extracts the finished video.
func (p GoogleAIProvider) PollVideo(operation string) (VideoJob, error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return VideoJob{}, &InvocationError{Msg: p.label() + ": an operation name is required"}
	}
	var decoded map[string]any
	var err error
	if p.surface == SurfaceVertex {
		// Vertex has no GET on the operation resource — that returns Google's
		// generic 404 page. Long-running predictions are fetched by POSTing the
		// operation name to the model's fetchPredictOperation action.
		model := vertexOperationModel(operation)
		if model == "" {
			return VideoJob{}, &InvocationError{Msg: p.label() + ": could not derive the model from operation " + operation}
		}
		var url string
		url, err = p.modelURL(model, "fetchPredictOperation")
		if err != nil {
			return VideoJob{}, err
		}
		decoded, _, err = p.do(http.MethodPost, url, map[string]any{"operationName": operation})
	} else {
		var url string
		url, err = p.operationURL(operation)
		if err != nil {
			return VideoJob{}, err
		}
		decoded, _, err = p.do(http.MethodGet, url, nil)
	}
	if err != nil {
		return VideoJob{}, err
	}
	job := VideoJob{Operation: operation}
	if done, ok := decoded["done"].(bool); ok {
		job.Done = done
	}
	if !job.Done {
		return job, nil
	}
	uri, data, mime := googleVideoResult(decoded)
	job.VideoURI, job.Data, job.MimeType = uri, data, mime
	if job.VideoURI == "" && len(job.Data) == 0 {
		return job, &InvocationError{Msg: p.label() + ": the operation finished without returning a video"}
	}
	return job, nil
}

// vertexOperationModel pulls the model id out of a Vertex operation name of the
// form projects/../locations/../publishers/google/models/{model}/operations/{id}.
func vertexOperationModel(operation string) string {
	const marker = "/models/"
	start := strings.Index(operation, marker)
	if start < 0 {
		return ""
	}
	rest := operation[start+len(marker):]
	if end := strings.Index(rest, "/operations/"); end >= 0 {
		return rest[:end]
	}
	return ""
}

func (p GoogleAIProvider) operationURL(operation string) (string, error) {
	if strings.HasPrefix(operation, "http://") || strings.HasPrefix(operation, "https://") {
		return operation, nil
	}
	if p.surface == SurfaceAIStudio {
		return p.baseURL + "/" + strings.TrimPrefix(operation, "/"), nil
	}
	base := p.baseURL
	if base == "" {
		base = "https://" + vertexHost(p.location) + "/" + vertexDefaultAPI
	}
	return base + "/" + strings.TrimPrefix(operation, "/"), nil
}

// googleVideoResult walks the two response shapes the surfaces use: Vertex
// nests videos under response.videos, AI Studio under
// response.generateVideoResponse.generatedSamples.
func googleVideoResult(decoded map[string]any) (string, []byte, string) {
	response, _ := decoded["response"].(map[string]any)
	if response == nil {
		return "", nil, ""
	}
	collect := func(entry map[string]any) (string, []byte, string) {
		if uri, ok := entry["uri"].(string); ok && uri != "" {
			mime, _ := entry["mimeType"].(string)
			return uri, nil, mime
		}
		if uri, ok := entry["gcsUri"].(string); ok && uri != "" {
			mime, _ := entry["mimeType"].(string)
			return uri, nil, mime
		}
		if encoded, ok := entry["bytesBase64Encoded"].(string); ok && encoded != "" {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				mime, _ := entry["mimeType"].(string)
				if mime == "" {
					mime = "video/mp4"
				}
				return "", raw, mime
			}
		}
		return "", nil, ""
	}
	if videos, ok := response["videos"].([]any); ok {
		for _, raw := range videos {
			if entry, ok := raw.(map[string]any); ok {
				if uri, data, mime := collect(entry); uri != "" || len(data) > 0 {
					return uri, data, mime
				}
			}
		}
	}
	if wrapper, ok := response["generateVideoResponse"].(map[string]any); ok {
		if samples, ok := wrapper["generatedSamples"].([]any); ok {
			for _, raw := range samples {
				sample, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if video, ok := sample["video"].(map[string]any); ok {
					if uri, data, mime := collect(video); uri != "" || len(data) > 0 {
						return uri, data, mime
					}
				}
			}
		}
	}
	return "", nil, ""
}

// ---- catalog ------------------------------------------------------------- //

// ListModels reports the models this surface exposes. AI Studio publishes a
// machine-readable catalogue with supportedGenerationMethods, which is the
// honest source for capability. Vertex was long believed to expose no public
// catalogue for publisher models; that belief was tested against the live API
// and is wrong. A real discovery route exists — GET
// v1beta1/publishers/{publisher}/models — but only on v1beta1 (the v1
// collection path serves a generic HTML 404; only the single-model resource
// route works on v1) and only with an OAuth principal (an API key gets
// "API keys are not supported by this API. Expected OAuth2 access token.").
// See vertexPublisherModels for the measured details.
func (p GoogleAIProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p GoogleAIProvider) ListModelsWithError() (
	[]ModelInfo, *iam.ProviderAccountObservation, error,
) {
	if p.surface == SurfaceVertex {
		if strings.TrimSpace(p.project) == "" {
			return nil, nil, catalogError(
				"catalog_configuration_incomplete",
				"Vertex AI requires a Google Cloud project ID.",
				0,
			)
		}
		// Discovery requires an OAuth principal. Google refuses API keys on
		// ListPublisherModels by design ("API keys are not supported by this
		// API. Expected OAuth2 access token."), so a key-only instance cannot
		// have a real catalog and must say so rather than advertise one it did
		// not measure.
		if strings.TrimSpace(p.bearerToken) == "" {
			return nil, nil, catalogError(
				"catalog_not_discoverable",
				"Vertex AI model discovery requires a service account credential; "+
					"the catalog is not discoverable with an API key alone.",
				0,
			)
		}
		models, err := p.vertexPublisherModels("google")
		if err != nil {
			return nil, nil, err
		}
		return models, nil, nil
	}
	if strings.TrimSpace(p.apiKey) == "" {
		return nil, nil, catalogError(
			"catalog_authentication_failed",
			"Provider API key is required for catalog access.",
			0,
		)
	}
	decoded, status, err := p.do(http.MethodGet, p.baseURL+"/models?pageSize=1000", nil)
	if err != nil {
		code := "catalog_transport_error"
		detail := "Provider catalog request could not reach the upstream service."
		if invocation, ok := err.(*InvocationError); ok && invocation.Status > 0 {
			code = "catalog_http_error"
			detail = fmt.Sprintf("Provider catalog returned HTTP %d.", invocation.Status)
			if invocation.Status == http.StatusUnauthorized ||
				invocation.Status == http.StatusForbidden {
				code = "catalog_authentication_failed"
				detail = "Provider credential was rejected by the catalog API."
			}
		}
		return nil, nil, catalogError(code, detail, status)
	}
	return parseAIStudioModels(decoded), nil, nil
}

// catalog is the terse spelling tests reach for; ListModelsWithError is the
// exported name the detailedModelLister interface requires elsewhere.
func (p GoogleAIProvider) catalog() ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	return p.ListModelsWithError()
}

func parseAIStudioModels(decoded map[string]any) []ModelInfo {
	rawModels, _ := decoded["models"].([]any)
	models := make([]ModelInfo, 0, len(rawModels))
	for _, raw := range rawModels {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		id := strings.TrimPrefix(name, "models/")
		if id == "" {
			continue
		}
		methods := make([]string, 0, 4)
		if raw, ok := entry["supportedGenerationMethods"].([]any); ok {
			for _, method := range raw {
				if text, ok := method.(string); ok {
					methods = append(methods, text)
				}
			}
		}
		capabilities, surfaces := googleCapabilities(id, methods)
		if len(capabilities) == 0 {
			continue
		}
		label, _ := entry["displayName"].(string)
		models = append(models, ModelInfo{
			ID: id, Vendor: "google", Label: label,
			Capabilities: capabilities, SupportedSurfaces: surfaces,
		})
	}
	return models
}

// googleCapabilities maps Google's own supportedGenerationMethods onto the
// gateway's capability vocabulary. Image models are identified by name because
// they advertise the same generateContent method as text models.
func googleCapabilities(id string, methods []string) (map[string]any, []string) {
	capabilities := map[string]any{}
	surfaces := []string{}
	has := func(name string) bool {
		for _, method := range methods {
			if method == name {
				return true
			}
		}
		return false
	}
	lower := strings.ToLower(id)
	switch {
	case strings.Contains(lower, "veo") && has("predictLongRunning"):
		capabilities["video"] = true
		surfaces = append(surfaces, "/v1/videos/generations")
	case strings.Contains(lower, "image") && has("generateContent"):
		capabilities["image"] = true
		surfaces = append(surfaces, "/v1/images/generations")
	case has("generateContent"):
		capabilities["chat"] = true
		surfaces = append(surfaces, "/v1/chat/completions", "/v1/messages")
	case has("embedContent"):
		capabilities["embedding"] = true
	}
	return capabilities, surfaces
}

// vertexPublisherModels lists a publisher's managed models for this
// instance's configured location, measured live against the Agent Platform
// endpoint rather than curated by hand:
//
//   - The collection route (this one) exists only on v1beta1. Every v1
//     variant returns a generic HTML 404 — no handler — while the
//     single-model *resource* route does work on v1, which is probably where
//     the old "no public catalogue" belief came from. Inference
//     (modelURL/p.do) stays on v1, so this provider deliberately speaks two
//     API versions.
//   - The project-scoped form (projects/*/locations/*/publishers/*/models)
//     has no list method — 404. The project comes from the OAuth token, not
//     the path, so the collection route below carries no project segment.
//   - The catalogue is region-specific and not nested: across 16 regions
//     counts ranged 2→128, and "global" carries ids a region like
//     us-central1 lacks and vice versa. Discovery is therefore always scoped
//     to p.location, never merged across locations.
//
// Callers must hold an OAuth bearer token: Google refuses API keys on this
// route by design (see ListModelsWithError), so that check happens before
// this function is ever reached.
func (p GoogleAIProvider) vertexPublisherModels(publisher string) ([]ModelInfo, error) {
	// Global-vs-regional host selection matches modelURL via vertexHost;
	// the version segment differs (v1beta1 here, v1 there), so only the
	// host rule is shared, not the whole URL builder.
	base := "https://" + vertexHost(p.location)
	if p.baseURL != "" {
		base = vertexDiscoveryBase(p.baseURL)
	}
	models := make([]ModelInfo, 0, 64)
	pageToken := ""
	for {
		// pageSize=1000 is rejected upstream; 200 is the measured working value.
		query := url.Values{"pageSize": {"200"}}
		if pageToken != "" {
			// nextPageToken is opaque and is not URL-safe by contract. Appended
			// raw it only survives while it happens to contain no reserved
			// character — a '+' in the token decodes back as a space upstream,
			// so the second page is requested with a token nobody issued.
			query.Set("pageToken", pageToken)
		}
		endpoint := fmt.Sprintf(
			"%s/v1beta1/publishers/%s/models?%s", base, publisher, query.Encode(),
		)
		decoded, status, err := p.do(http.MethodGet, endpoint, nil)
		if err != nil {
			code := "catalog_transport_error"
			detail := "Provider catalog request could not reach the upstream service."
			if invocation, ok := err.(*InvocationError); ok && invocation.Status > 0 {
				code = "catalog_http_error"
				detail = fmt.Sprintf("Provider catalog returned HTTP %d.", invocation.Status)
				if invocation.Status == http.StatusUnauthorized ||
					invocation.Status == http.StatusForbidden {
					code = "catalog_authentication_failed"
					detail = "Provider credential was rejected by the catalog API."
				}
			}
			return nil, catalogError(code, detail, status)
		}
		models = append(models, vertexManagedModels(decoded)...)
		nextToken, _ := decoded["nextPageToken"].(string)
		if nextToken == "" {
			break
		}
		pageToken = nextToken
	}
	return models, nil
}

// vertexDiscoveryBase turns a configured inference base_url into the root the
// discovery route hangs off.
//
// modelURL's contract is that a custom Vertex base_url already carries the
// inference API root (/v1) — its default is built that way — while the
// collection route this file uses exists only on v1beta1. Appending the
// discovery version to an unstripped base therefore produced
// <base>/v1/v1beta1/publishers/... and 404ed discovery on every proxied or
// custom Vertex instance, while inference on the same configuration kept
// working. A base that does not end in the inference version is left alone:
// the caller owns the path shape, and guessing a segment to remove would
// break a proxy that mounts the API somewhere else.
func vertexDiscoveryBase(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return strings.TrimSuffix(base, "/"+vertexDefaultAPI)
}

// vertexManagedModels keeps only the entries an operator can call directly:
// supportedActions containing openGenerationAiStudio (available in AI
// Studio-style generation) or requestAccess (gated but directly callable once
// granted). Everything else is a Model Garden entry whose only actions are
// deploy / deployGke / multiDeployVertex — self-deploy models that require
// the operator to stand up their own endpoint first. One measured region
// carried 11,841 of those against roughly 78 managed models; without this
// filter a single region's catalog balloons past 14,000 rows.
func vertexManagedModels(decoded map[string]any) []ModelInfo {
	raw, _ := decoded["publisherModels"].([]any)
	models := make([]ModelInfo, 0, len(raw))
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fields["name"].(string)
		id := name
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			id = name[idx+1:]
		}
		if id == "" {
			continue
		}
		actions, _ := fields["supportedActions"].(map[string]any)
		_, openGeneration := actions["openGenerationAiStudio"]
		_, gated := actions["requestAccess"]
		if !openGeneration && !gated {
			continue
		}
		// The list response carries no display-name field either — only
		// name/versionId/supportedActions and similar machine fields were
		// observed — so Label is left unset rather than guessed.
		capabilities, surfaces := vertexModelCapability(id)
		// An id the classifier cannot place is omitted, not listed bare.
		// Leaving it in the catalog does not make it unroutable: the console's
		// capabilitiesFor reads a row that declares no modality and no surface
		// as a chat model by convention, and Routes offers every catalog row as
		// a route member — so an unclassified family was selectable, went to
		// generateContent, and produced exactly the upstream 404 this
		// classifier exists to prevent. Omission costs nothing at call time:
		// `<provider>/<model>` resolves without consulting the catalog
		// (router.ResolveForPrincipal), so an operator who knows the id can
		// still address it directly.
		if len(capabilities) == 0 {
			continue
		}
		models = append(models, ModelInfo{
			ID: id, Vendor: "google",
			Capabilities: capabilities, SupportedSurfaces: surfaces,
		})
	}
	return models
}

// vertexModelCapability infers a discovered model's capability from its id.
// ListPublisherModels carries no supportedGenerationMethods, or any other
// capability field, the way AI Studio's /models response does, so this
// substring match is a floor, not a measurement.
//
// An earlier version reused googleCapabilities by passing it a fixed
// []string{"generateContent", "predictLongRunning"} methods list for every
// entry. That made has("generateContent") and has("predictLongRunning")
// unconditionally true, so the dispatch collapsed to "veo" -> video,
// "image" -> image, everything else -> chat — silently mis-tagging embedding
// models (text-embedding-005, gemini-embedding-001, ...) as chat-callable.
// Routed to /v1/chat/completions that 404s upstream, since generateContent is
// not an embedding model's action, and the failure surfaces as an opaque
// "model not available" rather than the real cause. embedding is checked
// first because "gemini-embedding-*" ids contain both "gemini" and
// "embedding".
//
// An id that matches nothing known below returns no capability at all, and
// vertexManagedModels drops that row rather than listing it bare. An earlier
// version kept it, reasoning that a row with no capability is unroutable — it
// is not. The console's capabilitiesFor treats a row declaring no modality and
// no surface as a chat model, so the row was offered as a route member and
// 404ed on generateContent. "No capability" is not a neutral answer anywhere
// this catalog is read.
func vertexModelCapability(id string) (map[string]any, []string) {
	lower := strings.ToLower(id)
	switch {
	case strings.Contains(lower, "embed"):
		return map[string]any{"embedding": true}, nil
	case strings.Contains(lower, "veo"):
		return map[string]any{"video": true}, []string{"/v1/videos/generations"}
	case strings.Contains(lower, "image"):
		return map[string]any{"image": true}, []string{"/v1/images/generations"}
	case strings.Contains(lower, "gemini") || strings.Contains(lower, "bison") ||
		strings.Contains(lower, "chat") || strings.Contains(lower, "codey"):
		return map[string]any{"chat": true}, []string{"/v1/chat/completions", "/v1/messages"}
	}
	return nil, nil
}

// AsImageGenerator and AsVideoGenerator unwrap resilience decorators to reach
// the concrete provider, mirroring AsSpeechSynthesizer.
func AsImageGenerator(provider Provider) (ImageGenerator, bool) {
	for provider != nil {
		if generator, ok := provider.(ImageGenerator); ok {
			return generator, true
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return nil, false
		}
		provider = unwrapper.Unwrap()
	}
	return nil, false
}

func AsVideoGenerator(provider Provider) (VideoGenerator, bool) {
	for provider != nil {
		if generator, ok := provider.(VideoGenerator); ok {
			return generator, true
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return nil, false
		}
		provider = unwrapper.Unwrap()
	}
	return nil, false
}
