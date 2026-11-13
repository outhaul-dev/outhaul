package core

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PushTokenPrefix starts every push token: this, then 64 lower-case hex
// characters.
const PushTokenPrefix = "ohp_"

// MaxPushTokensPerUser is how many tokens one account may hold, expired ones
// included until they are removed.
const MaxPushTokensPerUser = 25

// PushTokenKeepExpired is how long an expired token's row is kept before it is
// deleted.
const PushTokenKeepExpired = 30 * 24 * time.Hour

// PushToken is a credential for git over HTTPS. Only its SHA-256 is stored, so
// the value is never part of this type.
type PushToken struct {
	ID, UserID           int64
	Label                string
	CreatedAt, ExpiresAt time.Time
	LastUsedAt           *time.Time
	LastUsedIP           string
}

// EndsAt is the earlier of ExpiresAt and CreatedAt plus maxDays days.
func (t PushToken) EndsAt(maxDays int) time.Time {
	limit := t.CreatedAt.AddDate(0, 0, maxDays)
	if limit.Before(t.ExpiresAt) {
		return limit
	}
	return t.ExpiresAt
}

// Expired reports whether now is at or past EndsAt.
func (t PushToken) Expired(now time.Time, maxDays int) bool {
	return !now.Before(t.EndsAt(maxDays))
}

// PushTokenRow is a token with its owner's Display name, for the Users page.
type PushTokenRow struct {
	PushToken
	Owner string
}

// ValidPushTokenShape reports whether s is exactly "ohp_" and 64 of 0-9 a-f.
func ValidPushTokenShape(s string) bool {
	rest, ok := strings.CutPrefix(s, PushTokenPrefix)
	if !ok || len(rest) != 64 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CleanPushTokenLabel trims spaces; ok is false unless 1 to 64 characters
// remain and none is a control character.
func CleanPushTokenLabel(s string) (label string, ok bool) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > 64 || !utf8.ValidString(s) {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}
