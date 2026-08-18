package iam

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
)

func newID(prefix string) (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func normalizeSlug(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "", fmt.Errorf("slug must contain a letter or number")
	}
	return out, nil
}

func displayPrefix(token string) string {
	const visible = 16
	if len(token) <= visible {
		return token
	}
	return token[:visible] + "..."
}
