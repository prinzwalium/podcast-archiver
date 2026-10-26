package store

import (
	"database/sql"
	"errors"
)

type MergedFeed struct {
	ID        int64
	UserID    int64
	Name      string
	CreatedAt int64
}

// CreateMergedFeed creates a merged feed with the given member feed IDs.
func (db *DB) CreateMergedFeed(userID int64, name string, memberIDs []int64) (*MergedFeed, error) {
	if len(memberIDs) == 0 {
		return nil, errors.New("no member feeds")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO merged_feeds (user_id, name, created_at) VALUES (?, ?, ?)`, userID, name, now())
	if err != nil {
		return nil, err
	}
	mid, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	for _, fid := range memberIDs {
		// Verify ownership of each member feed.
		var owner int64
		if err := tx.QueryRow(`SELECT user_id FROM feeds WHERE id = ?`, fid).Scan(&owner); err != nil {
			return nil, errors.New("member feed not found")
		}
		if owner != userID {
			return nil, errors.New("member feed not owned by user")
		}
		if _, err := tx.Exec(`INSERT INTO merged_feed_members (merged_id, feed_id) VALUES (?, ?)`, mid, fid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MergedFeed{ID: mid, UserID: userID, Name: name, CreatedAt: now()}, nil
}

// ListMergedFeeds returns the user's merged feeds with their member feed IDs.
func (db *DB) ListMergedFeeds(userID int64) ([]*MergedFeed, map[int64][]int64, error) {
	rows, err := db.Query(`SELECT id, user_id, name, created_at FROM merged_feeds WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []*MergedFeed
	for rows.Next() {
		m := &MergedFeed{}
		if err := rows.Scan(&m.ID, &m.UserID, &m.Name, &m.CreatedAt); err != nil {
			return nil, nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	members := map[int64][]int64{}
	for _, m := range out {
		mrows, err := db.Query(`SELECT feed_id FROM merged_feed_members WHERE merged_id = ? ORDER BY feed_id`, m.ID)
		if err != nil {
			return nil, nil, err
		}
		for mrows.Next() {
			var fid int64
			if err := mrows.Scan(&fid); err != nil {
				mrows.Close()
				return nil, nil, err
			}
			members[m.ID] = append(members[m.ID], fid)
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return out, members, nil
}

// GetMergedFeed returns one merged feed owned by the user.
func (db *DB) GetMergedFeed(userID, mergedID int64) (*MergedFeed, error) {
	m := &MergedFeed{}
	err := db.QueryRow(`SELECT id, user_id, name, created_at FROM merged_feeds WHERE id = ? AND user_id = ?`, mergedID, userID).
		Scan(&m.ID, &m.UserID, &m.Name, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// MergedFeedMembers returns the member feed IDs of a merged feed.
func (db *DB) MergedFeedMembers(mergedID int64) ([]int64, error) {
	rows, err := db.Query(`SELECT feed_id FROM merged_feed_members WHERE merged_id = ? ORDER BY feed_id`, mergedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var fid int64
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out = append(out, fid)
	}
	return out, rows.Err()
}

// DeleteMergedFeed removes a merged feed (member feeds are untouched).
func (db *DB) DeleteMergedFeed(userID, mergedID int64) error {
	res, err := db.Exec(`DELETE FROM merged_feeds WHERE id = ? AND user_id = ?`, mergedID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateMergedFeedMembers replaces the member set of a merged feed.
func (db *DB) UpdateMergedFeedMembers(userID, mergedID int64, memberIDs []int64) error {
	if len(memberIDs) == 0 {
		return errors.New("no member feeds")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner int64
	if err := tx.QueryRow(`SELECT user_id FROM merged_feeds WHERE id = ?`, mergedID).Scan(&owner); err != nil {
		return ErrNotFound
	}
	if owner != userID {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM merged_feed_members WHERE merged_id = ?`, mergedID); err != nil {
		return err
	}
	for _, fid := range memberIDs {
		var fowner int64
		if err := tx.QueryRow(`SELECT user_id FROM feeds WHERE id = ?`, fid).Scan(&fowner); err != nil {
			return errors.New("member feed not found")
		}
		if fowner != userID {
			return errors.New("member feed not owned by user")
		}
		if _, err := tx.Exec(`INSERT INTO merged_feed_members (merged_id, feed_id) VALUES (?, ?)`, mergedID, fid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateMergedToken / merged token management mirror feed tokens.
func (db *DB) CreateMergedToken(mergedID int64, token string) error {
	_, err := db.Exec(`INSERT INTO merged_tokens (merged_id, token, created_at) VALUES (?, ?, ?)`, mergedID, token, now())
	return err
}

func (db *DB) DeleteMergedToken(mergedID int64, token string) error {
	_, err := db.Exec(`DELETE FROM merged_tokens WHERE merged_id = ? AND token = ?`, mergedID, token)
	return err
}

func (db *DB) ListMergedTokens(mergedID int64) ([]*FeedToken, error) {
	rows, err := db.Query(`SELECT id, merged_id, token, created_at FROM merged_tokens WHERE merged_id = ? ORDER BY id`, mergedID)
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

// ResolveMergedToken returns the merged feed for a token.
func (db *DB) ResolveMergedToken(token string) (*MergedFeed, error) {
	m := &MergedFeed{}
	err := db.QueryRow(`SELECT m.id, m.user_id, m.name, m.created_at FROM merged_tokens t
		JOIN merged_feeds m ON m.id = t.merged_id WHERE t.token = ?`, token).
		Scan(&m.ID, &m.UserID, &m.Name, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ListEpisodesForFeeds returns downloaded episodes of several feeds, newest first.
func (db *DB) ListEpisodesForFeeds(feedIDs []int64) ([]*Episode, error) {
	if len(feedIDs) == 0 {
		return nil, nil
	}
	q, args := sqlInInt64(`SELECT `+episodeCols+` FROM episodes WHERE downloaded = 1 AND feed_id IN (`, feedIDs)
	rows, err := db.Query(q+`) ORDER BY published DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

func sqlInInt64(prefix string, ids []int64) (string, []any) {
	q := prefix
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			q += ","
		}
		q += "?"
		args[i] = id
	}
	return q, args
}
