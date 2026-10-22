package httpd

import (
	"encoding/xml"
	"github.com/prinzwalium/podcast-archiver/internal/store"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// opmlXML is the minimal OPML 2.0 envelope for podcast subscription lists.
type opmlXML struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title string `xml:"title"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text    string `xml:"text,attr"`
	Type    string `xml:"type,attr,omitempty"`
	XMLURL  string `xml:"xmlUrl,attr,omitempty"`
	HTMLURL string `xml:"htmlUrl,attr,omitempty"`
}

func (h *Handler) exportOPML(w http.ResponseWriter, r *http.Request, u *store.User) {
	feeds, err := h.db.ListFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	doc := opmlXML{Version: "2.0", Head: opmlHead{Title: "Podcast Archiver subscriptions"}}
	for _, f := range feeds {
		doc.Body.Outlines = append(doc.Body.Outlines, opmlOutline{
			Text: f.Title, Type: "rss", XMLURL: f.SourceURL, HTMLURL: f.Link,
		})
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=podcast-archiver.opml")
	_ = xml.NewEncoder(w).Encode(doc)
}

func (h *Handler) importOPML(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("opml")
	if err != nil {
		http.Error(w, "opml file required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	body, err := io.ReadAll(io.LimitReader(file, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var doc opmlXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		http.Error(w, "invalid OPML: "+err.Error(), http.StatusBadRequest)
		return
	}

	feeds, err := h.db.ListFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	existing := map[string]bool{}
	for _, f := range feeds {
		existing[f.SourceURL] = true
	}

	added := 0
	var dupes, failed int
	for _, o := range doc.Body.Outlines {
		url := strings.TrimSpace(o.XMLURL)
		if url == "" {
			continue
		}
		if existing[url] {
			dupes++
			continue
		}
		if len(feeds)+added >= h.cfg.MaxFeedsPerUser {
			http.Error(w, "feed limit reached", http.StatusTooManyRequests)
			return
		}
		if _, err := h.db.CreateFeed(u.ID, url, o.Text, "", "", "", "mirror", "replace"); err != nil {
			failed++
			continue
		}
		added++
		existing[url] = true
	}
	http.Redirect(w, r, "/?imported="+strconv.Itoa(added), http.StatusFound)
}

func (h *Handler) pinEpisode(w http.ResponseWriter, r *http.Request, u *store.User) {
	h.setPin(w, r, u, true)
}

func (h *Handler) unpinEpisode(w http.ResponseWriter, r *http.Request, u *store.User) {
	h.setPin(w, r, u, false)
}

func (h *Handler) setPin(w http.ResponseWriter, r *http.Request, u *store.User, pinned bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	guid := r.PathValue("guid")
	if err := h.db.SetEpisodePin(u.ID, id, guid, pinned); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/feeds/"+strconv.FormatInt(id, 10), http.StatusFound)
}
