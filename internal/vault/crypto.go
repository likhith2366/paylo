// Package vault is the only component in this system that ever sees a raw card
// number (§2.4).
//
// Everything else — the Payments API, the Ledger, the dashboard, every log
// line — operates on opaque tokens. That is not a stylistic preference: it is
// what keeps those services out of PCI DSS scope entirely. A service that
// cannot see a PAN even in principle has nothing to audit.
//
// Two rules govern every change in this package:
//
//  1. A PAN exists in plaintext only in memory, only for the moment it takes to
//     encrypt it or to submit it to the processor. It is never logged, never
//     returned in an error, never written unencrypted.
//  2. CVCs are never persisted at all, in any form, encrypted or not. Storing
//     them is prohibited by PCI DSS outright, with no exception for encryption.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// KeyManager produces and unwraps data encryption keys.
//
// This interface exists because both implementations are real and both are
// needed — LocalKeyManager for development and tests, and a KMS-backed one in
// production. It is not speculative abstraction.
type KeyManager interface {
	// GenerateDataKey returns a fresh key both in plaintext (for immediate use,
	// never stored) and wrapped under the master key (safe to store).
	GenerateDataKey() (plaintext []byte, wrapped []byte, keyID string, err error)

	// UnwrapDataKey recovers a plaintext data key from its wrapped form.
	UnwrapDataKey(wrapped []byte, keyID string) ([]byte, error)
}

// Envelope encryption, rather than encrypting every PAN directly under one
// master key, buys two things: rotating the master key means re-wrapping small
// key blobs instead of re-encrypting every stored card, and the master key
// itself can live in an HSM that never releases it.

// LocalKeyManager wraps data keys under a master key held in memory.
//
// Development and tests only. In production the master key must live in KMS or
// an HSM and never be readable by the application process — here it sits in
// this process's memory, so a memory dump exposes every stored card.
type LocalKeyManager struct {
	masterKey []byte
	keyID     string
}

func NewLocalKeyManager(masterKey []byte) (*LocalKeyManager, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("vault: master key must be 32 bytes for AES-256, got %d", len(masterKey))
	}
	return &LocalKeyManager{masterKey: masterKey, keyID: "local-dev-master-key-v1"}, nil
}

func (m *LocalKeyManager) GenerateDataKey() ([]byte, []byte, string, error) {
	dataKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, dataKey); err != nil {
		return nil, nil, "", fmt.Errorf("vault: generate data key: %w", err)
	}

	wrapped, err := encryptAESGCM(m.masterKey, dataKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("vault: wrap data key: %w", err)
	}
	return dataKey, wrapped, m.keyID, nil
}

func (m *LocalKeyManager) UnwrapDataKey(wrapped []byte, keyID string) ([]byte, error) {
	if keyID != m.keyID {
		// A record encrypted under a retired key needs that key to read it.
		// Failing loudly is correct: silently returning garbage would corrupt
		// a charge submission in a way that is very hard to trace.
		return nil, fmt.Errorf("vault: unknown key id %q", keyID)
	}
	key, err := decryptAESGCM(m.masterKey, wrapped)
	if err != nil {
		return nil, fmt.Errorf("vault: unwrap data key: %w", err)
	}
	return key, nil
}

// encryptAESGCM encrypts with AES-256-GCM, prepending the nonce to the
// ciphertext.
//
// GCM is authenticated encryption: tampering with stored ciphertext causes
// decryption to fail rather than silently returning a different card number.
// The nonce is stored with its ciphertext rather than separately because it
// must be unique per encryption but is not secret, and keeping them together
// makes it structurally impossible to pair the wrong nonce with the wrong
// ciphertext.
func encryptAESGCM(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Seal appends the ciphertext to the nonce, producing nonce||ciphertext||tag.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptAESGCM(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}

	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext is shorter than the nonce")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Deliberately vague: a detailed decryption error is an oracle.
		return nil, fmt.Errorf("authentication failed")
	}
	return plaintext, nil
}

// NewMasterKey generates a random 32-byte master key, base64-encoded for
// storage in a secrets manager.
func NewMasterKey() (string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("vault: generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParseMasterKey decodes a base64 master key from configuration.
func ParseMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("vault: master key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("vault: master key must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
