package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Request bodies were unbounded: a multipart POST to /login was parsed in
// full (spilling to temp files past 32 MiB) before any check. A body
// declared over the limit is refused outright.
func TestOversizedBodyRefused(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	big := url.Values{"username": {"admin"}, "password": {"supersecret"}, "pad": {strings.Repeat("x", 2<<20)}}

	resp, err := http.Post(e.http.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader(big.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// A chunked body declares no length, so it is cut off while being read: the
// credentials after the padding are never reached, and sign-in fails.
func TestChunkedBodyIsCutOff(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("pad", strings.Repeat("x", 3<<20))
	mw.WriteField("username", "admin")
	mw.WriteField("password", "supersecret")
	mw.Close()
	// io.MultiReader hides the length, so the client sends it chunked.
	req, err := http.NewRequest(http.MethodPost, e.http.URL+"/login", io.MultiReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a 3 MiB chunked body was read in full: sign-in succeeded with credentials past the limit")
	}
}

// Each argon2 hash takes 64 MiB; concurrent sign-in attempts were unbounded,
// so a burst could exhaust the server's memory. Hashing waits for one of a
// few slots and gives up with 503 when none frees in time.
func TestLoginWaitsForAHashSlot(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.srv.hashWait = 50 * time.Millisecond
	for i := 0; i < cap(e.srv.hashSlots); i++ {
		e.srv.hashSlots <- struct{}{} // every slot busy
	}

	login := func() *http.Response {
		t.Helper()
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
			PostForm(e.http.URL+"/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := login()
	page := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status with every hash slot busy = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(page, "Too many sign-in attempts") {
		t.Errorf("busy page does not explain itself:\n%s", page)
	}

	for i := 0; i < cap(e.srv.hashSlots); i++ {
		<-e.srv.hashSlots
	}
	resp = login()
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status once slots are free = %d, want 303", resp.StatusCode)
	}
}
