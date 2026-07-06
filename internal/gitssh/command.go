package gitssh

import (
	"fmt"
	"strings"
)

// parseGitCommand validates an SSH exec payload against the git server
// allowlist and returns the verb and the requested repo name. It accepts only
//
//	git-receive-pack '<repo>'
//	git-upload-pack   '<repo>'
//
// where <repo> is a single quoted argument. A leading slash git may send is
// stripped; after that the repo must be a single safe path segment. Any other
// command is rejected.
func parseGitCommand(cmd string) (verb, repo string, err error) {
	cmd = strings.TrimSpace(cmd)
	for _, v := range []string{"git-receive-pack", "git-upload-pack"} {
		if !strings.HasPrefix(cmd, v+" ") {
			continue
		}
		arg := strings.TrimSpace(strings.TrimPrefix(cmd, v))
		unq, err := unquoteSingleArg(arg)
		if err != nil {
			return "", "", err
		}
		unq = strings.TrimPrefix(unq, "/")
		if unq == "" {
			return "", "", fmt.Errorf("empty repo name")
		}
		if strings.Contains(unq, "..") || strings.ContainsAny(unq, "/\\;&|`$\n\r ") || strings.ContainsRune(unq, 0) || strings.ContainsFunc(unq, func(r rune) bool { return r < 0x20 }) {
			return "", "", fmt.Errorf("illegal repo name")
		}
		return v, unq, nil
	}
	return "", "", fmt.Errorf("command not allowed")
}

// unquoteSingleArg accepts exactly one single- or double-quoted token, rejecting
// extra content and embedded quotes.
func unquoteSingleArg(s string) (string, error) {
	if len(s) < 2 {
		return "", fmt.Errorf("expected a quoted argument")
	}
	q := s[0]
	if (q != '\'' && q != '"') || s[len(s)-1] != q {
		return "", fmt.Errorf("argument must be quoted")
	}
	inner := s[1 : len(s)-1]
	if strings.ContainsAny(inner, "'\"") {
		return "", fmt.Errorf("unexpected quote in argument")
	}
	return inner, nil
}
