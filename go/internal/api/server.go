package api

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/web"
)

// pathAliases rewrites bare CLI paths onto their /v1 equivalents so tools that
// append to a base URL without /v1 still hit the right handler.
var pathAliases = map[string]string{
	"/chat/completions":     "/v1/chat/completions",
	"/responses":            "/v1/responses",
	"/completions":          "/v1/chat/completions",
	"/models":               "/v1/models",
	"/messages":             "/v1/messages",
	"/audio/transcriptions": "/v1/audio/transcriptions",
	"/audio/speech":         "/v1/audio/speech",
	"/embeddings":           "/v1/embeddings",
	"/images/generations":   "/v1/images/generations",
	"/videos/generations":   "/v1/videos/generations",
}

// NewServer builds the http.Handler with all routes registered.
func NewServer() http.Handler {
	mux := http.NewServeMux()

	// health (no auth)
	mux.HandleFunc("GET /health", handleHealth)

	// OpenAI facade
	mux.HandleFunc("POST /v1/chat/completions", handleChat)
	mux.HandleFunc("POST /v1/responses", handleResponses)
	mux.HandleFunc("POST /v1/embeddings", handleEmbeddings)
	mux.HandleFunc("POST /v1/audio/transcriptions", handleTranscriptions)
	mux.HandleFunc("POST /v1/audio/speech", handleSpeech)
	mux.HandleFunc("POST /v1/images/generations", handleImageGenerations)
	mux.HandleFunc("POST /v1/videos/generations", handleVideoGenerations)
	mux.HandleFunc("GET /v1/models", handleModels)

	// Anthropic facade
	mux.HandleFunc("POST /v1/messages", handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", handleCountTokens)

	// The local console is the default operational surface. Legacy pages stay
	// available at explicit paths while teams complete their migration.
	redirectConsole := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console", http.StatusFound)
	}
	mux.HandleFunc("GET /{$}", redirectConsole)
	mux.HandleFunc("GET /admin", redirectConsole)
	mux.HandleFunc("GET /admin/{$}", redirectConsole)

	serveConsole := func(w http.ResponseWriter, r *http.Request) {
		asset := strings.TrimPrefix(r.URL.Path, "/console/")
		if asset != "" && r.URL.Path != "/console" {
			if data, err := web.ConsoleAsset(asset); err == nil {
				http.ServeContent(w, r, asset, time.Time{}, bytes.NewReader(data))
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(web.ConsoleIndex())
	}
	mux.HandleFunc("GET /console", serveConsole)
	mux.HandleFunc("GET /console/{$}", serveConsole)
	mux.HandleFunc("GET /console/{path...}", serveConsole)

	serveLegacyAdmin := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(web.AdminHTML))
	}
	mux.HandleFunc("GET /admin-legacy", serveLegacyAdmin)
	mux.HandleFunc("GET /admin-legacy/{$}", serveLegacyAdmin)

	// Portal mode serves the same local bundle. Its client selects only user API
	// routes from the location, keeping the admin API boundary on the server.
	servePortal := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(web.ConsoleIndex())
	}
	mux.HandleFunc("GET /portal", servePortal)
	mux.HandleFunc("GET /portal/{$}", servePortal)
	serveLegacyPortal := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(web.PortalHTML))
	}
	mux.HandleFunc("GET /portal-legacy", serveLegacyPortal)
	mux.HandleFunc("GET /portal-legacy/{$}", serveLegacyPortal)
	mux.HandleFunc("GET /user/api/me", handleUserMe)
	mux.HandleFunc("GET /user/api/usage", handleUserUsage)
	mux.HandleFunc("GET /user/api/models", handleUserModels)
	mux.HandleFunc("POST /user/api/providers/{id}/test", handleUserTestProvider)
	mux.HandleFunc("POST /user/api/keys", handleUserCreateKey)
	mux.HandleFunc("DELETE /user/api/keys/{id}", handleUserRevokeKey)
	mux.HandleFunc("POST /user/api/keys/{id}/update", handleUserUpdateKey)
	mux.HandleFunc("GET /user/api/audit", handleUserAudit)
	mux.HandleFunc("GET /user/api/connections", handleUserConnections)
	mux.HandleFunc("POST /user/api/connections", handleUserCreateConnection)
	mux.HandleFunc("DELETE /user/api/connections/{connection_id}", handleUserRevokeConnection)
	mux.HandleFunc("POST /user/api/connections/{provider_id}/oauth/start", handleUserOAuthStart)
	mux.HandleFunc("POST /user/api/connections/{provider_id}/oauth/poll", handleUserOAuthPoll)
	mux.HandleFunc("POST /user/api/connections/{provider_id}/oauth/refresh", handleUserOAuthRefresh)
	mux.HandleFunc("POST /user/api/connections/{provider_id}/oauth/revoke", handleUserOAuthRevoke)
	mux.HandleFunc("POST /user/api/playground", handleUserPlayground)
	mux.HandleFunc("POST /user/api/copilot/login/start", handleUserCopilotLoginStart)
	mux.HandleFunc("POST /user/api/copilot/login/poll", handleUserCopilotLoginPoll)
	mux.HandleFunc("DELETE /user/api/copilot", handleUserCopilotRevoke)
	mux.HandleFunc("GET /admin/api/state", handleState)
	mux.HandleFunc("POST /admin/api/playground", handleAdminPlayground)
	mux.HandleFunc("POST /admin/api/playground/speech", handleAdminPlaygroundSpeech)
	mux.HandleFunc("POST /admin/api/playground/transcription", handleAdminPlaygroundTranscription)
	mux.HandleFunc("POST /admin/api/playground/image", handleAdminPlaygroundImage)
	mux.HandleFunc("POST /admin/api/playground/video", handleAdminPlaygroundVideo)
	mux.HandleFunc("GET /admin/api/models", handleAdminModels)
	mux.HandleFunc("POST /admin/api/providers", handleUpsertProvider)
	mux.HandleFunc("DELETE /admin/api/providers/{id}", handleDeleteProvider)
	mux.HandleFunc("POST /admin/api/providers/{id}/models", handleProviderModels)
	mux.HandleFunc("GET /admin/api/providers/{id}/catalog", handleProviderCatalog)
	mux.HandleFunc("POST /admin/api/providers/{id}/refresh", handleRefreshProvider)
	mux.HandleFunc("POST /admin/api/providers/{id}/test", handleTestProvider)
	mux.HandleFunc("POST /admin/api/providers/{id}/repair", handleRepairProvider)
	mux.HandleFunc("POST /admin/api/providers/{id}/verify", handleVerifyProvider)
	mux.HandleFunc("POST /admin/api/providers/{id}/enabled", handleProviderEnabled)
	mux.HandleFunc("POST /admin/api/endpoints", handleUpsertEndpoint)
	mux.HandleFunc("DELETE /admin/api/endpoints/{name}", handleDeleteEndpoint)
	// Deprecated: the routing layer is called an endpoint now. Retained until
	// client evidence and a documented release boundary make removal safe.
	mux.HandleFunc("POST /admin/api/categories", handleUpsertEndpoint)
	mux.HandleFunc("DELETE /admin/api/categories/{name}", handleDeleteEndpoint)
	mux.HandleFunc("POST /admin/api/keys", handleCreateKey)
	mux.HandleFunc("POST /admin/api/keys/update", handleUpdateKey)
	mux.HandleFunc("DELETE /admin/api/keys", handleRevokeKey)
	mux.HandleFunc("GET /admin/api/principals", handleListPrincipals)
	mux.HandleFunc("POST /admin/api/principals", handleCreatePrincipal)
	mux.HandleFunc("POST /admin/api/principals/status", handlePrincipalStatus)
	mux.HandleFunc("GET /admin/api/projects", handleListProjects)
	mux.HandleFunc("POST /admin/api/projects", handleCreateProject)
	mux.HandleFunc("POST /admin/api/projects/status", handleProjectStatus)
	mux.HandleFunc("GET /admin/api/projects/{id}/policy", handleGetProjectPolicy)
	mux.HandleFunc("POST /admin/api/projects/{id}/policy", handleSetProjectPolicy)
	mux.HandleFunc("GET /admin/api/projects/{id}/provider-credential-bindings", handleListProviderCredentialBindings)
	mux.HandleFunc("POST /admin/api/projects/{id}/provider-credential-bindings", handleSetProviderCredentialBinding)
	mux.HandleFunc("POST /admin/api/projects/{id}/provider-credential-bindings/status", handleProviderCredentialBindingStatus)
	mux.HandleFunc("POST /admin/api/providers/{id}/shared-credential/import", handleImportSharedProviderCredential)
	mux.HandleFunc("POST /admin/api/provider-credentials/status", handleProviderCredentialStatus)
	mux.HandleFunc("GET /admin/api/memberships", handleListMemberships)
	mux.HandleFunc("POST /admin/api/memberships", handleSetMembership)
	mux.HandleFunc("DELETE /admin/api/memberships", handleDeleteMembership)
	mux.HandleFunc("GET /admin/api/principals/{id}/credentials", handleListPrincipalCredentials)
	mux.HandleFunc("GET /admin/api/principals/{id}/connections", handleListPrincipalConnections)
	mux.HandleFunc("POST /admin/api/principals/{id}/connections", handleCreatePrincipalConnection)
	mux.HandleFunc("DELETE /admin/api/principals/{id}/connections/{connection_id}", handleRevokePrincipalConnection)
	mux.HandleFunc("POST /admin/api/principals/{id}/connections/{provider_id}/oauth/start", handlePrincipalOAuthStart)
	mux.HandleFunc("POST /admin/api/principals/{id}/connections/{provider_id}/oauth/poll", handlePrincipalOAuthPoll)
	mux.HandleFunc("POST /admin/api/principals/{id}/connections/{provider_id}/oauth/refresh", handlePrincipalOAuthRefresh)
	mux.HandleFunc("POST /admin/api/principals/{id}/connections/{provider_id}/oauth/revoke", handlePrincipalOAuthRevoke)
	mux.HandleFunc("POST /admin/api/principals/{id}/copilot/login/start", handlePrincipalCopilotLoginStart)
	mux.HandleFunc("POST /admin/api/principals/{id}/copilot/login/poll", handlePrincipalCopilotLoginPoll)
	mux.HandleFunc("DELETE /admin/api/principals/{id}/copilot", handlePrincipalCopilotRevoke)
	mux.HandleFunc("GET /admin/api/alerts", handleListAlerts)
	mux.HandleFunc("POST /admin/api/alerts", handleCreateAlert)
	mux.HandleFunc("POST /admin/api/alerts/status", handleAlertStatus)
	mux.HandleFunc("DELETE /admin/api/alerts/{id}", handleDeleteAlert)
	mux.HandleFunc("POST /admin/api/alerts/evaluate", handleEvaluateAlerts)
	mux.HandleFunc("GET /admin/api/outbox", handleListOutbox)
	mux.HandleFunc("POST /admin/api/outbox/claim", handleClaimOutbox)
	mux.HandleFunc("POST /admin/api/outbox/{id}/delivered", handleOutboxDelivered)
	mux.HandleFunc("POST /admin/api/outbox/{id}/failed", handleOutboxFailed)
	mux.HandleFunc("GET /admin/api/audit", handleAudit)
	mux.HandleFunc("POST /admin/api/detect", handleDetect)
	mux.HandleFunc("GET /admin/api/usage", handleUsage)
	mux.HandleFunc("GET /admin/api/telemetry", handleTelemetry)
	mux.HandleFunc("POST /admin/api/copilot/login/start", handleCopilotLoginStart)
	mux.HandleFunc("POST /admin/api/copilot/login/poll", handleCopilotLoginPoll)
	mux.HandleFunc("POST /admin/api/copilot/logout", handleCopilotLogout)

	return aliasMiddleware(requestLogMiddleware(mux))
}

// aliasMiddleware rewrites bare paths before routing.
func aliasMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target, ok := pathAliases[r.URL.Path]; ok {
			r.URL.Path = target
		}
		next.ServeHTTP(w, r)
	})
}
