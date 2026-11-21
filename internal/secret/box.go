// Package secret encrypts small values (such as API tokens) before they are
// stored in the database.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// Box encrypts and decrypts values with AES-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox derives a key from the given secret and purpose.
func NewBox(secret []byte, purpose string) (*Box, error) {
	if len(secret) == 0 {
		return nil, errors.New("secret: empty key material")
	}
	key := sha256.Sum256(append([]byte(purpose+"\x00"), secret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns it base64 encoded.
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("secret: value too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("secret: cannot decrypt, the session secret has probably changed")
	}
	return string(plain), nil
}
