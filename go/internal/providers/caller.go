package providers

import (
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// gatewayCaller is the caller of gateway-internal work: background refreshes,
// probes and the caller-less wrappers such as GetProvider. It resolves only
// shared credentials and uses the gateway-wide instance scope, as a nil
// Principal did.
func gatewayCaller() core.Caller { return core.Caller{Kind: core.CallerAnonymous} }

// callerPrincipal names the IAM principal whose personal connections, project
// bindings and private instance scope a caller uses, with its kind: "human",
// "service" or "system". Both are empty for anonymous and local callers and
// for the reserved IDs of the static admin and external keys, which keep the
// shared scope a Principal without a PrincipalID had. A system principal
// arrives as a service caller, so scopes key on this kind, not Caller.Kind.
func callerPrincipal(caller core.Caller) (id, kind string) { return iam.CallerPrincipalID(caller) }

// callerPrincipalID is callerPrincipal's ID alone.
func callerPrincipalID(caller core.Caller) string {
	id, _ := callerPrincipal(caller)
	return id
}
