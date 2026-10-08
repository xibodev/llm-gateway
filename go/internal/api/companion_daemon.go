package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
	"llmgw/internal/providers"

	"github.com/xibodev/llmgw-core/extension"
)

// companionDaemonProbeTimeout bounds the console's probe of the companion
// daemon, so a daemon that does not answer still lets the page load.
const companionDaemonProbeTimeout = 5 * time.Second

// companionDaemonDependent is a configured provider the companion daemon
// serves, as the type the daemon serves it as.
type companionDaemonDependent struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled,omitempty"`
}

// companionDaemonReport is what the console shows of the optional companion
// daemon.
type companionDaemonReport struct {
	// Address is where the gateway reaches the daemon, which
	// AddressConfigured says LLMGW_EXTENSION_URL names.
	Address           string `json:"address"`
	AddressConfigured bool   `json:"address_configured"`
	SecretSet         bool   `json:"secret_set"`
	// DependentProviders are the configured providers the daemon serves.
	DependentProviders []companionDaemonDependent `json:"dependent_providers"`
	// Probed reports that the gateway asked the daemon what it serves, which
	// it does once a provider depends on the daemon or its address is set.
	Probed    bool   `json:"probed"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Version   string `json:"version,omitempty"`
	// Served are the provider types the daemon reports it serves.
	Served []extension.ProviderInfo `json:"served"`
	// Warnings say what keeps a provider that depends on the daemon from
	// working, or exposes it.
	Warnings []string `json:"warnings"`
}

// GET /admin/api/companion-daemon reports the optional companion daemon:
// where the gateway reaches it, whether they share a secret, which configured
// providers depend on it, and whether it answers, with its version and the
// provider types it serves.
func (s *server) handleCompanionDaemon(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	writeJSON(w, 200, s.companionDaemonReport(r.Context()))
}

func (s *server) companionDaemonReport(ctx context.Context) companionDaemonReport {
	settings := config.Get()
	address, configured := providers.CompanionDaemonAddress()
	report := companionDaemonReport{
		Address: address, AddressConfigured: configured, SecretSet: providers.CompanionDaemonSecretSet(),
		DependentProviders: []companionDaemonDependent{}, Served: []extension.ProviderInfo{}, Warnings: []string{},
	}
	dependents := providers.CompanionDaemonProviders(settings)
	// The providers that would send requests, by the type the daemon serves.
	enabled := map[string][]string{}
	var enabledIDs []string
	for _, id := range dependents {
		cfg := settings.Providers[id]
		dependent := companionDaemonDependent{ID: id, Type: providers.CompanionDaemonType(cfg), Disabled: cfg.Disabled}
		report.DependentProviders = append(report.DependentProviders, dependent)
		if !cfg.Disabled {
			enabled[dependent.Type] = append(enabled[dependent.Type], id)
			enabledIDs = append(enabledIDs, id)
		}
	}
	if warning := companionDaemonSecretWarning(dependents); warning != "" {
		report.Warnings = append(report.Warnings, warning)
	}
	if len(dependents) == 0 && !configured {
		return report
	}
	report.Probed = true
	probe, cancel := context.WithTimeout(ctx, companionDaemonProbeTimeout)
	defer cancel()
	started := time.Now()
	info, err := s.providers().CompanionDaemonInfo(probe)
	report.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		report.Error = diagnostics.SanitizeTextLimit(err.Error(), 300)
		if len(enabledIDs) > 0 {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"the companion daemon did not answer, so provider(s) %s cannot serve requests: %s",
				strings.Join(enabledIDs, ", "), report.Error))
		}
		return report
	}
	report.Reachable, report.Version = true, info.Version
	served := map[string]bool{}
	for _, provider := range info.Providers {
		report.Served = append(report.Served, provider)
		served[provider.ID] = true
	}
	for _, providerType := range slices.Sorted(maps.Keys(enabled)) {
		if !served[providerType] {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"the companion daemon does not serve %s, which provider(s) %s use",
				providerType, strings.Join(enabled[providerType], ", ")))
		}
	}
	return report
}
