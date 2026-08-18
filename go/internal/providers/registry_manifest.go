package providers

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"llmgw/internal/gcpauth"
	"net/url"
	"regexp"
	"strings"
)

//go:embed registry_manifest.json
var registryManifestJSON []byte

var providerRegistry = mustLoadProviderRegistry(registryManifestJSON)

var registryIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func mustLoadProviderRegistry(payload []byte) []RegistryEntry {
	entries, err := decodeProviderRegistry(payload)
	if err != nil {
		panic("provider registry manifest: " + err.Error())
	}
	return entries
}

func decodeProviderRegistry(payload []byte) ([]RegistryEntry, error) {
	var entries []RegistryEntry
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode JSON: trailing content")
	}
	if err := validateProviderRegistry(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func validateProviderRegistry(entries []RegistryEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("manifest has no entries")
	}

	runtimeTypes := map[string]bool{}
	for _, providerType := range ProviderTypes {
		runtimeTypes[providerType] = true
	}
	allowedAvailability := map[string]bool{
		ProviderAvailable: true, ProviderPlanned: true, ProviderClientOnly: true,
	}
	allowedScopes := map[string]bool{
		ConnectionScopeSystemOrPersonal: true,
		ConnectionScopePersonal:         true,
		ConnectionScopeGatewayClient:    true,
	}
	allowedAuth := map[string]bool{
		"api_key": true, "gateway_client": true, "none": true, "oauth_browser": true,
		"oauth_device": true, "token_import": true, gcpauth.CredentialKind: true,
	}
	allowedRisk := map[string]bool{"green": true, "yellow": true, "red": true}
	claimedNames := map[string]string{}

	for index := range entries {
		entry := &entries[index]
		entry.ID = strings.ToLower(strings.TrimSpace(entry.ID))
		entry.DefaultProviderID = strings.ToLower(strings.TrimSpace(entry.DefaultProviderID))
		entry.RuntimeType = strings.ToLower(strings.TrimSpace(entry.RuntimeType))
		entry.Protocol = strings.ToLower(strings.TrimSpace(entry.Protocol))
		entry.Availability = strings.ToLower(strings.TrimSpace(entry.Availability))
		entry.ConnectionScope = strings.ToLower(strings.TrimSpace(entry.ConnectionScope))
		entry.RiskLevel = strings.ToLower(strings.TrimSpace(entry.RiskLevel))
		entry.AuthAdapter = strings.ToLower(strings.TrimSpace(entry.AuthAdapter))
		entry.QuotaAdapter = strings.ToLower(strings.TrimSpace(entry.QuotaAdapter))

		if !registryIdentifierPattern.MatchString(entry.ID) {
			return fmt.Errorf("entry %d has invalid id %q", index+1, entry.ID)
		}
		if entry.Label == "" || entry.Description == "" || entry.RuntimeType == "" ||
			entry.Protocol == "" || entry.DefaultProviderID == "" {
			return fmt.Errorf("entry %q is missing required metadata", entry.ID)
		}
		if !registryIdentifierPattern.MatchString(entry.DefaultProviderID) {
			return fmt.Errorf("entry %q has invalid default_provider_id %q", entry.ID, entry.DefaultProviderID)
		}
		if !allowedAvailability[entry.Availability] {
			return fmt.Errorf("entry %q has invalid availability %q", entry.ID, entry.Availability)
		}
		if !allowedScopes[entry.ConnectionScope] {
			return fmt.Errorf("entry %q has invalid connection scope %q", entry.ID, entry.ConnectionScope)
		}
		if !allowedRisk[entry.RiskLevel] {
			return fmt.Errorf("entry %q has invalid risk level %q", entry.ID, entry.RiskLevel)
		}
		if entry.RiskLevel != "green" && strings.TrimSpace(entry.RiskNotice) == "" {
			return fmt.Errorf("entry %q requires a risk notice for risk level %q", entry.ID, entry.RiskLevel)
		}
		if entry.Availability == ProviderAvailable && !runtimeTypes[entry.RuntimeType] {
			return fmt.Errorf("entry %q uses unsupported runtime type %q", entry.ID, entry.RuntimeType)
		}
		if len(entry.AuthMethods) == 0 {
			return fmt.Errorf("entry %q has no auth methods", entry.ID)
		}

		hasAPIKey := false
		hasNone := false
		requiresAuthAdapter := false
		for authIndex, method := range entry.AuthMethods {
			method = strings.ToLower(strings.TrimSpace(method))
			entry.AuthMethods[authIndex] = method
			if !allowedAuth[method] {
				return fmt.Errorf("entry %q has invalid auth method %q", entry.ID, method)
			}
			hasAPIKey = hasAPIKey || method == "api_key"
			hasNone = hasNone || method == "none"
			requiresAuthAdapter = requiresAuthAdapter ||
				method == "oauth_browser" || method == "oauth_device" || method == "token_import"
		}
		if entry.RequiresAPIKey && !hasAPIKey {
			return fmt.Errorf("entry %q requires an API key but does not advertise api_key auth", entry.ID)
		}
		if entry.ConnectionScope == ConnectionScopePersonal && hasNone {
			return fmt.Errorf("entry %q is personal but advertises no-auth access", entry.ID)
		}
		if entry.ClientOnly &&
			(entry.Availability != ProviderClientOnly || entry.ConnectionScope != ConnectionScopeGatewayClient) {
			return fmt.Errorf("entry %q has inconsistent client-only classification", entry.ID)
		}
		if requiresAuthAdapter && entry.Availability == ProviderAvailable {
			if !registryIdentifierPattern.MatchString(entry.AuthAdapter) {
				return fmt.Errorf("entry %q requires a valid auth_adapter", entry.ID)
			}
		}
		if entry.AuthAdapter != "" && !registryIdentifierPattern.MatchString(entry.AuthAdapter) {
			return fmt.Errorf("entry %q has invalid auth_adapter %q", entry.ID, entry.AuthAdapter)
		}
		if err := validateRegistryURL(entry.ID, "default_base_url", entry.DefaultBaseURL); err != nil {
			return err
		}
		if err := validateRegistryURL(entry.ID, "docs_url", entry.DocsURL); err != nil {
			return err
		}

		names := append([]string{entry.ID}, entry.Aliases...)
		for nameIndex, name := range names {
			name = strings.ToLower(strings.TrimSpace(name))
			if !registryIdentifierPattern.MatchString(name) {
				return fmt.Errorf("entry %q has invalid id/alias %q", entry.ID, name)
			}
			if owner, exists := claimedNames[name]; exists {
				return fmt.Errorf("entry %q reuses id/alias %q claimed by %q", entry.ID, name, owner)
			}
			claimedNames[name] = entry.ID
			if nameIndex > 0 {
				entry.Aliases[nameIndex-1] = name
			}
		}
		for categoryIndex, category := range entry.Categories {
			category = strings.ToLower(strings.TrimSpace(category))
			if !registryIdentifierPattern.MatchString(category) {
				return fmt.Errorf("entry %q has invalid category %q", entry.ID, category)
			}
			entry.Categories[categoryIndex] = category
		}
		for fieldIndex, field := range entry.OnboardingFields {
			field = strings.ToLower(strings.TrimSpace(field))
			if !registryIdentifierPattern.MatchString(field) {
				return fmt.Errorf("entry %q has invalid onboarding field %q", entry.ID, field)
			}
			entry.OnboardingFields[fieldIndex] = field
		}
	}
	return nil
}

func validateRegistryURL(entryID, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("entry %q has invalid %s %q", entryID, field, value)
	}
	return nil
}
