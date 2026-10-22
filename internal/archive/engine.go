package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

var ErrBudgetExceeded = errors.New("storage budget exceeded")

// budgetAllows checks the global storage cap and the per-user quota before
// downloading an episode of the given size.
func (e *Engine) budgetAllows(userID int64, episodeBytes int64) error {
	if e.cfg.MaxStorageBytes > 0 {
		used, err := e.db.StorageUsed()
		if err != nil {
			return err
		}
		if used+episodeBytes > e.cfg.MaxStorageBytes {
			return fmt.Errorf("%w: global", ErrBudgetExceeded)
		}
	}
	if e.cfg.DefaultUserMaxBytes > 0 || true {
		user, err := e.db.GetUserByID(userID)
		if err != nil {
			return err
		}
		quota := user.MaxBytes
		if quota == 0 {
			quota = e.cfg.DefaultUserMaxBytes
		}
		if quota > 0 {
			used, err := e.db.UserStorageBytes(userID)
			if err != nil {
				return err
			}
			if used+episodeBytes > quota {
				return fmt.Errorf("%w: user quota", ErrBudgetExceeded)
			}
		}
	}
	return nil
}

// ContentDir returns the root directory for content-addressed media files.
func (e *Engine) ContentDir() string {
	return filepath.Join(e.cfg.MediaDir, "objects")
}

// objectPath maps a SHA-256 to its on-disk path (sharded to avoid huge dirs).
func (e *Engine) objectPath(sha string) string {
	return filepath.Join(e.ContentDir(), sha[:2], sha)
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

// SyncFeed fetches the source feed, upserts episodes, applies the feed's sync
// and reupload policies, and downloads missing media files.
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

		length, _ := strconvParse64(strings.TrimSpace(enc.Length))
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
		isNew, enclosureChanged, err := e.db.UpsertEpisode(ep)
		if err != nil {
			slog.Warn("upsert episode failed", "feed", feed.ID, "guid", guid, "err", err)
			continue
		}
		if isNew {
			_ = e.db.AddEvent(feed.ID, "episode_added", ep.Title)
		} else if enclosureChanged {
			// Re-upload detected. "replace" clears the local file so the new
			// version is downloaded; "keep" retains the local copy as-is.
			if feed.KeepsReupload() {
				slog.Info("enclosure changed, keeping local copy (reupload=keep)", "feed", feed.Title, "guid", guid)
			} else if err := e.db.ClearEpisodeFile(feed.ID, guid); err != nil {
				slog.Warn("clear episode file failed", "feed", feed.ID, "guid", guid, "err", err)
			} else {
				_ = e.db.AddEvent(feed.ID, "episode_replaced", ep.Title)
			}
		}
	}

	// Mirror mode: delete local episodes the source removed (pins survive).
	if feed.IsMirror() {
		removed, shas, err := e.db.DeleteMissing(feed.ID, guids)
		if err != nil {
			return err
		}
		for _, guid := range removed {
			_ = e.db.AddEvent(feed.ID, "episode_removed", guid)
		}
		for _, sha := range shas {
			e.unrefFile(sha)
		}
	}

	// Download pending episodes (oldest first), respecting storage budgets.
	pending, err := e.db.UndownloadedEpisodes(feed.ID)
	if err != nil {
		return err
	}
	for _, ep := range pending {
		if err := e.budgetAllows(feed.UserID, ep.EnclosureBytes); err != nil {
			slog.Warn("skipping download, storage budget exceeded", "feed", feed.ID, "guid", ep.GUID, "err", err)
			_ = e.db.AddEvent(feed.ID, "budget_skipped", ep.Title)
			continue
		}
		if err := e.downloadEpisode(ctx, feed, ep); err != nil {
			slog.Warn("episode download failed", "feed", feed.ID, "guid", ep.GUID, "err", err)
		}
	}

	return e.db.UpdateFeedSync(feed.ID, time.Now().Unix(), "")
}

// UnrefFile decrements a file's refcount and removes it from disk when it
// drops to zero. Exported for admin operations.
func (e *Engine) UnrefFile(sha string) { e.unrefFile(sha) }

func (e *Engine) unrefFile(sha string) {
	remove, err := e.db.FileUnref(sha)
	if err != nil {
		slog.Warn("file unref failed", "sha", sha, "err", err)
		return
	}
	if remove {
		if err := os.Remove(e.objectPath(sha)); err != nil && !os.IsNotExist(err) {
			slog.Warn("file removal failed", "sha", sha, "err", err)
		}
	}
}

func (e *Engine) downloadEpisode(ctx context.Context, feed *store.Feed, ep *store.Episode) error {
	slog.Info("downloading episode", "feed", feed.Title, "episode", ep.Title)

	// Stream to a temp file while hashing; on success, rename into the
	// content-addressed store, which deduplicates across feeds and users.
	tmp, err := os.CreateTemp(e.ContentDir(), "dl-*")
	if err != nil {
		if err := os.MkdirAll(e.ContentDir(), 0o755); err != nil {
			return err
		}
		tmp, err = os.CreateTemp(e.ContentDir(), "dl-*")
		if err != nil {
			return err
		}
	}
	defer os.Remove(tmp.Name())

	sha, size, err := e.fetcher.DownloadHashed(ctx, ep.EnclosureURL, tmp.Name(), e.cfg.MaxEpisodeBytes)
	if err != nil {
		return err
	}

	dest := e.objectPath(sha)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		// Already stored (deduplicated); drop the temp copy.
		if err := tmp.Close(); err != nil {
			return err
		}
		slog.Info("episode already stored, deduplicated", "feed", feed.Title, "sha", sha[:12])
	} else {
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return err
		}
	}

	if err := e.db.FileRef(sha, size, ep.MimeType, mediaExt(ep.EnclosureURL, ep.MimeType)); err != nil {
		return err
	}
	return e.db.MarkEpisodeDownloaded(ep, sha)
}

// PredictSize fetches the source feed and estimates total archive size.
func (e *Engine) PredictSize(ctx context.Context, rawURL string) (*FeedPrediction, error) {
	return e.fetcher.FetchPrediction(ctx, rawURL)
}

// DeleteFeed removes all episodes and decrements file refcounts.
func (e *Engine) DeleteFeed(feed *store.Feed) error {
	episodes, err := e.db.ListEpisodes(feed.ID)
	if err != nil {
		return err
	}
	if err := e.db.DeleteFeed(feed.UserID, feed.ID); err != nil {
		return err
	}
	for _, ep := range episodes {
		if ep.Downloaded && ep.FileSHA != "" {
			e.unrefFile(ep.FileSHA)
		}
	}
	return nil
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

func strconvParse64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
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
