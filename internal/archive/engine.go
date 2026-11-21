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
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/prinzwalium/podcast-archiver/internal/config"
	"github.com/prinzwalium/podcast-archiver/internal/store"
)

type Engine struct {
	db      *store.DB
	fetcher *Fetcher
	cfg     *config.Config

	mu    sync.Mutex
	feeds map[int64]*feedState

	// objMu serializes changes to the object store with the refcount
	// updates that justify them: a file must not be removed between a
	// concurrent download finding it (dedup) and attaching to it.
	objMu sync.Mutex
}

// feedState serializes work on one feed. cancel aborts the running sync
// (used when the feed is deleted).
type feedState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewEngine(db *store.DB, fetcher *Fetcher, cfg *config.Config) *Engine {
	return &Engine{db: db, fetcher: fetcher, cfg: cfg, feeds: map[int64]*feedState{}}
}

func (e *Engine) feedState(id int64) *feedState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.feeds[id]
	if !ok {
		st = &feedState{}
		e.feeds[id] = st
	}
	return st
}

// lockFeedForDelete cancels a running sync of the feed and waits for it.
func (e *Engine) lockFeedForDelete(id int64) *feedState {
	st := e.feedState(id)
	e.mu.Lock()
	if st.cancel != nil {
		st.cancel()
	}
	e.mu.Unlock()
	st.mu.Lock()
	return st
}

var unsafeFilename = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

var ErrBudgetExceeded = errors.New("storage budget exceeded")

// mirrorGraceSyncs is how many consecutive syncs an episode must be missing
// from the source before mirror mode deletes it.
const mirrorGraceSyncs = 3

// budgetAllows checks the global storage cap and the per-user quota before
// adding globalBytes to the object store and userBytes to the user's usage.
func (e *Engine) budgetAllows(userID, globalBytes, userBytes int64) error {
	if e.cfg.MaxStorageBytes > 0 {
		used, err := e.db.StorageUsed()
		if err != nil {
			return err
		}
		if used+globalBytes > e.cfg.MaxStorageBytes {
			return fmt.Errorf("%w: global", ErrBudgetExceeded)
		}
	}
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
		if used+userBytes > quota {
			return fmt.Errorf("%w: user quota", ErrBudgetExceeded)
		}
	}
	return nil
}

// FeedLimit returns how many feeds the user may have; 0 = unlimited.
func (e *Engine) FeedLimit(u *store.User) int {
	if u.MaxFeeds > 0 {
		return int(u.MaxFeeds)
	}
	return e.cfg.MaxFeedsPerUser
}

// ValidateURL checks a user-supplied feed URL against the SSRF policy.
func (e *Engine) ValidateURL(raw string) error {
	return ValidateURL(raw, e.cfg.SSRFAllowPrivate)
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
// and reupload policies, and downloads missing media files. Syncs of the
// same feed never overlap.
func (e *Engine) SyncFeed(ctx context.Context, feed *store.Feed) error {
	st := e.feedState(feed.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	st.cancel = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		st.cancel = nil
		e.mu.Unlock()
	}()

	parsed, err := e.fetcher.FetchFeed(ctx, feed.SourceURL)
	if err != nil {
		_ = e.db.UpdateFeedSync(feed.ID, time.Now().Unix(), err.Error())
		return err
	}

	if err := e.db.UpdateFeedMeta(feed.ID, parsed.Title, feedImage(parsed), feedDescription(parsed), parsed.Link, parsed.Language); err != nil {
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
		isNew, oldURL, oldBytes, err := e.db.UpsertEpisode(ep)
		if err != nil {
			slog.Warn("upsert episode failed", "feed", feed.ID, "guid", guid, "err", err)
			continue
		}
		if isNew {
			_ = e.db.AddEvent(feed.ID, "episode_added", ep.Title)
		} else if IsReupload(oldURL, ep.EnclosureURL, oldBytes, ep.EnclosureBytes) {
			// "replace" clears the local file so the new version is
			// downloaded; "keep" retains the local copy as-is.
			if feed.KeepsReupload() {
				slog.Info("enclosure changed, keeping local copy (reupload=keep)", "feed", feed.Title, "guid", guid)
			} else if err := e.clearEpisodeFile(feed.ID, guid); err != nil {
				slog.Warn("clear episode file failed", "feed", feed.ID, "guid", guid, "err", err)
			} else {
				_ = e.db.AddEvent(feed.ID, "episode_replaced", ep.Title)
			}
		}
	}

	if feed.IsMirror() {
		if err := e.applyMirror(feed, guids); err != nil {
			return err
		}
	}

	// Download pending episodes (oldest first), respecting storage budgets.
	pending, err := e.db.UndownloadedEpisodes(feed.ID)
	if err != nil {
		return err
	}
	for _, ep := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.budgetAllows(feed.UserID, ep.EnclosureBytes, ep.EnclosureBytes); err != nil {
			slog.Warn("skipping download, storage budget exceeded", "feed", feed.ID, "guid", ep.GUID, "err", err)
			_ = e.db.AddEvent(feed.ID, "budget_skipped", ep.Title)
			continue
		}
		if err := e.downloadEpisode(ctx, feed, ep); err != nil {
			if errors.Is(err, ErrBudgetExceeded) {
				_ = e.db.AddEvent(feed.ID, "budget_skipped", ep.Title)
			}
			slog.Warn("episode download failed", "feed", feed.ID, "guid", ep.GUID, "err", err)
		}
	}

	return e.db.UpdateFeedSync(feed.ID, time.Now().Unix(), "")
}

// applyMirror deletes local episodes the source removed (pins survive).
// It refuses when the source lists nothing or drops more than half of a
// sizeable archive in one go, which usually means a broken or truncated
// feed rather than real deletions; and an episode is only deleted after
// it has been missing for mirrorGraceSyncs consecutive syncs.
func (e *Engine) applyMirror(feed *store.Feed, guids []string) error {
	if len(guids) == 0 {
		_ = e.db.AddEvent(feed.ID, "mirror_skipped", "source lists no episodes; nothing deleted")
		return nil
	}
	existing, err := e.db.EpisodeGUIDs(feed.ID)
	if err != nil {
		return err
	}
	listed := make(map[string]bool, len(guids))
	for _, g := range guids {
		listed[g] = true
	}
	missing := 0
	for _, g := range existing {
		if !listed[g] {
			missing++
		}
	}
	if len(existing) >= 10 && missing*2 > len(existing) {
		_ = e.db.AddEvent(feed.ID, "mirror_skipped",
			fmt.Sprintf("%d of %d episodes missing from source; nothing deleted (switch to archive mode or pin episodes to keep them)", missing, len(existing)))
		return nil
	}
	e.objMu.Lock()
	defer e.objMu.Unlock()
	removed, removable, err := e.db.DeleteMissing(feed.ID, guids, mirrorGraceSyncs)
	if err != nil {
		return err
	}
	e.removeObjectsLocked(removable)
	for _, guid := range removed {
		_ = e.db.AddEvent(feed.ID, "episode_removed", guid)
	}
	return nil
}

func (e *Engine) clearEpisodeFile(feedID int64, guid string) error {
	e.objMu.Lock()
	defer e.objMu.Unlock()
	removable, err := e.db.ClearEpisodeFile(feedID, guid)
	if err != nil {
		return err
	}
	e.removeObjectsLocked(removable)
	return nil
}

// removeObjectsLocked deletes stored objects whose last reference is gone.
// The caller holds objMu.
func (e *Engine) removeObjectsLocked(shas []string) {
	for _, sha := range shas {
		if err := os.Remove(e.objectPath(sha)); err != nil && !os.IsNotExist(err) {
			slog.Warn("file removal failed", "sha", sha, "err", err)
		}
	}
}

func (e *Engine) downloadEpisode(ctx context.Context, feed *store.Feed, ep *store.Episode) error {
	slog.Info("downloading episode", "feed", feed.Title, "episode", ep.Title)

	// Stream to a temp file while hashing; on success, rename into the
	// content-addressed store, which deduplicates across feeds and users.
	if err := os.MkdirAll(e.ContentDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(e.ContentDir(), tempPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)

	sha, size, err := e.fetcher.DownloadHashed(ctx, ep.EnclosureURL, tmpName, e.cfg.MaxEpisodeBytes)
	if err != nil {
		return err
	}

	e.objMu.Lock()
	defer e.objMu.Unlock()
	dest := e.objectPath(sha)
	_, statErr := os.Stat(dest)
	isNewObject := statErr != nil
	// Declared enclosure lengths are often missing or wrong; enforce the
	// budget again with the real size.
	globalBytes := size
	if !isNewObject {
		globalBytes = 0
	}
	if err := e.budgetAllows(feed.UserID, globalBytes, size); err != nil {
		return err
	}
	if isNewObject {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmpName, dest); err != nil {
			return err
		}
	} else {
		slog.Info("episode already stored, deduplicated", "feed", feed.Title, "sha", sha[:12])
	}

	if err := e.db.AttachFile(ep.ID, sha, size, ep.MimeType, mediaExt(ep.EnclosureURL, ep.MimeType)); err != nil {
		if isNewObject {
			if known, _ := e.db.FileExists(sha); !known {
				_ = os.Remove(dest)
			}
		}
		return err
	}
	return nil
}

// tempPrefix names in-progress downloads in ContentDir.
const tempPrefix = "dl-"

// Maintain runs at startup before any sync: it removes temp files left by
// interrupted downloads and repairs refcounts. Stored objects that the
// database does not know about are only reported, never deleted, so a lost
// or replaced database cannot wipe the media archive.
func (e *Engine) Maintain() error {
	if entries, err := os.ReadDir(e.ContentDir()); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() && strings.HasPrefix(ent.Name(), tempPrefix) {
				_ = os.Remove(filepath.Join(e.ContentDir(), ent.Name()))
			}
		}
	}
	e.objMu.Lock()
	defer e.objMu.Unlock()
	removable, err := e.db.RecomputeRefcounts()
	if err != nil {
		return err
	}
	e.removeObjectsLocked(removable)
	if len(removable) > 0 {
		slog.Info("removed unreferenced media files", "count", len(removable))
	}
	known, err := e.db.KnownFileSHAs()
	if err != nil {
		return err
	}
	untracked := 0
	shards, _ := os.ReadDir(e.ContentDir())
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		objs, _ := os.ReadDir(filepath.Join(e.ContentDir(), shard.Name()))
		for _, o := range objs {
			if !known[o.Name()] {
				untracked++
			}
		}
	}
	if untracked > 0 {
		slog.Warn("media files not referenced by the database (left in place)", "count", untracked, "dir", e.ContentDir())
	}
	return nil
}

// PredictSize fetches the source feed and estimates total archive size.
func (e *Engine) PredictSize(ctx context.Context, rawURL string) (*FeedPrediction, error) {
	return e.fetcher.FetchPrediction(ctx, rawURL)
}

// DeleteFeed cancels a running sync of the feed, removes all its episodes
// and drops their file references.
func (e *Engine) DeleteFeed(feed *store.Feed) error {
	st := e.lockFeedForDelete(feed.ID)
	defer st.mu.Unlock()
	e.objMu.Lock()
	defer e.objMu.Unlock()
	removable, err := e.db.DeleteFeed(feed.UserID, feed.ID)
	if err != nil {
		return err
	}
	e.removeObjectsLocked(removable)
	return nil
}

// DeleteUser removes a user and everything they own.
func (e *Engine) DeleteUser(userID int64) error {
	feeds, err := e.db.ListFeeds(userID)
	if err != nil {
		return err
	}
	for _, f := range feeds {
		st := e.lockFeedForDelete(f.ID)
		defer st.mu.Unlock()
	}
	e.objMu.Lock()
	defer e.objMu.Unlock()
	removable, err := e.db.DeleteUser(userID)
	if err != nil {
		return err
	}
	e.removeObjectsLocked(removable)
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
