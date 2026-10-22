package store

import (
	"database/sql"
	"errors"
	"time"
)

type User struct {
	ID        int64
	Sub       string
	Name      string
	CreatedAt int64
}

type Feed struct {
	ID           int64
	UserID       int64
	SourceURL    string
	Title        string
	ImageURL     string
	Description  string
	Link         string
	SyncMode     string // "mirror" (follow upstream deletions) or "archive" (immutable)
	ReuploadMode string // "replace" (re-download changed files) or "keep" (retain local copy)
	CreatedAt    int64
	LastSync     int64
	LastError    string
}

func (f *Feed) IsMirror() bool      { return f.SyncMode != "archive" }
func (f *Feed) KeepsReupload() bool { return f.ReuploadMode == "keep" }

type Episode struct {
	ID             int64
	FeedID         int64
	GUID           string
	Title          string
	Published      int64
	Description    string
	EnclosureURL   string
	EnclosureBytes int64
	MimeType       string
	DurationSecs   int
	ImageURL       string
	FileSHA        string
	Downloaded     bool
}

type File struct {
	SHA      string
	Size     int64
	MimeType string
	Ext      string
	Refcount int64
}

type FeedToken struct {
	ID        int64
	FeedID    int64
	Token     string
	CreatedAt int64
}

func (e *Episode) PublishedTime() time.Time { return time.Unix(e.Published, 0) }

func (db *DB) UpsertUser(sub, name string) (*User, error) {
	res, err := db.Exec(`INSERT INTO users (sub, name, created_at) VALUES (?, ?, ?)
		ON CONFLICT(sub) DO UPDATE SET name = excluded.name`, sub, name, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return db.GetUserByID(id)
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	u := &User{}
	err := db.QueryRow(`SELECT id, sub, name, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Sub, &u.Name, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (db *DB) CreateFeed(userID int64, sourceURL, title, imageURL, description, link, syncMode, reuploadMode string) (*Feed, error) {
	res, err := db.Exec(`INSERT INTO feeds (user_id, source_url, title, image_url, description, link, sync_mode, reupload_mode, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, sourceURL, title, imageURL, description, link, syncMode, reuploadMode, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Feed{ID: id, UserID: userID, SourceURL: sourceURL, Title: title,
		ImageURL: imageURL, Description: description, Link: link,
		SyncMode: syncMode, ReuploadMode: reuploadMode, CreatedAt: now()}, nil
}

const feedCols = `id, user_id, source_url, title, image_url, description, link, sync_mode, reupload_mode, created_at, last_sync, last_error`

func scanFeeds(rows *sql.Rows) ([]*Feed, error) {
	var out []*Feed
	for rows.Next() {
		f := &Feed{}
		if err := rows.Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link,
			&f.SyncMode, &f.ReuploadMode, &f.CreatedAt, &f.LastSync, &f.LastError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (db *DB) ListFeeds(userID int64) ([]*Feed, error) {
	rows, err := db.Query(`SELECT `+feedCols+` FROM feeds WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (db *DB) ListAllFeeds() ([]*Feed, error) {
	rows, err := db.Query(`SELECT ` + feedCols + ` FROM feeds`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (db *DB) GetFeed(userID, feedID int64) (*Feed, error) {
	f := &Feed{}
	err := db.QueryRow(`SELECT `+feedCols+` FROM feeds WHERE id = ? AND user_id = ?`, feedID, userID).
		Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link,
			&f.SyncMode, &f.ReuploadMode, &f.CreatedAt, &f.LastSync, &f.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (db *DB) DeleteFeed(userID, feedID int64) error {
	res, err := db.Exec(`DELETE FROM feeds WHERE id = ? AND user_id = ?`, feedID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateFeedSettings changes sync_mode / reupload_mode for a feed.
func (db *DB) UpdateFeedSettings(feedID int64, syncMode, reuploadMode string) error {
	_, err := db.Exec(`UPDATE feeds SET sync_mode = ?, reupload_mode = ? WHERE id = ?`, syncMode, reuploadMode, feedID)
	return err
}

func (db *DB) UpdateFeedSync(feedID int64, ts int64, syncErr string) error {
	_, err := db.Exec(`UPDATE feeds SET last_sync = ?, last_error = ? WHERE id = ?`, ts, syncErr, feedID)
	return err
}

func (db *DB) UpdateFeedMeta(feedID int64, title, imageURL, description, link string) error {
	_, err := db.Exec(`UPDATE feeds SET title = ?, image_url = ?, description = ?, link = ? WHERE id = ?`,
		title, imageURL, description, link, feedID)
	return err
}

// UpsertEpisode inserts or refreshes feed metadata for an episode. It never
// touches download state (file_sha/downloaded), which is managed by the engine.
// Returns true when the enclosure URL changed (candidate for reupload handling).
func (db *DB) UpsertEpisode(ep *Episode) (bool, error) {
	var oldEnclosure string
	err := db.QueryRow(`SELECT enclosure_url FROM episodes WHERE feed_id = ? AND guid = ?`, ep.FeedID, ep.GUID).Scan(&oldEnclosure)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := db.Exec(`INSERT INTO episodes
			(feed_id, guid, title, published, description, enclosure_url, enclosure_bytes, mime_type, duration_secs, image_url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ep.FeedID, ep.GUID, ep.Title, ep.Published, ep.Description, ep.EnclosureURL, ep.EnclosureBytes, ep.MimeType, ep.DurationSecs, ep.ImageURL)
		return false, err
	}
	if err != nil {
		return false, err
	}
	_, err = db.Exec(`UPDATE episodes SET
			title = ?, published = ?, description = ?, enclosure_url = ?, enclosure_bytes = ?, mime_type = ?, duration_secs = ?, image_url = ?
		WHERE feed_id = ? AND guid = ?`,
		ep.Title, ep.Published, ep.Description, ep.EnclosureURL, ep.EnclosureBytes, ep.MimeType, ep.DurationSecs, ep.ImageURL,
		ep.FeedID, ep.GUID)
	return oldEnclosure != ep.EnclosureURL, err
}

// ClearEpisodeFile clears download state for an episode whose content changed
// upstream (reupload with "replace" policy) so the engine re-downloads it.
func (db *DB) ClearEpisodeFile(feedID int64, guid string) error {
	_, err := db.Exec(`UPDATE episodes SET downloaded = 0, file_sha = '' WHERE feed_id = ? AND guid = ?`, feedID, guid)
	return err
}

// DeleteMissing removes episodes whose GUIDs are absent from the given set
// (mirror mode). Returns the SHAs of files that may need refcount decrement.
func (db *DB) DeleteMissing(feedID int64, keepGUIDs []string) ([]string, error) {
	rows, err := db.Query(`SELECT guid, file_sha FROM episodes WHERE feed_id = ?`, feedID)
	if err != nil {
		return nil, err
	}
	type row struct{ guid, sha string }
	var existing []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.guid, &r.sha); err != nil {
			rows.Close()
			return nil, err
		}
		existing = append(existing, r)
	}
	rows.Close()

	keep := map[string]bool{}
	for _, g := range keepGUIDs {
		keep[g] = true
	}
	var shas []string
	for _, r := range existing {
		if !keep[r.guid] {
			if _, err := db.Exec(`DELETE FROM episodes WHERE feed_id = ? AND guid = ?`, feedID, r.guid); err != nil {
				return shas, err
			}
			if r.sha != "" {
				shas = append(shas, r.sha)
			}
		}
	}
	return shas, nil
}

const episodeCols = `id, feed_id, guid, title, published, description, enclosure_url, enclosure_bytes,
	mime_type, duration_secs, image_url, file_sha, downloaded`

func scanEpisodes(rows *sql.Rows) ([]*Episode, error) {
	var out []*Episode
	for rows.Next() {
		e := &Episode{}
		if err := rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.Published, &e.Description, &e.EnclosureURL,
			&e.EnclosureBytes, &e.MimeType, &e.DurationSecs, &e.ImageURL, &e.FileSHA, &e.Downloaded); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) ListEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(`SELECT `+episodeCols+` FROM episodes WHERE feed_id = ? ORDER BY published DESC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

func (db *DB) UndownloadedEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(`SELECT `+episodeCols+` FROM episodes WHERE feed_id = ? AND downloaded = 0 ORDER BY published ASC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

// FileRef increments the refcount for a stored file (or inserts it).
func (db *DB) FileRef(sha string, size int64, mimeType, ext string) error {
	_, err := db.Exec(`INSERT INTO files (sha, size, mime_type, ext, refcount) VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(sha) DO UPDATE SET refcount = refcount + 1`, sha, size, mimeType, ext)
	return err
}

// FileUnref decrements the refcount and reports whether the file should be
// removed from disk (refcount reached zero).
func (db *DB) FileUnref(sha string) (bool, error) {
	res, err := db.Exec(`UPDATE files SET refcount = refcount - 1 WHERE sha = ?`, sha)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	var rc int64
	if err := db.QueryRow(`SELECT refcount FROM files WHERE sha = ?`, sha).Scan(&rc); err != nil {
		return false, err
	}
	if rc <= 0 {
		if _, err := db.Exec(`DELETE FROM files WHERE sha = ?`, sha); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// FileExists checks whether a content-addressed file is already stored.
func (db *DB) FileExists(sha string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM files WHERE sha = ?`, sha).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (db *DB) GetFile(sha string) (*File, error) {
	f := &File{}
	err := db.QueryRow(`SELECT sha, size, mime_type, ext, refcount FROM files WHERE sha = ?`, sha).
		Scan(&f.SHA, &f.Size, &f.MimeType, &f.Ext, &f.Refcount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (db *DB) MarkEpisodeDownloaded(ep *Episode, sha string) error {
	_, err := db.Exec(`UPDATE episodes SET downloaded = 1, file_sha = ? WHERE id = ?`, sha, ep.ID)
	return err
}

// FeedStats aggregates per-feed totals.
type FeedStats struct {
	TotalEpisodes   int
	Downloaded      int
	TotalBytes      int64
	DownloadedBytes int64
}

func (db *DB) FeedStatsFor(feedID int64) (*FeedStats, error) {
	s := &FeedStats{}
	err := db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN downloaded THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(enclosure_bytes), 0),
			COALESCE(SUM(CASE WHEN downloaded THEN fl.size IS NOT NULL THEN fl.size ELSE 0 END), 0)
		FROM episodes e LEFT JOIN files fl ON fl.sha = e.file_sha
		WHERE e.feed_id = ?`, feedID).
		Scan(&s.TotalEpisodes, &s.Downloaded, &s.TotalBytes, &s.DownloadedBytes)
	return s, err
}

func (db *DB) AllStats() (map[int64]*FeedStats, error) {
	rows, err := db.Query(`SELECT e.feed_id, COUNT(*),
			COALESCE(SUM(CASE WHEN e.downloaded THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(e.enclosure_bytes), 0),
			COALESCE(SUM(CASE WHEN e.downloaded THEN COALESCE(fl.size, 0) ELSE 0 END), 0)
		FROM episodes e LEFT JOIN files fl ON fl.sha = e.file_sha GROUP BY e.feed_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*FeedStats{}
	for rows.Next() {
		var fid int64
		s := &FeedStats{}
		if err := rows.Scan(&fid, &s.TotalEpisodes, &s.Downloaded, &s.TotalBytes, &s.DownloadedBytes); err != nil {
			return nil, err
		}
		out[fid] = s
	}
	return out, rows.Err()
}

func (db *DB) CreateFeedToken(feedID int64, token string) error {
	_, err := db.Exec(`INSERT INTO feed_tokens (feed_id, token, created_at) VALUES (?, ?, ?)`,
		feedID, token, now())
	return err
}

func (db *DB) DeleteFeedToken(feedID int64, token string) error {
	_, err := db.Exec(`DELETE FROM feed_tokens WHERE feed_id = ? AND token = ?`, feedID, token)
	return err
}

func (db *DB) ListFeedTokens(feedID int64) ([]*FeedToken, error) {
	rows, err := db.Query(`SELECT id, feed_id, token, created_at FROM feed_tokens WHERE feed_id = ?`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FeedToken
	for rows.Next() {
		t := &FeedToken{}
		if err := rows.Scan(&t.ID, &t.FeedID, &t.Token, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResolveFeedToken returns the feed for a token.
func (db *DB) ResolveFeedToken(token string) (*Feed, error) {
	rows, err := db.Query(`SELECT `+feedCols+` FROM feed_tokens t JOIN feeds f ON f.id = t.feed_id WHERE t.token = ?`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	feeds, err := scanFeeds(rows)
	if err != nil {
		return nil, err
	}
	if len(feeds) == 0 {
		return nil, ErrNotFound
	}
	return feeds[0], nil
}

// StorageUsed returns total unique bytes on disk (deduplicated).
func (db *DB) StorageUsed() (int64, error) {
	var v sql.NullInt64
	err := db.QueryRow(`SELECT COALESCE(SUM(size * (CASE WHEN refcount > 0 THEN 1 ELSE 0 END)), 0) FROM files`).Scan(&v)
	return v.Int64, err
}
