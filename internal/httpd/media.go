package httpd

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

// serveMedia streams a downloaded episode file. Token auth happens in the
// router; here we only resolve and stream the content-addressed file.
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
		if e.Downloaded && e.FileSHA != "" && h.mediaName(e) == filename {
			ep = e
			break
		}
	}
	if ep == nil {
		http.NotFound(w, r)
		return
	}

	file, err := h.db.GetFile(ep.FileSHA)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(h.mediaRoot(), "objects", ep.FileSHA[:2], ep.FileSHA)
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

	w.Header().Set("Content-Type", file.MimeType)
	http.ServeContent(w, r, filename, st.ModTime(), f)
}

// mediaName builds the served filename for an episode's stored file.
func (h *Handler) mediaName(e *store.Episode) string {
	file, err := h.db.GetFile(e.FileSHA)
	if err != nil {
		return ""
	}
	return e.FileSHA + file.Ext
}
