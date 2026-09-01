package api

import (
	"errors"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// Key governance enforcement: model/provider allowlists plus durable,
// transactionally-consumed SQLite quotas. Static admin/local principals remain
// unrestricted.

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func modelPolicyAllows(allowed []string, requestedModel, resolvedCategory string, targets []router.Target) bool {
	if len(allowed) == 0 {
		return true
	}
	if resolvedCategory != "" {
		return containsStr(allowed, resolvedCategory)
	}
	requestedModel = strings.TrimSpace(requestedModel)
	candidates := []string{requestedModel}
	settings := config.Get()
	for _, target := range targets {
		candidates = append(candidates, target.Provider+"/"+target.Model, target.Model)
		if alias, _ := discoveryAliasID(settings, target.Model); alias != "" {
			candidates = append(candidates, alias)
		}
	}
	for _, candidate := range candidates {
		if candidate != "" && containsStr(allowed, candidate) {
			return true
		}
	}
	return false
}

// enforceKeyPolicy applies a minted key's governance to a request. It returns
// the (possibly provider-filtered) targets and an HTTP error (status,msg) when
// the request is not allowed. status==0 means allowed.
func enforceKeyPolicy(p *config.Principal, requestedModel, resolvedCategory string, targets []router.Target) ([]router.Target, int, string) {
	targets, status, message := authorizeKeyPolicy(p, requestedModel, resolvedCategory, targets)
	if status != 0 || p == nil || p.Token == "" {
		return targets, status, message
	}
	if err := iam.CheckAndConsumeRequest(p, time.Now()); err != nil {
		var exceeded *iam.QuotaExceeded
		if errors.As(err, &exceeded) {
			return nil, 429, exceeded.Error()
		}
		return nil, 500, "Quota store unavailable."
	}
	return targets, 0, ""
}

// authorizeKeyPolicy enforces routing and credential policy without consuming
// inference quotas. Non-inference operations such as token counting use it.
func authorizeKeyPolicy(p *config.Principal, requestedModel, resolvedCategory string, targets []router.Target) ([]router.Target, int, string) {
	if p == nil || p.Token == "" {
		return targets, 0, "" // admin / unauthenticated-local: unrestricted
	}
	projectPolicy, err := iam.GetProjectPolicy(p.ProjectID)
	if err != nil {
		return nil, 500, "Project policy store unavailable."
	}
	if !modelPolicyAllows(projectPolicy.AllowedModels, requestedModel, resolvedCategory, targets) {
		return nil, 403, "This project is not allowed to use model '" + requestedModel + "'."
	}
	if !modelPolicyAllows(p.AllowedModels, requestedModel, resolvedCategory, targets) {
		return nil, 403, "This key is not allowed to use model '" + requestedModel + "'."
	}
	allowedProviders := p.AllowedProviders
	providerRestricted := len(p.AllowedProviders) > 0 || len(projectPolicy.AllowedProviders) > 0
	if len(projectPolicy.AllowedProviders) > 0 {
		if len(allowedProviders) == 0 {
			allowedProviders = projectPolicy.AllowedProviders
		} else {
			intersection := []string{}
			for _, provider := range allowedProviders {
				if containsStr(projectPolicy.AllowedProviders, provider) {
					intersection = append(intersection, provider)
				}
			}
			allowedProviders = intersection
		}
	}
	if providerRestricted {
		if len(allowedProviders) == 0 {
			return nil, 403, "Key and project provider policies have no allowed provider in common."
		}
		filtered := make([]router.Target, 0, len(targets))
		for _, t := range targets {
			if containsStr(allowedProviders, t.Provider) {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			return nil, 403, "This key is not allowed to use any provider in the requested route."
		}
		targets = filtered
	}
	credentialTargets := make([]router.Target, 0, len(targets))
	for _, target := range targets {
		authorized, err := providers.ProviderCredentialAuthorized(target.Provider, p)
		if err != nil {
			return nil, 500, "Provider credential store unavailable."
		}
		if authorized {
			credentialTargets = append(credentialTargets, target)
		}
	}
	if len(credentialTargets) == 0 {
		return nil, 403, "This principal has no active provider credential for the requested route."
	}
	targets = credentialTargets
	return targets, 0, ""
}
