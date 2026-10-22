package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sign(payload, key string) (string, error) {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func verify(payload, sigHex, key string) error {
	want, err := sign(payload, key)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(want), []byte(sigHex)) {
		return errors.New("bad signature")
	}
	return nil
}

// NewToken returns a cryptographically random URL-safe token.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
