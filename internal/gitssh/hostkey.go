package gitssh

import (
	"fmt"
	"os"

	"github.com/james-smart/outhaul/internal/sshkey"
	"golang.org/x/crypto/ssh"
)

// LoadOrCreateHostKey returns a persistent SSH host signer, generating and
// writing an Ed25519 key at path (mode 0600) on first use.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		priv, _, gerr := sshkey.Generate()
		if gerr != nil {
			return nil, fmt.Errorf("generate host key: %w", gerr)
		}
		if werr := os.WriteFile(path, []byte(priv), 0o600); werr != nil {
			return nil, fmt.Errorf("write host key: %w", werr)
		}
		pem = []byte(priv)
	} else if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	return signer, nil
}
