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
//
// Chat, embeddings and image generation on either surface go through core's
// Google (see googleProvider). This file is the gateway's own transport,
// which still serves video generation and the catalog.

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

// GoogleAIProvider is the gateway's transport for either surface, which the
// facade keeps for video and the catalog. Requests carry the key in
// x-goog-api-key; the value never appears in a URL, so it cannot leak through
// logs or referrers.
type GoogleAIProvider struct {
	surface GoogleAISurface
	apiKey  string
	// bearerToken carries a minted OAuth2 access token. When set it replaces
	// the x-goog-api-key header: service-account auth is a Bearer credential,
	// and Vertex rejects a request that presents both.
	bearerToken       string
	bearerTokenFor    func() (string, error)
	baseURL           string // ai_studio only
	project           string // vertex only
	location          string // vertex only
	vertexRequestType string // vertex invocation accounting only
	timeout           time.Duration
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

func NewVertexAIWithTokenSource(
	baseURL, project, location string, timeoutSeconds float64,
	tokenSource func() (string, error),
) GoogleAIProvider {
	provider := NewVertexAI(baseURL, "", project, location, timeoutSeconds)
	provider.bearerTokenFor = tokenSource
	return provider
}

func (p GoogleAIProvider) withVertexRequestType(value string) GoogleAIProvider {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "paygo":
		p.vertexRequestType = "shared"
	case "dedicated":
		p.vertexRequestType = "dedicated"
	default:
		p.vertexRequestType = ""
	}
	return p
}

func googleTimeout(seconds float64) time.Duration {
	if seconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(seconds * float64(time.Second))
}

func (p GoogleAIProvider) currentBearerToken() (string, error) {
	if p.bearerTokenFor != nil {
		token, err := p.bearerTokenFor()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(token), nil
	}
	return strings.TrimSpace(p.bearerToken), nil
}

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
	if p.surface == SurfaceVertex && p.vertexRequestType != "" {
		request.Header.Set("X-Vertex-AI-LLM-Request-Type", p.vertexRequestType)
	}
	bearerToken, err := p.currentBearerToken()
	if err != nil {
		return nil, 0, err
	}
	switch {
	case bearerToken != "":
		request.Header.Set("Authorization", "Bearer "+bearerToken)
	case p.apiKey != "":
		request.Header.Set("x-goog-api-key", p.apiKey)
	}
	response, err := (&http.Client{Timeout: p.timeout}).Do(request)
	if err != nil {
		return nil, 0, retryableInvocation(p.label() + ": " + err.Error())
	}
	defer response.Body.Close()
	raw, readErr := readInvocationResponseBody(response, p.label())
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	var decoded map[string]any
	decodeErr := json.Unmarshal(raw, &decoded)
	if response.StatusCode >= 400 {
		return decoded, response.StatusCode, p.upstreamError(decoded, raw, response.StatusCode)
	}
	if decodeErr != nil || len(decoded) == 0 {
		return nil, response.StatusCode, circuitFailureInvocation(p.label() + ": invalid JSON in upstream response")
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

// EmbeddingProvider is implemented by native providers that can return an
// OpenAI-shaped embedding envelope without an OpenAI-compatible HTTP surface.
type EmbeddingProvider interface {
	Embed(context.Context, string, any) (map[string]any, error)
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

type ContextImageGenerator interface {
	GenerateImagesContext(context.Context, string, string, int) ([]GeneratedImage, map[string]any, error)
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

type ContextVideoGenerator interface {
	StartVideoContext(context.Context, string, string, map[string]any) (VideoJob, error)
	PollVideoContext(context.Context, string) (VideoJob, error)
}

// StartVideo begins a Veo generation and returns its operation name.
// storageUri is deliberately optional: without it the service returns inline
// bytes, which keeps the gateway free of a Cloud Storage dependency.
func (p GoogleAIProvider) StartVideo(model, prompt string, parameters map[string]any) (VideoJob, error) {
	return p.StartVideoContext(context.Background(), model, prompt, parameters)
}

func (p GoogleAIProvider) StartVideoContext(ctx context.Context, model, prompt string, parameters map[string]any) (VideoJob, error) {
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
	decoded, _, err := p.doContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return VideoJob{}, err
	}
	name, _ := decoded["name"].(string)
	if name == "" {
		return VideoJob{}, &InvocationError{Msg: p.label() + ": the service did not return an operation name"}
	}
	if p.surface == SurfaceAIStudio && strings.HasPrefix(name, "operations/") {
		name = "models/" + strings.TrimPrefix(strings.TrimSpace(model), "models/") + "/" + name
	}
	return VideoJob{Operation: name}, nil
}

// PollVideo checks a long-running operation and extracts the finished video.
func (p GoogleAIProvider) PollVideo(operation string) (VideoJob, error) {
	return p.PollVideoContext(context.Background(), operation)
}

func (p GoogleAIProvider) PollVideoContext(ctx context.Context, operation string) (VideoJob, error) {
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
		decoded, _, err = p.doContext(ctx, http.MethodPost, url, map[string]any{"operationName": operation})
	} else {
		var url string
		url, err = p.operationURL(operation)
		if err != nil {
			return VideoJob{}, err
		}
		decoded, _, err = p.doContext(ctx, http.MethodGet, url, nil)
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
		return "", &ConfigError{Msg: p.label() + ": operation must not be a full URL"}
	}
	if p.surface == SurfaceAIStudio {
		const marker = "/operations/"
		index := strings.Index(operation, marker)
		if index < 0 {
			return "", &ConfigError{Msg: p.label() + ": operation is missing its model binding"}
		}
		return p.baseURL + "/operations/" + strings.TrimPrefix(operation[index+len(marker):], "/"), nil
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

// Vertex's unfiltered Model Garden can contain over 14,000 rows. Bound the
// entire walk, including duplicates and filtered rows, as well as each body:
// per-request timeouts and repeated-token detection do not bound unique tokens.
const (
	googleCatalogMaxPages  = 100
	googleCatalogMaxModels = 20000
)

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
	[]ModelInfo, *CredentialObservation, error,
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
		bearerToken, err := p.currentBearerToken()
		if err != nil {
			code, detail := "catalog_authentication_failed", "Provider credential refresh failed for catalog access."
			if InvocationRetryable(err) {
				code, detail = "catalog_transport_error", "Provider credential refresh could not reach the token service."
			}
			return nil, nil, catalogError(code, detail, UpstreamStatus(err))
		}
		if bearerToken == "" {
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
	models := make([]ModelInfo, 0)
	query := url.Values{"pageSize": {"1000"}}
	seen := map[string]bool{}
	modelCount := 0
	for page := 0; ; page++ {
		if page >= googleCatalogMaxPages {
			return nil, nil, catalogError("catalog_not_discoverable", "Provider catalog listing exceeded the page limit.", 0)
		}
		decoded, err := p.discoverModels(p.baseURL+"/models?"+query.Encode(), "models")
		if err != nil {
			return nil, nil, err
		}
		modelCount += len(decoded["models"].([]any))
		if modelCount > googleCatalogMaxModels {
			return nil, nil, catalogError("catalog_not_discoverable", "Provider catalog listing exceeded the model limit.", 0)
		}
		models = append(models, parseAIStudioModels(decoded)...)
		next, _ := decoded["nextPageToken"].(string)
		if next == "" {
			return models, nil, nil
		}
		if seen[next] {
			return nil, nil, catalogError("catalog_invalid_shape", "Provider catalog response repeated a page token.", http.StatusOK)
		}
		seen[next] = true
		query.Set("pageToken", next)
	}
}

func (p GoogleAIProvider) discoverModels(endpoint, field string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, catalogError("catalog_transport_error", "Provider catalog request could not be created.", 0)
	}
	bearerToken, tokenErr := p.currentBearerToken()
	if tokenErr != nil {
		code, detail := "catalog_authentication_failed", "Provider credential refresh failed for catalog access."
		if InvocationRetryable(tokenErr) {
			code, detail = "catalog_transport_error", "Provider credential refresh could not reach the token service."
		}
		return nil, catalogError(code, detail, UpstreamStatus(tokenErr))
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	} else if p.apiKey != "" {
		req.Header.Set("x-goog-api-key", p.apiKey)
	}
	resp, err := (&http.Client{Timeout: p.timeout}).Do(req)
	if err != nil {
		return nil, catalogError("catalog_transport_error", "Provider catalog request could not reach the upstream service.", 0)
	}
	decoded, err := decodeCatalogResponse(resp, field, "name")
	if err != nil {
		return nil, err
	}
	invalid := func() (map[string]any, error) {
		return nil, catalogError("catalog_invalid_shape", "Provider catalog response contained invalid model metadata.", resp.StatusCode)
	}
	if token, exists := decoded["nextPageToken"]; exists {
		if _, ok := token.(string); !ok {
			return invalid()
		}
	}
	for _, raw := range decoded[field].([]any) {
		row := raw.(map[string]any)
		name := row["name"].(string)
		if strings.TrimSpace(name[strings.LastIndex(name, "/")+1:]) == "" {
			return invalid()
		}
		// These fields drive filtering; malformed values are not evidence that
		// a model is unsupported. Unknown methods/actions remain legitimate.
		if field == "models" {
			if value, exists := row["supportedGenerationMethods"]; exists {
				methods, ok := value.([]any)
				if !ok {
					return invalid()
				}
				for _, method := range methods {
					if text, ok := method.(string); !ok || strings.TrimSpace(text) == "" {
						return invalid()
					}
				}
			}
		} else if value, exists := row["supportedActions"]; exists {
			actions, ok := value.(map[string]any)
			if !ok {
				return invalid()
			}
			// Native actions are messages, including empty objects. Leave unknown
			// extensions open, but never treat a malformed action as capability evidence.
			for _, key := range []string{"openGenerationAiStudio", "requestAccess", "deploy", "deployGke", "multiDeployVertex"} {
				if action, exists := actions[key]; exists {
					if _, ok := action.(map[string]any); !ok {
						return invalid()
					}
				}
			}
		}
	}
	return decoded, nil
}

// catalog is the terse spelling tests reach for; ListModelsWithError is the
// exported name the detailedModelLister interface requires elsewhere.
func (p GoogleAIProvider) catalog() ([]ModelInfo, *CredentialObservation, error) {
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
	seen := map[string]bool{}
	modelCount := 0
	for page := 0; ; page++ {
		if page >= googleCatalogMaxPages {
			return nil, catalogError("catalog_not_discoverable", "Provider catalog listing exceeded the page limit.", 0)
		}
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
		decoded, err := p.discoverModels(endpoint, "publisherModels")
		if err != nil {
			return nil, err
		}
		modelCount += len(decoded["publisherModels"].([]any))
		if modelCount > googleCatalogMaxModels {
			return nil, catalogError("catalog_not_discoverable", "Provider catalog listing exceeded the model limit.", 0)
		}
		models = append(models, vertexManagedModels(decoded, p.location == vertexDefaultLocation)...)
		nextToken, _ := decoded["nextPageToken"].(string)
		if nextToken == "" {
			break
		}
		if seen[nextToken] {
			return nil, catalogError("catalog_invalid_shape", "Provider catalog response repeated a page token.", http.StatusOK)
		}
		seen[nextToken] = true
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
func vertexManagedModels(decoded map[string]any, allowEmptyActions bool) []ModelInfo {
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
		rawActions, actionsPresent := fields["supportedActions"]
		actions, _ := rawActions.(map[string]any)
		_, openGeneration := actions["openGenerationAiStudio"]
		_, gated := actions["requestAccess"]
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
		if !openGeneration && !gated && !(allowEmptyActions && (!actionsPresent || len(actions) == 0)) {
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

// Resilience decorators implement these interfaces directly so the capability
// lookup preserves retry and circuit policy instead of bypassing it.
func AsImageGenerator(provider Provider) (ImageGenerator, bool) {
	for provider != nil {
		if resilient, ok := provider.(*ResilientProvider); ok {
			if _, supported := AsImageGenerator(resilient.inner); !supported {
				return nil, false
			}
			return resilient, true
		}
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
		if resilient, ok := provider.(*ResilientProvider); ok {
			if _, supported := AsVideoGenerator(resilient.inner); !supported {
				return nil, false
			}
			return resilient, true
		}
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

func AsEmbeddingProvider(provider Provider) (EmbeddingProvider, bool) {
	for provider != nil {
		if resilient, ok := provider.(*ResilientProvider); ok {
			if _, supported := AsEmbeddingProvider(resilient.inner); !supported {
				return nil, false
			}
			return resilient, true
		}
		if embedder, ok := provider.(EmbeddingProvider); ok {
			return embedder, true
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return nil, false
		}
		provider = unwrapper.Unwrap()
	}
	return nil, false
}
