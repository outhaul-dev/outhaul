package tagdeploy

import (
	"strings"
	"testing"
)

func TestPolicyVersionStripsLiteralPrefix(t *testing.T) {
	tests := []struct {
		pattern, tag, want string
		ok                 bool
	}{
		{"api/v*", "api/v1.2.0", "1.2.0", true},
		{"v*", "v1.4.0", "1.4.0", true},
		{"pkg@*", "pkg@1.2.3", "1.2.3", true},
		{"*", "v1.0.0", "1.0.0", true},
		{"v*", "vnext", "", false},
	}
	for _, tt := range tests {
		got, ok := Policy{Pattern: tt.pattern}.Version(tt.tag)
		if ok != tt.ok {
			t.Errorf("%q.Version(%q) ok = %v, want %v", tt.pattern, tt.tag, ok, tt.ok)
			continue
		}
		if ok {
			if want := mustParse(t, tt.want); got.Compare(want) != 0 {
				t.Errorf("%q.Version(%q) = %+v, want %s", tt.pattern, tt.tag, got, tt.want)
			}
		}
	}
}

func TestPolicyEligible(t *testing.T) {
	off := Policy{Pattern: "v*"}
	on := Policy{Pattern: "v*", Prereleases: true}
	tests := []struct {
		p    Policy
		tag  string
		want bool
	}{
		{off, "v1.4.0", true},
		{off, "v1.5.0-rc.1", false},
		{off, "release-oct", false},
		{off, "vnext", true},
		{on, "v1.5.0-rc.1", true},
	}
	for _, tt := range tests {
		if got := tt.p.Eligible(tt.tag); got != tt.want {
			t.Errorf("%+v.Eligible(%q) = %v, want %v", tt.p, tt.tag, got, tt.want)
		}
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		pre       bool
		pushed    []string
		inPlay    []string
		want      string
		reasonHas []string
	}{
		{"new tag", "v*", false, []string{"v1.4.0"}, nil, "v1.4.0", nil},
		{"older than live", "v*", false, []string{"v1.3.1"}, []string{"v1.4.0"}, "", []string{"v1.3.1", "v1.4.0"}},
		{"newer than rollback", "v*", false, []string{"v1.3.1"}, []string{"v1.3.0"}, "v1.3.1", nil},
		{"re-pushed", "v*", false, []string{"v1.3.1"}, []string{"v1.3.1"}, "", []string{"not newer"}},
		{"build metadata only", "v*", false, []string{"v1.4.0+2"}, []string{"v1.4.0"}, "", []string{"not newer"}},
		{"highest of several", "v*", false, []string{"v1.3.0", "v1.4.0", "v1.2.0"}, nil, "v1.4.0", nil},
		{"non-version fails open", "*", false, []string{"release-oct"}, []string{"v1.4.0"}, "release-oct", nil},
		{"last non-version", "nightly-*", false, []string{"nightly-a", "nightly-b"}, nil, "nightly-b", nil},
		{"pre-release off", "v*", false, []string{"v1.5.0-rc.1"}, []string{"v1.4.0"}, "", []string{"pre-release"}},
		{"pre-release on", "v*", true, []string{"v1.5.0-rc.1"}, []string{"v1.4.0"}, "v1.5.0-rc.1", nil},
		{"release beats its rc", "v*", true, []string{"v1.5.0"}, []string{"v1.5.0-rc.2"}, "v1.5.0", nil},
		{"no match", "v*", false, []string{"x1.0"}, nil, "", []string{`"v*"`}},
		{"pattern change", "api/v*", false, []string{"api/v1.1.0"}, []string{"v3.0.0"}, "api/v1.1.0", nil},
		{"non-version in play ignored", "v*", false, []string{"v1.0.0"}, []string{"release-oct"}, "v1.0.0", nil},
		{"empty in play ignored", "v*", false, []string{"v0.1.0"}, []string{""}, "v0.1.0", nil},
		{"nothing pushed", "v*", false, nil, nil, "", []string{"no tag was pushed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Policy{Pattern: tt.pattern, Prereleases: tt.pre}.Decide(tt.pushed, tt.inPlay)
			if d.Tag != tt.want {
				t.Fatalf("Tag = %q, want %q (reason %q)", d.Tag, tt.want, d.Reason)
			}
			for _, s := range tt.reasonHas {
				if !strings.Contains(d.Reason, s) {
					t.Errorf("Reason %q does not contain %q", d.Reason, s)
				}
			}
		})
	}
}

func TestNewest(t *testing.T) {
	tags := []string{"v1.2.0", "v1.10.0", "v1.9.9", "v2.0.0-rc.1", "notes"}
	if got, ok := (Policy{Pattern: "v*"}).Newest(tags); !ok || got != "v1.10.0" {
		t.Errorf("pre-releases off: got %q, %v", got, ok)
	}
	if got, ok := (Policy{Pattern: "v*", Prereleases: true}).Newest(tags); !ok || got != "v2.0.0-rc.1" {
		t.Errorf("pre-releases on: got %q, %v", got, ok)
	}
	if got, ok := (Policy{Pattern: "v*"}).Newest([]string{"vnext", "notes"}); ok || got != "" {
		t.Errorf("non-version tags: got %q, %v", got, ok)
	}
}
