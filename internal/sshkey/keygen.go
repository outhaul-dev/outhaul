// Package sshkey generates SSH deploy keypairs for cloning private repos.
// It is pure: no network, no persistence. The caller stores the private key
// encrypted and shows the public line to the operator to add as a deploy key.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Generate returns a fresh Ed25519 keypair as an OpenSSH-format private-key PEM
// and a single-line authorized_keys public entry (no trailing newline).
func Generate() (privatePEM, publicLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("wrap public key: %w", err)
	}
	privatePEM = string(pem.EncodeToMemory(block))
	publicLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return privatePEM, publicLine, nil
}
