package providers

// AzureOpenAIProvider speaks the OpenAI chat-completions wire format against
// one Azure OpenAI resource. It is its own provider type — not a case of
// openai_compatible — because Azure diverges in ways measured against a live
// resource, not assumed from vendor docs:
//
//   - Auth travels as the `api-key` header, never `Authorization: Bearer`.
//     This is the main break from every other OpenAI-compatible provider.
//   - There is no vendor catalogue worth trusting. GET <endpoint>/models on a
//     live resource returned 410 rows against 1 real deployment. Every one of
//     the 410 reported status "succeeded" (status cannot filter them), and
//     their lifecycle split 181 deprecated / 37 preview / 167 GA (lifecycle
//     cannot filter them either) — there is no field left in that response to
//     recover the truth from. The catalog is read from the resource's own
//     deployments instead, which is the only thing the resource can actually
//     serve, and /models is never requested here, on success or on failure.
//   - Deployment listing needs a legacy, pinned api-version
//     (2023-03-15-preview). Every newer api-version 404s on that route —
//     Microsoft moved deployment listing to the ARM control plane, which
//     needs Entra credentials and subscription scope this provider type
//     deliberately does not take.
//   - Responses carry Azure-only fields (prompt_filter_results, service_tier,
//     and usage.latency_checkpoint) that would break a decoder built around a
//     fixed struct. Everything here decodes into a plain map instead, so
//     unknown fields simply pass through.
//   - The request `model` is the deployment name (e.g. "gpt-5.6-sol"); the
//     response `model` is version-stamped (e.g. "gpt-5.6-sol-2026-07-09").
//     They are different identifiers: the response value is reported exactly
//     as upstream sent it and is never written back into a request or used
//     as a catalog key.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"llmgw/internal/iam"
)

// azureDeploymentsAPIVersion is pinned to the last api-version observed to
// serve GET /openai/deployments. Every api-version after this one 404s on
// that route (deployment listing moved to the ARM control plane, which needs
// Entra credentials + subscription scope this provider type does not take).
// Do not bump this "to the latest" — there is no newer value that works.
const azureDeploymentsAPIVersion = "2023-03-15-preview"

// AzureOpenAIProvider talks to one Azure OpenAI resource.
type AzureOpenAIProvider struct {
	// BaseURL is the full inference endpoint, already carrying /openai/v1
	// (Azure's own "Endpoint" value plus that suffix). Chat completions POST
	// here as-is — no api-version query parameter. The deployments catalog
	// route is derived from this value (scheme+host only), never configured
	// separately.
	BaseURL string
	APIKey  string
	Timeout float64

	observation *iam.ProviderAccountObservation
}

func (AzureOpenAIProvider) IsStub() bool { return false }

func (p AzureOpenAIProvider) headers() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	// api-key, not Authorization: Bearer — the main way Azure diverges from
	// every other OpenAI-compatible provider.
	h.Set("api-key", p.APIKey)
	return h
}

func (p AzureOpenAIProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteWithObservation(model, messages, kw)
	return response, err
}

func (p AzureOpenAIProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	kw = withOpenAIOutputLimit(kw)
	// model here is the deployment name the caller asked for. It is only
	// ever used as the outbound request id — never replaced by, or
	// reconciled with, whatever the response reports back in "model".
	body, _ := json.Marshal(buildOpenAIPayload(model, messages, false, kw))
	// A base_url that cannot be built into a request is a configuration fault,
	// not a transport one: discarding the error here dereferenced a nil request
	// on the very next line.
	req, err := http.NewRequest("POST", p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, p.observation, &ConfigError{Msg: "azure_openai: base_url is not a usable request URL"}
	}
	req.Header = p.headers()
	resp, err := httpClient(p.Timeout).Do(req)
	if err != nil {
		return nil, p.observation, invocation("azure_openai: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, p.observation, invocationStatus(
			fmt.Sprintf("azure_openai: upstream returned %d: %s", resp.StatusCode, redact(extractError(raw))),
			resp.StatusCode,
		)
	}
	// Decoded into a plain map, not a fixed struct, so Azure-only fields
	// (prompt_filter_results, service_tier, usage.latency_checkpoint) pass
	// through untouched instead of tripping a strict decoder. Whatever
	// upstream reports in "model" is returned exactly as sent — it is the
	// version-stamped id, not the deployment name that was requested, and
	// nothing here rewrites it.
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, p.observation, invocation("azure_openai: invalid JSON in upstream response")
	}
	return out, p.observation, nil
}

func (p AzureOpenAIProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	kw = withOpenAIOutputLimit(kw)
	body, _ := json.Marshal(buildOpenAIPayload(model, messages, true, kw))
	req, err := http.NewRequest("POST", p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &ConfigError{Msg: "azure_openai: base_url is not a usable request URL"}
	}
	req.Header = p.headers()
	resp, err := httpClient(p.Timeout).Do(req)
	if err != nil {
		return nil, invocation("azure_openai: streaming transport error: " + err.Error())
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, invocationStatus(
			fmt.Sprintf("azure_openai: upstream returned %d: %s", resp.StatusCode, redact(extractError(raw))),
			resp.StatusCode,
		)
	}
	return newHTTPStreamIter(resp, "azure_openai"), nil
}

// ---- catalog ------------------------------------------------------------- //

func (p AzureOpenAIProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the resource's own deployments — the only thing
// this provider type will ever report as its catalog. GET <endpoint>/models
// is Azure's entire vendor model catalogue, not this resource's, and there is
// no field in that response that recovers which models the resource can
// actually serve (see the package doc comment for the measurements). It is
// therefore never requested here, whether the deployments call succeeds or
// fails.
//
// A rejected credential is reported as catalog_authentication_failed, as the
// other catalogs in this package do; catalog_not_discoverable is kept for the
// deployments route being unusable — unreachable, underivable, or answering
// with something this provider cannot read.
//
// Pagination on this route has been measured, not assumed: queried directly
// against a real resource at the pinned api-version, it returned
// {"data": [...], "object": "list"} for its one deployment. nextLink,
// next_link, @odata.nextLink, next, has_more, first_id, and last_id were all
// explicitly checked for and were all absent, and the shape did not change
// when a paging query parameter was added to the request. That is the flat
// OpenAI-style list envelope, not the Azure/OData {value, nextLink} shape an
// earlier review assumed — OData paging belongs to the ARM management-plane
// deployments API, a different route that needs Entra credentials and
// subscription scope this provider deliberately does not take.
//
// The walk below is therefore defensive, not exercised against a real
// paginated response: if a future api-version starts paging this route, the
// envelope's own family is far more likely to follow the convention OpenAI's
// other list endpoints use — has_more plus an "after" cursor — than to switch
// to an OData nextLink, so that is what is checked, never nextLink. The next
// page is always requested against the already-validated resource origin
// with the cursor carried as a plain query value, never by parsing a URL out
// of the response body (azureNextDeploymentsPage), and that request still
// clears azureSameOriginPage anyway: the check is cheap, and it keeps the
// api-key header from ever being redirected by upstream data even if that
// assumption changes later.
func (p AzureOpenAIProvider) ListModelsWithError() ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	origin, err := azureResourceOrigin(p.BaseURL)
	if err != nil {
		return nil, p.observation, catalogError(
			"catalog_not_discoverable",
			"Azure OpenAI base_url must be an absolute endpoint URL to derive the deployments route.",
			0,
		)
	}
	pageURL := origin + "/openai/deployments?api-version=" + azureDeploymentsAPIVersion
	out := []ModelInfo{}
	seenCursors := map[string]bool{}
	for page := 0; ; page++ {
		if page >= azureDeploymentsMaxPages {
			return nil, p.observation, catalogError(
				"catalog_not_discoverable",
				"Azure OpenAI deployments listing did not end within the page limit; "+
					"the catalog is not discoverable.",
				0,
			)
		}
		decoded, failure := p.azureDeploymentsPage(pageURL)
		if failure != nil {
			return nil, p.observation, failure
		}
		out = append(out, azureDeploymentRows(decoded)...)
		hasMore, _ := decoded["has_more"].(bool)
		if !hasMore {
			break
		}
		cursor := azureDeploymentsCursor(decoded)
		if cursor == "" || seenCursors[cursor] {
			// has_more with no usable cursor (or one that repeats) cannot be
			// followed safely. Returning the pages gathered so far would be
			// the same silent truncation this handling exists to avoid, so
			// it is reported as a failure like any other reason the listing
			// could not be completed.
			return nil, p.observation, catalogError(
				"catalog_not_discoverable",
				"Azure OpenAI deployments listing reported more results but gave no usable "+
					"cursor to continue from; the catalog is not discoverable.",
				0,
			)
		}
		seenCursors[cursor] = true
		next, ok := azureNextDeploymentsPage(origin, azureDeploymentsAPIVersion, cursor)
		if !ok {
			// Not expected to be reachable today: the URL is built from the
			// already-validated origin, never from the response body. Kept as
			// a hard failure rather than an assumption so a future change
			// that starts trusting a response-supplied URL here fails safely
			// instead of silently.
			return nil, p.observation, catalogError(
				"catalog_not_discoverable",
				"Azure OpenAI deployments continuation left the configured resource; "+
					"the catalog is not discoverable.",
				0,
			)
		}
		pageURL = next
	}
	return out, p.observation, nil
}

// azureDeploymentsCursor reads the cursor a has_more:true page would carry
// under the OpenAI list convention this envelope belongs to: a dedicated
// last_id field if present, otherwise the id of the last row in data. Neither
// has been observed on this route — see the ListModelsWithError doc comment
// for the measurement — so this only ever runs against a future response
// that sets has_more.
func azureDeploymentsCursor(decoded map[string]any) string {
	if lastID, ok := decoded["last_id"].(string); ok && strings.TrimSpace(lastID) != "" {
		return strings.TrimSpace(lastID)
	}
	items, _ := decoded["data"].([]any)
	if len(items) == 0 {
		return ""
	}
	last, ok := items[len(items)-1].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := last["id"].(string)
	return strings.TrimSpace(id)
}

// azureDeploymentsMaxPages bounds the has_more/cursor walk (see
// ListModelsWithError — this path is defensive and has not been exercised
// against a real paginated response). seenCursors already breaks a walk that
// repeats a cursor, but a resource that kept minting new ones could otherwise
// continue forever; a resource's deployments run to tens of rows, so this
// ceiling sits far above any real catalog and hitting it is reported as an
// unreadable response rather than quietly truncated.
const azureDeploymentsMaxPages = 50

// azureNextDeploymentsPage builds the request for one more page of the
// deployments listing, keyed on has_more plus a cursor rather than on
// nextLink — see the ListModelsWithError doc comment for why. The URL is
// always built from the already-validated resource origin and the pinned
// api-version; only the cursor's value comes from the response, and it is
// carried strictly as a query parameter, never parsed as a URL, so nothing
// the resource sends back can choose which host the next api-key request
// goes to. azureSameOriginPage still checks the result anyway: the check is
// cheap, and it keeps holding even if this function is later changed to
// resolve a server-supplied URL instead of building one locally.
func azureNextDeploymentsPage(origin, apiVersion, cursor string) (string, bool) {
	candidate := origin + "/openai/deployments?api-version=" + url.QueryEscape(apiVersion) +
		"&after=" + url.QueryEscape(cursor)
	return azureSameOriginPage(origin, origin, candidate)
}

// azureSameOriginPage parses next (absolute, or resolved against current) and
// checks it resolves to origin with no embedded userinfo, refusing anything
// that doesn't.
//
// Any URL that will carry the api-key header must clear this first: the host
// must come from the operator's own configuration, never from data an
// upstream response supplied, regardless of whether that URL was parsed out
// of the response (as an OData nextLink would be) or, as
// azureNextDeploymentsPage does today, built locally from an
// already-validated origin. The api-key header travels on every page
// request, so the destination is not a choice a credential's caller can leave
// to response data. Embedded userinfo is refused for the same reason: it is a
// second credential this gateway never configured.
func azureSameOriginPage(origin, current, next string) (string, bool) {
	base, err := url.Parse(current)
	if err != nil {
		return "", false
	}
	resolved, err := base.Parse(strings.TrimSpace(next))
	if err != nil || resolved.User != nil {
		return "", false
	}
	if !strings.EqualFold(resolved.Scheme+"://"+resolved.Host, origin) {
		return "", false
	}
	return resolved.String(), true
}

// azureDeploymentsPage fetches and decodes one page of the deployments
// listing. Every failure it can see is a catalog failure, reported with the
// same codes the single-page version used.
func (p AzureOpenAIProvider) azureDeploymentsPage(pageURL string) (map[string]any, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, catalogError(
			"catalog_not_discoverable",
			"Azure OpenAI base_url could not be built into a deployments request.",
			0,
		)
	}
	req.Header = p.headers()
	timeout := p.Timeout
	if timeout > 10 {
		timeout = 10
	}
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil, catalogError(
			"catalog_not_discoverable",
			"Azure OpenAI deployments could not be reached; the catalog is not discoverable.",
			0,
		)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// The route answered and refused this credential, so the catalog is
		// not undiscoverable — the key was rejected. Saying "not discoverable"
		// both hands the operator the wrong diagnosis and keeps the failure out
		// of account health, which only reacts to the codes listed in
		// api.catalogFailureAffectsAccount, and that list has no entry for a
		// discovery failure precisely because no credential can clear one.
		if resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden {
			return nil, catalogError(
				"catalog_authentication_failed",
				"Provider credential was rejected by the catalog API.",
				resp.StatusCode,
			)
		}
		// Deliberately not falling back to /models here: that route answers
		// with the vendor's entire catalogue, not this resource's
		// deployments, and reporting it would be silently wrong rather than
		// merely degraded.
		return nil, catalogError(
			"catalog_not_discoverable",
			fmt.Sprintf("Azure OpenAI deployments request returned HTTP %d; the catalog is not discoverable.", resp.StatusCode),
			resp.StatusCode,
		)
	}
	decoded, err := decodeJSON(resp.Body)
	if err != nil {
		return nil, catalogError(
			"catalog_not_discoverable",
			"Azure OpenAI deployments response was not valid JSON; the catalog is not discoverable.",
			resp.StatusCode,
		)
	}
	return decoded, nil
}

// azureDeploymentRows turns one decoded deployments page into catalog rows.
func azureDeploymentRows(decoded map[string]any) []ModelInfo {
	items, _ := decoded["data"].([]any)
	out := make([]ModelInfo, 0, len(items))
	for _, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		// Only a succeeded deployment can actually serve a request; other
		// states (creating, failed, deleting) are not yet callable.
		if status, ok := m["status"].(string); ok && status != "" && status != "succeeded" {
			continue
		}
		// The deployment's underlying base model (e.g. "gpt-4o") is
		// informative but is never the id: the id callers must send as
		// `model` is the deployment name, not the base model name.
		base, _ := m["model"].(string)
		if !azureDeploymentIsChatCallable(base, id) {
			continue
		}
		row := ModelInfo{
			ID: id, Vendor: "azure-openai",
			// Declared, never left to convention: api/models.go passes an empty
			// capability map through untouched and the console then reads a row
			// that declares no modality as a chat model, so silence here is not
			// a neutral answer.
			Capabilities:      map[string]any{"chat": true},
			SupportedSurfaces: []string{"/v1/chat/completions", "/v1/messages"},
		}
		if base != "" && base != id {
			row.Label = base
		}
		out = append(out, row)
	}
	return out
}

// azureDeploymentIsChatCallable decides whether one deployment belongs in this
// provider's catalog at all.
//
// An Azure OpenAI resource holds embedding, image, audio and legacy completion
// deployments alongside chat ones, and every one of them reports status
// "succeeded". This provider implements /chat/completions and nothing else, so
// listing the rest advertised models it cannot serve — the same defect as a
// hardcoded model list, arrived at from the opposite direction.
//
// Excluded rather than listed with their real capability, because a row this
// gateway cannot route anywhere is not better than a missing row: embeddings
// and audio reach an upstream only through providers.ProviderHTTPTarget, which
// serves OpenAIProvider alone, and an Azure resource is not one. Exclusion also
// costs nothing at call time — `<provider>/<deployment>` resolves without
// consulting the catalog (router.ResolveForPrincipal), so an operator who knows
// a deployment name can still reach it.
//
// The classification reads the base model the deployment was created from
// ("model" in the deployments response, e.g. "text-embedding-3-large") and only
// falls back to the deployment name ("id") when that field is absent, since the
// deployment name is operator-chosen and says nothing reliable. An
// unrecognised base model is excluded for the same reason the capability is
// declared explicitly above: "no capability" is read as chat downstream, so
// there is no way to list something noncommittally.
func azureDeploymentIsChatCallable(baseModel, deploymentID string) bool {
	name := strings.ToLower(strings.TrimSpace(baseModel))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(deploymentID))
	}
	// Checked before the chat families below, because every one of these ids
	// also carries a chat-family prefix: gpt-image-1, gpt-4o-transcribe,
	// gpt-4o-mini-tts, gpt-4o-realtime-preview, gpt-35-turbo-instruct.
	// "audio" is deliberately NOT here: gpt-4o-audio-preview is served over
	// chat completions, only realtime is a different transport.
	for _, nonChat := range []string{
		"embedding", "dall-e", "image", "whisper", "tts", "transcribe",
		"realtime", "sora", "-instruct", "moderation",
	} {
		if strings.Contains(name, nonChat) {
			return false
		}
	}
	for _, chat := range []string{"gpt-", "chatgpt", "o1", "o3", "o4", "codex", "model-router"} {
		if strings.HasPrefix(name, chat) {
			return true
		}
	}
	return strings.Contains(name, "chat")
}

// catalog is the terse spelling tests reach for, matching the other providers
// in this package.
func (p AzureOpenAIProvider) catalog() ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	return p.ListModelsWithError()
}

// azureResourceOrigin strips everything but scheme+host from the configured
// endpoint. The configured base_url carries /openai/v1 (used as-is for chat
// completions); the deployments route lives directly under the resource
// origin instead, so the path is discarded rather than assumed to match.
func azureResourceOrigin(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("base_url is not an absolute URL")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// azureInferenceSuffix is the path chat completions hang off. Azure's own
// "Endpoint" value is the bare origin, so this suffix is what the operator is
// expected to append by hand — and what azureInferenceBaseURL appends for
// them when they do not.
const azureInferenceSuffix = "/openai/v1"

// azureInferenceBaseURL normalises a configured base_url into the full
// inference endpoint, and rejects anything that cannot be normalised.
//
// The two consumers of base_url disagreed about what it means, and only one of
// them said so. The catalog calls azureResourceOrigin, which discards the path
// entirely, so deployments listed correctly from ANY URL of the resource;
// inference appends "/chat/completions" verbatim, so the same value had to
// already carry /openai/v1. An operator who pasted the resource endpoint
// straight from the Azure portal — an origin, no path — therefore completed
// setup, watched the catalog populate with the real deployment names, and got
// 404 Resource not found on every completion, because the request went to
// <origin>/chat/completions, which is not a route Azure serves. Reconciling
// the two here means the disagreement is settled once at config time instead
// of surfacing as an upstream 404 at request time.
func azureInferenceBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf(
			"azure_openai requires base_url: the resource endpoint, optionally with the %s suffix",
			azureInferenceSuffix,
		)
	}
	parsed, err := url.Parse(trimmed)
	// The scheme is held to http(s), the same rule validateRegistryURL applies
	// to the registry's own URLs. Any other absolute scheme parses cleanly and
	// so cleared setup, then failed every catalog and completion request with
	// an unsupported-protocol transport error that names nothing an operator
	// can connect back to base_url.
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf(
			"azure_openai base_url must be an absolute http(s) endpoint URL with no query or "+
				"fragment, optionally with the %s suffix",
			azureInferenceSuffix,
		)
	}
	// Matched exactly, not by suffix. A suffix test also accepts a proxy mount
	// such as /proxy/openai/v1, and this provider cannot serve one: catalog
	// discovery calls azureResourceOrigin, which keeps scheme+host and DISCARDS
	// the path, then requests /openai/deployments from the host root. So a
	// prefixed base_url passed validation, inference worked through the prefix,
	// and catalog sync went to a route the proxy does not mount — the same
	// split between the two consumers this function exists to settle, moved one
	// level down. base_url documents a resource endpoint here; a proxy mount is
	// a different shape that would need the discovery route derived from the
	// configured path instead.
	path := strings.TrimRight(parsed.Path, "/")
	switch path {
	case "":
		path = azureInferenceSuffix
	case azureInferenceSuffix:
		// Already the full contract.
	case "/openai":
		path = azureInferenceSuffix
	default:
		// Deliberately not repaired: the legacy deployment-scoped route
		// (/openai/deployments/<name>/chat/completions) and an arbitrary
		// mount path are different endpoints, not a missing suffix, and
		// guessing which one was meant is how the 404 above happened.
		return "", fmt.Errorf(
			"azure_openai base_url path %q is not a resource endpoint; expected no path, %s, or %s",
			path, "/openai", azureInferenceSuffix,
		)
	}
	parsed.Path = path
	return parsed.String(), nil
}
