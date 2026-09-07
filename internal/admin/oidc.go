package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var errOIDCUnavailable = errors.New("oidc is not configured")

func exactRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != "" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func (s *Server) oidcProvider(ctx context.Context) (*oidc.Provider, *OIDCConfig, error) {
	cfg := s.deps.OIDC
	if cfg == nil || cfg.Issuer == "" || cfg.ClientID == "" || !exactRedirect(cfg.RedirectURI) {
		return nil, nil, errOIDCUnavailable
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, nil, errOIDCUnavailable
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	return provider, cfg, nil
}
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	provider, cfg, err := s.oidcProvider(r.Context())
	if err != nil {
		authError(w, http.StatusServiceUnavailable)
		return
	}
	cookieName, ok := s.loginCookieName(r)
	if !ok {
		authError(w, http.StatusForbidden)
		return
	}
	state, err := randomSecret()
	if err != nil {
		authError(w, http.StatusInternalServerError)
		return
	}
	browser, err := randomSecret()
	if err != nil {
		authError(w, http.StatusInternalServerError)
		return
	}
	nonce, err := randomSecret()
	if err != nil {
		authError(w, http.StatusInternalServerError)
		return
	}
	verifier, err := randomSecret()
	if err != nil {
		authError(w, http.StatusInternalServerError)
		return
	}
	expiry := time.Now().Add(10 * time.Minute)
	if err = s.deps.Auth.SaveAdminLogin(r.Context(), LoginState{Hash: authHash(state + "\x00" + browser), Nonce: nonce, Verifier: verifier, RedirectURI: cfg.RedirectURI, ExpiresAt: expiry}); err != nil {
		authError(w, http.StatusServiceUnavailable)
		return
	}
	setAuthCookie(w, cookieName, browser, expiry, 600)
	oauth := oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: cfg.RedirectURI, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
	http.Redirect(w, r, oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}
func singleQuery(r *http.Request, key string) (string, error) {
	v, ok := r.URL.Query()[key]
	if !ok || len(v) != 1 || v[0] == "" {
		return "", errors.New("invalid callback")
	}
	return v[0], nil
}
func (s *Server) clearLoginCookie(w http.ResponseWriter, r *http.Request) {
	if name, ok := s.loginCookieName(r); ok {
		clearAuthCookie(w, name)
	}
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if errv := r.URL.Query().Get("error"); errv != "" {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	state, err := singleQuery(r, "state")
	if err != nil {
		authError(w, http.StatusBadRequest)
		return
	}
	code, err := singleQuery(r, "code")
	if err != nil {
		authError(w, http.StatusBadRequest)
		return
	}
	cookieName, ok := s.loginCookieName(r)
	if !ok {
		authError(w, http.StatusForbidden)
		return
	}
	browser, err := uniqueCookie(r, cookieName)
	if err != nil {
		authError(w, http.StatusUnauthorized)
		return
	}
	login, err := s.deps.Auth.ConsumeAdminLogin(r.Context(), authHash(state+"\x00"+browser))
	if err != nil || !login.ExpiresAt.After(time.Now()) || login.RedirectURI == "" {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	provider, cfg, err := s.oidcProvider(r.Context())
	if err != nil || cfg == nil {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusServiceUnavailable)
		return
	}
	redirect, parseErr := url.Parse(cfg.RedirectURI)
	if parseErr != nil || cfg.RedirectURI != login.RedirectURI || r.URL.Path != redirect.Path || (redirect.Scheme == "https" && r.TLS == nil) {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusServiceUnavailable)
		return
	}
	oauth := oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: login.RedirectURI, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
	token, err := oauth.Exchange(r.Context(), code, oauth2.VerifierOption(login.Verifier))
	if err != nil {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	idToken, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	var claims struct {
		Nonce   string `json:"nonce"`
		Subject string `json:"sub"`
	}
	if err = idToken.Claims(&claims); err != nil || claims.Subject == "" || claims.Nonce != login.Nonce || claims.Nonce == "" {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusUnauthorized)
		return
	}
	p, err := s.deps.Auth.ResolveIdentity(r.Context(), cfg.Issuer, claims.Subject)
	if err != nil || !validPrincipal(p) {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusForbidden)
		return
	}
	if err = s.issueSession(w, r, p); err != nil {
		s.clearLoginCookie(w, r)
		authError(w, http.StatusServiceUnavailable)
		return
	}
	s.clearLoginCookie(w, r)
	http.Redirect(w, r, "/admin/", http.StatusFound)
}
