package githook

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

func TestRequestRoundTripWithOrigin(t *testing.T) {
	req := request{App: "api", Refs: []RefUpdate{{"000", "abc", "refs/heads/main"}},
		Origin: Origin{TokenID: 42, Addr: "203.0.113.9"}}
	var buf strings.Builder
	if err := writeRequest(&buf, req); err != nil {
		t.Fatalf("writeRequest: %v", err)
	}
	got, err := readRequest(newBufReader(buf.String()))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if got.Origin != (Origin{TokenID: 42, Addr: "203.0.113.9"}) {
		t.Fatalf("origin = %+v", got.Origin)
	}
	if !got.Origin.HTTPS() {
		t.Fatal("HTTPS() = false")
	}

	var plain strings.Builder
	if err := writeRequest(&plain, request{App: "api", Refs: []RefUpdate{{"000", "abc", "refs/heads/main"}}}); err != nil {
		t.Fatal(err)
	}
	if want := "app api\nref 000 abc refs/heads/main\n\n"; plain.String() != want {
		t.Fatalf("SSH bytes = %q, want %q", plain.String(), want)
	}
}

func TestReadRequestRefusesBadOrigin(t *testing.T) {
	for _, line := range []string{"token x", "token -1", "addr not-an-ip"} {
		if _, err := readRequest(newBufReader("app api\n" + line + "\n\n")); err == nil {
			t.Errorf("%q: no error", line)
		}
	}
}

func TestOriginFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	got := OriginFromEnv(env(map[string]string{EnvTokenID: "7", EnvClientAddr: "2001:db8::1"}))
	if got != (Origin{TokenID: 7, Addr: "2001:db8::1"}) {
		t.Fatalf("both set: %+v", got)
	}
	for _, id := range []string{"", "abc", "0", "-3"} {
		got := OriginFromEnv(env(map[string]string{EnvTokenID: id, EnvClientAddr: "203.0.113.9"}))
		if got != (Origin{}) {
			t.Errorf("id %q: %+v, want zero", id, got)
		}
	}
	got = OriginFromEnv(env(map[string]string{EnvTokenID: "7", EnvClientAddr: "nope"}))
	if got != (Origin{TokenID: 7}) {
		t.Fatalf("bad addr: %+v", got)
	}
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func httpsFixture(t *testing.T) (*Bridge, *store.Store, core.App, int64) {
	t.Helper()
	b, st, _ := bridgeEnv(t)
	ctx := context.Background()
	u, err := st.CreatePasswordAccount(ctx, "alice", core.RoleSystemAdmin)
	if err != nil {
		t.Fatalf("CreatePasswordAccount: %v", err)
	}
	now := time.Now()
	_, tok, err := st.CreatePushToken(ctx, u.ID, "laptop", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CreatePushToken: %v", err)
	}
	app, err := st.CreateApp(ctx, core.App{Name: "api", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	b.SetPublicURL("https://outhaul.example.com")
	return b, st, app, tok.ID
}

func TestBridgeLogsAnHTTPSPush(t *testing.T) {
	b, st, app, tokID := httpsFixture(t)
	logs := captureLog(t)
	br := b.broker

	var wg sync.WaitGroup
	wg.Add(1)
	var depID int64
	go func() {
		defer wg.Done()
		dep := waitForDeployment(t, st, app.ID)
		depID = dep.ID
		markTerminal(t, st, dep, core.StatusRunning)
		br.Close(dep.ID)
	}()
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc123", "refs/heads/main"}},
		Origin: Origin{TokenID: tokID, Addr: "203.0.113.9"}})
	wg.Wait()

	first, _, _ := strings.Cut(out, "\n")
	if want := fmt.Sprintf("→ build log: https://outhaul.example.com/deployments/%d", depID); first != want {
		t.Fatalf("first line = %q, want %q", first, want)
	}
	line := logs.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Fatalf("%d log lines:\n%s", n, line)
	}
	for _, want := range []string{"alice", "laptop", fmt.Sprint(tokID), "203.0.113.9", "api", "refs/heads/main", "abc123"} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q:\n%s", want, line)
		}
	}
}

func TestBridgeSSHPushUnchanged(t *testing.T) {
	b, st, app, _ := httpsFixture(t)
	logs := captureLog(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dep := waitForDeployment(t, st, app.ID)
		markTerminal(t, st, dep, core.StatusRunning)
		b.broker.Close(dep.ID)
	}()
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc123", "refs/heads/main"}}})
	wg.Wait()
	if strings.Contains(out, "build log") {
		t.Fatalf("SSH push printed a build log line:\n%s", out)
	}
	if logs.Len() != 0 {
		t.Fatalf("SSH push logged: %s", logs.String())
	}
}

func TestBridgeLogsAPushWhoseTokenIsGone(t *testing.T) {
	b, st, app, tokID := httpsFixture(t)
	ctx := context.Background()
	tok, err := st.GetPushToken(ctx, tokID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePushTokenForUser(ctx, tok.UserID, tokID); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dep := waitForDeployment(t, st, app.ID)
		markTerminal(t, st, dep, core.StatusRunning)
		b.broker.Close(dep.ID)
	}()
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc123", "refs/heads/main"}},
		Origin: Origin{TokenID: tokID, Addr: "203.0.113.9"}})
	wg.Wait()
	if !strings.Contains(out, "✓ deployed") {
		t.Fatalf("push did not deploy:\n%s", out)
	}
	line := logs.String()
	if !strings.Contains(line, fmt.Sprint(tokID)) || !strings.Contains(line, "removed") {
		t.Fatalf("log = %q", line)
	}
}

func TestBridgeLogsAPushThatDeploysNothing(t *testing.T) {
	b, _, _, tokID := httpsFixture(t)
	logs := captureLog(t)
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc123", "refs/heads/dev"}},
		Origin: Origin{TokenID: tokID, Addr: "203.0.113.9"}})
	if strings.Contains(out, "build log") {
		t.Fatalf("printed a build log line:\n%s", out)
	}
	if !strings.Contains(out, "no deploy") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(logs.String(), "refs/heads/dev") || !strings.Contains(logs.String(), "alice") {
		t.Fatalf("log = %q", logs.String())
	}
}
