package httpd

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
)

// serveFont serves the embedded theme fonts.
func (h *Handler) serveFont(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if !strings.HasSuffix(name, ".woff2") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(fontFS, "fonts/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(data)
}
