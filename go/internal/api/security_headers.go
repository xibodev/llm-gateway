package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"

	"llmgw/internal/web"
)

var (
	inlineScriptRE          = regexp.MustCompile(`(?is)<script(\s[^>]*)?>(.*?)</script\s*>`)
	scriptSourceAttributeRE = regexp.MustCompile(`(?i)(?:^|\s)src\s*=`)
)

// securityHeaders sets what every response carries. Management API responses
// hold keys, audit history and usage, so no cache may keep them; a handler
// under those prefixes must not replace that Cache-Control.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/admin/api/") || strings.HasPrefix(r.URL.Path, "/user/api/") {
			header.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// consoleContentSecurityPolicy returns the policy of the console's entry
// document. The document's inline theme script runs before the bundle so the
// first paint already has the right theme; the policy admits it by hash, taken
// from the embedded document so a rebuilt console stays admitted. The
// playground shows the images, audio and video it generates as data: URLs, as
// the provider roster does its logos; nothing in the console creates a blob:
// URL, so none is admitted.
func consoleContentSecurityPolicy(document []byte) string {
	scripts := append([]string{"'self'"}, inlineScriptHashes(document)...)
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"media-src 'self' data:",
		"connect-src 'self'",
		"font-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
	}, "; ")
}

// inlineScriptHashes returns a CSP hash source for each inline script in
// document. A browser hashes a script's text after the HTML parser turned
// CR LF and lone CR into LF, so the text is normalized the same way first.
func inlineScriptHashes(document []byte) []string {
	var sources []string
	for _, script := range inlineScriptRE.FindAllSubmatch(document, -1) {
		if scriptSourceAttributeRE.Match(script[1]) {
			continue
		}
		text := bytes.ReplaceAll(script[2], []byte("\r\n"), []byte("\n"))
		text = bytes.ReplaceAll(text, []byte("\r"), []byte("\n"))
		sum := sha256.Sum256(text)
		sources = append(sources, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return sources
}

// writeConsoleDocument serves the console's entry document under policy. The
// console never runs inside a frame, so no other page can frame it to steer
// an operator's clicks.
func writeConsoleDocument(w http.ResponseWriter, policy string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Content-Security-Policy", policy)
	header.Set("X-Frame-Options", "DENY")
	_, _ = w.Write(web.ConsoleIndex())
}
