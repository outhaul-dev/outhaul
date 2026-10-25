package update

import "testing"

func setStamp(t *testing.T, c, r, l string) {
	t.Helper()
	oc, or, ol := commit, repo, local
	commit, repo, local = c, r, l
	t.Cleanup(func() { commit, repo, local = oc, or, ol })
}

func TestCurrentReadsTheStamp(t *testing.T) {
	setStamp(t, "0123456789abcdef", "https://github.com/o/r.git", "true")
	got := Current()
	want := Build{Commit: "0123456789abcdef", Repo: "https://github.com/o/r.git", Local: true}
	if got != want {
		t.Fatalf("Current() = %+v, want %+v", got, want)
	}
	if !got.Known() {
		t.Error("Known() = false, want true")
	}
	if got.Short() != "0123456" {
		t.Errorf("Short() = %q, want 0123456", got.Short())
	}
}

func TestCurrentWithoutAStamp(t *testing.T) {
	setStamp(t, "", "", "")
	got := Current()
	if got.Known() {
		t.Error("Known() = true, want false")
	}
	if got.Short() != "unknown" {
		t.Errorf("Short() = %q, want unknown", got.Short())
	}
	if got.Repo != "https://github.com/outhaul-dev/outhaul.git" {
		t.Errorf("Repo = %q, want the default", got.Repo)
	}
	if got.Local {
		t.Error("Local = true, want false")
	}
}
