package router

import "context"

// Governance is the gateway's per-key policy envelope for one request. A
// core.Caller names a principal, not a key: two keys of one service principal
// in one project share a Caller but carry their own allowlists, and the static
// admin and external keys have no IAM identity at all. So the api layer, which
// owns key governance, attaches the envelope to the request context beside the
// Caller, and the router's signatures keep core's shape: a context and a
// Caller.
//
// Work without an envelope, such as gateway-internal refreshes and tests of
// routing alone, is ungoverned, exactly as a nil Principal was. Project policy
// is not carried: a governed request reads it for the Caller's project.
type Governance struct {
	// AllowedRoutes limits the configured routes a key may address; any other
	// route reads as model-not-found.
	AllowedRoutes []string
	// RoutesOnly hides everything that is not a configured route.
	RoutesOnly bool
	// AllowedModels and AllowedProviders filter native-alias candidates.
	AllowedModels    []string
	AllowedProviders []string
	// Project and Key name the caller in telemetry.
	Project, Key string
}

type governanceKey struct{}

// WithGovernance attaches a request's governance envelope to ctx.
func WithGovernance(ctx context.Context, governance Governance) context.Context {
	return context.WithValue(ctx, governanceKey{}, governance)
}

// governanceFrom returns the envelope attached to ctx, or nil when the work is
// ungoverned.
func governanceFrom(ctx context.Context) *Governance {
	governance, ok := ctx.Value(governanceKey{}).(Governance)
	if !ok {
		return nil
	}
	return &governance
}
