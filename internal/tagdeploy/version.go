// Package tagdeploy decides which pushed tag, if any, a tag-mode app deploys.
// Pure: no I/O.
package tagdeploy

import (
	"strconv"
	"strings"
)

// Version is a parsed version tag such as "v1.4.0" or "v1.5.0-rc.1". Build
// metadata ("+build.7") is dropped: it takes no part in ordering.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string // pre-release identifiers; nil for a release
}

// ParseVersion parses a SemVer-style tag. A leading "v" or "V" is optional and
// the minor and patch parts may be left out ("v2" is 2.0.0). It reports false
// for anything else, so non-version tags such as "release-oct" never deploy.
func ParseVersion(s string) (Version, bool) {
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	var v Version
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if i == len(s)-1 {
			return v, false
		}
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		for _, id := range strings.Split(s[i+1:], ".") {
			if !validIdent(id) {
				return v, false
			}
			v.Pre = append(v.Pre, id)
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, false
	}
	var nums [3]uint64
	for i, p := range parts {
		if !isDigits(p) {
			return Version{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, false
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, true
}

// IsPrerelease reports whether the version carries a pre-release suffix.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare orders versions by SemVer 2.0 precedence, returning -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			return cmp(p[0] < p[1])
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1 // a release sorts above its pre-releases
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := compareIdent(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.Pre) < len(o.Pre):
		return -1
	case len(v.Pre) > len(o.Pre):
		return 1
	}
	return 0
}

// compareIdent compares two pre-release identifiers: numeric ones numerically,
// numeric below alphanumeric, otherwise in ASCII order.
func compareIdent(a, b string) int {
	an, bn := isDigits(a), isDigits(b)
	switch {
	case an && bn:
		// Compare by length after trimming zeros, then lexically, so
		// identifiers too long for a uint64 still order correctly.
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			return cmp(len(a) < len(b))
		}
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func cmp(less bool) int {
	if less {
		return -1
	}
	return 1
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validIdent reports whether s is a non-empty run of [0-9A-Za-z-].
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}
