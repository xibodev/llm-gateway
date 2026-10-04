package api

import (
	"fmt"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// StartupWarnings returns what the current settings expose, given the host the
// gateway listens on, so the process can log it before it serves.
func StartupWarnings(listenHost string) []string {
	s := config.Get()
	var warnings []string
	if s.AllowUnauthenticatedAPI && !loopbackHost(listenHost) {
		warnings = append(warnings, fmt.Sprintf(
			"unauthenticated local mode is on and the gateway listens on %s, which is not a loopback address: "+
				"every client that can reach it uses the data plane without a key", listenHost))
	}
	// Only the count is reported: the keys are credentials and logs travel.
	additionalKeys := 0
	for _, key := range s.APIKeys {
		if key != "" {
			additionalKeys++
		}
	}
	if additionalKeys > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"LLMGW_API_KEYS holds %d static key(s), each a full administrator credential: "+
				"give clients gateway-issued project keys instead", additionalKeys))
	}
	// A daemon started without a secret serves any caller that can reach it,
	// and every credential the gateway sends it passes through it.
	if served := providers.CompanionDaemonProviders(s); len(served) > 0 && !providers.CompanionDaemonSecretSet() {
		warnings = append(warnings, fmt.Sprintf(
			"the companion daemon serves provider(s) %s and LLMGW_EXTENSION_SECRET is empty: "+
				"start the daemon with a shared secret and set LLMGW_EXTENSION_SECRET to the same value",
			strings.Join(served, ", ")))
	}
	return warnings
}
