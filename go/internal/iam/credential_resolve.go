package iam

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// CredentialPrecedence is one of the credential resolution orders the gateway
// applies today; which one serves an instance depends on its provider type.
type CredentialPrecedence int

const (
	// ConnectionPrecedence is the order of the provider factory's
	// resolveCredentialObserved, used by every API-key transport: the
	// caller's active default connection of any kind, then the system
	// connection, then the configured key. Without credential encryption
	// only the configured key applies. Project bindings are not consulted.
	ConnectionPrecedence CredentialPrecedence = iota
	// OAuthPrecedence is ResolveProviderOAuthCredentialSecretWithObservation's
	// order, used by Copilot: a human gets their active default connection,
	// then their legacy provider_credentials row; any other principal gets
	// the gateway-owned credential its project binds for its kind. The
	// credential must be OAuth, otherwise resolution fails. Callers without a
	// principal resolve nothing. It also reproduces the owner-private Codex
	// and Antigravity resolution, because no legacy row or binding can exist
	// for those providers.
	OAuthPrecedence
)

// Resolve implements core.CredentialStore. It reads identifiers and kinds
// only; Load decrypts, so a credential that fails to decrypt fails there and,
// as today, never falls through to the next source.
//
// A caller maps onto the gateway's config.Principal as follows. A human or
// service caller is the principal with that ID and kind (PrincipalKind "human"
// or "service") acting in ProjectID. Anonymous and local callers, and the
// reserved IDs of the static admin and external keys, carry no principal, so
// they skip personal credentials and project bindings.
func (s *CredentialStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := caller.Validate(); err != nil {
		return "", fmt.Errorf("resolve credential: %w", err)
	}
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return "", core.ErrNoCredential
	}
	precedence := ConnectionPrecedence
	if s.precedence != nil {
		precedence = s.precedence(instance)
	}
	principal := callerPrincipal(caller)
	switch precedence {
	case ConnectionPrecedence:
		return s.resolveConnectionPrecedence(ctx, principal, instance)
	case OAuthPrecedence:
		return s.resolveOAuthPrecedence(ctx, principal, instance)
	default:
		return "", fmt.Errorf("unknown credential precedence %d", precedence)
	}
}

func callerPrincipal(caller core.Caller) *config.Principal {
	id := CallerPrincipalID(caller)
	if id == "" {
		return nil
	}
	kind := "service"
	if caller.Kind == core.CallerHuman {
		kind = "human"
	}
	return &config.Principal{PrincipalID: id, PrincipalKind: kind, ProjectID: caller.ProjectID}
}

func (s *CredentialStore) resolveConnectionPrecedence(
	ctx context.Context, principal *config.Principal, providerID string,
) (string, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return configuredCredentialKey(providerID)
	}
	if principal != nil && principal.PrincipalID != "" {
		id, _, found, err := s.activeConnection(ctx, "c.principal_id=?", strings.TrimSpace(principal.PrincipalID), providerID)
		if err != nil || found {
			return id, err
		}
	}
	id, _, found, err := s.activeConnection(ctx, "p.external_subject=?", systemPrincipalSubject, providerID)
	if err != nil || found {
		return id, err
	}
	return configuredCredentialKey(providerID)
}

// configuredCredentialKey names the configured key, which the factory falls
// back to even when it is empty; an empty key means no credential.
func configuredCredentialKey(providerID string) (string, error) {
	if strings.TrimSpace(configuredCredential(providerID)) == "" {
		return "", core.ErrNoCredential
	}
	return ConfiguredCredentialKeyPrefix + providerID, nil
}

// activeConnection selects the default connection of one owner exactly as
// providerConnectionSecretObserved does. owner is a trusted SQL condition on
// the connection or its owner.
func (s *CredentialStore) activeConnection(
	ctx context.Context, owner, ownerValue, providerID string,
) (id, kind string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT c.id,c.credential_kind
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
WHERE `+owner+` AND c.provider_id=? AND c.status='active' AND p.status='active'
ORDER BY c.is_default DESC,c.updated_at DESC,c.connection_name LIMIT 1`,
		ownerValue, providerID,
	).Scan(&id, &kind)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return id, kind, err == nil, err
}

func (s *CredentialStore) resolveOAuthPrecedence(
	ctx context.Context, principal *config.Principal, providerID string,
) (string, error) {
	if principal == nil || strings.TrimSpace(principal.PrincipalID) == "" {
		return "", core.ErrNoCredential
	}
	if principal.PrincipalKind == "human" {
		id, kind, found, err := s.activeConnection(ctx, "c.principal_id=?", strings.TrimSpace(principal.PrincipalID), providerID)
		if err != nil {
			return "", err
		}
		if found {
			if !isOAuthCredentialKind(kind) {
				return "", fmt.Errorf("active provider connection is not an OAuth credential")
			}
			return id, nil
		}
		id, kind, found, err = s.legacyCredential(ctx, principal.PrincipalID, providerID)
		if err != nil || !found {
			return "", noCredential(err)
		}
		if !isOAuthCredentialKind(kind) {
			return "", fmt.Errorf("legacy provider credential is not an OAuth credential")
		}
		return ProviderCredentialKeyPrefix + id, nil
	}
	id, kind, found, err := s.boundCredential(ctx, principal, providerID)
	if err != nil || !found {
		return "", noCredential(err)
	}
	if !isOAuthCredentialKind(kind) {
		return "", fmt.Errorf("bound provider credential is not an OAuth credential")
	}
	return ProviderCredentialKeyPrefix + id, nil
}

func noCredential(err error) error {
	if err != nil {
		return err
	}
	return core.ErrNoCredential
}

// legacyCredential selects a principal's provider_credentials row as
// ProviderCredentialSecretWithKind does.
func (s *CredentialStore) legacyCredential(
	ctx context.Context, principalID, providerID string,
) (id, kind string, found bool, err error) {
	var status string
	err = s.db.QueryRowContext(ctx, `
SELECT c.id,c.status,c.credential_kind
FROM provider_credentials c
JOIN principals p ON p.id=c.principal_id
WHERE c.principal_id=? AND c.provider_id=? AND p.status='active'`,
		principalID, providerID,
	).Scan(&id, &status, &kind)
	if err == sql.ErrNoRows || (err == nil && status != "active") {
		return "", "", false, nil
	}
	return id, kind, err == nil, err
}

// boundCredential selects the gateway-owned credential a project binding
// shares, with every status and membership check of
// boundProviderCredentialSecretWithKind.
func (s *CredentialStore) boundCredential(
	ctx context.Context, principal *config.Principal, providerID string,
) (id, kind string, found bool, err error) {
	if strings.TrimSpace(principal.ProjectID) == "" || !validPrincipalKind(principal.PrincipalKind) {
		return "", "", false, nil
	}
	err = s.db.QueryRowContext(ctx, `
SELECT c.id,c.credential_kind
FROM provider_credential_bindings b
JOIN projects p ON p.id=b.project_id AND p.status='active'
JOIN project_memberships m ON m.project_id=p.id AND m.principal_id=?
JOIN principals n ON n.id=m.principal_id AND n.status='active' AND n.kind=?
JOIN provider_credentials c ON c.id=b.credential_id
 AND c.provider_id=b.provider_id AND c.status='active'
JOIN principals o ON o.id=c.principal_id AND o.kind='system' AND o.status='active'
WHERE b.project_id=? AND b.provider_id=? AND b.principal_kind=? AND b.status='active'`,
		principal.PrincipalID, principal.PrincipalKind, principal.ProjectID,
		providerID, principal.PrincipalKind,
	).Scan(&id, &kind)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return id, kind, err == nil, err
}
