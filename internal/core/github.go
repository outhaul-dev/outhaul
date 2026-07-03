package core

import "time"

// GithubApp is the single GitHub App record for a Slipway instance. Secret
// fields hold plaintext in memory (decrypted); the store encrypts them at rest.
type GithubApp struct {
	AppID          int64
	Slug           string
	PrivateKey     string // PEM
	WebhookSecret  string
	ClientID       string
	ClientSecret   string
	InstallationID int64
	CreatedAt      time.Time
}
