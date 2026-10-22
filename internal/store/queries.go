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
	ID          int64
	UserID      int64
	SourceURL   string
	Title       string
	ImageURL    string
	Description string
	Link        string
	CreatedAt   int64
	LastSync    int64
	LastError   string
}

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
	FilePath       string
	FileBytes      int64
	Downloaded     bool
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

func (db *DB) CreateFeed(userID int64, sourceURL, title, imageURL, description, link string) (*Feed, error) {
	res, err := db.Exec(`INSERT INTO feeds (user_id, source_url, title, image_url, description, link, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, sourceURL, title, imageURL, description, link, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Feed{ID: id, UserID: userID, SourceURL: sourceURL, Title: title,
		ImageURL: imageURL, Description: description, Link: link, CreatedAt: now()}, nil
}

func (db *DB) ListFeeds(userID int64) ([]*Feed, error) {
	rows, err := db.Query(`SELECT id, user_id, source_url, title, image_url, description, link, created_at, last_sync, last_error
		FROM feeds WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (db *DB) ListAllFeeds() ([]*Feed, error) {
	rows, err := db.Query(`SELECT id, user_id, source_url, title, image_url, description, link, created_at, last_sync, last_error FROM feeds`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func scanFeeds(rows *sql.Rows) ([]*Feed, error) {
	var out []*Feed
	for rows.Next() {
		f := &Feed{}
		if err := rows.Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link, &f.CreatedAt, &f.LastSync, &f.LastError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (db *DB) GetFeed(userID, feedID int64) (*Feed, error) {
	f := &Feed{}
	err := db.QueryRow(`SELECT id, user_id, source_url, title, image_url, description, link, created_at, last_sync, last_error
		FROM feeds WHERE id = ? AND user_id = ?`, feedID, userID).
		Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link, &f.CreatedAt, &f.LastSync, &f.LastError)
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

func (db *DB) UpdateFeedSync(feedID int64, ts int64, syncErr string) error {
	_, err := db.Exec(`UPDATE feeds SET last_sync = ?, last_error = ? WHERE id = ?`, ts, syncErr, feedID)
	return err
}

func (db *DB) UpdateFeedMeta(feedID int64, title, imageURL, description, link string) error {
	_, err := db.Exec(`UPDATE feeds SET title = ?, image_url = ?, description = ?, link = ? WHERE id = ?`,
		title, imageURL, description, link, feedID)
	return err
}

func (db *DB) UpsertEpisode(ep *Episode) (bool, error) {
	_, err := db.Exec(`INSERT INTO episodes
		(feed_id, guid, title, published, description, enclosure_url, enclosure_bytes, mime_type, duration_secs, image_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(feed_id, guid) DO UPDATE SET
			title = excluded.title,
			published = excluded.published,
			description = excluded.description,
			enclosure_url = excluded.enclosure_url,
			enclosure_bytes = excluded.enclosure_bytes,
			mime_type = excluded.mime_type,
			duration_secs = excluded.duration_secs,
			image_url = excluded.image_url`,
		ep.FeedID, ep.GUID, ep.Title, ep.Published, ep.Description, ep.EnclosureURL, ep.EnclosureBytes, ep.MimeType, ep.DurationSecs, ep.ImageURL)
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteMissing removes episodes whose GUIDs are absent from the given set (mirror mode).
// Returns the number of removed rows and their file paths for disk cleanup.
func (db *DB) DeleteMissing(feedID int64, keepGUIDs []string) ([]string, error) {
	var removed []string
	for _, guid := range keepGUIDs {
		_ = guid
	}
	rows, err := db.Query(`SELECT guid, file_path FROM episodes WHERE feed_id = ?`, feedID)
	if err != nil {
		return nil, err
	}
	type row struct{ guid, path string }
	var existing []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.guid, &r.path); err != nil {
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
	for _, r := range existing {
		if !keep[r.guid] {
			if _, err := db.Exec(`DELETE FROM episodes WHERE feed_id = ? AND guid = ?`, feedID, r.guid); err != nil {
				return removed, err
			}
			if r.path != "" {
				removed = append(removed, r.path)
			}
		}
	}
	return removed, nil
}

func (db *DB) ListEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(`SELECT id, feed_id, guid, title, published, description, enclosure_url, enclosure_bytes,
		mime_type, duration_secs, image_url, file_path, file_bytes, downloaded
		FROM episodes WHERE feed_id = ? ORDER BY published DESC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Episode
	for rows.Next() {
		e := &Episode{}
		if err := rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.Published, &e.Description, &e.EnclosureURL,
			&e.EnclosureBytes, &e.MimeType, &e.DurationSecs, &e.ImageURL, &e.FilePath, &e.FileBytes, &e.Downloaded); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) UndownloadedEpisodes(feedID int64) ([]*Episode, error) {
	rows, err := db.Query(`SELECT id, feed_id, guid, title, published, description, enclosure_url, enclosure_bytes,
		mime_type, duration_secs, image_url, file_path, file_bytes, downloaded
		FROM episodes WHERE feed_id = ? AND downloaded = 0 ORDER BY published ASC`, feedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Episode
	for rows.Next() {
		e := &Episode{}
		if err := rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.Published, &e.Description, &e.EnclosureURL,
			&e.EnclosureBytes, &e.MimeType, &e.DurationSecs, &e.ImageURL, &e.FilePath, &e.FileBytes, &e.Downloaded); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) MarkEpisodeDownloaded(ep *Episode) error {
	_, err := db.Exec(`UPDATE episodes SET downloaded = 1, file_path = ?, file_bytes = ? WHERE id = ?`,
		ep.FilePath, ep.FileBytes, ep.ID)
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
			COALESCE(SUM(CASE WHEN downloaded THEN enclosure_bytes ELSE 0 END), 0)
		FROM episodes WHERE feed_id = ?`, feedID).
		Scan(&s.TotalEpisodes, &s.Downloaded, &s.TotalBytes, &s.DownloadedBytes)
	return s, err
}

// PredictedArchiveBytes estimates the full download size for a feed from the
// source feed's enclosure lengths. fetched outside the DB; see archive.FetchStats.
func (db *DB) AllStats() (map[int64]*FeedStats, error) {
	rows, err := db.Query(`SELECT feed_id, COUNT(*),
			COALESCE(SUM(CASE WHEN downloaded THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(enclosure_bytes), 0),
			COALESCE(SUM(CASE WHEN downloaded THEN enclosure_bytes ELSE 0 END), 0)
		FROM episodes GROUP BY feed_id`)
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

// ResolveFeedToken returns the feed for a token, validating it belongs to feedID.
func (db *DB) ResolveFeedToken(token string) (*Feed, error) {
	f := &Feed{}
	err := db.QueryRow(`SELECT f.id, f.user_id, f.source_url, f.title, f.image_url, f.description, f.link, f.created_at, f.last_sync, f.last_error
		FROM feed_tokens t JOIN feeds f ON f.id = t.feed_id WHERE t.token = ?`, token).
		Scan(&f.ID, &f.UserID, &f.SourceURL, &f.Title, &f.ImageURL, &f.Description, &f.Link, &f.CreatedAt, &f.LastSync, &f.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (db *DB) StorageUsed() (int64, error) {
	var v sql.NullInt64
	err := db.QueryRow(`SELECT SUM(file_bytes) FROM episodes WHERE downloaded = 1`).Scan(&v)
	return v.Int64, err
}
