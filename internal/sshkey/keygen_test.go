package sshkey

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateRoundTrips(t *testing.T) {
	priv, pub, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The private PEM must parse as an OpenSSH private key.
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}

	// The public line must parse and be an ed25519 key.
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}
	if got := parsed.Type(); got != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %q, want %q", got, ssh.KeyAlgoED25519)
	}

	// The public line must match the private key's own public half.
	want := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if pub != want {
		t.Errorf("public line %q does not match signer public key %q", pub, want)
	}

	// Public line is a single trimmed line (safe to render / copy).
	if strings.Contains(pub, "\n") {
		t.Errorf("public line contains a newline: %q", pub)
	}

	// Two calls produce different keys.
	priv2, _, err := Generate()
	if err != nil {
		t.Fatalf("Generate #2: %v", err)
	}
	if priv2 == priv {
		t.Error("two Generate calls produced identical private keys")
	}
}
