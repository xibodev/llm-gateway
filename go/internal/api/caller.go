package api

import (
	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// callerSource names who built a Principal. A Principal without a PrincipalID
// looks the same whether local mode, the static admin key or an external key
// produced it, so each producer names itself instead of the Caller being
// guessed from project or key names.
type callerSource int

const (
	// sourcePrincipal is an IAM principal: the owner of an IAM API key, or a
	// principal the console, playground or provider checks act for.
	sourcePrincipal callerSource = iota
	// sourceExternalKey is an externally managed key. It belongs to a
	// project but to no principal.
	sourceExternalKey
	// sourceAdminKey is the static admin key from settings.
	sourceAdminKey
	// sourceLocal is an unauthenticated request in local mode.
	sourceLocal
)

// requestCaller maps a Principal onto the core.Caller that providers and the
// router see. It is the only place the api layer makes that decision.
//
// Only human and service callers with a non-reserved ID reach personal
// connections, project bindings and private instance scopes, so every caller
// without a PrincipalID keeps the gateway-wide shared scope it had: the
// reserved IDs of the admin and external keys, the local caller, and the
// anonymous caller of gateway-internal work. An IAM principal is human only
// when its kind says so; service and system principals are automation.
func requestCaller(source callerSource, p *config.Principal) core.Caller {
	if p == nil {
		return core.Caller{Kind: core.CallerAnonymous}
	}
	switch source {
	case sourceExternalKey:
		return core.Caller{
			ID: iam.ExternalKeyCallerID(p.ProjectID, p.Key), Kind: core.CallerService, ProjectID: p.ProjectID,
		}
	case sourceAdminKey:
		return core.Caller{ID: iam.AdminCallerID, Kind: core.CallerService}
	case sourceLocal:
		return core.LocalCaller()
	}
	if p.PrincipalID == "" {
		return core.Caller{Kind: core.CallerAnonymous, ProjectID: p.ProjectID}
	}
	kind := core.CallerService
	if p.PrincipalKind == "human" {
		kind = core.CallerHuman
	}
	return core.Caller{ID: p.PrincipalID, Kind: kind, ProjectID: p.ProjectID}
}

// withCaller records a Principal's Caller where the Principal is built.
func withCaller(source callerSource, p *config.Principal) *config.Principal {
	p.Caller = requestCaller(source, p)
	return p
}

// apiKeySource tells the two Principals iam.ResolveAPIKey returns apart: an
// IAM key always has an owning principal, an external key never has one.
func apiKeySource(p *config.Principal) callerSource {
	if p.PrincipalID == "" {
		return sourceExternalKey
	}
	return sourcePrincipal
}

// callerOf is the Caller a Principal acts as at the provider and router
// boundary: the one recorded where it was built, otherwise the mapping of an
// IAM principal, which is anonymous without a PrincipalID. A nil Principal is
// gateway-internal work.
func callerOf(p *config.Principal) core.Caller {
	if p != nil && p.Caller.Kind != "" {
		return p.Caller
	}
	return requestCaller(sourcePrincipal, p)
}
