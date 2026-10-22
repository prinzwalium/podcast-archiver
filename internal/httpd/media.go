package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// serveMedia streams a downloaded episode file. Token auth happens in the
// router; here we only resolve and stream the file with sanitization.
func (h *Handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	filename := filepath.Base(r.PathValue("filename"))

	feed, err := h.db.ResolveFeedToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	episodes, err := h.db.ListEpisodes(feed.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var ep *store.Episode
	for _, e := range episodes {
		if e.Downloaded && filepath.Base(e.FilePath) == filename {
			ep = e
			break
		}
	}
	if ep == nil {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(h.mediaRoot(), filepath.Base(ep.FilePath))
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", ep.MimeType)
	http.ServeContent(w, r, filename, st.ModTime(), f)
	_ = strings.TrimSpace
}
