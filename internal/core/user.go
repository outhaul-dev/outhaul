package core

import "time"

// User is the single admin (v1 is single-user). PasswordHash is an argon2id
// encoded hash.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// Session is a server-side login session keyed by an opaque cookie token.
type Session struct {
	Token     string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}
