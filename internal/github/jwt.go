package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// AppJWT builds a short-lived RS256 JWT for authenticating as the GitHub App.
// iat is backdated 60s for clock drift; exp is +9m (GitHub's max is 10m). The
// key PEM may be PKCS#1 ("RSA PRIVATE KEY") or PKCS#8.
func AppJWT(privateKeyPEM string, appID int64, now time.Time) (string, error) {
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	header := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims := b64(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%d}`,
		now.Add(-60*time.Second).Unix(), now.Add(9*time.Minute).Unix(), appID))
	signingInput := header + "." + claims

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func parseRSAKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("invalid PEM: no block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}
