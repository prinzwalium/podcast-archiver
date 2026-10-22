package httpd

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/prinzwalium/podcast-archiver/internal/archive"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

func fmtBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

type pageData struct {
	Title       string
	UserName    string
	AuthEnabled bool
	ExternalURL string

	Feeds              []*store.Feed
	Feed               *store.Feed
	Episodes           []*episodeView
	Stats              *statsView
	Tokens             []*store.FeedToken
	StorageUsed        string
	Prediction         *archive.FeedPrediction
	PredictionURL      string
	PredictionBytesFmt string
}

type episodeView struct {
	Title        string
	Downloaded   bool
	FileBytesFmt string
}

type statsView struct {
	TotalEpisodes      int
	Downloaded         int
	TotalBytesFmt      string
	DownloadedBytesFmt string
}

func (h *Handler) render(w http.ResponseWriter, status int, data *pageData) {
	data.AuthEnabled = h.cfg.AuthEnabled()
	data.ExternalURL = h.cfg.ExternalURL
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := templates.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("template render", "err", err)
	}
}

func (h *Handler) pageFeeds(w http.ResponseWriter, r *http.Request, u *store.User) {
	feeds, err := h.db.ListFeeds(u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	used, _ := h.db.StorageUsed()
	h.render(w, http.StatusOK, &pageData{
		Title:       "Feeds",
		UserName:    u.Name,
		Feeds:       feeds,
		StorageUsed: fmtBytes(used),
	})
}

func (h *Handler) pageFeedDetail(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconvParse64(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	feed, err := h.db.GetFeed(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	episodes, err := h.db.ListEpisodes(feed.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	stats, err := h.db.FeedStatsFor(feed.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	tokens, err := h.db.ListFeedTokens(feed.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var views []*episodeView
	for _, e := range episodes {
		size := int64(0)
		if e.Downloaded && e.FileSHA != "" {
			if file, err := h.db.GetFile(e.FileSHA); err == nil {
				size = file.Size
			}
		}
		views = append(views, &episodeView{Title: e.Title, Downloaded: e.Downloaded, FileBytesFmt: fmtBytes(size)})
	}
	h.render(w, http.StatusOK, &pageData{
		Title:    feed.Title,
		UserName: u.Name,
		Feed:     feed,
		Episodes: views,
		Stats: &statsView{
			TotalEpisodes:      stats.TotalEpisodes,
			Downloaded:         stats.Downloaded,
			TotalBytesFmt:      fmtBytes(stats.TotalBytes),
			DownloadedBytesFmt: fmtBytes(stats.DownloadedBytes),
		},
		Tokens: tokens,
	})
}

func (h *Handler) pagePredict(w http.ResponseWriter, r *http.Request, u *store.User) {
	h.render(w, http.StatusOK, &pageData{Title: "Predict", UserName: u.Name})
}

func (h *Handler) renderPredictResult(w http.ResponseWriter, p *archive.FeedPrediction, rawURL string) {
	h.render(w, http.StatusOK, &pageData{
		Title:              "Predict",
		Prediction:         p,
		PredictionURL:      rawURL,
		PredictionBytesFmt: fmtBytes(p.TotalBytes),
	})
}

func strconvParse64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
