package iam

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// OAuthClientProfile is caller-owned OAuth application configuration. Secret
// material is available only to server-side authorization and refresh paths.
type OAuthClientProfile struct {
	ProviderID   string
	Profile      string
	ClientID     string
	ClientSecret string
	ClientMode   string
	RedirectURI  string
}

func oauthClientProfileAAD(providerID, profile string) []byte {
	return []byte("oauth-client-profile|" + providerID + "|" + profile + "|v1")
}

func PutOAuthClientProfile(input OAuthClientProfile) error {
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.Profile = strings.TrimSpace(input.Profile)
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.ClientMode = strings.ToLower(strings.TrimSpace(input.ClientMode))
	input.RedirectURI = strings.TrimSpace(input.RedirectURI)
	if input.ProviderID == "" || input.Profile == "" || input.ClientID == "" || input.RedirectURI == "" {
		return fmt.Errorf("OAuth client profile is incomplete")
	}
	if input.ClientMode != "public" && input.ClientMode != "confidential" {
		return fmt.Errorf("OAuth client mode must be public or confidential")
	}
	if input.ClientMode == "public" {
		input.ClientSecret = ""
	}
	if input.ClientMode == "confidential" && strings.TrimSpace(input.ClientSecret) == "" {
		return fmt.Errorf("confidential OAuth clients require a client secret")
	}
	key, err := credentialKey()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	ciphertext, nonce, err := encryptCredential(key, payload, oauthClientProfileAAD(input.ProviderID, input.Profile))
	if err != nil {
		return err
	}
	db, err := DB()
	if err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO oauth_client_profiles(provider_id,profile,ciphertext,nonce,key_version,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(provider_id,profile) DO UPDATE SET
 ciphertext=excluded.ciphertext,nonce=excluded.nonce,key_version=excluded.key_version,updated_at=excluded.updated_at`,
		input.ProviderID, input.Profile, ciphertext, nonce, 1, time.Now().Unix(),
	)
	return err
}

func OAuthClientProfileByName(providerID, profile string) (OAuthClientProfile, bool, error) {
	providerID, profile = strings.TrimSpace(providerID), strings.TrimSpace(profile)
	db, err := DB()
	if err != nil {
		return OAuthClientProfile{}, false, err
	}
	var ciphertext, nonce []byte
	err = db.QueryRow(`
SELECT ciphertext,nonce FROM oauth_client_profiles WHERE provider_id=? AND profile=?`, providerID, profile).Scan(&ciphertext, &nonce)
	if err == sql.ErrNoRows {
		return OAuthClientProfile{}, false, nil
	}
	if err != nil {
		return OAuthClientProfile{}, false, err
	}
	key, err := credentialKey()
	if err != nil {
		return OAuthClientProfile{}, false, err
	}
	payload, err := decryptCredential(key, ciphertext, nonce, oauthClientProfileAAD(providerID, profile))
	if err != nil {
		return OAuthClientProfile{}, false, err
	}
	var out OAuthClientProfile
	if err := json.Unmarshal(payload, &out); err != nil {
		return OAuthClientProfile{}, false, err
	}
	return out, true, nil
}
