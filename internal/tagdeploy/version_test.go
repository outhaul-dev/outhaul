package tagdeploy

import (
	"reflect"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"v1.4.0", Version{1, 4, 0, nil}, true},
		{"V2", Version{2, 0, 0, nil}, true},
		{"1.2", Version{1, 2, 0, nil}, true},
		{"v1.5.0-rc.1", Version{1, 5, 0, []string{"rc", "1"}}, true},
		{"v1.4.0+build.7", Version{1, 4, 0, nil}, true},
		{"v1.5.0-rc.1+b", Version{1, 5, 0, []string{"rc", "1"}}, true},
		{"", Version{}, false},
		{"v", Version{}, false},
		{"release-oct", Version{}, false},
		{"v1.2.3.4", Version{}, false},
		{"v1.x", Version{}, false},
		{"1.0.0-", Version{}, false},
		{"1.0.0-a..b", Version{}, false},
		{"v-1", Version{}, false},
		{"1..2", Version{}, false},
		{"1.0.0+", Version{}, false},
		{"vV1", Version{}, false},
		{"vv1", Version{}, false},
	}
	for _, tc := range tests {
		got, ok := ParseVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("ParseVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, ok := ParseVersion(s)
	if !ok {
		t.Fatalf("ParseVersion(%q) failed", s)
	}
	return v
}

func TestCompareVersions(t *testing.T) {
	chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.9.2", "v1.10.0"}
	for i := 0; i+1 < len(chain); i++ {
		a, b := mustParse(t, chain[i]), mustParse(t, chain[i+1])
		if got := a.Compare(b); got >= 0 {
			t.Errorf("%s.Compare(%s) = %d, want < 0", chain[i], chain[i+1], got)
		}
		if got := b.Compare(a); got <= 0 {
			t.Errorf("%s.Compare(%s) = %d, want > 0", chain[i+1], chain[i], got)
		}
	}
	for _, p := range [][2]string{{"v1.4.0+1", "v1.4.0"}, {"v2", "v2.0.0"}} {
		if got := mustParse(t, p[0]).Compare(mustParse(t, p[1])); got != 0 {
			t.Errorf("%s.Compare(%s) = %d, want 0", p[0], p[1], got)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	for in, want := range map[string]bool{"v1.5.0-rc.1": true, "v1.5.0": false, "v1.5.0+meta": false} {
		if got := mustParse(t, in).IsPrerelease(); got != want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", in, got, want)
		}
	}
}
