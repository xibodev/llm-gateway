package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// Extension-served provider types
const (
	ExtensionTypeCopilot        = "github_copilot"
	ExtensionTypeCodex          = "openai_codex"
	ExtensionTypeAntigravity    = "google_antigravity"
	ExtensionTypeEdgeTTS        = "edge_tts"
	ExtensionTypeZenAnonymous   = "opencode_zen_anonymous"
	ExtensionTypeAnthropicSetup = "anthropic_setup_token"
)

func isExtensionType(ptype string) bool {
	switch strings.ToLower(strings.TrimSpace(ptype)) {
	case ExtensionTypeCopilot, ExtensionTypeCodex, ExtensionTypeAntigravity,
		ExtensionTypeEdgeTTS, ExtensionTypeZenAnonymous, ExtensionTypeAnthropicSetup, "extension":
		return true
	default:
		return false
	}
}

func (rt *Runtime) isExtensionEnabled() bool {
	val := strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_ENABLED")))
	if val == "1" || val == "true" || val == "yes" || val == "on" {
		return true
	}
	return strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_URL")) != ""
}

type ExtensionClient struct {
	BaseURL    string
	Secret     string
	HTTPClient *http.Client
}

func (rt *Runtime) extensionClient() *ExtensionClient {
	url := strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_URL"))
	if url == "" {
		url = "http://127.0.0.1:18888"
	}
	secret := strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_SECRET"))
	return &ExtensionClient{
		BaseURL:    strings.TrimRight(url, "/"),
		Secret:     secret,
		HTTPClient: &http.Client{Timeout: 180 * time.Second},
	}
}

// extensionCoreVertical returns a coreVertical that delegates to the local extension sidecar.
func (rt *Runtime) extensionCoreVertical(providerType string) coreVertical {
	client := rt.extensionClient()
	isOAuth := providerType == ExtensionTypeCodex || providerType == ExtensionTypeCopilot || providerType == ExtensionTypeAntigravity
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			if cfg == nil {
				return false
			}
			t := strings.ToLower(strings.TrimSpace(cfg.Type))
			r := strings.ToLower(strings.TrimSpace(cfg.RegistryID))
			if providerType == ExtensionTypeCodex {
				return t == ExtensionTypeCodex || r == ExtensionTypeCodex
			}
			return t == providerType
		},
		provider: func(_ *config.Settings, instance string) (core.Provider, error) {
			return &extensionProvider{
				client:     client,
				instance:   instance,
				providerID: providerType,
			}, nil
		},
		refresh: func(_ *config.Settings, _ string) tokenstore.RefreshFunc {
			return func(ctx context.Context, current tokenstore.Record) (tokenstore.Record, error) {
				return client.Refresh(ctx, providerType, current)
			}
		},
		credentials: extensionCredentials{
			open: func() (core.CredentialStore, error) { return rt.openCredentials(isOAuth) },
		},
	}
}

type extensionCredentials struct {
	open func() (core.CredentialStore, error)
}

func (c extensionCredentials) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	store, err := c.open()
	if err != nil {
		return "", err
	}
	return store.Resolve(ctx, caller, instance)
}

func (c extensionCredentials) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Load(ctx, key)
}

func (c extensionCredentials) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

func (c extensionCredentials) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.ReplaceIfCurrent(ctx, key, revision, record)
}

func (c extensionCredentials) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := c.open()
	if err != nil {
		return err
	}
	return store.RevokeIfCurrent(ctx, key, revision)
}

func (c extensionCredentials) Lease(ctx context.Context, key string) (func(), error) {
	store, err := c.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}

type extensionProvider struct {
	client     *ExtensionClient
	instance   string
	providerID string
}

func (p *extensionProvider) NativeSurfaces(model string) []core.ModelSurface {
	return []core.ModelSurface{
		core.ModelSurfaceChatCompletions,
		core.ModelSurfaceResponses,
		core.ModelSurfaceMessages,
		core.ModelSurfaceAudioSpeech,
		core.ModelSurfaceImages,
	}
}

func (p *extensionProvider) Invoke(ctx context.Context, req core.Request) (core.Response, error) {
	return p.client.Invoke(ctx, p.providerID, req)
}

func (p *extensionProvider) Stream(ctx context.Context, req core.Request) (core.StreamIter, error) {
	return p.client.Stream(ctx, p.providerID, req)
}

func (p *extensionProvider) ListModels(ctx context.Context, cred *core.Credential) ([]core.ModelInfo, error) {
	return p.client.ListModels(ctx, p.providerID, cred)
}

// Invoke executes an invoke call on the extension daemon.
func (c *ExtensionClient) Invoke(ctx context.Context, provider string, req core.Request) (core.Response, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/invoke", c.BaseURL, provider)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return core.Response{}, err
	}
	c.applyHeaders(httpReq, provider, req)
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return core.Response{}, fmt.Errorf("extension invoke %s: %w", provider, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Response{}, fmt.Errorf("read extension response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return core.Response{}, parseExtensionError(resp.StatusCode, body)
	}
	var losses []core.Loss
	if lossesHdr := resp.Header.Get("X-Losses"); lossesHdr != "" {
		if raw, err := base64.StdEncoding.DecodeString(lossesHdr); err == nil {
			_ = json.Unmarshal(raw, &losses)
		}
	}
	return core.Response{
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Losses:      losses,
	}, nil
}

// Stream executes a streaming call on the extension daemon.
func (c *ExtensionClient) Stream(ctx context.Context, provider string, req core.Request) (core.StreamIter, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/stream", c.BaseURL, provider)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	c.applyHeaders(httpReq, provider, req)
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("extension stream %s: %w", provider, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, parseExtensionError(resp.StatusCode, body)
	}
	return &extensionStream{
		body: resp.Body,
		r:    bufio.NewReader(resp.Body),
	}, nil
}

type extensionStream struct {
	body io.ReadCloser
	r    *bufio.Reader
}

func (s *extensionStream) Next() ([]byte, error) {
	var frame []byte
	for {
		line, err := s.r.ReadBytes('\n')
		if len(line) > 0 {
			frame = append(frame, line...)
		}
		if err != nil {
			if len(frame) > 0 {
				return frame, nil
			}
			return nil, err
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			if len(frame) > 0 {
				return frame, nil
			}
		}
	}
}

func (s *extensionStream) Close() error {
	return s.body.Close()
}

// ListModels gets the models from the extension daemon.
func (c *ExtensionClient) ListModels(ctx context.Context, provider string, cred *core.Credential) ([]core.ModelInfo, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/models", c.BaseURL, provider)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.applyAuthHeader(httpReq)
	if cred != nil {
		c.applyCredentialHeaders(httpReq, cred)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("extension list models %s: %w", provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, parseExtensionError(resp.StatusCode, body)
	}
	var res struct {
		Models []core.ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode extension models: %w", err)
	}
	return res.Models, nil
}

// Refresh calls the extension daemon to refresh tokens.
func (c *ExtensionClient) Refresh(ctx context.Context, provider string, record tokenstore.Record) (tokenstore.Record, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/refresh", c.BaseURL, provider)
	reqBody, _ := json.Marshal(map[string]any{"record": record})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return tokenstore.Record{}, err
	}
	c.applyAuthHeader(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return tokenstore.Record{}, fmt.Errorf("extension refresh %s: %w", provider, err)
	}
	defer resp.Body.Close()
	var res struct {
		Record   tokenstore.Record `json:"record"`
		Terminal bool              `json:"terminal"`
		Error    string            `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode >= 400 || res.Error != "" {
		if res.Terminal {
			return tokenstore.Record{}, &extensionTerminalError{msg: res.Error}
		}
		return tokenstore.Record{}, errors.New(res.Error)
	}
	return res.Record, nil
}

func (c *ExtensionClient) applyHeaders(httpReq *http.Request, provider string, req core.Request) {
	c.applyAuthHeader(httpReq)
	httpReq.Header.Set("Content-Type", req.ContentType)
	httpReq.Header.Set("X-Surface", string(req.Surface))
	httpReq.Header.Set("X-Model", req.Model)
	if req.Credential != nil {
		c.applyCredentialHeaders(httpReq, req.Credential)
	}
}

func (c *ExtensionClient) applyCredentialHeaders(httpReq *http.Request, cred *core.Credential) {
	if cred.Token != "" {
		httpReq.Header.Set("X-Credential-Token", cred.Token)
	}
	if cred.AccountID != "" {
		httpReq.Header.Set("X-Credential-Account-ID", cred.AccountID)
	}
	if cred.TokenType != "" {
		httpReq.Header.Set("X-Credential-Token-Type", cred.TokenType)
	}
	if len(cred.Metadata) > 0 {
		if b, err := json.Marshal(cred.Metadata); err == nil {
			httpReq.Header.Set("X-Credential-Metadata", base64.StdEncoding.EncodeToString(b))
		}
	}
}

func (c *ExtensionClient) applyAuthHeader(httpReq *http.Request) {
	if c.Secret != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Secret)
	}
}

type extensionTerminalError struct {
	msg string
}

func (e *extensionTerminalError) Error() string  { return e.msg }
func (e *extensionTerminalError) Terminal() bool { return true }

func parseExtensionError(status int, body []byte) error {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return &core.ProviderError{
			Message:        parsed.Error.Message,
			Classification: core.ProviderErrorClassification{StatusCode: status},
		}
	}
	return &core.ProviderError{
		Message:        string(body),
		Classification: core.ProviderErrorClassification{StatusCode: status},
	}
}

// OAuthDriver returns an oauthflow.Driver that delegates OAuth calls to the extension.
func (c *ExtensionClient) OAuthDriver(provider string) oauthflow.Driver {
	return &extensionOAuthDriver{client: c, provider: provider}
}

type extensionOAuthDriver struct {
	client   *ExtensionClient
	provider string
}

func (d *extensionOAuthDriver) Start(ctx context.Context, req oauthflow.StartRequest) (oauthflow.Authorization, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/oauth/start", d.client.BaseURL, d.provider)
	reqBody, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	d.client.applyAuthHeader(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := d.client.HTTPClient.Do(httpReq)
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	defer resp.Body.Close()
	var res struct {
		Authorization oauthflow.Authorization `json:"authorization"`
		Error         string                  `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if res.Error != "" {
		return oauthflow.Authorization{}, errors.New(res.Error)
	}
	return res.Authorization, nil
}

func (d *extensionOAuthDriver) Poll(ctx context.Context, flow oauthflow.Flow) (oauthflow.PollResult, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/oauth/poll", d.client.BaseURL, d.provider)
	reqBody, _ := json.Marshal(map[string]any{"flow": flow})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return oauthflow.PollResult{}, err
	}
	d.client.applyAuthHeader(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := d.client.HTTPClient.Do(httpReq)
	if err != nil {
		return oauthflow.PollResult{}, err
	}
	defer resp.Body.Close()
	var res struct {
		Result oauthflow.PollResult `json:"result"`
		Error  string               `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if res.Error != "" {
		return oauthflow.PollResult{}, errors.New(res.Error)
	}
	return res.Result, nil
}

func (d *extensionOAuthDriver) Exchange(ctx context.Context, flow oauthflow.Flow, code string) (tokenstore.Record, error) {
	url := fmt.Sprintf("%s/extension/v1/%s/oauth/exchange", d.client.BaseURL, d.provider)
	reqBody, _ := json.Marshal(map[string]any{"flow": flow, "code": code})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return tokenstore.Record{}, err
	}
	d.client.applyAuthHeader(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := d.client.HTTPClient.Do(httpReq)
	if err != nil {
		return tokenstore.Record{}, err
	}
	defer resp.Body.Close()
	var res struct {
		Record tokenstore.Record `json:"record"`
		Error  string            `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if res.Error != "" {
		return tokenstore.Record{}, errors.New(res.Error)
	}
	return res.Record, nil
}

var (
	_ oauthflow.DeviceDriver = (*extensionOAuthDriver)(nil)
	_ oauthflow.CodeDriver   = (*extensionOAuthDriver)(nil)
)

// ExtensionProviderFacade wraps an extension-served provider for the gateway's router.
type ExtensionProviderFacade struct {
	runtime    *Runtime
	instance   string
	providerID string
	caller     core.Caller
}

func (rt *Runtime) newExtensionFacade(instance, providerID string, caller core.Caller) Provider {
	return &ExtensionProviderFacade{
		runtime:    rt,
		instance:   instance,
		providerID: providerID,
		caller:     caller,
	}
}

func (f *ExtensionProviderFacade) IsStub() bool {
	return false
}

func (f *ExtensionProviderFacade) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	resp, _, err := f.CompleteContextWithObservation(context.Background(), model, messages, kw)
	return resp, err
}

func (f *ExtensionProviderFacade) CompleteWithObservation(model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	return f.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	payload := map[string]any{
		"model":    model,
		"messages": messages,
	}
	for k, v := range kw {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	req := core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       model,
		Body:        raw,
		ContentType: core.ContentTypeJSON,
	}
	ctx, collector := collectCredentials(ctx)
	resp, err := f.runtime.core.Invoke(ctx, f.caller, f.instance, req)
	if err != nil {
		return nil, collector.Observation(), err
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, collector.Observation(), fmt.Errorf("decode response: %w", err)
	}
	return out, collector.Observation(), nil
}

func (f *ExtensionProviderFacade) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return f.StreamContext(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
	}
	for k, v := range kw {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req := core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       model,
		Body:        raw,
		ContentType: core.ContentTypeJSON,
	}
	iter, err := f.runtime.core.Stream(ctx, f.caller, f.instance, req)
	if err != nil {
		return nil, err
	}
	return &relayedStream{inner: iter, prefix: f.instance}, nil
}

func (f *ExtensionProviderFacade) ListModels() []ModelInfo {
	models, _, _ := f.ListModelsWithError()
	return models
}

func (f *ExtensionProviderFacade) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	ctx, collector := collectCredentials(context.Background())
	cred, err := f.runtime.coreCredential(ctx, f.providerID, f.caller, f.instance)
	if err != nil {
		return nil, nil, err
	}
	models, err := f.runtime.extensionClient().ListModels(ctx, f.providerID, cred)
	if err != nil {
		return nil, collector.Observation(), err
	}
	return gatewayRows(models), collector.Observation(), nil
}

func (f *ExtensionProviderFacade) DefaultVoice() string {
	return "en-US-ChristopherNeural"
}

func (f *ExtensionProviderFacade) Synthesize(voice, text, speed string) ([]byte, string, error) {
	resp, err := f.runtime.extensionClient().Invoke(context.Background(), f.providerID, core.Request{
		Surface: core.ModelSurfaceAudioSpeech,
		Model:   voice,
		Body:    []byte(text),
	})
	if err != nil {
		return nil, "", err
	}
	return resp.Body, "audio/mpeg", nil
}
