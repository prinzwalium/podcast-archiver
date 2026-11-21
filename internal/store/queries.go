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
	MaxFeeds  int64 // 0 = unlimited (or server default)
	MaxBytes  int64 // 0 = unlimited (or server default)
	IsAdmin   bool
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
	RefreshSecs  int64  // per-feed override; 0 = server default interval
	CreatedAt    int64
	LastSync     int64
	LastError    string
	FailCount    int64
	Language     string
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
	Pinned         bool

	// Joined from files for downloaded episodes; zero otherwise.
	FileSize int64
	FileExt  string
	FileMime string
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
	_, err := db.Exec(`INSERT INTO users (sub, name, created_at) VALUES (?, ?, ?)
		ON CONFLICT(sub) DO UPDATE SET name = excluded.name`, sub, name, now())
	if err != nil {
		return nil, err
	}
	return db.GetUserBySub(sub)
}

const userCols = `id, sub, name, created_at, max_feeds, max_bytes, is_admin`

func scanUsers(rows *sql.Rows) ([]*User, error) {
	var out []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Sub, &u.Name, &u.CreatedAt, &u.MaxFeeds, &u.MaxBytes, &u.IsAdmin); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	rows, err := db.Query(`SELECT `+userCols+` FROM users WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users, err := scanUsers(rows)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrNotFound
	}
	return users[0], nil
}

func (db *DB) GetUserBySub(sub string) (*User, error) {
	rows, err := db.Query(`SELECT `+userCols+` FROM users WHERE sub = ?`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users, err := scanUsers(rows)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, ErrNotFound
	}
	return users[0], nil
}

func (db *DB) ListUsers() ([]*User, error) {
	rows, err := db.Query(`SELECT ` + userCols + ` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsers(rows)
}

// SetUserAdmin grants or revokes admin rights (driven by OIDC groups).
func (db *DB) SetUserAdmin(userID int64, isAdmin bool) error {
	_, err := db.Exec(`UPDATE users SET is_admin = ? WHERE id = ?`, btoi(isAdmin), userID)
	return err
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

const feedCols = `id, user_id, source_url, title, image_url, description, link, sync_mode, reupload_mode, refresh_secs, created_at, last_sync, last_error, fail_count, language`

func scanFeeds(rows *sql.Rows) ([]*Feed, error) {
	var out []*Feed
	for rows.Next() {
		f := &Feed{}
		if err := rows.Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link,
			&f.SyncMode, &f.ReuploadMode, &f.RefreshSecs, &f.CreatedAt, &f.LastSync, &f.LastError, &f.FailCount, &f.Language); err != nil {
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
	rows, err := db.Query(`SELECT `+feedCols+` FROM feeds WHERE id = ? AND user_id = ?`, feedID, userID)
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

// CountFeeds returns how many feeds a user has.
func (db *DB) CountFeeds(userID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM feeds WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// UpdateFeedSettings changes sync_mode / reupload_mode / refresh interval.
// refreshSecs <= 0 means "use server default".
func (db *DB) UpdateFeedSettings(feedID int64, syncMode, reuploadMode string, refreshSecs int64) error {
	_, err := db.Exec(`UPDATE feeds SET sync_mode = ?, reupload_mode = ?, refresh_secs = ? WHERE id = ?`,
		syncMode, reuploadMode, refreshSecs, feedID)
	return err
}

// AddEvent records a sync activity event. Kinds: episode_added, episode_removed,
// episode_replaced, feed_error, feed_recovered.
func (db *DB) AddEvent(feedID int64, kind, detail string) error {
	_, err := db.Exec(`INSERT INTO events (feed_id, kind, detail, created_at) VALUES (?, ?, ?, ?)`,
		feedID, kind, detail, now())
	return err
}

type Event struct {
	ID        int64
	FeedID    int64
	Kind      string
	Detail    string
	CreatedAt int64
}

func (db *DB) ListEvents(feedID int64, limit int) ([]*Event, error) {
	rows, err := db.Query(`SELECT id, feed_id, kind, detail, created_at FROM events
		WHERE feed_id = ? ORDER BY id DESC LIMIT ?`, feedID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		ev := &Event{}
		if err := rows.Scan(&ev.ID, &ev.FeedID, &ev.Kind, &ev.Detail, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// RecentFailures returns a user's feeds with consecutive sync failures, for
// alerting. userID 0 returns failing feeds of all users (admin view).
func (db *DB) RecentFailures(userID int64) ([]*Feed, error) {
	q := `SELECT ` + feedCols + ` FROM feeds WHERE fail_count > 0`
	var args []any
	if userID != 0 {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	rows, err := db.Query(q+` ORDER BY fail_count DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// SetEpisodePin pins or unpins an episode (pins survive mirror deletions).
func (db *DB) SetEpisodePin(userID, feedID int64, guid string, pinned bool) error {
	res, err := db.Exec(`UPDATE episodes SET pinned = ?
		WHERE guid = ? AND feed_id = ? AND feed_id IN (SELECT id FROM feeds WHERE id = ? AND user_id = ?)`,
		btoi(pinned), guid, feedID, feedID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserQuotas sets per-user feed/byte limits (0 = unlimited).
func (db *DB) UpdateUserQuotas(userID, maxFeeds, maxBytes int64) error {
	_, err := db.Exec(`UPDATE users SET max_feeds = ?, max_bytes = ? WHERE id = ?`, maxFeeds, maxBytes, userID)
	return err
}

// UserStorageBytes sums the sizes of files referenced by a user's episodes,
// apportioned by refcount (rounded) so shared files count fractionally toward quotas.
func (db *DB) UserStorageBytes(userID int64) (int64, error) {
	var v int64
	err := db.QueryRow(`SELECT COALESCE(SUM((fl.size + MAX(fl.refcount, 1) / 2) / MAX(fl.refcount, 1)), 0)
		FROM episodes e
		JOIN feeds f ON f.id = e.feed_id
		JOIN files fl ON fl.sha = e.file_sha
		WHERE f.user_id = ? AND e.downloaded = 1`, userID).Scan(&v)
	return v, err
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (db *DB) UpdateFeedSync(feedID int64, ts int64, syncErr string) error {
	var failCount int64
	if syncErr != "" {
		if err := db.QueryRow(`SELECT fail_count FROM feeds WHERE id = ?`, feedID).Scan(&failCount); err != nil {
			return err
		}
		failCount++
		_, err := db.Exec(`UPDATE feeds SET last_sync = ?, last_error = ?, fail_count = ? WHERE id = ?`, ts, syncErr, failCount, feedID)
		return err
	}
	_, err := db.Exec(`UPDATE feeds SET last_sync = ?, last_error = '', fail_count = 0 WHERE id = ?`, ts, feedID)
	return err
}

func (db *DB) UpdateFeedMeta(feedID int64, title, imageURL, description, link, language string) error {
	_, err := db.Exec(`UPDATE feeds SET title = ?, image_url = ?, description = ?, link = ?, language = ? WHERE id = ?`,
		title, imageURL, description, link, language, feedID)
	return err
}

// UpsertEpisode inserts or refreshes feed metadata for an episode and resets
// its mirror-missing counter. It never touches download state
// (file_sha/downloaded/pinned), which the engine manages. For existing
// episodes it returns the previous enclosure URL and declared length.
func (db *DB) UpsertEpisode(ep *Episode) (isNew bool, oldURL string, oldBytes int64, err error) {
	err = db.QueryRow(`SELECT enclosure_url, enclosure_bytes FROM episodes WHERE feed_id = ? AND guid = ?`, ep.FeedID, ep.GUID).Scan(&oldURL, &oldBytes)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := db.Exec(`INSERT INTO episodes
			(feed_id, guid, title, published, description, enclosure_url, enclosure_bytes, mime_type, duration_secs, image_url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ep.FeedID, ep.GUID, ep.Title, ep.Published, ep.Description, ep.EnclosureURL, ep.EnclosureBytes, ep.MimeType, ep.DurationSecs, ep.ImageURL)
		return true, "", 0, err
	}
	if err != nil {
		return false, "", 0, err
	}
	_, err = db.Exec(`UPDATE episodes SET
			title = ?, published = ?, description = ?, enclosure_url = ?, enclosure_bytes = ?, mime_type = ?, duration_secs = ?, image_url = ?,
			missing_syncs = 0
		WHERE feed_id = ? AND guid = ?`,
		ep.Title, ep.Published, ep.Description, ep.EnclosureURL, ep.EnclosureBytes, ep.MimeType, ep.DurationSecs, ep.ImageURL,
		ep.FeedID, ep.GUID)
	return false, oldURL, oldBytes, err
}

// EpisodeGUIDs returns the GUIDs of a feed's unpinned episodes.
func (db *DB) EpisodeGUIDs(feedID int64) ([]string, error) {
	rows, err := db.Query(`SELECT guid FROM episodes WHERE feed_id = ? AND pinned = 0`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

const episodeSelect = `SELECT e.id, e.feed_id, e.guid, e.title, e.published, e.description, e.enclosure_url, e.enclosure_bytes,
	e.mime_type, e.duration_secs, e.image_url, e.file_sha, e.downloaded, e.pinned,
	COALESCE(fl.size, 0), COALESCE(fl.ext, ''), COALESCE(fl.mime_type, '')
	FROM episodes e LEFT JOIN files fl ON e.downloaded = 1 AND fl.sha = e.file_sha`

func scanEpisodes(rows *sql.Rows) ([]*Episode, error) {
	var out []*Episode
	for rows.Next() {
		e := &Episode{}
		if err := rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.Published, &e.Description, &e.EnclosureURL,
			&e.EnclosureBytes, &e.MimeType, &e.DurationSecs, &e.ImageURL, &e.FileSHA, &e.Downloaded, &e.Pinned,
			&e.FileSize, &e.FileExt, &e.FileMime); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) ListEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(episodeSelect+` WHERE e.feed_id = ? ORDER BY e.published DESC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

func (db *DB) UndownloadedEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(episodeSelect+` WHERE e.feed_id = ? AND e.downloaded = 0 ORDER BY e.published ASC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

// FindDownloadedEpisode returns a downloaded episode of one of the given
// feeds that is stored as the file with this SHA.
func (db *DB) FindDownloadedEpisode(feedIDs []int64, sha string) (*Episode, error) {
	if len(feedIDs) == 0 {
		return nil, ErrNotFound
	}
	q, args := sqlInInt64(episodeSelect+` WHERE e.downloaded = 1 AND e.file_sha = ? AND fl.sha IS NOT NULL AND e.feed_id IN (`, feedIDs)
	rows, err := db.Query(q+`) LIMIT 1`, append([]any{sha}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	eps, err := scanEpisodes(rows)
	if err != nil {
		return nil, err
	}
	if len(eps) == 0 {
		return nil, ErrNotFound
	}
	return eps[0], nil
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
			COALESCE(SUM(CASE WHEN e.downloaded THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(e.enclosure_bytes), 0),
			COALESCE(SUM(CASE WHEN e.downloaded THEN COALESCE(fl.size, 0) ELSE 0 END), 0)
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

const feedColsF = `f.id, f.user_id, f.source_url, f.title, f.image_url, f.description, f.link, f.sync_mode, f.reupload_mode, f.refresh_secs, f.created_at, f.last_sync, f.last_error, f.fail_count, f.language`

// ResolveFeedToken returns the feed for a token.
func (db *DB) ResolveFeedToken(token string) (*Feed, error) {
	rows, err := db.Query(`SELECT `+feedColsF+` FROM feed_tokens t JOIN feeds f ON f.id = t.feed_id WHERE t.token = ?`, token)
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
