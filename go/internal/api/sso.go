package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

const (
	ssoSecretHeader   = "X-LLMGW-SSO-Secret"
	ssoSubjectHeader  = "X-Authentik-Uid"
	ssoUsernameHeader = "X-Authentik-Username"
	ssoEmailHeader    = "X-Authentik-Email"
	ssoNameHeader     = "X-Authentik-Name"
	ssoGroupsHeader   = "X-Authentik-Groups"
)

type ssoIdentity struct {
	Subject  string
	Username string
	Email    string
	Name     string
	Groups   []string
}

type adminActor struct {
	PrincipalID string
	KeyID       string
	Source      string
}

type adminActorContextKey struct{}

// requireAdmin accepts only a static admin key or a verified Authentik identity
// in the configured admin group. Minted project keys are intentionally not
// admin credentials.
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token := extractAPIKey(r)
	if token != "" && matchesAnyKey(token, validKeys()) {
		setAdminActor(r, adminActor{Source: "static-admin-key"})
		return true
	}
	if identity, ok, status, msg := verifiedSSOIdentity(r); ok {
		if !containsGroup(identity.Groups, config.Get().SSOAdminGroup) {
			writeError(w, http.StatusForbidden, "Admin role required.")
			return false
		}
		if mutatingMethod(r.Method) && !sameOriginRequest(r) {
			writeError(w, http.StatusForbidden, "SSO admin mutation requires a same-origin request.")
			return false
		}
		var principal iam.Principal
		var principalFound bool
		if config.Get().SSOAutoProvision {
			var err error
			principal, err = iam.EnsurePrincipalBySubject(
				"human", "authentik:"+identity.Subject, identity.Email,
				firstNonEmpty(identity.Name, identity.Username, identity.Email, identity.Subject),
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "Could not provision SSO principal.")
				return false
			}
			principalFound = true
		} else {
			principal, principalFound, _ = iam.PrincipalBySubject("authentik:" + identity.Subject)
		}
		if !principalFound {
			writeError(w, http.StatusForbidden, "SSO principal is not provisioned.")
			return false
		}
		if principal.Status != "active" {
			writeError(w, http.StatusForbidden, "Principal is disabled.")
			return false
		}
		setAdminActor(r, adminActor{PrincipalID: principal.ID, Source: "sso"})
		return true
	} else if status != 0 {
		writeError(w, status, msg)
		return false
	}

	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "Admin authentication required.")
	return false
}

func setAdminActor(r *http.Request, actor adminActor) {
	*r = *r.WithContext(context.WithValue(r.Context(), adminActorContextKey{}, actor))
}

func getAdminActor(r *http.Request) adminActor {
	actor, _ := r.Context().Value(adminActorContextKey{}).(adminActor)
	return actor
}

func requireSSOUser(w http.ResponseWriter, r *http.Request) (iam.Principal, bool) {
	identity, ok, status, msg := verifiedSSOIdentity(r)
	if !ok {
		if status == 0 {
			status = http.StatusUnauthorized
			msg = "SSO authentication required."
		}
		writeError(w, status, msg)
		return iam.Principal{}, false
	}
	if mutatingMethod(r.Method) && !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "SSO mutation requires a same-origin request.")
		return iam.Principal{}, false
	}
	subject := "authentik:" + identity.Subject
	principal := iam.Principal{}
	if config.Get().SSOAutoProvision {
		var err error
		principal, err = iam.EnsurePrincipalBySubject(
			"human", subject, identity.Email,
			firstNonEmpty(identity.Name, identity.Username, identity.Email, identity.Subject),
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not provision SSO principal.")
			return iam.Principal{}, false
		}
	} else {
		var found bool
		var err error
		principal, found, err = iam.PrincipalBySubject(subject)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Identity store unavailable.")
			return iam.Principal{}, false
		}
		if !found {
			writeError(w, http.StatusForbidden, "SSO principal is not provisioned.")
			return iam.Principal{}, false
		}
	}
	if principal.Status != "active" {
		writeError(w, http.StatusForbidden, "Principal is disabled.")
		return iam.Principal{}, false
	}
	return principal, true
}

func mutatingMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func sameOriginRequest(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return strings.EqualFold(parsed.Scheme, scheme) &&
		strings.EqualFold(parsed.Host, r.Host)
}

func verifiedSSOIdentity(r *http.Request) (ssoIdentity, bool, int, string) {
	s := config.Get()
	if !s.SSOEnabled {
		return ssoIdentity{}, false, 0, ""
	}
	if strings.TrimSpace(s.SSOSharedSecret) == "" {
		return ssoIdentity{}, false, http.StatusInternalServerError, "SSO is enabled but not configured."
	}
	presented := r.Header.Get(ssoSecretHeader)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.SSOSharedSecret)) != 1 {
		// No secret means this may simply be an API-key request. A wrong non-empty
		// secret is an explicit proxy-auth failure.
		if presented == "" {
			return ssoIdentity{}, false, 0, ""
		}
		return ssoIdentity{}, false, http.StatusUnauthorized, "Invalid SSO proxy assertion."
	}
	subject := strings.TrimSpace(r.Header.Get(ssoSubjectHeader))
	if subject == "" {
		return ssoIdentity{}, false, http.StatusUnauthorized, "SSO subject missing."
	}
	return ssoIdentity{
		Subject: subject, Username: strings.TrimSpace(r.Header.Get(ssoUsernameHeader)),
		Email:  strings.TrimSpace(r.Header.Get(ssoEmailHeader)),
		Name:   strings.TrimSpace(r.Header.Get(ssoNameHeader)),
		Groups: parseGroups(r.Header.Get(ssoGroupsHeader)),
	}, true, 0, ""
}

func parseGroups(raw string) []string {
	raw = strings.NewReplacer("|", ",", ";", ",").Replace(raw)
	out := []string{}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func containsGroup(groups []string, wanted string) bool {
	wanted = strings.TrimSpace(strings.ToLower(wanted))
	for _, group := range groups {
		if strings.ToLower(strings.TrimSpace(group)) == wanted {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "user"
}
