package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

type Engine struct {
	db      *store.DB
	fetcher *Fetcher
	cfg     *config.Config
}

func NewEngine(db *store.DB, fetcher *Fetcher, cfg *config.Config) *Engine {
	return &Engine{db: db, fetcher: fetcher, cfg: cfg}
}

var unsafeFilename = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// EpisodeDir returns the on-disk directory for a feed's media files.
func (e *Engine) EpisodeDir(feed *store.Feed) string {
	sum := sha256.Sum256([]byte(feed.SourceURL))
	name := unsafeFilename.ReplaceAllString(strings.TrimSpace(feed.Title), "-")
	if name == "" {
		name = "feed"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	return filepath.Join(e.cfg.DataDir, "media", fmt.Sprintf("%s-%s", name, hex.EncodeToString(sum[:4])))
}

// enclosure returns the first audio enclosure of an item, or nil.
func enclosure(item *gofeed.Item) *gofeed.Enclosure {
	for _, enc := range item.Enclosures {
		if enc.URL != "" {
			return enc
		}
	}
	return nil
}

// SyncFeed fetches the source feed, upserts episodes, deletes removed ones
// (mirror mode), and downloads missing media files.
func (e *Engine) SyncFeed(ctx context.Context, feed *store.Feed) error {
	parsed, err := e.fetcher.FetchFeed(ctx, feed.SourceURL)
	if err != nil {
		_ = e.db.UpdateFeedSync(feed.ID, time.Now().Unix(), err.Error())
		return err
	}

	if err := e.db.UpdateFeedMeta(feed.ID, parsed.Title, feedImage(parsed), feedDescription(parsed), parsed.Link); err != nil {
		return err
	}

	var guids []string
	dir := e.EpisodeDir(feed)
	for _, item := range parsed.Items {
		enc := enclosure(item)
		if enc == nil {
			continue
		}
		guid := item.GUID
		if guid == "" {
			guid = enc.URL
		}
		guids = append(guids, guid)

		length, _ := strconv.ParseInt(strings.TrimSpace(enc.Length), 10, 64)
		ep := &store.Episode{
			FeedID:         feed.ID,
			GUID:           guid,
			Title:          item.Title,
			Published:      pubTime(item),
			Description:    item.Description,
			EnclosureURL:   enc.URL,
			EnclosureBytes: length,
			MimeType:       enclosureMime(enc.Type),
			DurationSecs:   itunesDuration(item),
			ImageURL:       itemImage(item),
		}
		if _, err := e.db.UpsertEpisode(ep); err != nil {
			slog.Warn("upsert episode failed", "feed", feed.ID, "guid", guid, "err", err)
		}
	}

	// Mirror mode: delete local episodes the source removed.
	removed, err := e.db.DeleteMissing(feed.ID, guids)
	if err != nil {
		return err
	}
	for _, p := range removed {
		if p != "" {
			full := filepath.Join(e.cfg.DataDir, "media", p)
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				slog.Warn("cleanup failed", "path", full, "err", err)
			}
		}
	}

	// Download pending episodes (oldest first).
	pending, err := e.db.UndownloadedEpisodes(feed.ID)
	if err != nil {
		return err
	}
	for _, ep := range pending {
		if err := e.downloadEpisode(ctx, feed, ep, dir); err != nil {
			slog.Warn("episode download failed", "feed", feed.ID, "guid", ep.GUID, "err", err)
		}
	}

	return e.db.UpdateFeedSync(feed.ID, time.Now().Unix(), "")
}

func (e *Engine) downloadEpisode(ctx context.Context, feed *store.Feed, ep *store.Episode, dir string) error {
	filename := unsafeFilename.ReplaceAllString(ep.GUID, "-")
	if len(filename) > 100 {
		filename = filename[:100]
	}
	if filename == "" {
		filename = strconv.FormatInt(ep.ID, 10)
	}
	ext := mediaExt(ep.EnclosureURL, ep.MimeType)
	dest := filepath.Join(dir, filename+ext)

	slog.Info("downloading episode", "feed", feed.Title, "episode", ep.Title)
	n, err := e.fetcher.Download(ctx, ep.EnclosureURL, dest, e.cfg.MaxEpisodeBytes, nil)
	if err != nil {
		return err
	}
	ep.FilePath = filepath.Join(filepath.Base(dir), filepath.Base(dest))
	ep.FileBytes = n
	return e.db.MarkEpisodeDownloaded(ep)
}

// PredictSize fetches the source feed and estimates total archive size.
func (e *Engine) PredictSize(ctx context.Context, rawURL string) (*FeedPrediction, error) {
	return e.fetcher.FetchPrediction(ctx, rawURL)
}

func feedImage(f *gofeed.Feed) string {
	if f.Image != nil && f.Image.URL != "" {
		return f.Image.URL
	}
	if f.ITunesExt != nil && f.ITunesExt.Image != "" {
		return f.ITunesExt.Image
	}
	return ""
}

func itemImage(item *gofeed.Item) string {
	if item.ITunesExt != nil && item.ITunesExt.Image != "" {
		return item.ITunesExt.Image
	}
	if item.Image != nil && item.Image.URL != "" {
		return item.Image.URL
	}
	return ""
}

func feedDescription(f *gofeed.Feed) string {
	if d := strings.TrimSpace(f.Description); d != "" {
		return d
	}
	if f.ITunesExt != nil {
		return f.ITunesExt.Summary
	}
	return ""
}

func pubTime(item *gofeed.Item) int64 {
	if item.PublishedParsed != nil {
		return item.PublishedParsed.Unix()
	}
	if item.UpdatedParsed != nil {
		return item.UpdatedParsed.Unix()
	}
	return 0
}

func enclosureMime(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "audio/mpeg"
	}
	return t
}

func itunesDuration(item *gofeed.Item) int {
	d := ""
	if item.ITunesExt != nil {
		d = strings.TrimSpace(item.ITunesExt.Duration)
	}
	if d == "" {
		return 0
	}
	var h, m, s int
	parts := strings.Split(d, ":")
	switch len(parts) {
	case 3:
		fmt.Sscanf(parts[0], "%d", &h)
		fmt.Sscanf(parts[1], "%d", &m)
		fmt.Sscanf(parts[2], "%d", &s)
	case 2:
		fmt.Sscanf(parts[0], "%d", &m)
		fmt.Sscanf(parts[1], "%d", &s)
	case 1:
		fmt.Sscanf(parts[0], "%d", &s)
	}
	return h*3600 + m*60 + s
}

func mediaExt(enclosureURL, mime string) string {
	if u, err := url.Parse(enclosureURL); err == nil {
		ext := strings.ToLower(filepath.Ext(u.Path))
		switch ext {
		case ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".flac", ".wav", ".mp4":
			return ext
		}
	}
	switch mime {
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/opus":
		return ".opus"
	case "audio/flac":
		return ".flac"
	default:
		return ".mp3"
	}
}
