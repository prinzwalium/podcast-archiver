package httpd

import (
	"net/http"
	"strconv"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

type userView struct {
	ID          int64
	Name        string
	Sub         string
	IsAdmin     bool
	MaxFeeds    int64
	MaxBytes    int64
	MaxBytesFmt string
	UsedFmt     string
}

// requireAdmin wraps handlers that need admin rights. In single-user local
// mode the local user is implicitly admin.
func (h *Handler) requireAdmin(next func(w http.ResponseWriter, r *http.Request, u *store.User)) http.HandlerFunc {
	return h.requireUser(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if !h.isAdmin(u) {
			http.Error(w, "forbidden: admin required", http.StatusForbidden)
			return
		}
		next(w, r, u)
	})
}

func (h *Handler) isAdmin(u *store.User) bool {
	if !h.cfg.AuthEnabled() {
		return true // local mode
	}
	return u.IsAdmin
}

func (h *Handler) pageAdmin(w http.ResponseWriter, r *http.Request, admin *store.User) {
	users, err := h.db.ListUsers()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var views []*userView
	for _, usr := range users {
		used, _ := h.db.UserStorageBytes(usr.ID)
		views = append(views, &userView{
			ID: usr.ID, Name: usr.Name, Sub: usr.Sub, IsAdmin: usr.IsAdmin,
			MaxFeeds: usr.MaxFeeds, MaxBytes: usr.MaxBytes,
			MaxBytesFmt: fmtBytes(usr.MaxBytes), UsedFmt: fmtBytes(used),
		})
	}
	used, _ := h.db.StorageUsed()
	h.render(w, http.StatusOK, "content-admin", admin, &pageData{
		Title: "Admin", UserName: admin.Name, Users: views, StorageUsed: fmtBytes(used),
	})
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request, admin *store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	maxFeeds, _ := strconv.ParseInt(r.FormValue("max_feeds"), 10, 64)
	if maxFeeds < 0 {
		maxFeeds = 0
	}
	maxBytes, _ := strconv.ParseInt(r.FormValue("max_bytes"), 10, 64)
	if maxBytes < 0 {
		maxBytes = 0
	}
	if err := h.db.UpdateUserQuotas(id, maxFeeds, maxBytes); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request, admin *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if id == admin.ID {
		http.Error(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}
	shas, err := h.db.UserFileSHAs(id)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if err := h.db.DeleteUser(id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	for _, sha := range shas {
		h.engine.UnrefFile(sha)
	}
	http.Redirect(w, r, "/admin", http.StatusFound)
}
