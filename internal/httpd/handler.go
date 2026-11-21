package httpd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/auth"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"github.com/prinzwalium/podcast-archiver/internal/sync"
)

type Handler struct {
	db     *store.DB
	engine *archive.Engine
	cfg    *config.Config
	auth   *auth.Service
	syncer *sync.Service

	// predicting marks users with a size prediction in flight (one each).
	predicting stdsync.Map
}

func New(db *store.DB, engine *archive.Engine, cfg *config.Config, authSvc *auth.Service, syncer *sync.Service) *Handler {
	return &Handler{db: db, engine: engine, cfg: cfg, auth: authSvc, syncer: syncer}
}

func (h *Handler) mediaRoot() string {
	return h.cfg.MediaDir
}

func (h *Handler) Handler() http.Handler {
	mux := http.NewServeMux()

	// Token-authed podcatcher endpoints (no cookies needed).
	// Podcatchers normalize feed URLs in various ways (trailing slash, .xml
	// suffix); a prefix route lets the handler accept all common spellings.
	mux.HandleFunc("GET /feed/{token}", h.feedEndpoint)
	mux.HandleFunc("GET /feed/{token}/{$}", h.feedEndpoint)
	mux.HandleFunc("GET /feed/", h.feedEndpointLegacy)
	mux.HandleFunc("GET /media/{token}/{filename}", h.serveMedia)

	// Web UI.
	mux.HandleFunc("GET /{$}", h.requireUser(h.pageFeeds))
	mux.HandleFunc("POST /feeds", h.requireUser(h.addFeed))
	mux.HandleFunc("POST /feeds/{id}/delete", h.requireUser(h.deleteFeed))
	mux.HandleFunc("POST /feeds/{id}/settings", h.requireUser(h.updateSettings))
	mux.HandleFunc("POST /feeds/{id}/episodes/{guid}/pin", h.requireUser(h.pinEpisode))
	mux.HandleFunc("POST /feeds/{id}/episodes/{guid}/unpin", h.requireUser(h.unpinEpisode))
	mux.HandleFunc("GET /export/opml", h.requireUser(h.exportOPML))
	mux.HandleFunc("POST /import/opml", h.requireUser(h.importOPML))
	mux.HandleFunc("GET /feeds/{id}/events", h.requireUser(h.pageEvents))
	// Embedded fonts for the UI theme.
	mux.HandleFunc("GET /fonts/{name}", h.serveFont)
	mux.HandleFunc("GET /merged", h.requireUser(h.pageMerged))
	mux.HandleFunc("GET /merged/{id}", h.requireUser(h.pageMergedDetail))
	mux.HandleFunc("POST /merged", h.requireUser(h.createMerged))
	mux.HandleFunc("POST /merged/{id}", h.requireUser(h.updateMerged))
	mux.HandleFunc("POST /merged/{id}/delete", h.requireUser(h.deleteMerged))
	mux.HandleFunc("POST /merged/{id}/tokens", h.requireUser(h.createMergedToken))
	mux.HandleFunc("POST /merged/{id}/tokens/{token}/delete", h.requireUser(h.deleteMergedToken))
	mux.HandleFunc("GET /mfeed/{token}", h.mergedEndpoint)
	mux.HandleFunc("GET /mfeed/{token}/{$}", h.mergedEndpoint)
	mux.HandleFunc("GET /admin", h.requireAdmin(h.pageAdmin))
	mux.HandleFunc("POST /admin/users/{id}/update", h.requireAdmin(h.updateUser))
	mux.HandleFunc("POST /admin/users/{id}/delete", h.requireAdmin(h.deleteUser))
	mux.HandleFunc("POST /feeds/{id}/sync", h.requireUser(h.syncFeed))
	mux.HandleFunc("POST /feeds/{id}/tokens", h.requireUser(h.createToken))
	mux.HandleFunc("POST /feeds/{id}/tokens/{token}/delete", h.requireUser(h.deleteToken))
	mux.HandleFunc("GET /feeds/{id}", h.requireUser(h.pageFeedDetail))
	mux.HandleFunc("GET /feeds/{id}/syncing", h.requireUser(h.feedSyncingStatus))
	mux.HandleFunc("GET /predict", h.requireUser(h.pagePredict))
	mux.HandleFunc("POST /predict", h.requireUser(h.predict))
	mux.HandleFunc("GET /auth/login", h.auth.Login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("GET /healthz", h.healthz)

	return h.securityHeaders(h.crossOriginProtection(mux))
}

// crossOriginProtection rejects cross-site state-changing requests
// (CSRF) using Sec-Fetch-Site / Origin, so protection doesn't rely on
// SameSite cookies alone, which treat sibling subdomains as same-site
// and do nothing in single-user mode. The configured external URL is
// trusted in case a proxy rewrites Host.
func (h *Handler) crossOriginProtection(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	if u, err := url.Parse(h.cfg.ExternalURL); err == nil && u.Scheme != "" && u.Host != "" {
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			slog.Warn("PA_EXTERNAL_URL is not a valid origin", "err", err)
		}
	}
	return cop.Handler(next)
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var one int
	if err := h.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' https: data:; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// requireUser wraps handlers that need a logged-in user. When OIDC is not
// configured the app runs in single-user mode with an implicit user.
func (h *Handler) requireUser(next func(w http.ResponseWriter, r *http.Request, u *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.AuthEnabled() {
			u, err := h.db.UpsertUser("local", "Local User")
			if err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			next(w, r, u)
			return
		}
		u, err := h.auth.CurrentUser(r)
		if err != nil {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		next(w, r, u)
	}
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if !h.auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if _, err := h.auth.Callback(w, r); err != nil {
		slog.Warn("oidc callback failed", "err", err)
		switch {
		case errors.Is(err, auth.ErrGroupNotAllowed):
			http.Error(w, "access denied: you are not in an allowed group", http.StatusForbidden)
		case errors.Is(err, auth.ErrProviderUnavailable):
			http.Error(w, "identity provider unavailable, try again shortly", http.StatusServiceUnavailable)
		default:
			http.Error(w, "authentication failed", http.StatusUnauthorized)
		}
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	h.auth.Logout(w, r)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) feedEndpoint(w http.ResponseWriter, r *http.Request) {
	// Podcatchers sometimes append .xml to feed URLs; accept it.
	token := strings.TrimSuffix(r.PathValue("token"), ".xml")
	feed, err := h.db.ResolveFeedToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.feedRSS(w, r, feed, token)
}

// feedEndpointLegacy handles /feed/<token> spellings the pattern router
// can't express (e.g. ".xml" suffixes appended by podcatchers).
func (h *Handler) feedEndpointLegacy(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(strings.Trim(r.URL.Path, "/"), "feed/")
	token = strings.TrimSuffix(token, "/")
	token = strings.TrimSuffix(token, ".xml")
	token = strings.TrimSuffix(token, "/")
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	feed, err := h.db.ResolveFeedToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.feedRSS(w, r, feed, token)
}

func (h *Handler) addFeed(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	if err := h.engine.ValidateURL(rawURL); err != nil {
		http.Error(w, "invalid or blocked URL: "+err.Error(), http.StatusBadRequest)
		return
	}
	count, err := h.db.CountFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if limit := h.engine.FeedLimit(u); limit > 0 && count >= limit {
		http.Error(w, "feed limit reached", http.StatusTooManyRequests)
		return
	}
	syncMode := sanitizeMode(r.FormValue("sync_mode"), []string{"mirror", "archive"}, "mirror")
	reuploadMode := sanitizeMode(r.FormValue("reupload_mode"), []string{"replace", "keep"}, "replace")
	// The title and metadata arrive with the first sync; the feed is not
	// fetched here so the request stays fast and the feed is fetched once.
	feed, err := h.db.CreateFeed(u.ID, rawURL, "", "", "", "", syncMode, reuploadMode)
	if err != nil {
		slog.Error("create feed", "err", err)
		http.Error(w, "could not add feed (duplicate?)", http.StatusConflict)
		return
	}
	// The initial sync goes through the sync queue like every other sync,
	// so it can't overlap a scheduled sync of the same feed.
	h.syncer.QueueFeedAsync(feed)
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(feed.ID, 10), http.StatusFound)
}

func (h *Handler) deleteFeed(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.engine.DeleteFeed(feed); err != nil {
		slog.Error("delete feed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func sanitizeMode(v string, allowed []string, def string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	syncMode := sanitizeMode(r.FormValue("sync_mode"), []string{"mirror", "archive"}, feed.SyncMode)
	reuploadMode := sanitizeMode(r.FormValue("reupload_mode"), []string{"replace", "keep"}, feed.ReuploadMode)
	refreshSecs, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("refresh_secs")), 10, 64)
	if err != nil || refreshSecs < 0 {
		refreshSecs = feed.RefreshSecs
	}
	if refreshSecs > 0 && refreshSecs < 60 {
		refreshSecs = 60 // floor: don't hammer small creators' servers
	}
	if err := h.db.UpdateFeedSettings(feed.ID, syncMode, reuploadMode, refreshSecs); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(feed.ID, 10), http.StatusFound)
}

// feedSyncingStatus reports whether a sync is currently running for a feed,
// as JSON for the UI's polling badge.
func (h *Handler) feedSyncingStatus(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.db.GetFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"syncing":%t}`, h.syncer.IsSyncing(id))
}

func (h *Handler) syncFeed(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Queue the sync; it runs on the sync worker, not in this request, so
	// the page returns immediately instead of blocking on a big download.
	h.syncer.QueueFeedAsync(feed)
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(feed.ID, 10), http.StatusFound)
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tok, err := auth.NewToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.db.CreateFeedToken(feed.ID, tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(feed.ID, 10), http.StatusFound)
}

func (h *Handler) deleteToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = h.db.DeleteFeedToken(feed.ID, r.PathValue("token"))
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(feed.ID, 10), http.StatusFound)
}

func (h *Handler) predict(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	if rawURL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	if err := h.engine.ValidateURL(rawURL); err != nil {
		http.Error(w, "invalid or blocked URL: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, busy := h.predicting.LoadOrStore(u.ID, true); busy {
		http.Error(w, "a size prediction is already running, try again when it is done", http.StatusTooManyRequests)
		return
	}
	defer h.predicting.Delete(u.ID)
	pred, err := h.engine.PredictSize(r.Context(), rawURL)
	if err != nil {
		http.Error(w, "could not fetch feed: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.renderPredictResult(w, u, pred, rawURL)
}
