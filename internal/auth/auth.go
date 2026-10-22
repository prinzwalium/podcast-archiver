package auth

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"golang.org/x/oauth2"
)

func init() {
	gob.Register(map[string]interface{}{})
}

const sessionCookie = "pa_session"

type Service struct {
	cfg      *config.Config
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

func New(cfg *config.Config) *Service {
	s := &Service{cfg: cfg}
	if !cfg.AuthEnabled() {
		return s
	}
	provider, err := oidc.NewProvider(context.Background(), cfg.OIDCIssuer)
	if err != nil {
		// go-oidc requires the issuer to match the discovery document exactly;
		// Authentik serves it with a trailing slash. Retry with the slash
		// toggled so both spellings work.
		alt := toggleTrailingSlash(cfg.OIDCIssuer)
		if alt == cfg.OIDCIssuer {
			slog.Error("failed to initialize OIDC provider", "issuer", cfg.OIDCIssuer, "err", err)
			return s
		}
		provider, err = oidc.NewProvider(context.Background(), alt)
		if err != nil {
			slog.Error("failed to initialize OIDC provider", "issuer", cfg.OIDCIssuer, "err", err)
			return s
		}
		slog.Info("OIDC provider initialized with alternate issuer spelling", "issuer", alt)
	}
	s.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
	s.oauth = &oauth2.Config{
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  cfg.OIDCRedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	return s
}

func toggleTrailingSlash(u string) string {
	if strings.HasSuffix(u, "/") {
		return strings.TrimSuffix(u, "/")
	}
	return u + "/"
}

func (s *Service) Enabled() bool { return s.cfg.AuthEnabled() && s.verifier != nil }

// Login starts the OIDC Authorization Code + PKCE flow.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state, err := randomHex(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randomHex(16)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "pa_oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   isSecure(s.cfg),
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "pa_oauth_nonce",
		Value:    nonce,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   isSecure(s.cfg),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce)), http.StatusFound)
}

// Callback verifies state, nonce and ID token, then creates a session.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request, db *store.DB) (*store.User, error) {
	if !s.Enabled() {
		return nil, errors.New("auth disabled")
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	stateCookie, err := r.Cookie("pa_oauth_state")
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.FormValue("state") {
		return nil, errors.New("invalid state")
	}
	nonceCookie, err := r.Cookie("pa_oauth_nonce")
	if err != nil {
		return nil, errors.New("missing nonce")
	}

	code := r.FormValue("code")
	tok, err := s.oauth.Exchange(r.Context(), code)
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}
	idTok, err := s.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("id_token verify: %w", err)
	}
	if idTok.Nonce != nonceCookie.Value {
		return nil, errors.New("nonce mismatch")
	}

	var claims struct {
		Sub    string   `json:"sub"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return nil, err
	}

	// Group allowlist: when configured, only members of an allowed group
	// may use the application.
	if allowed := s.cfg.AllowedGroups(); len(allowed) > 0 {
		if !hasGroup(claims.Groups, allowed) {
			return nil, ErrGroupNotAllowed
		}
	}

	name := claims.Name
	if name == "" {
		name = claims.Email
	}
	u, err := db.UpsertUser(claims.Sub, name)
	if err != nil {
		return nil, err
	}
	// Admin status derives from OIDC groups; persist it so the UI can show it.
	isAdmin := hasGroup(claims.Groups, s.cfg.AdminGroups())
	if err := db.SetUserAdmin(u.ID, isAdmin); err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin
	return u, nil
}

var ErrGroupNotAllowed = errors.New("user is not in an allowed group")

func hasGroup(userGroups []string, want map[string]bool) bool {
	if len(want) == 0 {
		return false
	}
	for _, g := range userGroups {
		if want[g] {
			return true
		}
	}
	return false
}

// SetSessionCookie issues a signed session cookie for the user.
func (s *Service) SetSessionCookie(w http.ResponseWriter, userID int64) error {
	expires := time.Now().Add(7 * 24 * time.Hour)
	payload := fmt.Sprintf("%d|%s", userID, expires.Format(time.RFC3339))
	sig, err := sign(payload, s.cfg.SecretKey)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    payload + "|" + sig,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   isSecure(s.cfg),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// CurrentUser resolves the signed session cookie to a user.
func (s *Service) CurrentUser(r *http.Request, db *store.DB) (*store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, errors.New("no session")
	}
	parts := strings.SplitN(c.Value, "|", 3)
	if len(parts) != 3 {
		return nil, errors.New("malformed session")
	}
	if err := verify(parts[0]+"|"+parts[1], parts[2], s.cfg.SecretKey); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, parts[1]); err != nil || t.Before(time.Now()) {
		return nil, errors.New("session expired")
	}
	var id int64
	if _, err := fmt.Sscanf(parts[0], "%d", &id); err != nil {
		return nil, errors.New("bad session")
	}
	return db.GetUserByID(id)
}

func (s *Service) Logout(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func isSecure(cfg *config.Config) bool {
	return strings.HasPrefix(cfg.ExternalURL, "https://")
}

// hasGroup reports whether any of the user's groups is in the want set.
