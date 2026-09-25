package api

import (
	"context"

	"llmgw/internal/config"
	"llmgw/internal/router"
)

// governed attaches a request principal's per-key envelope to ctx for the
// router: the route allowlist and routes-only flag that hide models as
// not-found, the key allowlists that filter native aliases, and the names
// telemetry attributes to. The router reads the project policy for the
// Caller's project itself. A nil Principal is gateway-internal work and stays
// ungoverned, as the router treated a nil Principal. Key policy and quota
// remain in keypolicy.go, on the Principal.
func governed(ctx context.Context, p *config.Principal) context.Context {
	if p == nil {
		return ctx
	}
	return router.WithGovernance(ctx, router.Governance{
		AllowedRoutes: p.AllowedRoutes, RoutesOnly: p.RoutesOnly,
		AllowedModels: p.AllowedModels, AllowedProviders: p.AllowedProviders,
		Project: p.Project, Key: p.Key,
	})
}

// resolveModel resolves a requested model as a request principal: the router
// takes its Caller and reads its governance from ctx. Resolution observes no
// cancellation, so helpers without a request context pass a background one.
func resolveModel(ctx context.Context, model string, p *config.Principal) (router.Resolution, error) {
	return router.ResolveForPrincipal(governed(ctx, p), model, callerOf(p))
}
