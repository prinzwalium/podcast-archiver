package store

import (
	"database/sql"
	"errors"
)

// Media files are content addressed and refcounted. The invariant is
// files.refcount == number of downloaded episodes whose file_sha is the file.
// Every change to an episode's download state and the matching refcount
// change happen in the same transaction. Functions that drop references
// return the SHAs that reached zero; the caller removes those objects from disk.

// unrefTx decrements a file's refcount and deletes the row when it reaches
// zero, reporting whether it did.
func unrefTx(tx *sql.Tx, sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE files SET refcount = refcount - 1 WHERE sha = ?`, sha); err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM files WHERE sha = ? AND refcount <= 0`, sha)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// unrefAllTx drops one reference per SHA in the list (duplicates included).
func unrefAllTx(tx *sql.Tx, shas []string) ([]string, error) {
	var removable []string
	for _, sha := range shas {
		gone, err := unrefTx(tx, sha)
		if err != nil {
			return nil, err
		}
		if gone {
			removable = append(removable, sha)
		}
	}
	return removable, nil
}

// AttachFile records a finished download: it marks the episode downloaded
// with the file and takes a reference on it, atomically. It returns
// ErrNotFound when the episode no longer exists or is already downloaded.
func (db *DB) AttachFile(episodeID int64, sha string, size int64, mimeType, ext string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE episodes SET downloaded = 1, file_sha = ? WHERE id = ? AND downloaded = 0`, sha, episodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`INSERT INTO files (sha, size, mime_type, ext, refcount) VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(sha) DO UPDATE SET refcount = refcount + 1`, sha, size, mimeType, ext); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearEpisodeFile clears download state for an episode whose content
// changed upstream (reupload with "replace" policy) so it is downloaded
// again, and drops its reference on the old file.
func (db *DB) ClearEpisodeFile(feedID int64, guid string) (removable []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sha string
	var downloaded bool
	err = tx.QueryRow(`SELECT file_sha, downloaded FROM episodes WHERE feed_id = ? AND guid = ?`, feedID, guid).Scan(&sha, &downloaded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE episodes SET downloaded = 0, file_sha = '' WHERE feed_id = ? AND guid = ?`, feedID, guid); err != nil {
		return nil, err
	}
	if downloaded {
		if removable, err = unrefAllTx(tx, []string{sha}); err != nil {
			return nil, err
		}
	}
	return removable, tx.Commit()
}

// DeleteMissing implements mirror deletions. Unpinned episodes whose GUIDs
// are not in keepGUIDs have their missing counter increased; once an episode
// has been missing from graceSyncs consecutive syncs it is deleted. Returns
// the deleted GUIDs and the SHAs whose last reference went away.
func (db *DB) DeleteMissing(feedID int64, keepGUIDs []string, graceSyncs int) (removedGUIDs, removable []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT guid, file_sha, downloaded, missing_syncs FROM episodes WHERE feed_id = ? AND pinned = 0`, feedID)
	if err != nil {
		return nil, nil, err
	}
	type row struct {
		guid, sha  string
		downloaded bool
		missing    int
	}
	var existing []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.guid, &r.sha, &r.downloaded, &r.missing); err != nil {
			rows.Close()
			return nil, nil, err
		}
		existing = append(existing, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	keep := make(map[string]bool, len(keepGUIDs))
	for _, g := range keepGUIDs {
		keep[g] = true
	}
	var unref []string
	for _, r := range existing {
		if keep[r.guid] {
			continue
		}
		if r.missing+1 < graceSyncs {
			if _, err := tx.Exec(`UPDATE episodes SET missing_syncs = missing_syncs + 1 WHERE feed_id = ? AND guid = ?`, feedID, r.guid); err != nil {
				return nil, nil, err
			}
			continue
		}
		if _, err := tx.Exec(`DELETE FROM episodes WHERE feed_id = ? AND guid = ?`, feedID, r.guid); err != nil {
			return nil, nil, err
		}
		removedGUIDs = append(removedGUIDs, r.guid)
		if r.downloaded {
			unref = append(unref, r.sha)
		}
	}
	if removable, err = unrefAllTx(tx, unref); err != nil {
		return nil, nil, err
	}
	return removedGUIDs, removable, tx.Commit()
}

// downloadedSHAsTx lists the file SHA of every downloaded episode matching
// the WHERE clause, one entry per episode (not DISTINCT): each episode holds
// its own reference.
func downloadedSHAsTx(tx *sql.Tx, where string, args ...any) ([]string, error) {
	rows, err := tx.Query(`SELECT e.file_sha FROM episodes e JOIN feeds f ON f.id = e.feed_id
		WHERE e.downloaded = 1 AND e.file_sha != '' AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		shas = append(shas, sha)
	}
	return shas, rows.Err()
}

// DeleteFeed removes a feed (episodes, tokens and events cascade) and drops
// its episodes' file references.
func (db *DB) DeleteFeed(userID, feedID int64) (removable []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	shas, err := downloadedSHAsTx(tx, `f.id = ? AND f.user_id = ?`, feedID, userID)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`DELETE FROM feeds WHERE id = ? AND user_id = ?`, feedID, userID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if removable, err = unrefAllTx(tx, shas); err != nil {
		return nil, err
	}
	return removable, tx.Commit()
}

// DeleteUser removes a user with their feeds, merged feeds and sessions and
// drops their episodes' file references.
func (db *DB) DeleteUser(userID int64) (removable []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	shas, err := downloadedSHAsTx(tx, `f.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{
		`DELETE FROM merged_feeds WHERE user_id = ?`,
		`DELETE FROM feeds WHERE user_id = ?`,
		`DELETE FROM sessions WHERE user_id = ?`,
	} {
		if _, err := tx.Exec(q, userID); err != nil {
			return nil, err
		}
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if removable, err = unrefAllTx(tx, shas); err != nil {
		return nil, err
	}
	return removable, tx.Commit()
}

// RecomputeRefcounts repairs refcount drift: every file's refcount is set to
// the number of downloaded episodes using it, and files nobody uses are
// deleted. Returns the SHAs of deleted files.
func (db *DB) RecomputeRefcounts() (removable []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE files SET refcount =
		(SELECT COUNT(*) FROM episodes e WHERE e.downloaded = 1 AND e.file_sha = files.sha)`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT sha FROM files WHERE refcount = 0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return nil, err
		}
		removable = append(removable, sha)
	}
	rows.Close()
	if _, err := tx.Exec(`DELETE FROM files WHERE refcount = 0`); err != nil {
		return nil, err
	}
	// Episodes pointing at a file row that does not exist cannot be served;
	// queue them for download again.
	if _, err := tx.Exec(`UPDATE episodes SET downloaded = 0, file_sha = ''
		WHERE downloaded = 1 AND file_sha NOT IN (SELECT sha FROM files)`); err != nil {
		return nil, err
	}
	return removable, tx.Commit()
}

// KnownFileSHAs returns the set of SHAs tracked in the files table.
func (db *DB) KnownFileSHAs() (map[string]bool, error) {
	rows, err := db.Query(`SELECT sha FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}
