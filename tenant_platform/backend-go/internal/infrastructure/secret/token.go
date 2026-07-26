// Package secret provides token encryption for bot and transport credentials.
// The platform stores only ciphertext; plaintext tokens never enter logs or
// the Worker.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// currentKeyVersion is the default version for newly encrypted tokens.
	currentKeyVersion = 1
	// aesGCMNonceSize is the standard AES-GCM nonce size (12 bytes).
	aesGCMNonceSize = 12
)

// TokenCipher encrypts and decrypts sensitive tokens stored in PostgreSQL.
type TokenCipher interface {
	// Encrypt returns the ciphertext and key version for the plaintext token.
	Encrypt(plaintext []byte) (ciphertext []byte, keyVersion int, err error)
	// Decrypt returns the plaintext for the given ciphertext and key version.
	Decrypt(ciphertext []byte, keyVersion int) (plaintext []byte, err error)
}

// StaticKeyCipher implements TokenCipher with a single AES-256-GCM key.
type StaticKeyCipher struct {
	version int
	gcm     cipher.AEAD
}

// NewStaticKeyCipher derives an AES-256-GCM cipher from key material.
// The key must be exactly 32 bytes; use NewStaticKeyCipherFromHex for env vars.
func NewStaticKeyCipher(key []byte) (*StaticKeyCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("static key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &StaticKeyCipher{version: currentKeyVersion, gcm: gcm}, nil
}

// NewStaticKeyCipherFromHex parses a 64-character hex key and returns a cipher.
func NewStaticKeyCipherFromHex(hexKey string) (*StaticKeyCipher, error) {
	key, err := hexToBytes(hexKey)
	if err != nil {
		return nil, err
	}
	return NewStaticKeyCipher(key)
}

// Encrypt seals plaintext with a random nonce and prepends version + nonce.
func (c *StaticKeyCipher) Encrypt(plaintext []byte) ([]byte, int, error) {
	if len(plaintext) == 0 {
		return nil, 0, errors.New("plaintext is empty")
	}
	nonce := make([]byte, aesGCMNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("nonce: %w", err)
	}
	// Wire format: [1 byte version][12 byte nonce][ciphertext+tag]
	sealed := c.gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 1+aesGCMNonceSize+len(sealed))
	out[0] = byte(c.version)
	copy(out[1:], nonce)
	copy(out[1+aesGCMNonceSize:], sealed)
	return out, c.version, nil
}

// Decrypt unpacks version, nonce and ciphertext, then opens the seal.
func (c *StaticKeyCipher) Decrypt(ciphertext []byte, keyVersion int) ([]byte, error) {
	if keyVersion != c.version {
		return nil, fmt.Errorf("unsupported key version %d", keyVersion)
	}
	if len(ciphertext) < 1+aesGCMNonceSize+c.gcm.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	storedVersion := int(ciphertext[0])
	if storedVersion != c.version {
		return nil, fmt.Errorf("ciphertext version mismatch: got %d want %d", storedVersion, c.version)
	}
	nonce := ciphertext[1 : 1+aesGCMNonceSize]
	sealed := ciphertext[1+aesGCMNonceSize:]
	return c.gcm.Open(nil, nonce, sealed, nil)
}

// hexToBytes converts a hex string to bytes, validating length and characters.
func hexToBytes(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, fmt.Errorf("hex key must be 64 characters, got %d", len(s))
	}
	out := make([]byte, 32)
	for i := 0; i < 64; i += 2 {
		b1, ok1 := hexValue(s[i])
		b2, ok2 := hexValue(s[i+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex character at position %d", i)
		}
		out[i/2] = b1<<4 | b2
	}
	return out, nil
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ConstantTimeEqual compares two byte slices in constant time.
func ConstantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// IntToBytes returns a little-endian 8-byte representation of v.
func IntToBytes(v int64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	return b[:]
}
