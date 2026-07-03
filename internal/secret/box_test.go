package secret

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b := newTestBox(t)
	enc, err := b.Seal([]byte("hunter2"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if enc == "" {
		t.Fatal("Seal returned empty string")
	}
	got, err := b.Open(enc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != "hunter2" {
		t.Errorf("round trip = %q, want hunter2", got)
	}
}

func TestSealIsNotPlaintext(t *testing.T) {
	b := newTestBox(t)
	enc, err := b.Seal([]byte("topsecret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if enc == "topsecret" {
		t.Fatal("ciphertext equals plaintext")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	b := newTestBox(t)
	first, err := b.Seal([]byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := b.Seal([]byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first == second {
		t.Error("two seals of the same value are identical (nonce reused)")
	}
}

func TestOpenRejectsTamper(t *testing.T) {
	b := newTestBox(t)
	enc, err := b.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	raw[len(raw)/2] ^= 0x01 // flip a byte in the payload
	if _, err := b.Open(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Error("Open accepted tampered ciphertext")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	a := newTestBox(t)
	enc, err := a.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other := newTestBox(t) // Different key
	if _, err := other.Open(enc); err == nil {
		t.Error("Open accepted ciphertext sealed with a different key")
	}
}

func TestOpenRejectsShortCiphertext(t *testing.T) {
	b := newTestBox(t)
	enc := base64.StdEncoding.EncodeToString(make([]byte, 10)) // fewer than 24 bytes
	if _, err := b.Open(enc); err == nil {
		t.Error("Open accepted a ciphertext shorter than the nonce")
	}
}

func TestLoadGeneratesKeyFileWithTightPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file perm = %o, want 600", perm)
	}
}

func TestLoadReusesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	b1, err := Load(path)
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	enc, err := b1.Seal([]byte("v"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b2, err := Load(path) // must read the same key back
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if _, err := b2.Open(enc); err != nil {
		t.Errorf("second Load produced a different key: %v", err)
	}
}

func TestLoadRejectsWrongLengthKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	short := make([]byte, 16)
	if _, err := rand.Read(short); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(path, short, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a key of the wrong length")
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("error %q does not mention expected length 32", err)
	}
}

// newTestBox builds a Box from a freshly generated key file.
func newTestBox(t *testing.T) *Box {
	t.Helper()
	b, err := Load(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return b
}
