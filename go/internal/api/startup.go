package api

import (
	"fmt"

	"llmgw/internal/config"
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
	return warnings
}
