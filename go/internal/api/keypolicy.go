package api

import (
	"errors"
	"net/http"
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

// admitKeyPolicy consumes one request from the principal's key and project
// quotas; status==0 means admitted. It is the second governance step: a
// handler authorizes as soon as the route is resolved, so a denial wins over a
// validation error, and admits only after every check that can refuse the
// request without contacting a provider, so a request the gateway rejects
// itself spends no quota.
func admitKeyPolicy(p *config.Principal) (int, string) {
	if p == nil || p.Token == "" {
		return 0, "" // admin / unauthenticated-local: unmetered
	}
	if err := iam.CheckAndConsumeRequest(p, time.Now()); err != nil {
		var exceeded *iam.QuotaExceeded
		if errors.As(err, &exceeded) {
			return 429, exceeded.Error()
		}
		return 500, "Quota store unavailable."
	}
	return 0, ""
}

// admitRequest admits a handler's request immediately before it is executed.
// It reports false once it has recorded and written the refusal, so the
// handler only returns.
func admitRequest(
	w http.ResponseWriter, endpoint, requestedModel string,
	p *config.Principal, errorCode string, started time.Time,
) bool {
	status, message := admitKeyPolicy(p)
	if status == 0 {
		return true
	}
	recordFailureUsage(endpoint, requestedModel, p, status, errorCode, started)
	writeError(w, status, message)
	return false
}

// authorizeKeyPolicy enforces routing and credential policy without consuming
// inference quotas, then checks availability. Token counting also uses it.
func authorizeKeyPolicy(p *config.Principal, requestedModel, resolvedCategory string, targets []router.Target) ([]router.Target, int, string) {
	if p == nil || p.Token == "" {
		return availableRouteTargets(targets) // admin / unauthenticated-local: unrestricted
	}
	projectPolicy, err := iam.GetProjectPolicy(p.ProjectID)
	if err != nil {
		return nil, 500, "Project policy store unavailable."
	}
	if (resolvedCategory == "" && p.RoutesOnly) ||
		(resolvedCategory != "" && (p.RoutesOnly || len(p.AllowedRoutes) > 0) &&
			!containsStr(p.AllowedRoutes, resolvedCategory)) {
		return nil, 403, "This key is not allowed to use the requested route or direct model."
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
		authorized, err := providers.ProviderCredentialAuthorized(target.Provider, callerOf(p))
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
	return availableRouteTargets(targets)
}

// Resolution retains disabled-only routes so policy denials take precedence
// over availability. Never pass those targets to execution or consume a quota.
func availableRouteTargets(targets []router.Target) ([]router.Target, int, string) {
	available := make([]router.Target, 0, len(targets))
	for _, target := range targets {
		if cfg := config.Get().Providers[target.Provider]; cfg != nil && cfg.Disabled {
			continue
		}
		available = append(available, target)
	}
	if len(available) == 0 {
		return nil, 404, (&router.ModelNotFoundError{Unavailable: true}).Error()
	}
	return available, 0, ""
}
