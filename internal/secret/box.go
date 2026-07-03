// Package secret encrypts small values (app env vars) at rest with NaCl
// secretbox and a locally-stored 32-byte key. It is pure: no other Slipway
// imports, no network, no logging. Losing the key file means losing the ability
// to decrypt existing values.
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/nacl/secretbox"
)

const nonceLen = 24

// Box seals and opens values with a fixed symmetric key. Construct it via Load;
// the zero value seals and opens with an all-zero key and must not be used.
type Box struct {
	key [32]byte
}

// Load reads the 32-byte key at path, generating one (0600) if it is absent.
func Load(path string) (*Box, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw, err = generateKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("load secret key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("secret key at %s is %d bytes, want 32", path, len(raw))
	}
	b := &Box{}
	copy(b.key[:], raw)
	return b, nil
}

// generateKey creates a new 32-byte key at path without clobbering an existing
// one. It opens the file O_EXCL; if another writer already created it, we read
// that key back rather than overwrite a key that may already encrypt data.
func generateKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal encrypts plaintext, returning base64(nonce || ciphertext). It returns an
// error only if the system randomness source fails.
func (b *Box) Seal(plaintext []byte) (string, error) {
	var nonce [nonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	sealed := secretbox.Seal(nonce[:], plaintext, &nonce, &b.key)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. It errors if the input is malformed, tampered, or was
// sealed with a different key.
func (b *Box) Open(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("bad base64: %w", err)
	}
	if len(raw) < nonceLen {
		return nil, errors.New("ciphertext too short")
	}
	var nonce [nonceLen]byte
	copy(nonce[:], raw[:nonceLen])
	out, ok := secretbox.Open(nil, raw[nonceLen:], &nonce, &b.key)
	if !ok {
		return nil, errors.New("decryption failed (wrong key or tampered)")
	}
	return out, nil
}
