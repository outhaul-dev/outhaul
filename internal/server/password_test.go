package server

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not in argon2id PHC format: %q", hash)
	}

	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("correct password should verify")
	}

	ok, err = VerifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("VerifyPassword (wrong): %v", err)
	}
	if ok {
		t.Error("wrong password should not verify")
	}
}

func TestHashesAreSaltedUniquely(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Error("two hashes of the same password should differ (random salt)")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	if _, err := VerifyPassword("not-a-real-hash", "x"); err == nil {
		t.Error("expected error verifying against a malformed hash")
	}
}
