package httpd

import (
	"net/http"
	"strconv"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// updateFeedNote saves the user's free-text note on an archive feed.
func (h *Handler) updateFeedNote(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateFeedNote(u.ID, id, r.FormValue("note")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// pauseFeedSync disables syncing for a feed (scheduled and manual).
func (h *Handler) pauseFeedSync(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.db.GetFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.SetFeedSyncEnabled(u.ID, id, false); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.db.AddEvent(id, "sync_paused", "Sync paused by user")
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// resumeFeedSync re-enables syncing for a feed.
func (h *Handler) resumeFeedSync(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := h.db.GetFeed(u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.SetFeedSyncEnabled(u.ID, id, true); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.db.AddEvent(id, "sync_resumed", "Sync resumed by user")
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(id, 10), http.StatusFound)
}

// updateMergedNote saves the user's free-text note on a merged feed.
func (h *Handler) updateMergedNote(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateMergedNote(u.ID, id, r.FormValue("note")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/merged/"+strconv.FormatInt(id, 10), http.StatusFound)
}
