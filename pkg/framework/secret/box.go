// Package secret encrypts the secrets the framework keeps in its database,
// such as backup repository credentials, under a key held outside it.
//
// A database dump or backup on its own then reveals nothing; reading the
// secrets also takes the key file. Losing the key loses the secrets, which
// have to be entered again — the rows that hold them stay usable.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is the key length: AES-256.
const KeySize = 32

// ErrDecrypt reports a ciphertext that does not open under this key and
// associated data: a different key, a row it was not sealed for, or damage.
var ErrDecrypt = errors.New("secret cannot be decrypted with this key")

// Box seals and opens secrets with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox returns a Box for key, which must be KeySize bytes.
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// LoadKeyFile reads a key written as hexadecimal, as "openssl rand -hex 32"
// prints it, and returns a Box for it.
func LoadKeyFile(path string) (*Box, error) {
	if path == "" {
		return nil, errors.New("no secret key file is configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key file: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("secret key file %s is not hexadecimal", path)
	}
	return NewBox(key)
}

// Seal encrypts plain. aad binds the result to its context — the row it is
// stored in — so that a ciphertext copied elsewhere does not open there.
//
// Each call draws a fresh random nonce: GCM must never see the same nonce
// twice under one key, and a counter would have to survive restarts and
// concurrent writers to guarantee that.
func (b *Box) Seal(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plain, aad), nil
}

// Open decrypts what Seal produced with the same aad.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
