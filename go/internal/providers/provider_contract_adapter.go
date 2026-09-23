package providers

import (
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// AdaptCoreProviderConnection describes a configured gateway connection without
// loading or exposing its secret. A nil IAM connection represents config-backed
// system access, or reviewed anonymous access when the provider explicitly
// supports it.
func AdaptCoreProviderConnection(
	providerID string, cfg *config.ProviderConfig, connection *iam.ProviderConnection,
) (core.ProviderConnection, error) {
	providerID = strings.TrimSpace(providerID)
	if cfg == nil {
		return core.ProviderConnection{}, fmt.Errorf("provider config is required")
	}
	registryID := EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type)
	isCodex := registryID == "openai_codex"
	isAntigravity := registryID == "google_antigravity"
	out := core.ProviderConnection{ProviderID: providerID}

	if connection == nil {
		if isCodex || isAntigravity {
			return core.ProviderConnection{}, fmt.Errorf("%s requires a personal connection", registryID)
		}
		if explicitlyAnonymousProvider(providerID, cfg, registryID) {
			out.Kind = core.ProviderConnectionAnonymous
			out.AuthKind = core.ProviderAuthAnonymous
		} else {
			out.Kind = core.ProviderConnectionSystem
			out.AuthKind = core.ProviderAuthAPIKey
		}
		return out, out.Validate()
	}

	out.ID = connection.ID
	out.OwnerID = strings.TrimSpace(connection.PrincipalID)
	if isCodex && (connection.PrincipalKind != "human" || out.OwnerID == "") {
		return core.ProviderConnection{}, fmt.Errorf("codex requires a human connection owner")
	}
	if connection.PrincipalKind == "human" {
		out.Kind = core.ProviderConnectionPersonal
	} else {
		out.Kind = core.ProviderConnectionSystem
		out.OwnerID = ""
	}
	switch {
	case isCodex:
		out.Kind = core.ProviderConnectionPersonalSubscription
		out.AuthKind = core.ProviderAuthOAuthDevice
	case isAntigravity:
		out.Kind = core.ProviderConnectionPersonalSubscription
		out.AuthKind = core.ProviderAuthOAuthBrowser
	case strings.Contains(strings.ToLower(connection.Kind), "oauth"):
		out.AuthKind = core.ProviderAuthOAuthDevice
	case strings.EqualFold(strings.TrimSpace(connection.Kind), CredentialKindAPIKey):
		out.AuthKind = core.ProviderAuthAPIKey
	default:
		return core.ProviderConnection{}, fmt.Errorf("unsupported provider credential kind %q", connection.Kind)
	}
	return out, out.Validate()
}

func explicitlyAnonymousProvider(providerID string, cfg *config.ProviderConfig, registryID string) bool {
	entry, _ := RegistryProviderByID(registryID)
	isZen := registryID == "opencode_zen" || isZenBaseURL(cfg.BaseURL)
	if !entry.AnonymousAutomation && !isZen {
		return false
	}
	return AnonymousAPIKey(config.ResolveProviderAPIKey(providerID, cfg))
}

// CoreProviderEvidence is the core representation of the gateway's Phase-1
// catalog and completion checks.
type CoreProviderEvidence struct {
	Catalog core.CatalogEvidence
	Probes  []core.CompletionProbeEvidence
	Health  core.ProviderHealthEvidence
}

// AdaptCoreProviderEvidence preserves the Phase-1 distinction between catalog
// discovery and successful inference. Check details are intentionally excluded.
func AdaptCoreProviderEvidence(
	providerID string, models []ModelInfo, checks []iam.ProviderCheck,
) CoreProviderEvidence {
	result := CoreProviderEvidence{
		Catalog: core.CatalogEvidence{Status: core.CatalogNotProbed},
		Health: core.ProviderHealthEvidence{
			Status: core.ProviderHealthUnknown, ErrorClass: core.ProviderErrorNone,
		},
	}
	var latestCatalog, latestHealth *iam.ProviderCheck
	for index := range checks {
		check := &checks[index]
		if latestHealth == nil || check.CheckedAt >= latestHealth.CheckedAt {
			latestHealth = check
		}
		if catalogEvidenceOperation(check.Operation) &&
			(latestCatalog == nil || check.CheckedAt >= latestCatalog.CheckedAt) {
			latestCatalog = check
		}
		if check.Operation == iam.CheckVerify && strings.TrimSpace(check.Model) != "" {
			status := core.CompletionFailed
			if check.Success {
				status = core.CompletionVerified
			}
			result.Probes = append(result.Probes, core.CompletionProbeEvidence{
				Target: core.Target{Provider: providerID, Model: check.Model},
				Status: status, ObservedAt: unixEvidenceTime(check.CheckedAt),
				Latency: time.Duration(check.LatencyMS) * time.Millisecond,
			})
		}
	}
	if latestCatalog != nil {
		result.Catalog.ObservedAt = unixEvidenceTime(latestCatalog.CheckedAt)
		switch {
		case !latestCatalog.Success:
			result.Catalog.Status = core.CatalogFailed
		case len(models) == 0:
			result.Catalog.Status = core.CatalogEmpty
		default:
			result.Catalog.Status = core.CatalogDiscovered
			result.Catalog.Models = make([]core.ModelInfo, 0, len(models))
			for _, model := range models {
				var verifiedAt time.Time
				for _, probe := range result.Probes {
					if probe.Status == core.CompletionVerified && probe.Target.Model == model.ID && probe.ObservedAt.After(verifiedAt) {
						verifiedAt = probe.ObservedAt
					}
				}
				result.Catalog.Models = append(result.Catalog.Models, core.ModelInfo{
					ID: model.ID, Object: "model", OwnedBy: model.Vendor, Description: model.Label,
					Capabilities: AdaptModelCapabilities(
						model.Capabilities, model.SupportedSurfaces, result.Catalog.ObservedAt, verifiedAt,
					),
				})
			}
		}
	}
	if latestHealth != nil {
		result.Health.ObservedAt = unixEvidenceTime(latestHealth.CheckedAt)
		if latestHealth.Success {
			result.Health.Status = core.ProviderHealthHealthy
		} else {
			result.Health.Status = core.ProviderHealthUnhealthy
			result.Health.ErrorClass = core.ProviderErrorUpstream
		}
	}
	return result
}

func catalogEvidenceOperation(operation string) bool {
	switch operation {
	case iam.CheckReachability, iam.CheckCatalogSync, iam.CheckCacheReset:
		return true
	default:
		return false
	}
}

func unixEvidenceTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}
