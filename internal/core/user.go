package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// Sign-in methods an account can use.
const (
	SignInPassword = "password"
	SignInSSO      = "sso"
)

// User is an account. PasswordHash is an argon2id encoded hash; it is empty
// for SSO accounts and for password accounts that have not been set up yet.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	Role         Role
	SignIn       string // SignInPassword or SignInSSO
	SSOIssuer    string
	SSOSubject   string
	Email        string
	LastSignInAt *time.Time

	RoleFromGroups bool // Role came from the provider's groups at the last sign-in
}

// Display is the name shown for the account: the email for SSO accounts,
// otherwise the username.
func (u User) Display() string {
	if u.SignIn == SignInSSO && u.Email != "" {
		return u.Email
	}
	return u.Username
}

// UserRow is a User with the number of projects it is a member of.
type UserRow struct {
	User
	ProjectCount int
}

// Session is a server-side login session keyed by an opaque cookie token.
type Session struct {
	Token     string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// GateSession is a sign-in on Outhaul's own host. Subject names who signed in,
// Kind is SignInSSO or SignInPassword, and Display is what apps receive in the
// identity header.
type GateSession struct {
	Subject, Kind, Display string
	ExpiresAt              time.Time
}

// SSOGateSubject is the gateway subject of the SSO identity (issuer, subject):
// "sso:" and the SHA-256 hex of the pair joined by a NUL that neither can
// contain, so no two identities collide. It is also the account's internal
// username, and names route visits, which have no account.
func SSOGateSubject(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "sso:" + hex.EncodeToString(sum[:])
}

// GateSubject is the gateway subject of the account's sign-ins.
func (u User) GateSubject() string {
	if u.SignIn == SignInSSO {
		return SSOGateSubject(u.SSOIssuer, u.SSOSubject)
	}
	return "user:" + strconv.FormatInt(u.ID, 10)
}
