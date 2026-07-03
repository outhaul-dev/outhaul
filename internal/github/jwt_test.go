package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block)), &key.PublicKey
}

func TestAppJWT(t *testing.T) {
	pemStr, pub := testKeyPEM(t)
	now := time.Unix(1_700_000_000, 0)

	tok, err := AppJWT(pemStr, 12345, now)
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	// Verify the RS256 signature over header.claims with the public key.
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	// Claims: iss is the app id, exp within 10 minutes of iat.
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		IAT int64 `json:"iat"`
		EXP int64 `json:"exp"`
		ISS int64 `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &c); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if c.ISS != 12345 {
		t.Errorf("iss = %d, want 12345", c.ISS)
	}
	if c.EXP-c.IAT > 600 || c.EXP <= c.IAT {
		t.Errorf("exp-iat = %d, want (0,600]", c.EXP-c.IAT)
	}
}

func TestAppJWTBadPEM(t *testing.T) {
	if _, err := AppJWT("not a pem", 1, time.Unix(0, 0)); err == nil {
		t.Error("expected error for bad PEM")
	}
}
