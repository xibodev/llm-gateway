package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/web"
)

const immutableAsset = "public, max-age=31536000, immutable"

var (
	// startTagRE matches a start tag whose attribute values may be quoted, so
	// a ">" inside a value does not end the tag.
	startTagRE  = regexp.MustCompile(`<([a-zA-Z][^\s/>]*)((?:\s*[^\s"'>/=]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'>]+))?|\s*/)*)\s*>`)
	attributeRE = regexp.MustCompile(`([^\s"'>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
)

// htmlStartTag is a start tag of a document and, for a script, the text
// between it and its end tag.
type htmlStartTag struct {
	name       string
	attributes map[string]string
	text       string
}

// readStartTags reads the start tags of a well-formed document independently
// of the policy builder: comments are skipped, and a script's text runs to its
// end tag, as it does in a browser.
func readStartTags(t *testing.T, document string) []htmlStartTag {
	t.Helper()
	var tags []htmlStartTag
	for at := 0; at < len(document); {
		rest := document[at:]
		match := startTagRE.FindStringSubmatchIndex(rest)
		if match == nil {
			break
		}
		if comment := strings.Index(rest, "<!--"); comment >= 0 && comment < match[0] {
			end := strings.Index(rest[comment+len("<!--"):], "-->")
			if end < 0 {
				t.Fatal("unterminated comment")
			}
			at += comment + len("<!--") + end + len("-->")
			continue
		}
		tag := htmlStartTag{name: strings.ToLower(rest[match[2]:match[3]]), attributes: map[string]string{}}
		for _, attribute := range attributeRE.FindAllStringSubmatch(rest[match[4]:match[5]], -1) {
			tag.attributes[strings.ToLower(attribute[1])] = attribute[2] + attribute[3] + attribute[4]
		}
		at += match[1]
		if tag.name == "script" {
			end := strings.Index(strings.ToLower(document[at:]), "</script")
			if end < 0 {
				t.Fatal("unterminated script")
			}
			tag.text = document[at : at+end]
			at += end
		}
		tags = append(tags, tag)
	}
	return tags
}

// scriptHashSource is the source that admits an inline script with text,
// hashed after the newline normalization a browser's parser applies.
func scriptHashSource(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	sum := sha256.Sum256([]byte(text))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// policyDirective returns the sources policy lists under directive.
func policyDirective(policy, directive string) []string {
	for _, part := range strings.Split(policy, ";") {
		if fields := strings.Fields(part); len(fields) > 0 && fields[0] == directive {
			return fields[1:]
		}
	}
	return nil
}

// wantConsolePolicy is the policy of the console document, its inline
// scripts hashed from the embedded document.
func wantConsolePolicy(t *testing.T) string {
	t.Helper()
	scripts := []string{"'self'"}
	for _, tag := range readStartTags(t, string(web.ConsoleIndex())) {
		if _, external := tag.attributes["src"]; tag.name == "script" && !external {
			scripts = append(scripts, scriptHashSource(tag.text))
		}
	}
	return "default-src 'self'; script-src " + strings.Join(scripts, " ") + "; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; media-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; " +
		"base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
}

// useHeaderProbe serves fresh state with the static administrator key
// fixture-admin and an echo provider.
func useHeaderProbe(t *testing.T) http.Handler {
	t.Helper()
	resetState(t)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey, s.APIKeys, s.AllowUnauthenticatedAPI, s.SSOEnabled = "fixture-admin", nil, false, false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	return NewServer(Runtime{})
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func probe(handler http.Handler, method, target, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return serve(handler, request)
}

func consoleScriptAsset(t *testing.T) string {
	t.Helper()
	asset := regexp.MustCompile(`src="(/console/assets/[^"]+\.js)"`).FindStringSubmatch(string(web.ConsoleIndex()))
	if len(asset) != 2 {
		t.Fatal("the console document names no script asset")
	}
	return asset[1]
}

// Each class of response carries the headers that fit what it holds.
func TestSecurityHeadersByResponseClass(t *testing.T) {
	handler := useHeaderProbe(t)
	t.Setenv("LLMGW_MAX_REQUEST_BODY_BYTES", strconv.Itoa(lowestMaxRequestBodyBytes))
	index, policy := string(web.ConsoleIndex()), wantConsolePolicy(t)
	get := func(target, token string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		return request
	}
	overLimit := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	overLimit.ContentLength = lowestMaxRequestBodyBytes + 1

	type expectation struct {
		class   string
		request *http.Request
		status  int
		// contentType is a part of the response's Content-Type.
		contentType, cacheControl, policy, frameOptions string
	}
	cases := []expectation{
		{"data-plane JSON", get("/v1/models", "fixture-admin"), http.StatusOK, "application/json", "", "", ""},
		{"admin API", get("/admin/api/state", "fixture-admin"), http.StatusOK, "application/json", "no-store", "", ""},
		{"asset", get(consoleScriptAsset(t), ""), http.StatusOK, "javascript", immutableAsset, "", ""},
		{"missing asset", get("/console/assets/missing.js", ""), http.StatusNotFound, "text/plain", "", "", ""},
		{"OAuth page", get("http://127.0.0.1:8787/oauth/callback/google_antigravity?code=fixture&state=fixture", ""),
			http.StatusBadRequest, "text/plain", "", oauthCallbackContentSecurityPolicy, "DENY"},
		{"body over the limit", overLimit, http.StatusRequestEntityTooLarge, "application/json", "", "", ""},
	}
	for _, target := range []string{"/console", "/console/", "/console/index.html", "/console/providers"} {
		cases = append(cases, expectation{"console HTML " + target, get(target, ""), http.StatusOK, "text/html", "no-cache", policy, "DENY"})
	}
	for _, target := range []string{"/portal", "/portal/"} {
		cases = append(cases, expectation{"portal HTML " + target, get(target, ""), http.StatusOK, "text/html", "no-cache", policy, "DENY"})
	}

	for _, c := range cases {
		response := serve(handler, c.request)
		header := response.Header()
		if response.Code != c.status || !strings.Contains(header.Get("Content-Type"), c.contentType) {
			t.Errorf("%s: status=%d content-type=%q, want %d and %s", c.class, response.Code, header.Get("Content-Type"), c.status, c.contentType)
		}
		if c.contentType == "text/html" && response.Body.String() != index {
			t.Errorf("%s: the body is not the console document", c.class)
		}
		for _, want := range [][2]string{
			{"X-Content-Type-Options", "nosniff"}, {"Referrer-Policy", "same-origin"}, {"Cache-Control", c.cacheControl},
			{"Content-Security-Policy", c.policy}, {"X-Frame-Options", c.frameOptions},
		} {
			if got := header.Get(want[0]); got != want[1] {
				t.Errorf("%s: %s=%q, want %q", c.class, want[0], got, want[1])
			}
		}
	}
}

// The console starts only if the browser runs every script of its document, so
// each must be one the policy served with it admits: a script the gateway
// serves, or an inline script whose hash the policy lists. An inline event
// handler is an inline script no hash admits.
func TestEveryScriptOfTheConsoleDocumentIsAdmitted(t *testing.T) {
	handler := useHeaderProbe(t)
	policy := probe(handler, http.MethodGet, "/console", "").Header().Get("Content-Security-Policy")
	admitted := policyDirective(policy, "script-src")
	scripts := 0
	for _, tag := range readStartTags(t, string(web.ConsoleIndex())) {
		for name, value := range tag.attributes {
			if strings.HasPrefix(name, "on") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "javascript:") {
				t.Errorf("<%s %s=%q> is an inline script no policy hash admits", tag.name, name, value)
			}
		}
		if tag.name != "script" {
			continue
		}
		scripts++
		if source, external := tag.attributes["src"]; external {
			if parsed, err := url.Parse(source); err != nil || parsed.Scheme != "" || parsed.Host != "" {
				t.Errorf("script %q is not served by the gateway, the only origin script-src admits", source)
			}
			continue
		}
		if !slices.Contains(admitted, scriptHashSource(tag.text)) {
			t.Errorf("the inline script %q is not admitted by script-src %v", tag.text, admitted)
		}
	}
	if scripts == 0 {
		t.Fatal("read no script from the console document")
	}
}

// The policy builder hashes what a browser hashes: the text of each inline
// script with its newlines normalized, and nothing of a script with a src.
func TestInlineScriptHashesFollowTheBrowser(t *testing.T) {
	document := "<head><script>first()</script>\r\n" +
		`<script type="module" crossorigin src="/console/assets/app.js"></script>` +
		"<SCRIPT nonce=\"n\">\r\n  second()\r\n</SCRIPT >" +
		"<script\n  data-src=\"x\">third()\rfourth()</script>" +
		`<script-loader>fifth()</script-loader></head>`
	got := inlineScriptHashes([]byte(document))
	want := []string{scriptHashSource("first()"), scriptHashSource("\n  second()\n"), scriptHashSource("third()\nfourth()")}
	if !slices.Equal(got, want) {
		t.Fatalf("hashes=%v, want %v", got, want)
	}
}

// Management responses hold keys, audit history and usage whatever their
// status, so no browser or shared cache may keep one, a newly created key
// least of all.
func TestManagementAPIResponsesAreNeverCached(t *testing.T) {
	handler := useHeaderProbe(t)
	t.Setenv("LLMGW_MAX_REQUEST_BODY_BYTES", strconv.Itoa(lowestMaxRequestBodyBytes))
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "fixture-proxy-secret", true
	})
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:fixture-user", "fixture@example.com", "Fixture User")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("fixture-project", "Fixture Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "member"); err != nil {
		t.Fatal(err)
	}
	admin := func(method, target, body string) *http.Request {
		request := httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer fixture-admin")
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	portal := func(method, target, body string) *http.Request {
		request := httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set(ssoSecretHeader, "fixture-proxy-secret")
		request.Header.Set(ssoSubjectHeader, "fixture-user")
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	overLimit := admin(http.MethodPost, "/admin/api/keys", "{}")
	overLimit.ContentLength = lowestMaxRequestBodyBytes + 1

	for _, c := range []struct {
		name    string
		request *http.Request
		status  int
		newKey  bool
	}{
		{"admin key creation", admin(http.MethodPost, "/admin/api/keys", `{"name":"fixture"}`), http.StatusOK, true},
		{"portal key creation", portal(http.MethodPost, "/user/api/keys", `{"project_id":"`+project.ID+`","name":"fixture"}`), http.StatusOK, true},
		{"admin state", admin(http.MethodGet, "/admin/api/state", ""), http.StatusOK, false},
		{"admin audit", admin(http.MethodGet, "/admin/api/audit", ""), http.StatusOK, false},
		{"portal profile", portal(http.MethodGet, "/user/api/me", ""), http.StatusOK, false},
		{"unauthenticated", httptest.NewRequest(http.MethodGet, "/admin/api/state", nil), http.StatusUnauthorized, false},
		{"unknown route", admin(http.MethodGet, "/admin/api/missing", ""), http.StatusNotFound, false},
		{"wrong method", admin(http.MethodPut, "/admin/api/state", ""), http.StatusMethodNotAllowed, false},
		{"body over the limit", overLimit, http.StatusRequestEntityTooLarge, false},
	} {
		response := serve(handler, c.request)
		var issued struct {
			Token string `json:"token"`
		}
		if response.Code != c.status || c.newKey && (json.Unmarshal(response.Body.Bytes(), &issued) != nil || issued.Token == "") {
			// The body is left out: it can hold a key.
			t.Errorf("%s: status=%d, want %d", c.name, response.Code, c.status)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control=%q, want no-store", c.name, got)
		}
	}
	if got := probe(handler, http.MethodGet, "/health", "").Header().Get("Cache-Control"); got != "" {
		t.Errorf("/health: Cache-Control=%q, but only management responses are no-store", got)
	}
}

// A built asset is named by its content, so a browser may keep it for good; a
// missing one is a 404 rather than the console document in its place.
func TestConsoleAssetsAreImmutableAndMissingOnesAreNotFound(t *testing.T) {
	handler := useHeaderProbe(t)
	assets, err := fs.ReadDir(web.ConsoleFS, "console/dist/assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("read the built assets: %v", err)
	}
	named := regexp.MustCompile(`^[^/]+-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$`)
	for _, asset := range assets {
		if asset.IsDir() {
			continue
		}
		if !named.MatchString(asset.Name()) {
			t.Errorf("assets/%s carries no content hash, so a browser keeping it for good would keep it stale", asset.Name())
		}
		response := probe(handler, http.MethodGet, "/console/assets/"+asset.Name(), "")
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != immutableAsset {
			t.Errorf("assets/%s: status=%d Cache-Control=%q", asset.Name(), response.Code, response.Header().Get("Cache-Control"))
		}
	}
	if got := probe(handler, http.MethodGet, "/console/favicon.svg", "").Header().Get("Cache-Control"); got != "" {
		t.Errorf("the favicon carries no content hash but was sent Cache-Control=%q", got)
	}
	for _, missing := range []string{"/console/assets/missing.js", "/console/assets/index-00000000.css", "/console/assets/"} {
		response := probe(handler, http.MethodGet, missing, "")
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), `id="app"`) {
			t.Errorf("%s: status=%d, want 404 without the console document", missing, response.Code)
		}
		if got := response.Header().Get("Cache-Control"); got != "" {
			t.Errorf("%s: a missing asset was sent Cache-Control=%q", missing, got)
		}
	}
}

// The console policy admits media from data: URLs but not from blob: URLs,
// which holds only while the console creates none.
func TestConsoleBundleCreatesNoObjectURLs(t *testing.T) {
	assets, err := fs.ReadDir(web.ConsoleFS, "console/dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range assets {
		if !strings.HasSuffix(asset.Name(), ".js") {
			continue
		}
		script, err := web.ConsoleAsset("assets/" + asset.Name())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(script, []byte("createObjectURL")) {
			t.Errorf("assets/%s creates object URLs; admit blob: in consoleContentSecurityPolicy where the console uses them", asset.Name())
		}
	}
}

// The OAuth callback's policy admits nothing, so the page it shows once a flow
// completed may only hold text.
func TestOAuthConnectedPageLoadsNothing(t *testing.T) {
	if !slices.Equal(policyDirective(oauthCallbackContentSecurityPolicy, "default-src"), []string{"'none'"}) {
		t.Fatalf("policy %q admits a default source", oauthCallbackContentSecurityPolicy)
	}
	tags := readStartTags(t, oauthConnectedPage)
	if len(tags) == 0 {
		t.Fatal("read no tag from the connected page")
	}
	for _, tag := range tags {
		if !slices.Contains([]string{"html", "head", "body", "title", "h1", "p", "br", "strong", "em"}, tag.name) || len(tag.attributes) != 0 {
			t.Errorf("the connected page holds <%s> with %v, which its policy may block", tag.name, tag.attributes)
		}
	}
}
