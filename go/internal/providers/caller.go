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

// callerPrincipalID names the IAM principal whose personal connections,
// project bindings and private instance scope a caller uses. It is empty for
// anonymous and local callers and for the reserved IDs of the static admin and
// external keys, which keep the shared scope a Principal without a
// PrincipalID had.
func callerPrincipalID(caller core.Caller) string { return iam.CallerPrincipalID(caller) }
