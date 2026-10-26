package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func mkCommit(sha, msg string, date time.Time) ghCommit {
	var c ghCommit
	c.SHA = sha
	c.Commit.Message = msg
	c.Commit.Committer.Date = date
	return c
}

type ghFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch,omitempty"`
}

type ghReply struct {
	Status   string     `json:"status"`
	AheadBy  int        `json:"ahead_by"`
	BehindBy int        `json:"behind_by"`
	HTMLURL  string     `json:"html_url"`
	Commits  []ghCommit `json:"commits"`
	Files    []ghFile   `json:"files"`
}

// fakeGitHub answers every request with reply (or code, if non-zero) and
// records the request paths.
type fakeGitHub struct {
	*httptest.Server
	reply ghReply
	code  int
	paths []string
	auth  []string
	acc   []string
}

func newFakeGitHub(t *testing.T, reply ghReply) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.acc = append(f.acc, r.Header.Get("Accept"))
		if f.code != 0 {
			http.Error(w, "nope", f.code)
			return
		}
		_ = json.NewEncoder(w).Encode(f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) comparer() *Comparer { return &Comparer{APIBase: f.URL} }

const repoURL = "https://github.com/o/r.git"

func TestCompareBehind(t *testing.T) {
	t1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	f := newFakeGitHub(t, ghReply{
		Status: "ahead", AheadBy: 2, HTMLURL: "https://github.com/o/r/compare/abc...main",
		Commits: []ghCommit{mkCommit("c1", "old\n\nbody", t1), mkCommit("c2", "new", t2)},
	})
	got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != CompareBehind || got.Behind != 2 {
		t.Fatalf("got %+v", got)
	}
	want := []Commit{{SHA: "c2", Subject: "new", Date: t2}, {SHA: "c1", Subject: "old", Date: t1}}
	if len(got.Commits) != 2 || got.Commits[0] != want[0] || got.Commits[1] != want[1] {
		t.Fatalf("commits = %+v", got.Commits)
	}
	if got.URL != "https://github.com/o/r/compare/abc...main" {
		t.Fatalf("url = %q", got.URL)
	}
	if len(f.paths) != 1 || f.paths[0] != "/repos/o/r/compare/abc...main" {
		t.Fatalf("paths = %v", f.paths)
	}
	if f.acc[0] != "application/vnd.github+json" || f.auth[0] != "" {
		t.Fatalf("accept %q auth %q", f.acc[0], f.auth[0])
	}
}

func TestCompareCapsTheList(t *testing.T) {
	var cs []ghCommit
	for i := 0; i < 60; i++ {
		cs = append(cs, mkCommit(fmt.Sprintf("c%d", i), "m", time.Time{}))
	}
	f := newFakeGitHub(t, ghReply{Status: "ahead", AheadBy: 60, Commits: cs})
	got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Commits) != 50 || got.Commits[0].SHA != "c59" || got.Commits[49].SHA != "c10" {
		t.Fatalf("len %d first %s last %s", len(got.Commits), got.Commits[0].SHA, got.Commits[len(got.Commits)-1].SHA)
	}
	if got.Behind != 60 {
		t.Fatalf("behind = %d", got.Behind)
	}
}

func TestCompareUpToDate(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "identical"})
	got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
	if err != nil || got.Status != CompareUpToDate || got.Behind != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestCompareAheadOrDiverged(t *testing.T) {
	for _, status := range []string{"behind", "diverged"} {
		f := newFakeGitHub(t, ghReply{Status: status, AheadBy: 3})
		got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
		if err != nil || got.Status != CompareAhead || got.Behind != 0 {
			t.Fatalf("%s: got %+v, %v", status, got, err)
		}
	}
	f := newFakeGitHub(t, ghReply{})
	f.code = http.StatusNotFound
	got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
	if err != nil || got.Status != CompareAhead {
		t.Fatalf("404: got %+v, %v", got, err)
	}
}

func TestCompareOtherStatusIsAnError(t *testing.T) {
	f := newFakeGitHub(t, ghReply{})
	f.code = http.StatusInternalServerError
	_, err := f.comparer().Compare(context.Background(), repoURL, "abc")
	if err == nil {
		t.Fatal("want an error for 500")
	}
	if errors.Is(err, ErrRefused) {
		t.Errorf("500 is %v, want an error other than ErrRefused", err)
	}
}

// GitHub answers 403 (or 429) when it refuses an anonymous caller, most
// often for its rate limit.
func TestCompareRefusedIsErrRefused(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		f := newFakeGitHub(t, ghReply{})
		f.code = code
		if _, err := f.comparer().Compare(context.Background(), repoURL, "abc"); !errors.Is(err, ErrRefused) {
			t.Errorf("%d: err = %v, want ErrRefused", code, err)
		}
	}
}

func TestCompareSpotsInstallerAndTraefikChanges(t *testing.T) {
	cases := []struct {
		name               string
		files              []ghFile
		installer, traefik bool
	}{
		{"installer", []ghFile{{Filename: "deploy/bootstrap.sh"}}, true, false},
		{"traefik proxy", []ghFile{{Filename: "internal/traefik/proxy.go"}}, false, true},
		{"traefik image", []ghFile{{Filename: "internal/config/config.go", Patch: "@@\n-const DefaultTraefikImage = \"traefik:v3.7.6\"\n+const DefaultTraefikImage = \"traefik:v3.8.0\""}}, false, true},
		{"unrelated config", []ghFile{{Filename: "internal/config/config.go", Patch: "@@\n-const Other = 1\n+const Other = 2\n DefaultTraefikImage context"}}, false, false},
		{"none", []ghFile{{Filename: "README.md"}}, false, false},
	}
	for _, tc := range cases {
		f := newFakeGitHub(t, ghReply{Status: "ahead", AheadBy: 1, Files: tc.files})
		got, err := f.comparer().Compare(context.Background(), repoURL, "abc")
		if err != nil {
			t.Fatal(err)
		}
		if got.InstallerChanged != tc.installer || got.TraefikChanged != tc.traefik {
			t.Errorf("%s: installer=%v traefik=%v", tc.name, got.InstallerChanged, got.TraefikChanged)
		}
	}
}

func TestCompareRejectsNonGitHub(t *testing.T) {
	f := newFakeGitHub(t, ghReply{Status: "identical"})
	_, err := f.comparer().Compare(context.Background(), "https://gitlab.com/o/r.git", "abc")
	if !errors.Is(err, ErrNotGitHub) || len(f.paths) != 0 {
		t.Fatalf("err %v, requests %v", err, f.paths)
	}
	for _, u := range []string{"git@github.com:o/r.git", "git@github.com:o/r", "https://github.com/o/r"} {
		if _, err := f.comparer().Compare(context.Background(), u, "abc"); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if len(f.paths) != 3 || f.paths[0] != "/repos/o/r/compare/abc...main" {
		t.Fatalf("paths = %v", f.paths)
	}
}
