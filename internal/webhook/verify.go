package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// VerifyGitHub checks an X-Hub-Signature-256 header ("sha256=<hex>") against the
// HMAC-SHA256 of body keyed by secret. Constant-time. Used by GitHub and Gitea.
func VerifyGitHub(secret, signatureHeader string, body []byte) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	want := strings.TrimPrefix(signatureHeader, prefix)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

// VerifyGitLabToken checks a GitLab X-Gitlab-Token header for exact equality
// with secret (constant-time). An empty secret never verifies.
func VerifyGitLabToken(secret, tokenHeader string) bool {
	if secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(tokenHeader)) == 1
}
