package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/prinzwalium/podcast-archiver/internal/store"
)

var shaName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// serveMedia streams a downloaded episode file. The token authorizes one
// feed, or every member feed of a merged feed; the file name is
// <sha256><ext>, so the episode is found with one indexed lookup.
func (h *Handler) serveMedia(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	filename := filepath.Base(r.PathValue("filename"))
	ext := filepath.Ext(filename)
	sha := strings.TrimSuffix(filename, ext)
	if !shaName.MatchString(sha) {
		http.NotFound(w, r)
		return
	}

	var feedIDs []int64
	if feed, err := h.db.ResolveFeedToken(token); err == nil {
		feedIDs = []int64{feed.ID}
	} else {
		merged, err := h.db.ResolveMergedToken(token)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if feedIDs, err = h.db.MergedFeedMembers(merged.ID); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
	}
	ep, err := h.db.FindDownloadedEpisode(feedIDs, sha)
	if err != nil || ep.FileExt != ext {
		http.NotFound(w, r)
		return
	}
	h.serveEpisodeFile(w, r, ep)
}

func (h *Handler) serveEpisodeFile(w http.ResponseWriter, r *http.Request, ep *store.Episode) {
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

	w.Header().Set("Content-Type", ep.FileMime)
	http.ServeContent(w, r, h.mediaName(ep), st.ModTime(), f)
}

// mediaName builds the served filename for an episode's stored file.
func (h *Handler) mediaName(e *store.Episode) string {
	return e.FileSHA + e.FileExt
}
