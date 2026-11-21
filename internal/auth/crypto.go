package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

// NewToken returns a cryptographically random URL-safe token.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sealer encrypts refresh tokens at rest with AES-GCM under a key derived
// from PA_SECRET_KEY.
type sealer struct{ aead cipher.AEAD }

func newSealer(secret string) *sealer {
	key := sha256.Sum256([]byte("podcast-archiver refresh token v1|" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &sealer{aead: aead}
}

func (s *sealer) seal(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *sealer) open(sealed string) (string, error) {
	b, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	if len(b) < s.aead.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	plain, err := s.aead.Open(nil, b[:s.aead.NonceSize()], b[s.aead.NonceSize():], nil)
	return string(plain), err
}
