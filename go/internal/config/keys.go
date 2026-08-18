package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// KeyEntry is one minted project key (stored value).
type keyEntry struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	// Governance (all optional; zero/empty = unrestricted).
	Disabled         bool     `json:"disabled,omitempty"`
	ExpiresAt        int64    `json:"expires_at,omitempty"`        // unix seconds; 0 = never
	AllowedModels    []string `json:"allowed_models,omitempty"`    // exact request models (category or provider/model); empty = all
	AllowedProviders []string `json:"allowed_providers,omitempty"` // provider ids the routed target may use; empty = all
	RPM              int      `json:"rpm,omitempty"`               // requests/minute; 0 = unlimited
	DailyRequests    int      `json:"daily_requests,omitempty"`    // requests/day (UTC); 0 = unlimited
}

// KeyInfo is the admin-surface view of a key (token included — local tool).
type KeyInfo struct {
	Token            string   `json:"token"`
	Project          string   `json:"project"`
	Name             string   `json:"name"`
	Created          int64    `json:"created"`
	Disabled         bool     `json:"disabled"`
	ExpiresAt        int64    `json:"expires_at,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	AllowedProviders []string `json:"allowed_providers,omitempty"`
	RPM              int      `json:"rpm,omitempty"`
	DailyRequests    int      `json:"daily_requests,omitempty"`
	Expired          bool     `json:"expired"`
}

// Principal is the resolved identity + governance envelope of a request.
type Principal struct {
	PrincipalID         string   `json:"principal_id,omitempty"`
	PrincipalKind       string   `json:"principal_kind,omitempty"`
	ProjectID           string   `json:"project_id,omitempty"`
	KeyID               string   `json:"key_id,omitempty"`
	Role                string   `json:"role,omitempty"`
	Project             string   `json:"project"`
	Key                 string   `json:"key"`
	Token               string   `json:"-"`
	AllowedModels       []string `json:"-"`
	AllowedProviders    []string `json:"-"`
	RPM                 int      `json:"-"`
	DailyRequests       int      `json:"-"`
	MonthlyRequests     int      `json:"-"`
	DailyInputTokens    int64    `json:"-"`
	DailyOutputTokens   int64    `json:"-"`
	MonthlyTotalTokens  int64    `json:"-"`
	DailyCostMicroUSD   int64    `json:"-"`
	MonthlyCostMicroUSD int64    `json:"-"`
	DailyCreditsMilli   int64    `json:"-"`
	MonthlyCreditsMilli int64    `json:"-"`
}

func loadKeys() map[string]keyEntry {
	out := map[string]keyEntry{}
	b, err := os.ReadFile(keysFilePath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

func writeKeys(data map[string]keyEntry) {
	_ = os.MkdirAll(StateDir(), 0o755)
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.WriteFile(keysFilePath(), b, 0o600)
}

// MintKey creates a new project key and returns the token (shown once).
func MintKey(project, name string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		project = "default"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "key"
	}
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := "llmgw_" + base64.RawURLEncoding.EncodeToString(buf)
	data := loadKeys()
	data[token] = keyEntry{Project: project, Name: name, Created: time.Now().Unix()}
	writeKeys(data)
	return token
}

// RevokeKey removes a token; returns whether it existed.
func RevokeKey(token string) bool {
	data := loadKeys()
	if _, ok := data[token]; ok {
		delete(data, token)
		writeKeys(data)
		return true
	}
	return false
}

// ListKeys returns all minted keys (with full tokens — local tool).
func ListKeys() []KeyInfo {
	data := loadKeys()
	out := []KeyInfo{}
	now := time.Now().Unix()
	for tok, e := range data {
		out = append(out, KeyInfo{
			Token: tok, Project: e.Project, Name: e.Name, Created: e.Created,
			Disabled: e.Disabled, ExpiresAt: e.ExpiresAt,
			AllowedModels: e.AllowedModels, AllowedProviders: e.AllowedProviders,
			RPM: e.RPM, DailyRequests: e.DailyRequests,
			Expired: e.ExpiresAt > 0 && now >= e.ExpiresAt,
		})
	}
	return out
}

// HasKeys reports whether any project keys exist.
func HasKeys() bool { return len(loadKeys()) > 0 }

// KeyUpdate carries editable governance fields for UpdateKey (nil = leave as-is).
type KeyUpdate struct {
	Disabled         *bool
	ExpiresAt        *int64
	AllowedModels    *[]string
	AllowedProviders *[]string
	RPM              *int
	DailyRequests    *int
}

// UpdateKey applies governance changes to an existing token; returns whether it existed.
func UpdateKey(token string, u KeyUpdate) bool {
	data := loadKeys()
	e, ok := data[token]
	if !ok {
		return false
	}
	if u.Disabled != nil {
		e.Disabled = *u.Disabled
	}
	if u.ExpiresAt != nil {
		e.ExpiresAt = *u.ExpiresAt
	}
	if u.AllowedModels != nil {
		e.AllowedModels = *u.AllowedModels
	}
	if u.AllowedProviders != nil {
		e.AllowedProviders = *u.AllowedProviders
	}
	if u.RPM != nil {
		e.RPM = *u.RPM
	}
	if u.DailyRequests != nil {
		e.DailyRequests = *u.DailyRequests
	}
	data[token] = e
	writeKeys(data)
	return true
}

// ResolvePrincipal maps a minted token to its {project, key} identity + governance
// envelope. Returns nil for unknown, disabled, or expired keys.
func ResolvePrincipal(token string) *Principal {
	e, ok := loadKeys()[token]
	if !ok || e.Disabled {
		return nil
	}
	if e.ExpiresAt > 0 && time.Now().Unix() >= e.ExpiresAt {
		return nil
	}
	proj := e.Project
	if proj == "" {
		proj = "default"
	}
	name := e.Name
	if name == "" {
		name = "key"
	}
	return &Principal{
		Project: proj, Key: name, Token: token,
		AllowedModels: e.AllowedModels, AllowedProviders: e.AllowedProviders,
		RPM: e.RPM, DailyRequests: e.DailyRequests,
	}
}
