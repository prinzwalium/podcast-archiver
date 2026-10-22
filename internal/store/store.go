package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

var ErrNotFound = errors.New("not found")

func now() int64 { return time.Now().Unix() }

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: single writer avoids SQLITE_BUSY
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db}, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			sub TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			max_feeds INTEGER NOT NULL DEFAULT 0,
			max_bytes INTEGER NOT NULL DEFAULT 0,
			is_admin INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS feeds (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id),
			source_url TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			image_url TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			link TEXT NOT NULL DEFAULT '',
			sync_mode TEXT NOT NULL DEFAULT 'mirror',
			reupload_mode TEXT NOT NULL DEFAULT 'replace',
			created_at INTEGER NOT NULL,
			last_sync INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			refresh_secs INTEGER NOT NULL DEFAULT 0,
			fail_count INTEGER NOT NULL DEFAULT 0,
			CONSTRAINT uq_user_feed UNIQUE (user_id, source_url)
		)`,
		`CREATE TABLE IF NOT EXISTS episodes (
			id INTEGER PRIMARY KEY,
			feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
			guid TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			published INTEGER NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			enclosure_url TEXT NOT NULL,
			enclosure_bytes INTEGER NOT NULL DEFAULT 0,
			mime_type TEXT NOT NULL DEFAULT 'audio/mpeg',
			duration_secs INTEGER NOT NULL DEFAULT 0,
			image_url TEXT NOT NULL DEFAULT '',
			file_sha TEXT NOT NULL DEFAULT '',
			downloaded INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			CONSTRAINT uq_feed_guid UNIQUE (feed_id, guid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_episodes_feed ON episodes(feed_id, published DESC)`,
		`CREATE TABLE IF NOT EXISTS files (
			sha TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			mime_type TEXT NOT NULL DEFAULT 'audio/mpeg',
			ext TEXT NOT NULL DEFAULT '',
			refcount INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS feed_tokens (
			id INTEGER PRIMARY KEY,
			feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY,
			feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_feed ON events(feed_id, id DESC)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%w: %s", err, s)
		}
	}
	// Additive column migrations for databases created before v0.2.
	additive := []struct{ table, col, ddl string }{
		{"feeds", "sync_mode", "ALTER TABLE feeds ADD COLUMN sync_mode TEXT NOT NULL DEFAULT 'mirror'"},
		{"feeds", "reupload_mode", "ALTER TABLE feeds ADD COLUMN reupload_mode TEXT NOT NULL DEFAULT 'replace'"},
		{"episodes", "file_sha", "ALTER TABLE episodes ADD COLUMN file_sha TEXT NOT NULL DEFAULT ''"},
		{"files", "refcount", "ALTER TABLE files ADD COLUMN refcount INTEGER NOT NULL DEFAULT 1"},
		{"feeds", "refresh_secs", "ALTER TABLE feeds ADD COLUMN refresh_secs INTEGER NOT NULL DEFAULT 0"},
		{"feeds", "fail_count", "ALTER TABLE feeds ADD COLUMN fail_count INTEGER NOT NULL DEFAULT 0"},
		{"episodes", "pinned", "ALTER TABLE episodes ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0"},
		{"users", "max_feeds", "ALTER TABLE users ADD COLUMN max_feeds INTEGER NOT NULL DEFAULT 0"},
		{"users", "max_bytes", "ALTER TABLE users ADD COLUMN max_bytes INTEGER NOT NULL DEFAULT 0"},
		{"users", "is_admin", "ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range additive {
		var ck string
		if err := db.QueryRow(`SELECT name FROM pragma_table_info(?) WHERE name = ?`, a.table, a.col).Scan(&ck); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := db.Exec(a.ddl); err != nil {
					return fmt.Errorf("%w: %s", err, a.ddl)
				}
			}
		}
	}
	return nil
}
