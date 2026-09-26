package iam

import (
	"strings"

	core "github.com/xibodev/llmgw-core"
)

// Reserved caller IDs name callers that core cannot describe with an IAM
// principal's own ID and kind. The static admin key and external keys are
// automation, so they are Kind service, but the Principal built for them never
// carried a PrincipalID: they resolve only system and shared credentials. A
// system principal can own a key too, and core has no system kind, so its
// caller is Kind service with a system: ID that maps back to that principal
// and its kind. The prefixes cannot collide with principal IDs, which newID
// generates without a colon.
const (
	// AdminCallerID is the caller of the static admin key.
	AdminCallerID = gatewayCallerPrefix + "admin"

	gatewayCallerPrefix  = "gateway:"
	externalCallerPrefix = "external:"
	systemCallerPrefix   = "system:"
)

// ExternalKeyCallerID is the reserved caller ID of an externally managed key,
// which names a project and a key but no principal.
func ExternalKeyCallerID(projectID, key string) string {
	return externalCallerPrefix + projectID + "/" + key
}

// SystemPrincipalCallerID is the reserved caller ID of a system principal,
// such as the owner of a key issued to the gateway's system principal.
func SystemPrincipalCallerID(principalID string) string {
	return systemCallerPrefix + principalID
}

// CallerPrincipalID returns the IAM principal whose personal connections,
// project bindings and private cache scope a caller uses, with that
// principal's kind: "human", "service" or "system". Both are empty for callers
// that resolve only system and shared credentials, exactly as a Principal
// without a PrincipalID did: anonymous and local callers, and the admin and
// external IDs. Only a service caller can carry a system principal. The ID is
// returned untrimmed so callers keep their own blank-ID checks.
func CallerPrincipalID(caller core.Caller) (id, kind string) {
	switch caller.Kind {
	case core.CallerHuman:
		kind = "human"
	case core.CallerService:
		kind = "service"
	default:
		return "", ""
	}
	switch {
	case strings.HasPrefix(caller.ID, systemCallerPrefix):
		id = strings.TrimPrefix(caller.ID, systemCallerPrefix)
		if kind != "service" || id == "" {
			return "", ""
		}
		return id, "system"
	case strings.HasPrefix(caller.ID, gatewayCallerPrefix), strings.HasPrefix(caller.ID, externalCallerPrefix):
		return "", ""
	}
	return caller.ID, kind
}

// ResolveCallerOAuthCredentialSecretWithObservation is
// ResolveProviderOAuthCredentialSecretWithObservation for a core.Caller: a
// human gets their own connection or legacy credential, a service or system
// principal gets its project's binding for its kind, and a shared-only caller
// resolves nothing.
func ResolveCallerOAuthCredentialSecretWithObservation(
	caller core.Caller, providerID string,
) (string, *ProviderAccountObservation, bool, error) {
	return ResolveProviderOAuthCredentialSecretWithObservation(callerPrincipal(caller), providerID)
}
