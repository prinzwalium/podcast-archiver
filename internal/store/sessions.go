package store

import (
	"database/sql"
	"errors"
)

// Session is a server-side login session. Only a hash of the cookie token is stored.
type Session struct {
	TokenHash    string
	UserID       int64
	CreatedAt    int64
	ExpiresAt    int64
	CheckedAt    int64  // last time group membership was confirmed with the IdP
	RefreshToken string // encrypted; empty when the IdP issued none
}

func (db *DB) CreateSession(s *Session) error {
	if _, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now()); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, checked_at, refresh_token)
		VALUES (?, ?, ?, ?, ?, ?)`, s.TokenHash, s.UserID, s.CreatedAt, s.ExpiresAt, s.CheckedAt, s.RefreshToken)
	return err
}

func (db *DB) GetSession(tokenHash string) (*Session, error) {
	s := &Session{}
	err := db.QueryRow(`SELECT token_hash, user_id, created_at, expires_at, checked_at, refresh_token
		FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&s.TokenHash, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.CheckedAt, &s.RefreshToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// TouchSession records a successful group re-check and the (possibly rotated) refresh token.
func (db *DB) TouchSession(tokenHash string, checkedAt int64, refreshToken string) error {
	_, err := db.Exec(`UPDATE sessions SET checked_at = ?, refresh_token = ? WHERE token_hash = ?`, checkedAt, refreshToken, tokenHash)
	return err
}

func (db *DB) DeleteSession(tokenHash string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}
