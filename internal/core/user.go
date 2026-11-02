package core

import "time"

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
