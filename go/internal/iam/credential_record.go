package iam

import (
	"errors"
	"maps"
	"strings"
	"time"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

// Metadata keys under which CredentialStore records carry the OAuth envelope
// fields tokenstore.Record has no field for; a key whose field is empty is
// omitted. Any other metadata key is kept verbatim in the envelope, so
// nothing a record or an envelope holds is lost in either direction.
const (
	CredentialMetadataAccountLabel      = "account_label"
	CredentialMetadataProjectID         = "project_id"
	CredentialMetadataOAuthProfile      = "oauth_profile"
	CredentialMetadataOAuthClientID     = "oauth_client_id"
	CredentialMetadataOAuthClientMode   = "oauth_client_mode"
	CredentialMetadataOAuthRedirectURI  = "oauth_redirect_uri"
	CredentialMetadataOAuthClientSecret = "oauth_client_secret"
	CredentialMetadataOAuthStatus       = "oauth_status"
)

// envelopeMetadataFields binds each metadata key to its envelope field.
var envelopeMetadataFields = map[string]func(*OAuthTokenEnvelope) *string{
	CredentialMetadataAccountLabel:      func(e *OAuthTokenEnvelope) *string { return &e.AccountLabel },
	CredentialMetadataProjectID:         func(e *OAuthTokenEnvelope) *string { return &e.ProjectID },
	CredentialMetadataOAuthProfile:      func(e *OAuthTokenEnvelope) *string { return &e.OAuthProfile },
	CredentialMetadataOAuthClientID:     func(e *OAuthTokenEnvelope) *string { return &e.OAuthClientID },
	CredentialMetadataOAuthClientMode:   func(e *OAuthTokenEnvelope) *string { return &e.OAuthClientMode },
	CredentialMetadataOAuthRedirectURI:  func(e *OAuthTokenEnvelope) *string { return &e.OAuthRedirectURI },
	CredentialMetadataOAuthClientSecret: func(e *OAuthTokenEnvelope) *string { return &e.OAuthClientSecret },
	CredentialMetadataOAuthStatus:       func(e *OAuthTokenEnvelope) *string { return &e.Status },
}

var errCredentialKindMismatch = errors.New("record does not match the connection's credential kind")

// connectionRecord maps a decrypted connection secret to a record. OAuth
// kinds hold an envelope; API keys become core.APIKeyRecord; any other kind,
// such as a service-account key or a setup token, keeps its raw secret with
// the kind's token type, so the consumer can tell how to use it.
func connectionRecord(kind, secret string) (tokenstore.Record, error) {
	kind = strings.TrimSpace(kind)
	switch {
	case strings.EqualFold(kind, core.TokenTypeAPIKey):
		return core.APIKeyRecord(secret), nil
	case isOAuthCredentialKind(kind):
		envelope, err := decodeOAuthEnvelope(secret)
		if err != nil {
			return tokenstore.Record{}, err
		}
		return envelopeRecord(envelope), nil
	default:
		return tokenstore.Record{AccessToken: secret, TokenType: connectionTokenType(kind)}, nil
	}
}

// connectionTokenType is the token type of a record of a connection of a
// kind that is not OAuth. A setup token is the one kind whose name llmgw-core
// spells differently: core's Anthropic reads a setup token only as
// core.TokenTypeAnthropicSetupToken, and would refuse one of the connection's
// kind. Every other kind is its own token type, a service-account key's
// spelling being core's already.
func connectionTokenType(kind string) string {
	switch {
	case strings.EqualFold(kind, core.TokenTypeAPIKey):
		return core.TokenTypeAPIKey
	case strings.EqualFold(kind, string(anthropicauth.CredentialSetupToken)):
		return core.TokenTypeAnthropicSetupToken
	}
	return kind
}

// legacyCredentialRecord maps a provider_credentials secret. Those rows hold
// raw tokens, never an envelope, and the resolvers hand them out verbatim.
func legacyCredentialRecord(kind, secret string) tokenstore.Record {
	kind = strings.TrimSpace(kind)
	switch {
	case strings.EqualFold(kind, core.TokenTypeAPIKey):
		return core.APIKeyRecord(secret)
	case isOAuthCredentialKind(kind):
		return tokenstore.Record{AccessToken: secret}
	default:
		return tokenstore.Record{AccessToken: secret, TokenType: connectionTokenType(kind)}
	}
}

func envelopeRecord(envelope OAuthTokenEnvelope) tokenstore.Record {
	record := tokenstore.Record{
		AccessToken: envelope.AccessToken, RefreshToken: envelope.RefreshToken,
		IDToken: envelope.IDToken, TokenType: envelope.TokenType, AccountID: envelope.AccountID,
	}
	if envelope.ExpiresAt > 0 {
		record.Expiry = time.Unix(envelope.ExpiresAt, 0)
	}
	metadata := maps.Clone(envelope.Metadata)
	for key, field := range envelopeMetadataFields {
		if value := *field(&envelope); value != "" {
			if metadata == nil {
				metadata = map[string]string{}
			}
			metadata[key] = value
		}
	}
	record.Metadata = metadata
	return record
}

// connectionSecret is the inverse of connectionRecord for a connection of
// kind. A record the kind cannot hold is rejected rather than truncated.
func connectionSecret(kind string, record tokenstore.Record) (string, error) {
	if strings.TrimSpace(record.AccessToken) == "" {
		return "", errors.New("credential record has no access token")
	}
	kind = strings.TrimSpace(kind)
	if isOAuthCredentialKind(kind) {
		if record.TokenType == core.TokenTypeAPIKey {
			return "", errCredentialKindMismatch
		}
		envelope, err := recordEnvelope(record)
		if err != nil {
			return "", err
		}
		return encodeOAuthEnvelope(envelope)
	}
	if !strings.EqualFold(record.TokenType, connectionTokenType(kind)) || record.RefreshToken != "" ||
		record.IDToken != "" || !record.Expiry.IsZero() || record.AccountID != "" ||
		len(record.Metadata) != 0 {
		return "", errCredentialKindMismatch
	}
	if strings.EqualFold(kind, gcpauth.CredentialKind) {
		// Parse before storing, as PutProviderConnection does, so a malformed
		// key fails here instead of on the request path.
		if _, err := gcpauth.Parse([]byte(strings.TrimSpace(record.AccessToken))); err != nil {
			return "", err
		}
	}
	// PutProviderConnection stores every secret trimmed.
	return strings.TrimSpace(record.AccessToken), nil
}

// recordEnvelope builds the envelope PutOAuthProviderConnection and
// ReplaceOAuthProviderConnectionIfCurrent would store: both trim every field
// except the client secret, which is kept byte for byte.
func recordEnvelope(record tokenstore.Record) (OAuthTokenEnvelope, error) {
	envelope := OAuthTokenEnvelope{
		AccessToken: record.AccessToken, RefreshToken: record.RefreshToken,
		IDToken: record.IDToken, TokenType: record.TokenType, AccountID: record.AccountID,
	}
	if !record.Expiry.IsZero() {
		envelope.ExpiresAt = record.Expiry.Unix()
		if envelope.ExpiresAt <= 0 {
			return OAuthTokenEnvelope{}, errors.New("credential expiry predates the Unix epoch")
		}
	}
	for key, value := range record.Metadata {
		if field, known := envelopeMetadataFields[key]; known {
			*field(&envelope) = value
			continue
		}
		if envelope.Metadata == nil {
			envelope.Metadata = map[string]string{}
		}
		envelope.Metadata[key] = value
	}
	for _, field := range []*string{
		&envelope.AccessToken, &envelope.RefreshToken, &envelope.IDToken, &envelope.TokenType,
		&envelope.AccountID, &envelope.AccountLabel, &envelope.ProjectID, &envelope.OAuthProfile,
		&envelope.OAuthClientID, &envelope.OAuthClientMode, &envelope.OAuthRedirectURI, &envelope.Status,
	} {
		*field = strings.TrimSpace(*field)
	}
	return envelope, nil
}
