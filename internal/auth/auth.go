package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"golang.org/x/oauth2"
)

const (
	sessionCookie  = "pa_session"
	stateCookie    = "pa_oauth_state"
	nonceCookie    = "pa_oauth_nonce"
	verifierCookie = "pa_oauth_pkce"
)

var (
	ErrGroupNotAllowed     = errors.New("user is not in an allowed group")
	ErrProviderUnavailable = errors.New("identity provider unavailable")
	errNoSession           = errors.New("no session")
)

// discoveryRetry is the minimum time between OIDC discovery attempts while
// the provider is unreachable.
const discoveryRetry = 10 * time.Second

type Service struct {
	cfg  *config.Config
	db   *store.DB
	seal *sealer

	mu       sync.Mutex
	lastTry  time.Time
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config

	// rechecks deduplicates concurrent group re-checks of the same session.
	rechecks sync.Map
}

func New(cfg *config.Config, db *store.DB) *Service {
	s := &Service{cfg: cfg, db: db, seal: newSealer(cfg.SecretKey)}
	if cfg.AuthEnabled() {
		if err := s.ensureProvider(context.Background()); err != nil {
			slog.Error("OIDC provider not reachable yet; will retry on login", "issuer", cfg.OIDCIssuer, "err", err)
		}
	}
	return s
}

// ensureProvider runs OIDC discovery if it has not succeeded yet. Discovery
// is retried (at most every discoveryRetry), so an identity provider that
// was down when the container started does not break login until restart.
func (s *Service) ensureProvider(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauth != nil {
		return nil
	}
	if time.Since(s.lastTry) < discoveryRetry {
		return ErrProviderUnavailable
	}
	s.lastTry = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, s.cfg.OIDCIssuer)
	if err != nil {
		// go-oidc requires the issuer to match the discovery document exactly;
		// Authentik serves it with a trailing slash. Retry with the slash
		// toggled so both spellings work.
		alt := toggleTrailingSlash(s.cfg.OIDCIssuer)
		var altErr error
		if provider, altErr = oidc.NewProvider(ctx, alt); altErr != nil {
			return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
		}
		slog.Info("OIDC provider initialized with alternate issuer spelling", "issuer", alt)
	}
	s.provider = provider
	s.verifier = provider.Verifier(&oidc.Config{ClientID: s.cfg.OIDCClientID})
	s.oauth = &oauth2.Config{
		ClientID:     s.cfg.OIDCClientID,
		ClientSecret: s.cfg.OIDCClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  s.cfg.OIDCRedirectURL,
		Scopes:       s.cfg.OIDCScopes,
	}
	return nil
}

func (s *Service) client() (*oauth2.Config, *oidc.IDTokenVerifier, *oidc.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.oauth, s.verifier, s.provider
}

func toggleTrailingSlash(u string) string {
	if strings.HasSuffix(u, "/") {
		return strings.TrimSuffix(u, "/")
	}
	return u + "/"
}

// Enabled reports whether OIDC login is configured.
func (s *Service) Enabled() bool { return s.cfg.AuthEnabled() }

func (s *Service) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   isSecure(s.cfg),
		SameSite: http.SameSiteLaxMode,
	})
}

// Login starts the OIDC Authorization Code flow with PKCE (S256).
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := s.ensureProvider(r.Context()); err != nil {
		slog.Warn("login unavailable", "err", err)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "Sign-in is temporarily unavailable because the identity provider can't be reached. Try again in a moment.", http.StatusServiceUnavailable)
		return
	}
	oauthCfg, _, _ := s.client()
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
	verifier := oauth2.GenerateVerifier()
	s.setCookie(w, stateCookie, state, 600)
	s.setCookie(w, nonceCookie, nonce, 600)
	s.setCookie(w, verifierCookie, verifier, 600)
	http.Redirect(w, r, oauthCfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

type idClaims struct {
	Sub    string   `json:"sub"`
	Name   string   `json:"name"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

// Callback verifies state, nonce, PKCE and the ID token, then starts a
// server-side session.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) (*store.User, error) {
	if !s.Enabled() {
		return nil, errors.New("auth disabled")
	}
	// The one-time login cookies are spent whatever the outcome.
	for _, name := range []string{stateCookie, nonceCookie, verifierCookie} {
		s.setCookie(w, name, "", -1)
	}
	if err := s.ensureProvider(r.Context()); err != nil {
		return nil, err
	}
	oauthCfg, verifier, _ := s.client()
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	state, err := r.Cookie(stateCookie)
	if err != nil || state.Value == "" || state.Value != r.FormValue("state") {
		return nil, errors.New("invalid state")
	}
	nonce, err := r.Cookie(nonceCookie)
	if err != nil {
		return nil, errors.New("missing nonce")
	}
	pkce, err := r.Cookie(verifierCookie)
	if err != nil || pkce.Value == "" {
		return nil, errors.New("missing PKCE verifier")
	}

	tok, err := oauthCfg.Exchange(r.Context(), r.FormValue("code"), oauth2.VerifierOption(pkce.Value))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}
	idTok, err := verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("id_token verify: %w", err)
	}
	if idTok.Nonce != nonce.Value {
		return nil, errors.New("nonce mismatch")
	}
	var claims idClaims
	if err := idTok.Claims(&claims); err != nil {
		return nil, err
	}
	if !s.groupsAllowed(claims.Groups) {
		return nil, ErrGroupNotAllowed
	}

	name := claims.Name
	if name == "" {
		name = claims.Email
	}
	u, err := s.db.UpsertUser(claims.Sub, name)
	if err != nil {
		return nil, err
	}
	// Admin status derives from OIDC groups; persist it so the UI can show it.
	isAdmin := hasGroup(claims.Groups, s.cfg.AdminGroups())
	if err := s.db.SetUserAdmin(u.ID, isAdmin); err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin
	if err := s.startSession(w, u.ID, tok.RefreshToken); err != nil {
		return nil, err
	}
	return u, nil
}

// groupsAllowed applies PA_OIDC_ALLOWED_GROUPS (empty = everyone).
func (s *Service) groupsAllowed(groups []string) bool {
	allowed := s.cfg.AllowedGroups()
	return len(allowed) == 0 || hasGroup(groups, allowed)
}

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

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) startSession(w http.ResponseWriter, userID int64, refreshToken string) error {
	token, err := NewToken()
	if err != nil {
		return err
	}
	sealed := ""
	if refreshToken != "" {
		if sealed, err = s.seal.seal(refreshToken); err != nil {
			return err
		}
	}
	now := time.Now()
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.db.CreateSession(&store.Session{
		TokenHash: hashToken(token), UserID: userID,
		CreatedAt: now.Unix(), ExpiresAt: expires.Unix(), CheckedAt: now.Unix(),
		RefreshToken: sealed,
	}); err != nil {
		return err
	}
	s.setCookie(w, sessionCookie, token, int(s.cfg.SessionTTL.Seconds()))
	return nil
}

// CurrentUser resolves the session cookie to a user. Sessions live in the
// database, so logout and user deletion end them immediately. When the IdP
// issued a refresh token, group membership is re-checked every
// PA_OIDC_RECHECK_INTERVAL; a refused refresh or a user who left the
// allowed groups ends the session, while an unreachable IdP does not.
func (s *Service) CurrentUser(r *http.Request) (*store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, errNoSession
	}
	hash := hashToken(c.Value)
	sess, err := s.db.GetSession(hash)
	if err != nil {
		return nil, errNoSession
	}
	if time.Now().Unix() >= sess.ExpiresAt {
		_ = s.db.DeleteSession(hash)
		return nil, errors.New("session expired")
	}
	if s.cfg.OIDCRecheck > 0 && sess.RefreshToken != "" &&
		time.Since(time.Unix(sess.CheckedAt, 0)) >= s.cfg.OIDCRecheck {
		if err := s.recheckOnce(r.Context(), sess); err != nil {
			return nil, err
		}
	}
	return s.db.GetUserByID(sess.UserID)
}

type recheckCall struct {
	done chan struct{}
	err  error
}

func (s *Service) recheckOnce(ctx context.Context, sess *store.Session) error {
	call := &recheckCall{done: make(chan struct{})}
	if existing, loaded := s.rechecks.LoadOrStore(sess.TokenHash, call); loaded {
		c := existing.(*recheckCall)
		<-c.done
		return c.err
	}
	call.err = s.recheck(ctx, sess)
	s.rechecks.Delete(sess.TokenHash)
	close(call.done)
	return call.err
}

var errSessionRevoked = errors.New("session revoked by identity provider")

func (s *Service) recheck(ctx context.Context, sess *store.Session) error {
	if err := s.ensureProvider(ctx); err != nil {
		return nil // IdP down: keep the session, retry later
	}
	oauthCfg, verifier, provider := s.client()
	refresh, err := s.seal.open(sess.RefreshToken)
	if err != nil {
		_ = s.db.DeleteSession(sess.TokenHash)
		return errSessionRevoked
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tok, err := oauthCfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 {
			_ = s.db.DeleteSession(sess.TokenHash)
			return errSessionRevoked
		}
		slog.Warn("group re-check failed; keeping session", "err", err)
		return nil
	}
	var claims idClaims
	if raw, ok := tok.Extra("id_token").(string); ok {
		idTok, err := verifier.Verify(ctx, raw)
		if err != nil {
			slog.Warn("group re-check: bad id_token; keeping session", "err", err)
			return nil
		}
		if err := idTok.Claims(&claims); err != nil {
			return nil
		}
	} else {
		info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			slog.Warn("group re-check: userinfo failed; keeping session", "err", err)
			return nil
		}
		if err := info.Claims(&claims); err != nil {
			return nil
		}
	}
	if !s.groupsAllowed(claims.Groups) {
		_ = s.db.DeleteSession(sess.TokenHash)
		return ErrGroupNotAllowed
	}
	if err := s.db.SetUserAdmin(sess.UserID, hasGroup(claims.Groups, s.cfg.AdminGroups())); err != nil {
		return err
	}
	sealed := sess.RefreshToken
	if tok.RefreshToken != "" && tok.RefreshToken != refresh {
		if sealed, err = s.seal.seal(tok.RefreshToken); err != nil {
			return err
		}
	}
	return s.db.TouchSession(sess.TokenHash, time.Now().Unix(), sealed)
}

// Logout ends the server-side session and clears the cookie.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.db.DeleteSession(hashToken(c.Value))
	}
	s.setCookie(w, sessionCookie, "", -1)
}

func isSecure(cfg *config.Config) bool {
	return strings.HasPrefix(cfg.ExternalURL, "https://")
}
