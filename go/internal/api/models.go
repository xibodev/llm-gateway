package api

import (
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func handleModels(w http.ResponseWriter, r *http.Request) {
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	response, err := buildModelList(principal)
	if err != nil {
		writeError(w, 500, "Model catalog unavailable.")
		return
	}
	writeJSON(w, 200, response)
}

func handleUserModels(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	selected := &config.Principal{PrincipalID: principal.ID, PrincipalKind: principal.Kind}
	if projectID := strings.TrimSpace(r.URL.Query().Get("project_id")); projectID != "" {
		project, found, err := iam.ProjectByID(projectID)
		if err != nil {
			writeError(w, 500, "Identity store unavailable.")
			return
		}
		if !found || project.Status != "active" {
			writeError(w, 404, "unknown active project")
			return
		}
		role, member, err := iam.MembershipRole(project.ID, principal.ID)
		if err != nil {
			writeError(w, 500, "Membership store unavailable.")
			return
		}
		if !member || (role != "owner" && role != "admin") {
			writeError(w, 403, "Project owner or admin membership is required.")
			return
		}
		selected.ProjectID = project.ID
		selected.Project = project.Slug
		selected.Role = role
	}
	response, err := buildModelList(selected)
	if err != nil {
		writeError(w, 500, "Model catalog unavailable.")
		return
	}
	writeJSON(w, 200, response)
}

func handleUserTestProvider(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	providerID := strings.TrimSpace(r.PathValue("id"))
	if _, exists := config.Get().Providers[providerID]; !exists {
		writeError(w, 404, "unknown provider")
		return
	}
	connections, err := iam.ListProviderConnections(principal.ID, providerID)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	allowed := false
	for _, connection := range connections {
		if connection.Status == "active" {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, 403, "An active private provider connection is required.")
		return
	}
	selected := &config.Principal{
		PrincipalID: principal.ID, PrincipalKind: principal.Kind,
	}
	writeJSON(w, 200, runProviderProbe(providerID, "test", selected))
}

func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := selectedCatalogPrincipal(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	response, err := buildModelList(principal)
	if err != nil {
		writeError(w, 500, "Model catalog unavailable.")
		return
	}
	writeJSON(w, 200, response)
}

func buildModelList(principal *config.Principal) (map[string]any, error) {
	data := []any{}
	seen := map[string]bool{}
	s := config.Get()
	projectPolicy := iam.ProjectPolicy{}
	if principal != nil && principal.ProjectID != "" {
		var err error
		projectPolicy, err = iam.GetProjectPolicy(principal.ProjectID)
		if err != nil {
			return nil, err
		}
	}

	for providerID := range s.Providers {
		if !providerAllowed(principal, projectPolicy, providerID) {
			continue
		}
		authorized, err := providers.ProviderCredentialAuthorized(providerID, principal)
		if err != nil {
			return nil, err
		}
		if !authorized {
			continue
		}
		for _, row := range providers.CatalogModelsForPrincipal(providerID, principal) {
			if row.ID == "" {
				continue
			}
			namespaced := providerID + "/" + row.ID
			aliasID, _ := discoveryAliasID(s, row.ID)
			if !modelAllowed(principal, projectPolicy, namespaced, row.ID, aliasID) {
				continue
			}
			if seen[namespaced] {
				continue
			}
			seen[namespaced] = true
			capabilities, surfaces := modelPresentationMetadata(providerID, row)
			entry := map[string]any{"id": namespaced, "object": "model", "owned_by": providerID}
			if row.Label != "" {
				entry["display_name"] = row.Label
			}
			if len(capabilities) > 0 {
				entry["capabilities"] = capabilities
			}
			setSurfaces(entry, surfaces)
			data = append(data, entry)
		}
	}

	for name, cat := range s.Endpoints {
		if !modelAllowed(principal, projectPolicy, name) {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		fo := []any{}
		presentationMembers := []config.EndpointMember{}
		for _, m := range cat.Failover {
			authorized, err := providers.ProviderCredentialAuthorized(m.Provider, principal)
			if err != nil {
				return nil, err
			}
			if authorized && providerAllowed(principal, projectPolicy, m.Provider) &&
				modelAllowed(principal, projectPolicy, name, m.Provider+"/"+m.Model, m.Model) {
				fo = append(fo, map[string]any{"provider": m.Provider, "model": m.Model})
				presentationMembers = append(presentationMembers, m)
			}
		}
		if len(fo) == 0 {
			continue
		}
		// owned_by is "endpoint", not "category": GET /v1/models is a public,
		// client-visible wire format, and live traffic has been observed
		// filtering on this field. This rename is deliberate — it tracks the
		// EndpointConfig/EndpointMember rename elsewhere — not a typo.
		categoryEntry := map[string]any{
			"id": name, "object": "model", "owned_by": "endpoint", "failover": fo,
		}
		if capabilities, surfaces := categoryPresentationMetadata(
			principal, presentationMembers,
		); len(capabilities) > 0 {
			categoryEntry["capabilities"] = capabilities
			setSurfaces(categoryEntry, surfaces)
		}
		data = append(data, categoryEntry)
	}

	// Discovery aliases: also list Claude-family models under their bare id
	// (claude-â€¦ / anthropic-â€¦) so Claude Code's gateway model discovery, which
	// ignores ids not beginning with "claude"/"anthropic", surfaces them. With
	// AnthropicDiscoveryAllModels, non-Claude models are surfaced too under a
	// "claude-<id>" alias (the resolver strips the prefix to route). The bare/
	// aliased id routes via the resolver's native-name normalization. Deduped.
	// Only chat/coding models are aliased â€” embeddings, audio (TTS/STT), and other
	// non-chat models are kept out of Claude Code's /model picker.
	if s.AnthropicDiscoveryAliases {
		for providerID := range s.Providers {
			if !providerAllowed(principal, projectPolicy, providerID) {
				continue
			}
			authorized, err := providers.ProviderCredentialAuthorized(providerID, principal)
			if err != nil {
				return nil, err
			}
			if !authorized {
				continue
			}
			for _, row := range providers.CatalogModelsForPrincipal(providerID, principal) {
				if row.ID == "" || !isChatModel(row) {
					continue
				}
				aliasID, isClaude := discoveryAliasID(s, row.ID)
				if aliasID == "" ||
					!modelAllowed(principal, projectPolicy, providerID+"/"+row.ID, row.ID, aliasID) {
					continue
				}
				if seen[aliasID] {
					continue
				}
				seen[aliasID] = true
				entry := map[string]any{"id": aliasID, "object": "model", "owned_by": providerID}
				switch {
				case row.Label != "":
					entry["display_name"] = row.Label
				case !isClaude:
					entry["display_name"] = row.ID // show the real model name, not the alias
				}
				data = append(data, entry)
			}
		}
	}

	return map[string]any{"object": "list", "data": data}, nil
}

// setSurfaces stores a model row's HTTP surfaces under both the canonical
// "supported_surfaces" key and the pre-rename "supported_endpoints" key, with
// identical values. GET /v1/models is public and client-visible, and a live
// gateway showed supported_endpoints present on only 488 of 5,229 rows
// (9.3%) — consumers already tolerate its absence — so the deprecated key is
// kept for one release to avoid breaking anything still reading it, then
// should be removed.
func setSurfaces(entry map[string]any, surfaces []string) {
	if len(surfaces) == 0 {
		return
	}
	entry["supported_surfaces"] = surfaces
	entry["supported_endpoints"] = surfaces // Deprecated: use supported_surfaces.
}

func discoveryAliasID(s *config.Settings, modelID string) (string, bool) {
	low := strings.ToLower(modelID)
	isClaude := strings.HasPrefix(low, "claude") || strings.HasPrefix(low, "anthropic")
	if isClaude {
		return modelID, true
	}
	if s.AnthropicDiscoveryAllModels {
		return "claude-" + modelID, false
	}
	return "", false
}

func modelPresentationMetadata(
	providerID string, row providers.ModelInfo,
) (map[string]any, []string) {
	capabilities := map[string]any{}
	for key, value := range row.Capabilities {
		capabilities[key] = value
	}
	surfaces := append([]string(nil), row.SupportedSurfaces...)
	hasSurface := func(wanted string) bool {
		for _, surface := range surfaces {
			if strings.EqualFold(strings.TrimSpace(surface), wanted) {
				return true
			}
		}
		return false
	}
	haystack := strings.ToLower(row.ID + " " + row.Label)

	// Embeddings are inferred BEFORE the audio branch and independently of it.
	// A provider that reports nothing (LocalAI returns a bare `{"id": ...}`)
	// otherwise lists its embedding model with no capabilities and no surfaces —
	// which is exactly how `localai/granite-embedding-107m-multilingual` came to
	// be advertised by `GET /v1/models` while no surface would accept it.
	// Note this is a name-shape inference and is therefore not authoritative: a
	// provider that DOES report `embedding` in its capabilities (googleai does,
	// at googleai.go:703) keeps its own answer and only gains the surface.
	if declared, _ := row.Capabilities["embedding"].(bool); declared || looksLikeEmbeddingModel(haystack) {
		capabilities["embedding"] = true
		if !hasSurface("/v1/embeddings") {
			surfaces = append(surfaces, "/v1/embeddings")
		}
		// An embedding model is not a chat model, and claiming both would put it
		// into failover chains that would try to converse with it.
		return capabilities, surfaces
	}

	explicitAudio := false
	for _, key := range []string{
		"transcription", "stt", "asr", "tts", "speech",
	} {
		if _, exists := row.Capabilities[key]; exists {
			explicitAudio = true
		}
	}
	for _, surface := range surfaces {
		if strings.Contains(strings.ToLower(surface), "/audio/") {
			explicitAudio = true
		}
	}
	providerConfig := config.Get().Providers[providerID]
	if explicitAudio || !providerInfersAudioCapabilities(providerID, providerConfig) {
		return capabilities, surfaces
	}
	switch {
	case strings.Contains(haystack, "whisper") ||
		strings.Contains(haystack, "transcription") ||
		strings.Contains(haystack, "speech-to-text") ||
		strings.Contains(haystack, "-stt") ||
		strings.Contains(haystack, "-asr"):
		capabilities["transcription"] = true
		if !hasSurface("/v1/audio/transcriptions") {
			surfaces = append(surfaces, "/v1/audio/transcriptions")
		}
	case strings.Contains(haystack, "piper") ||
		strings.Contains(haystack, "vits") ||
		strings.Contains(haystack, "text-to-speech") ||
		strings.Contains(haystack, "-tts"):
		capabilities["tts"] = true
		if !hasSurface("/v1/audio/speech") {
			surfaces = append(surfaces, "/v1/audio/speech")
		}
	}
	return capabilities, surfaces
}

func providerInfersAudioCapabilities(
	providerID string, providerConfig *config.ProviderConfig,
) bool {
	if providerConfig == nil {
		return false
	}
	registryID := providers.EffectiveRegistryID(
		providerID, providerConfig.RegistryID, providerConfig.Type,
	)
	entry, found := providers.RegistryProviderByID(registryID)
	return found && entry.InferAudioCapabilities &&
		providers.RegistryRuntimeMatches(entry, providerConfig.Type)
}

func categoryPresentationMetadata(
	principal *config.Principal,
	members []config.EndpointMember,
) (map[string]any, []string) {
	modalities := make([]map[string]bool, 0, len(members))
	for _, member := range members {
		row, found := providers.CatalogLookupForPrincipal(
			member.Provider, member.Model, principal,
		)
		if !found {
			return nil, nil
		}
		capabilities, surfaces := modelPresentationMetadata(member.Provider, row)
		current := modalitySet(capabilities, surfaces)
		if len(current) == 0 {
			return nil, nil
		}
		modalities = append(modalities, current)
	}
	return commonCategoryPresentationMetadata(modalities)
}

func commonCategoryPresentationMetadata(
	modalities []map[string]bool,
) (map[string]any, []string) {
	if len(modalities) == 0 {
		return nil, nil
	}
	shared := map[string]bool{}
	for capability, enabled := range modalities[0] {
		shared[capability] = enabled
	}
	for _, current := range modalities[1:] {
		for capability := range shared {
			if !current[capability] {
				delete(shared, capability)
			}
		}
	}
	for _, modality := range []string{"transcription", "tts", "image", "video", "embedding"} {
		if shared[modality] {
			surface := map[string]string{
				"transcription": "/v1/audio/transcriptions",
				"tts":           "/v1/audio/speech",
				"image":         "/v1/images/generations",
				"video":         "/v1/videos/generations",
				"embedding":     "/v1/embeddings",
			}[modality]
			return map[string]any{modality: true}, []string{surface}
		}
	}
	return nil, nil
}

func modalitySet(
	capabilities map[string]any, surfaces []string,
) map[string]bool {
	out := map[string]bool{}
	for key, value := range capabilities {
		if enabled, ok := value.(bool); ok && !enabled {
			continue
		}
		switch strings.ToLower(key) {
		case "transcription", "stt", "asr":
			out["transcription"] = true
		case "tts", "speech":
			out["tts"] = true
		case "image":
			out["image"] = true
		case "video":
			out["video"] = true
		case "embedding", "embeddings":
			out["embedding"] = true
		}
	}
	for _, surface := range surfaces {
		switch {
		case strings.Contains(surface, "/audio/transcriptions"):
			out["transcription"] = true
		case strings.Contains(surface, "/audio/speech"):
			out["tts"] = true
		case strings.Contains(surface, "/images/generations"):
			out["image"] = true
		case strings.Contains(surface, "/videos/generations"):
			out["video"] = true
		case strings.Contains(surface, "/embeddings"):
			out["embedding"] = true
		}
	}
	return out
}

func providerAllowed(
	principal *config.Principal, project iam.ProjectPolicy, providerID string,
) bool {
	if principal == nil {
		return true
	}
	if len(principal.AllowedProviders) > 0 &&
		!containsStr(principal.AllowedProviders, providerID) {
		return false
	}
	return len(project.AllowedProviders) == 0 ||
		containsStr(project.AllowedProviders, providerID)
}

func modelAllowed(
	principal *config.Principal, project iam.ProjectPolicy, candidates ...string,
) bool {
	if principal == nil {
		return true
	}
	matches := func(allowed []string) bool {
		if len(allowed) == 0 {
			return true
		}
		for _, candidate := range candidates {
			if candidate != "" && containsStr(allowed, candidate) {
				return true
			}
		}
		return false
	}
	return matches(principal.AllowedModels) && matches(project.AllowedModels)
}

// chatSurfaces are the OpenAI/Anthropic chat surfaces; a model that declares any
// of them is conversational (as opposed to an embedding or audio model).
var chatSurfaces = map[string]bool{
	"/chat/completions": true, "/v1/chat/completions": true,
	"/messages": true, "/v1/messages": true,
	"/responses": true, "/v1/responses": true, "ws:/responses": true,
}

// chatCapabilities are capability keys only conversational models report; an
// embedding advertises just "family", and local audio (TTS/STT) models none.
var chatCapabilities = []string{
	"context_window", "max_output_tokens", "tool_calls", "streaming",
	"vision", "reasoning_effort", "structured_outputs",
}

// nonChatNameHints mark non-conversational utility models â€” embeddings, audio
// (TTS/STT), rerankers, and internal agent-utility models â€” that can otherwise
// carry chat-like metadata (e.g. Copilot's "trajectory-compaction" advertises a
// context window and tool_calls). Matched against the id and the "family"
// capability. Kept deliberately specific so real coding models never match.
var nonChatNameHints = []string{
	"embedding", "whisper", "piper", "vits", "-tts", "-stt", "-asr",
	"rerank", "trajectory", "compaction",
}

// isChatModel reports whether a catalog model is a chat/coding model (vs an
// embedding, TTS/STT, reranker, or other non-chat model). It keeps Claude Code's
// /model picker free of non-chat noise. A model qualifies if it declares a chat
// surface or a conversational capability AND is not a known utility model;
// models with no such metadata (e.g. local audio/embedding models that expose
// only an id) are excluded.
func isChatModel(row providers.ModelInfo) bool {
	hay := strings.ToLower(row.ID)
	if fam, ok := row.Capabilities["family"].(string); ok {
		hay += " " + strings.ToLower(fam)
	}
	for _, h := range nonChatNameHints {
		if strings.Contains(hay, h) {
			return false
		}
	}
	for _, surface := range row.SupportedSurfaces {
		if chatSurfaces[strings.TrimSpace(strings.ToLower(surface))] {
			return true
		}
	}
	for _, c := range chatCapabilities {
		if _, ok := row.Capabilities[c]; ok {
			return true
		}
	}
	return false
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status": "ok", "service": "llm-gateway", "version": config.Version,
	})
}
