package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hoorific/internal/core"
)

const sessionCookie = "hoorific_session"
const loginCookie = "hoorific_login"

var errUnauthenticated = errors.New("authentication required")
var errForbidden = errors.New("forbidden")

func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.Trim(r.RemoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (s *Server) secureRequest(r *http.Request) bool {
	u, err := url.Parse(s.deps.PublicOrigin)
	return err == nil && u.Scheme == "https" && r.TLS != nil
}
func (s *Server) sessionCookieName(r *http.Request) (string, bool) {
	if s.secureRequest(r) {
		return "__Host-hoorific_session", true
	}
	if loopbackRequest(r) {
		return "hoorific_session", true
	}
	return "", false
}
func (s *Server) loginCookieName(r *http.Request) (string, bool) {
	if s.secureRequest(r) {
		return "__Host-hoorific_login", true
	}
	if loopbackRequest(r) {
		return "hoorific_login", true
	}
	return "", false
}

func authHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func csrfToken(secret string) string { return authHash("hoorific:session:csrf:v1\x00" + secret) }
func validPrincipal(p core.Principal) bool {
	return p.TenantID != "" && p.SubjectID != "" && len(permissionsFor(p)) > 0
}

// Permissions are bounded by the current server-managed role and explicit token scopes.
func permissionsFor(p core.Principal) []string {
	role := rolePermissions(p.Role)
	if p.Permissions == nil {
		return role
	}
	allowed := make([]string, 0, len(p.Permissions))
	for _, scope := range p.Permissions {
		for _, grant := range role {
			if grant == "*" || grant == scope {
				allowed = append(allowed, scope)
				break
			}
		}
	}
	return allowed
}
func rolePermissions(role string) []string {
	switch role {
	case "owner":
		return []string{"*"}
	case "admin":
		return []string{"tenant:read", "connection:read", "connection:write", "connection:test", "connection:discover", "key:read", "key:write", "route:read", "route:write", "budget:read", "budget:write", "accounting:reconcile", "config:read", "config:write", "job:read", "job:write", "catalog:read", "catalog:write", "usage:read", "audit:read", "playground:execute", "session:read"}
	case "operator":
		return []string{"connection:read", "connection:test", "connection:discover", "catalog:read", "job:read", "job:write", "playground:execute", "usage:read"}
	case "auditor":
		return []string{"usage:read", "audit:read", "budget:read"}
	case "viewer":
		return []string{"catalog:read", "health:read"}
	default:
		return []string{}
	}
}
func hasPermission(p core.Principal, permission string) bool {
	for _, v := range permissionsFor(p) {
		if v == "*" || v == permission {
			return true
		}
	}
	return false
}

// HasPermission evaluates the server-owned role policy; stored claims are not trusted.
func HasPermission(p core.Principal, permission string) bool { return hasPermission(p, permission) }
func bearerToken(r *http.Request) (string, bool, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", true, errUnauthenticated
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
		return "", true, errUnauthenticated
	}
	return parts[1], true, nil
}
func uniqueCookie(r *http.Request, name string) (string, error) {
	value := ""
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			value = c.Value
			count++
		}
	}
	if count != 1 || len(value) != 43 {
		return "", errUnauthenticated
	}
	if _, err := base64.RawURLEncoding.DecodeString(value); err != nil {
		return "", errUnauthenticated
	}
	return value, nil
}
func (s *Server) resolveSession(r *http.Request) (Session, string, error) {
	if s.deps.Auth == nil {
		return Session{}, "", errUnauthenticated
	}
	name, ok := s.sessionCookieName(r)
	if !ok {
		return Session{}, "", errUnauthenticated
	}
	secret, err := uniqueCookie(r, name)
	if err != nil {
		return Session{}, "", err
	}
	session, err := s.deps.Auth.ResolveSession(r.Context(), authHash(secret))
	if err != nil || session.Hash != authHash(secret) || !session.ExpiresAt.After(time.Now()) || !validPrincipal(session.Principal) {
		return Session{}, "", errUnauthenticated
	}
	return session, secret, nil
}

// authenticate always resolves durable credentials, so revocation and role changes take effect on the next request.
func (s *Server) authenticate(r *http.Request) (core.Principal, error) {
	if s.deps.Auth == nil {
		return core.Principal{}, errUnauthenticated
	}
	token, present, err := bearerToken(r)
	if err != nil {
		return core.Principal{}, err
	}
	if present {
		p, err := s.deps.Auth.ResolveAdminToken(r.Context(), authHash(token))
		if err != nil || len(p.Permissions) == 0 || !validPrincipal(p) {
			return core.Principal{}, errUnauthenticated
		}
		p.SessionID = ""
		p.Permissions = permissionsFor(p)
		return p, nil
	}
	session, secret, err := s.resolveSession(r)
	if err != nil {
		return core.Principal{}, err
	}
	session.Principal.SessionID = authHash(secret)
	session.Principal.Permissions = rolePermissions(session.Principal.Role)
	return session.Principal, nil
}
func (s *Server) checkOrigin(r *http.Request) error {
	origin := s.deps.PublicOrigin
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errForbidden
	}
	values := r.Header.Values("Origin")
	if len(values) != 1 || values[0] != origin {
		return errForbidden
	}
	return nil
}
func (s *Server) checkMutation(r *http.Request) error {
	_, present, err := bearerToken(r)
	if err != nil {
		return err
	}
	if present {
		_, err = s.authenticate(r)
		return err
	}
	if err = s.checkOrigin(r); err != nil {
		return err
	}
	session, secret, err := s.resolveSession(r)
	if err != nil {
		return err
	}
	values := r.Header.Values("X-CSRF-Token")
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(csrfToken(secret))) != 1 || subtle.ConstantTimeCompare([]byte(authHash(values[0])), []byte(session.CSRFHash)) != 1 {
		return errForbidden
	}
	return nil
}
func setAuthCookie(w http.ResponseWriter, name, value string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: strings.HasPrefix(name, "__Host-"), HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge})
}
func clearAuthCookie(w http.ResponseWriter, name string) {
	setAuthCookie(w, name, "", time.Unix(1, 0), -1)
}
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	if s.deps.Auth == nil || !validPrincipal(p) {
		return errUnauthenticated
	}
	name, ok := s.sessionCookieName(r)
	if !ok {
		return errUnauthenticated
	}
	p.Permissions = rolePermissions(p.Role)
	secret, err := randomSecret()
	if err != nil {
		return err
	}
	expiry := time.Now().Add(12 * time.Hour)
	if err = s.deps.Auth.CreateSession(r.Context(), Session{Hash: authHash(secret), CSRFHash: authHash(csrfToken(secret)), Principal: p, ExpiresAt: expiry}); err != nil {
		return err
	}
	setAuthCookie(w, name, secret, expiry, 43200)
	return nil
}
func authJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func authError(w http.ResponseWriter, status int) {
	authJSON(w, status, map[string]string{"error": http.StatusText(status)})
}
func (s *Server) registerAuth() {
	s.mux.HandleFunc("POST /admin/api/v1/auth/bootstrap", s.bootstrap)
	s.mux.HandleFunc("GET /admin/api/v1/auth/login", s.oidcLogin)
	s.mux.HandleFunc("GET /admin/api/v1/auth/callback", s.oidcCallback)
	s.mux.HandleFunc("POST /admin/api/v1/auth/logout", s.logout)
	s.registerHumaAuth(s.api)
}

// RegisterAuth exposes auth route registration for embedders; New calls it indirectly.
func (s *Server) RegisterAuth() { s.registerAuth() }
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !loopbackRequest(r) {
		authError(w, http.StatusForbidden)
		return
	}
	// CLI bootstrap needs no Origin; browsers must present the exact trusted origin.
	if len(r.Header.Values("Origin")) > 0 {
		if err := s.checkOrigin(r); err != nil {
			authError(w, http.StatusForbidden)
			return
		}
	}
	var body struct {
		Code string `json:"code"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Code == "" || len(body.Code) > 1024 {
		authError(w, http.StatusBadRequest)
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		authError(w, http.StatusBadRequest)
		return
	}
	p, err := s.deps.Auth.ConsumeBootstrap(r.Context(), authHash(body.Code))
	if err != nil {
		authError(w, http.StatusUnauthorized)
		return
	}
	if err = s.issueSession(w, r, p); err != nil {
		authError(w, http.StatusServiceUnavailable)
		return
	}
	authJSON(w, http.StatusOK, map[string]any{"principal": p, "permissions": permissionsFor(p)})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, present, err := bearerToken(r); err != nil || present {
		authError(w, http.StatusForbidden)
		return
	}
	if err := s.checkMutation(r); err != nil {
		authError(w, http.StatusForbidden)
		return
	}
	_, secret, err := s.resolveSession(r)
	if err != nil {
		authError(w, http.StatusUnauthorized)
		return
	}
	if err = s.deps.Auth.DeleteSession(r.Context(), authHash(secret)); err != nil {
		authError(w, http.StatusServiceUnavailable)
		return
	}
	name, ok := s.sessionCookieName(r)
	if ok {
		clearAuthCookie(w, name)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
