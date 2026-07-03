package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func githubSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHub(t *testing.T) {
	body := `{"ref":"refs/heads/main"}`
	secret := "s3cr3t"
	good := githubSig(secret, body)

	if !VerifyGitHub(secret, good, []byte(body)) {
		t.Error("valid signature rejected")
	}
	if VerifyGitHub("wrong", good, []byte(body)) {
		t.Error("wrong secret accepted")
	}
	if VerifyGitHub(secret, good, []byte(body+"x")) {
		t.Error("tampered body accepted")
	}
	if VerifyGitHub(secret, "sha1=abc", []byte(body)) {
		t.Error("non-sha256 header accepted")
	}
	if VerifyGitHub(secret, "", []byte(body)) {
		t.Error("empty header accepted")
	}
	if VerifyGitHub("", good, []byte(body)) {
		t.Error("empty secret accepted")
	}
}

func TestVerifyGitLabToken(t *testing.T) {
	if !VerifyGitLabToken("tok", "tok") {
		t.Error("matching token rejected")
	}
	if VerifyGitLabToken("tok", "nope") {
		t.Error("mismatched token accepted")
	}
	if VerifyGitLabToken("", "") {
		t.Error("empty secret must never verify")
	}
}
