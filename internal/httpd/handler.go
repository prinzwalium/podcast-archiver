package httpd

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/auth"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

type Handler struct {
	db     *store.DB
	engine *archive.Engine
	cfg    *config.Config
	auth   *auth.Service
}

func New(db *store.DB, engine *archive.Engine, cfg *config.Config, authSvc *auth.Service) *Handler {
	return &Handler{db: db, engine: engine, cfg: cfg, auth: authSvc}
}

func (h *Handler) mediaRoot() string {
	return h.cfg.MediaDir
}

func (h *Handler) Handler() http.Handler {
	mux := http.NewServeMux()

	// Token-authed podcatcher endpoints (no cookies needed).
	mux.HandleFunc("GET /feed/{token}", h.feedEndpoint)
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
	mux.HandleFunc("GET /admin", h.requireAdmin(h.pageAdmin))
	mux.HandleFunc("POST /admin/users/{id}/update", h.requireAdmin(h.updateUser))
	mux.HandleFunc("POST /admin/users/{id}/delete", h.requireAdmin(h.deleteUser))
	mux.HandleFunc("POST /feeds/{id}/sync", h.requireUser(h.syncFeed))
	mux.HandleFunc("POST /feeds/{id}/tokens", h.requireUser(h.createToken))
	mux.HandleFunc("POST /feeds/{id}/tokens/{token}/delete", h.requireUser(h.deleteToken))
	mux.HandleFunc("GET /feeds/{id}", h.requireUser(h.pageFeedDetail))
	mux.HandleFunc("GET /predict", h.requireUser(h.pagePredict))
	mux.HandleFunc("POST /predict", h.requireUser(h.predict))
	mux.HandleFunc("GET /auth/login", h.auth.Login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)

	return h.securityHeaders(mux)
}

func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; frame-ancestors 'none'")
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
		u, err := h.auth.CurrentUser(r, h.db)
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
	user, err := h.auth.Callback(w, r, h.db)
	if err != nil {
		slog.Warn("oidc callback failed", "err", err)
		if errors.Is(err, auth.ErrGroupNotAllowed) {
			http.Error(w, "access denied: you are not in an allowed group", http.StatusForbidden)
			return
		}
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	if err := h.auth.SetSessionCookie(w, user.ID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	h.auth.Logout(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) feedEndpoint(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
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
	if err := archive.ValidateURL(rawURL); err != nil {
		http.Error(w, "invalid or blocked URL: "+err.Error(), http.StatusBadRequest)
		return
	}
	feeds, err := h.db.ListFeeds(u.ID)
	if err == nil && len(feeds) >= h.cfg.MaxFeedsPerUser {
		http.Error(w, "feed limit reached", http.StatusTooManyRequests)
		return
	}
	// Fetch metadata for a sane title before creating.
	pred, err := h.engine.PredictSize(r.Context(), rawURL)
	title := ""
	if err == nil {
		title = pred.Title
	}
	syncMode := sanitizeMode(r.FormValue("sync_mode"), []string{"mirror", "archive"}, "mirror")
	reuploadMode := sanitizeMode(r.FormValue("reupload_mode"), []string{"replace", "keep"}, "replace")
	feed, err := h.db.CreateFeed(u.ID, rawURL, title, "", "", "", syncMode, reuploadMode)
	if err != nil {
		slog.Error("create feed", "err", err)
		http.Error(w, "could not add feed (duplicate?)", http.StatusConflict)
		return
	}
	// Initial sync in background so the request returns fast.
	go func() {
		if err := h.engine.SyncFeed(r.Context(), feed); err != nil {
			slog.Warn("initial sync failed", "feed", rawURL, "err", err)
		}
	}()
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

func (h *Handler) syncFeed(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.engine.SyncFeed(r.Context(), feed); err != nil {
		slog.Warn("manual sync failed", "feed", feed.Title, "err", err)
	}
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
	pred, err := h.engine.PredictSize(r.Context(), rawURL)
	if err != nil {
		http.Error(w, "could not fetch feed: "+err.Error(), http.StatusBadRequest)
		return
	}
	h.renderPredictResult(w, u, pred, rawURL)
}
