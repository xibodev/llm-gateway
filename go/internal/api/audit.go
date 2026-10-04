package api

import (
	"log"
	"net/http"
	"strconv"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func auditAdmin(
	r *http.Request, action, targetType, targetID string, detail map[string]any,
) {
	auditAdminResult(r, action, targetType, targetID, "success", detail)
}

func auditAdminResult(
	r *http.Request, action, targetType, targetID, result string, detail map[string]any,
) {
	actor := getAdminActor(r)
	if detail == nil {
		detail = map[string]any{}
	}
	detail["actor_source"] = actor.Source
	if actor.KeyFingerprint != "" {
		detail["actor_key_fingerprint"] = actor.KeyFingerprint
	}
	if id := requestIDFrom(r.Context()); id != "" {
		detail["request_id"] = id
	}
	if err := iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: actor.PrincipalID, ActorKeyID: actor.KeyID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Result: result, Detail: detail,
	}); err != nil {
		log.Printf("record admin audit: %v", err)
	}
}

// auditProviderSaved records a provider an administrator created or changed.
// It names the settings that changed rather than their values: a base URL can
// carry credentials, and the credential itself is only reported as replaced.
func auditProviderSaved(
	r *http.Request, providerID string, before *config.ProviderConfig,
	credentialReplaced bool, credentialKind string,
) {
	action := "provider.update"
	if before == nil {
		action = "provider.create"
	}
	after := config.Get().Providers[providerID]
	changed := providerConfigChanges(before, after)
	detail := map[string]any{}
	if after != nil {
		detail["type"] = after.Type
	}
	if credentialReplaced {
		changed = append(changed, "credential")
		detail["credential_kind"] = credentialKind
	}
	detail["changed"] = changed
	auditAdmin(r, action, "provider", providerID, detail)
}

func providerConfigChanges(before, after *config.ProviderConfig) []string {
	if before == nil {
		before = &config.ProviderConfig{}
	}
	if after == nil {
		after = &config.ProviderConfig{}
	}
	changed := []string{}
	for _, field := range []struct {
		name          string
		before, after any
	}{
		{"type", before.Type, after.Type},
		{"registry_id", before.RegistryID, after.RegistryID},
		{"base_url", before.BaseURL, after.BaseURL},
		{"region", before.Region, after.Region},
		{"project", before.Project, after.Project},
		{"location", before.Location, after.Location},
		{"vertex_request_type", before.VertexRequestType, after.VertexRequestType},
		{"public_oauth_client_id", before.PublicOAuthClientID, after.PublicOAuthClientID},
		{"force_api_support", before.ForceApiSupport, after.ForceApiSupport},
	} {
		if field.before != field.after {
			changed = append(changed, field.name)
		}
	}
	return changed
}

// auditEndpointSaved records an endpoint an administrator created or
// replaced, with the failover members it now routes to.
func auditEndpointSaved(r *http.Request, name string, existed bool, members []config.EndpointMember) {
	action := "endpoint.update"
	if !existed {
		action = "endpoint.create"
	}
	recorded := make([]map[string]any, 0, len(members))
	for _, member := range members {
		entry := map[string]any{"provider": member.Provider, "model": member.Model}
		if member.AllowUnverified {
			entry["allow_unverified"] = true
		}
		recorded = append(recorded, entry)
	}
	auditAdmin(r, action, "endpoint", name, map[string]any{"members": recorded})
}

// auditCodexClientID records an administrator's sign-in start that changed
// the Codex OAuth client ID. A start that fails puts the previous ID back,
// so only a change that is still in place is recorded.
func auditCodexClientID(r *http.Request, previous string) {
	if config.Get().OpenAICodexClientID == previous {
		return
	}
	auditAdmin(r, "oauth_client.update", "oauth_client", "openai_codex", map[string]any{
		"changed": []string{"client_id"},
	})
}

// consumerManualClient reads the stored consumer_manual OAuth client when
// providerRef names Google Antigravity, the only provider that persists one.
func consumerManualClient(providerRef string) (iam.OAuthClientProfile, bool) {
	if oauthRegistryIDForRef(providerRef) != "google_antigravity" {
		return iam.OAuthClientProfile{}, false
	}
	profile, found, err := iam.OAuthClientProfileByName("google_antigravity", antigravityManualProfile)
	return profile, err == nil && found
}

// auditConsumerManualClient records the consumer_manual OAuth client a
// sign-in stored, naming the fields that changed. The client secret is
// reported as changed, never by value. The client is compared with the one
// stored before the sign-in, because a completion can store it and still fail
// afterwards. A sign-in an administrator started stores the client the
// administrator entered even when its owner completes it, so owner names the
// principal who completed it through the self-service route; nil records the
// administrator of r.
func auditConsumerManualClient(
	r *http.Request, providerRef string, before iam.OAuthClientProfile, existed bool, owner *iam.Principal,
) {
	after, found := consumerManualClient(providerRef)
	if !found {
		return
	}
	if !existed {
		before = iam.OAuthClientProfile{}
	}
	changed := []string{}
	for _, field := range []struct {
		name          string
		before, after string
	}{
		{"client_id", before.ClientID, after.ClientID},
		{"client_secret", before.ClientSecret, after.ClientSecret},
		{"client_mode", before.ClientMode, after.ClientMode},
		{"redirect_uri", before.RedirectURI, after.RedirectURI},
	} {
		if field.before != field.after {
			changed = append(changed, field.name)
		}
	}
	if len(changed) == 0 {
		return
	}
	detail := map[string]any{"profile": antigravityManualProfile, "changed": changed}
	if owner != nil {
		detail["source"] = "self-service"
		_ = iam.RecordAudit(iam.AuditEvent{
			ActorPrincipalID: owner.ID, Action: "oauth_client.update", TargetType: "oauth_client",
			TargetID: "google_antigravity", Result: "success", Detail: detail,
		})
		return
	}
	auditAdmin(r, "oauth_client.update", "oauth_client", "google_antigravity", detail)
}

// auditPlayground records a playground request. A self-service request is its
// owner's own. An administrator's request runs with a principal's access, so
// its record names the administrator as the actor and that principal under
// on_behalf_of: crediting the principal would attribute the administrator's
// action to someone who never took it.
func auditPlayground(
	r *http.Request, source, action, projectID, principalID, result string, detail map[string]any,
) {
	if source == "admin" {
		auditAdminPlayground(r, action, projectID, principalID, result, detail)
		return
	}
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principalID, Action: action, TargetType: "project", TargetID: projectID,
		Result: result, Detail: detail,
	})
}

func auditAdminPlayground(
	r *http.Request, action, projectID, principalID, result string, detail map[string]any,
) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["on_behalf_of"] = principalID
	auditAdminResult(r, action, "project", projectID, result, detail)
}

func handleAudit(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := iam.ListAudit(limit)
	if err != nil {
		writeError(w, 500, "Audit store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}
