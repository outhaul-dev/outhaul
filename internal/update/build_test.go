package update

import "testing"

func setStamp(t *testing.T, c, r, l, v string) {
	t.Helper()
	oc, or, ol, ov := commit, repo, local, version
	commit, repo, local, version = c, r, l, v
	t.Cleanup(func() { commit, repo, local, version = oc, or, ol, ov })
}

func TestCurrentReadsTheStamp(t *testing.T) {
	setStamp(t, "0123456789abcdef", "https://github.com/o/r.git", "true", "v1.2.3")
	got := Current()
	want := Build{Commit: "0123456789abcdef", Repo: "https://github.com/o/r.git", Local: true, Version: "v1.2.3"}
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
	setStamp(t, "", "", "", "")
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
	if got.Version != "" {
		t.Errorf("Version = %q, want empty", got.Version)
	}
}

// A build stamped with a commit but no tag (a branch or a full commit was
// installed) is still a complete stamp: only the version is empty.
func TestCurrentWithoutAVersion(t *testing.T) {
	setStamp(t, "0123456789abcdef", "https://github.com/o/r.git", "false", "")
	got := Current()
	if got.Version != "" {
		t.Errorf("Version = %q, want empty", got.Version)
	}
	if got.Commit != "0123456789abcdef" {
		t.Errorf("Commit = %q, want the stamped commit", got.Commit)
	}
}
