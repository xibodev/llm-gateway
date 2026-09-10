// Package roster maintains discovery data independently of gateway configuration.
package roster

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"time"
	"unicode/utf8"
)

const maxFeedBytes = 4 << 20

type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Payload struct {
	SchemaVersion int      `json:"schema_version"`
	Revision      int64    `json:"revision"`
	PublishedAt   string   `json:"published_at"`
	Entries       []Entry  `json:"entries"`
	Sources       []Source `json:"sources"`
}

type Source struct {
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	URL        string `json:"url,omitempty"`
	Status     string `json:"status,omitempty"`
	License    string `json:"license,omitempty"`
	LicenseURL string `json:"license_url,omitempty"`
	Notice     string `json:"notice,omitempty"`
}

type Entry struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Protocol       string   `json:"protocol"`
	BaseURL        string   `json:"base_url"`
	SignupURL      string   `json:"signup_url,omitempty"`
	DocsURL        string   `json:"docs_url,omitempty"`
	Auth           string   `json:"auth"`
	Offer          string   `json:"offer"`
	Description    string   `json:"description,omitempty"`
	Setup          string   `json:"setup"`
	State          string   `json:"state"`
	Sources        []Source `json:"sources,omitempty"`
	Probe          *Probe   `json:"probe,omitempty"`
	Logo           *Logo    `json:"logo,omitempty"`
	Report         *Report  `json:"report,omitempty"`
	OfferExpiresAt string   `json:"offer_expires_at,omitempty"`
	Requirements   []string `json:"requirements,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
}

type Probe struct {
	Status     string `json:"status"`
	CheckedAt  string `json:"checked_at"`
	HTTPStatus int    `json:"http_status"`
}

type Logo struct {
	MIME      string `json:"mime"`
	Data      string `json:"data"`
	SHA256    string `json:"sha256"`
	License   string `json:"license"`
	SourceURL string `json:"source_url"`
}

type Report struct {
	IssueURL      string `json:"issue_url"`
	Confirmations int    `json:"confirmations"`
	Reason        string `json:"reason"`
}

// Snapshot is the shared admin/portal response. Empty timestamps are unknown;
// logo.data is base64 raster bytes, for a locally constructed data URI.
type Snapshot struct {
	Entries     []Entry  `json:"entries"`
	Revision    int64    `json:"revision"`
	PublishedAt string   `json:"published_at"`
	LastChecked string   `json:"last_checked"`
	LastSuccess string   `json:"last_success"`
	AutoRefresh bool     `json:"auto_refresh"`
	Configured  bool     `json:"configured"`
	Error       string   `json:"error"`
	Stale       bool     `json:"stale"`
	Sources     []Source `json:"sources"`
}

func verify(raw []byte, keys map[string]ed25519.PublicKey, now time.Time) (Payload, [32]byte, error) {
	var p Payload
	var digest [32]byte
	invalid := errors.New("Roster signature or payload is invalid.")
	var env Envelope
	if len(raw) > maxFeedBytes || json.Unmarshal(raw, &env) != nil {
		return p, digest, invalid
	}
	key := keys[env.KeyID]
	data, err := base64.StdEncoding.Strict().DecodeString(env.Payload)
	sig, sigErr := base64.StdEncoding.Strict().DecodeString(env.Signature)
	if err != nil || sigErr != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, data, sig) || !utf8.Valid(data) {
		return p, digest, invalid
	}
	if json.Unmarshal(data, &p) != nil || !validPayload(&p, now) {
		return Payload{}, digest, invalid
	}
	return p, sha256.Sum256(data), nil
}

// verifyPlain validates a plain JSON payload without Ed25519 signing. The
// transport is public HTTPS with strict structural validation. This is the
// normal path when no signing keys are configured.
func verifyPlain(raw []byte, now time.Time) (Payload, [32]byte, error) {
	var p Payload
	if len(raw) > maxFeedBytes || !utf8.Valid(raw) || json.Unmarshal(raw, &p) != nil || !validPayload(&p, now) {
		return Payload{}, [32]byte{}, errors.New("Roster payload is invalid.")
	}
	return p, sha256.Sum256(raw), nil
}

func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

func validPayload(p *Payload, now time.Time) bool {
	published, err := time.Parse(time.RFC3339, p.PublishedAt)
	// Keep revisions exactly representable by JSON browser clients. A signed
	// future timestamp must not pin an installation to an unusable high-water mark.
	if err != nil || p.SchemaVersion != 1 || p.Revision < 1 || p.Revision > 1<<53-1 || published.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) || published.After(now.Add(10*time.Minute)) || len(p.Entries) > 5000 {
		return false
	}
	seen := make(map[string]bool)
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.Auth == "" {
			e.Auth = "unknown"
		}
		if e.Offer == "" {
			e.Offer = "unknown"
		}
		if e.Setup == "" {
			e.Setup = "candidate"
		}
		if e.Protocol == "" {
			e.Protocol = "unknown"
		}
		if e.ID == "" || len(e.ID) > 128 || strings.ContainsAny(e.ID, " /\\\r\n\t") || seen[e.ID] || strings.TrimSpace(e.Name) == "" || len(e.Name) > 256 || len(e.Description) > 8192 {
			return false
		}
		seen[e.ID] = true
		if !oneOf(e.Protocol, "openai", "anthropic", "unknown") || !oneOf(e.Auth, "api_key", "none", "unknown") || !oneOf(e.Offer, "free_tier", "recurring_credit", "trial", "paid", "unknown") || !oneOf(e.Setup, "compatible", "candidate") || !oneOf(e.State, "active", "quarantined", "withdrawn") {
			return false
		}
		if e.Setup == "compatible" && (e.Auth == "unknown" || e.Protocol == "unknown") {
			return false
		}
		if _, err := publicURL(e.BaseURL); err != nil {
			return false
		}
		if !optionalURL(e.SignupURL) || !optionalURL(e.DocsURL) || !validSources(e.Sources) || !optionalTime(e.OfferExpiresAt) {
			return false
		}
		if e.Probe != nil && (!oneOf(e.Probe.Status, "not_checked", "reachable", "auth_required", "rate_limited", "failed", "blocked") || !optionalTime(e.Probe.CheckedAt) || e.Probe.HTTPStatus < 0 || e.Probe.HTTPStatus > 599) {
			return false
		}
		if e.Report != nil && (!optionalURL(e.Report.IssueURL) || e.Report.Confirmations < 0) {
			return false
		}
		if e.Logo != nil && !validLogo(e.Logo) {
			return false
		}
	}
	if p.Entries == nil {
		p.Entries = []Entry{}
	}
	if p.Sources == nil {
		p.Sources = []Source{}
	}
	return validSources(p.Sources)
}

func optionalTime(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func optionalURL(s string) bool {
	if s == "" {
		return true
	}
	_, err := publicURL(s)
	return err == nil
}

func validSources(sources []Source) bool {
	for _, s := range sources {
		if !optionalURL(s.URL) || !optionalURL(s.LicenseURL) {
			return false
		}
	}
	return true
}

func staleSources(sources []Source) bool {
	for _, source := range sources {
		if source.Status == "stale" {
			return true
		}
	}
	return false
}

func validLogo(l *Logo) bool {
	if !optionalURL(l.SourceURL) {
		return false
	}
	if l.Data == "" {
		return l.SHA256 == "" && oneOf(l.MIME, "", "image/png", "image/jpeg")
	}
	if !oneOf(l.MIME, "image/png", "image/jpeg") || len(l.Data) > base64.StdEncoding.EncodedLen(128<<10) {
		return false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(l.Data)
	if err != nil {
		return false
	}
	h := sha256.Sum256(b)
	if l.SHA256 != hex.EncodeToString(h[:]) {
		return false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || "image/"+format != l.MIME || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 {
		return false
	}
	// Decode the actual image too: a valid header alone can hide truncated data.
	_, _, err = image.Decode(bytes.NewReader(b))
	return err == nil
}
