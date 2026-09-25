package iam

import (
	"strings"

	core "github.com/xibodev/llmgw-core"
)

// Reserved caller IDs name callers that have no IAM principal. The static
// admin key and external keys are automation, so they are Kind service, but
// the Principal built for them never carried a PrincipalID: they resolve only
// system and shared credentials. The prefixes cannot collide with principal
// IDs, which newID generates without a colon.
const (
	// AdminCallerID is the caller of the static admin key.
	AdminCallerID = gatewayCallerPrefix + "admin"

	gatewayCallerPrefix  = "gateway:"
	externalCallerPrefix = "external:"
)

// ExternalKeyCallerID is the reserved caller ID of an externally managed key,
// which names a project and a key but no principal.
func ExternalKeyCallerID(projectID, key string) string {
	return externalCallerPrefix + projectID + "/" + key
}

// CallerPrincipalID returns the IAM principal whose personal connections,
// project bindings and private cache scope a caller uses. It is empty for
// callers that resolve only system and shared credentials, exactly as a
// Principal without a PrincipalID did: anonymous and local callers, and the
// reserved IDs above. The ID is returned untrimmed so callers keep their own
// blank-ID checks.
func CallerPrincipalID(caller core.Caller) string {
	if caller.Kind != core.CallerHuman && caller.Kind != core.CallerService {
		return ""
	}
	if strings.HasPrefix(caller.ID, gatewayCallerPrefix) || strings.HasPrefix(caller.ID, externalCallerPrefix) {
		return ""
	}
	return caller.ID
}
