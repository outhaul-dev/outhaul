# Log Streams That Stay Open and Reconnect (R14) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep every Server-Sent Events log view alive through a quiet build or a CDN, and make a reconnect show the same log once instead of repeating or losing lines.

**Architecture:** The two handlers in `internal/server/sse.go` send a `: ping` comment every 20 seconds. Runtime logs move their blocking Docker scan into a goroutine so one handler goroutine owns every write and can also flush headers before the first line and stop on the stream context. One shared static script clears the box when a stream opens and reconnects with 2s-to-30s backoff for five minutes when the browser stops retrying, relying on the server's existing replay.

**Tech Stack:** Go 1.26, `net/http` SSE, Docker Engine log follow, plain browser JavaScript, POSIX `sh` only where the docs are checked.

**Spec:** `docs/superpowers/specs/2026-11-11-client-ips-streams-sign-in-design.md`, sections "R14: Streams that stay open" and "Tests"/"Docs" for R14.

**Before you start:** this series has no dependency on the proxy-ingress plan and can start now. Its docs task treats `docs/BEHIND-A-PROXY.md`, which that plan creates, as optional; see Task 4.

## Global Constraints

- Streams stay Server-Sent Events, not WebSockets (a locked decision). Long-lived handlers wait on `s.streamContext(r)`, not `r.Context()`.
- Every stream sends the comment `: ping` and flushes every 20 seconds. `ssePingEvery` is a package variable so tests can shorten it; there is no setting.
- No event ids. A reconnect replays from the start, and the page clears its log box on `open`; no handler change is needed for replay.
- Build logs stay in memory only, bounded as today. Runtime logs still replay the last `tail` lines.
- A `done` or `err` event closes the stream for good. Only a connection error may retry.
- Test first: write the failing test, watch it fail for the stated reason, then implement. No test in this repository runs a page's script; the reconnect is checked by hand in a browser, as the spec says.
- This repository is public: `example.com` names only.
- After each task: `go build ./... && go vet ./... && go test ./...`. Before calling the plan done: `go test -race -timeout 30m ./...`.
- Commits: `feat(scope): summary` or `fix(scope): summary`, with a body saying what the user saw go wrong and what changed.

## Review Focus

The inputs and failures most likely to bite that no requirement spells out. Each has its test in the task named.

1. A build stream whose source sends nothing for minutes: the response headers and a `: ping` still arrive, and the handler keeps waiting (Task 1).
2. A quiet runtime container: the page's `open` fires before any log line, a `: ping` follows, and a container stop still ends with `done` (Task 2).
3. The server shuts down, or the client disconnects, while the scanner is blocked: the follow reader is closed, the scan goroutine gives up, and the handler returns without sending `done` (Task 2, both paths).
4. A reconnect after a network error, or after a reply that is not a stream (a 502 or the sign-in page): `open`/`onopen` clears the box, the replay appears once, the 2s→30s backoff and the 5-minute stop use the spec's strings (Task 3, checked by hand in a browser as the spec requires).
5. A stream that ended with `done` or `err` does not reconnect; changing the tail or compose service opens a fresh one (Task 3, checked in the same browser pass).

## File map

| File | Responsibility | Tasks |
|---|---|---|
| `internal/server/sse.go` | `ssePingEvery`, `writeSSEComment`, the build-stream ticker, the runtime scan goroutine and ticker | 1, 2 |
| `internal/server/sse_ping_test.go` (new) | ping tests for all three handlers, and the stream-context end | 1, 2 |
| `internal/server/static/logstream.js` (new) | the shared clear-and-reconnect helper | 3 |
| `internal/server/templates/base.tmpl` | loads the helper before page scripts | 3 |
| `internal/server/templates/deployment.tmpl`, `app.tmpl`, `database.tmpl` | use the helper; keep each page's done/err wording | 3 |
| `internal/server/logstream_page_test.go` (new) | the scripts are served and all three pages load them | 3 |
| `docs/BEHIND-A-PROXY.md` (if present) or `docs/STREAMS.md` (new), `AGENTS.md` | streams, WebSockets, and the kept decision | 4 |

---

## Task 1: Pings on the build-log stream

**Files:**
- Modify: `internal/server/sse.go`
- Create: `internal/server/sse_ping_test.go`
- Test: `internal/server/sse_ping_test.go`

**Interfaces:**
- Produces:

```go
// sse.go
var ssePingEvery = 20 * time.Second
func writeSSEComment(w http.ResponseWriter, text string) // writes ": text\n\n"
```

- [ ] **Step 1: Write the failing tests**

Create `internal/server/sse_ping_test.go`:

```go
// shortPing makes every stream ping quickly for one test.
func shortPing(t *testing.T) {
	t.Helper()
	old := ssePingEvery
	ssePingEvery = 10 * time.Millisecond
	t.Cleanup(func() { ssePingEvery = old })
}

// readSSEUntil reads lines from br until they contain want. The request's
// deadline ends the read, so a stream that never sends want fails the test
// instead of hanging it.
func readSSEUntil(t *testing.T, br *bufio.Reader, want string) string {
	t.Helper()
	var got strings.Builder
	for {
		line, err := br.ReadString('\n')
		got.WriteString(line)
		if strings.Contains(got.String(), want) {
			return got.String()
		}
		if err != nil {
			t.Fatalf("stream ended before %q: %v\n%s", want, err, got.String())
		}
	}
}

func TestWriteSSEComment(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEComment(rec, "ping")
	if got := rec.Body.String(); got != ": ping\n\n" {
		t.Errorf("body = %q, want %q", got, ": ping\n\n")
	}
}

func TestBuildLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	shortPing(t)

	resp, err := e.getWithin(t, "/deployments/"+itoa(dep.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("the log stream did not open while quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if got := readSSEUntil(t, br, ": ping\n\n"); !strings.Contains(got, ": ping\n\n") {
		t.Fatalf("no ping before the deadline:\n%s", got)
	}
}
```

Use the existing helpers: `newTestEnv`, `e.login`, `e.store.CreateApp`, `e.store.CreateDeployment`, `e.getWithin`, `itoa`.

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/server/ -run 'WriteSSEComment|BuildLogStreamPings'`

Expected: FAIL — `ssePingEvery` and `writeSSEComment` are undefined; without the ticker the read ends at the request deadline.

- [ ] **Step 3: Implement**

In `internal/server/sse.go`:

```go
var ssePingEvery = 20 * time.Second

func writeSSEComment(w http.ResponseWriter, text string) {
	fmt.Fprintf(w, ": %s\n\n", text)
}
```

In `handleLogsSSE`, after the history is flushed and before the existing loop:

```go
ticker := time.NewTicker(ssePingEvery)
defer ticker.Stop()
```

Add a `case <-ticker.C:` to the `select` that writes `writeSSEComment(w, "ping")` and flushes.

Do not ping while replaying history; the history write is already output.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/server/ -run 'WriteSSEComment|BuildLogStreamPings'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/sse.go internal/server/sse_ping_test.go
git commit -m "feat(server): a ping every 20 seconds on the build-log stream" -m "A quiet build stream sent nothing, so a CDN could close it. Ping every 20 seconds."
```

---

## Task 2: Pings, early headers and a non-blocking scan on runtime logs

**Files:**
- Modify: `internal/server/sse.go`
- Test: `internal/server/sse_ping_test.go`

**Interfaces:**
- Consumes: `ssePingEvery` and `writeSSEComment` from Task 1.
- Produces: no new exported API. `streamContainerLogs` writes headers before the first line, scans in a goroutine, and selects on the channel, the ticker and the stream context.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/sse_ping_test.go` (import `context`, `io`, `bufio`, `net/http`, `strings`, `sync` and `time` as needed):

```go
func TestRuntimeLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/apps/"+itoa(app.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	pw.Close()
	if got := readSSEUntil(t, br, "event: done"); !strings.Contains(got, "event: done") {
		t.Fatalf("stream did not end with done:\n%s", got)
	}
}

func TestDatabaseLogStreamPingsWhileQuiet(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	e.runtime.container = &docker.Container{ID: "db1", Name: "outhaul-db-shop", State: "running"}
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/databases/"+itoa(d.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	pw.Close()
	if got := readSSEUntil(t, br, "event: done"); !strings.Contains(got, "event: done") {
		t.Fatalf("stream did not end with done:\n%s", got)
	}
}

func TestRuntimeLogStreamEndsWhenStreamsClose(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	pr, pw := io.Pipe()
	e.runtime.follow = pr
	shortPing(t)
	defer pw.Close()

	resp, err := e.getWithin(t, "/apps/"+itoa(app.ID)+"/logs", 2*time.Second)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")
	e.srv.CloseStreams()
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading after CloseStreams: %v", err)
	}
	if strings.Contains(string(rest), "event: done") {
		t.Errorf("stream ended with done after CloseStreams:\n%s", rest)
	}
}

// blockingLogs is a follow stream that blocks in Read until the handler closes
// it. The closed channel records that Close happened, so the test can see the
// scanner was released even though it cannot see the goroutine itself.
type blockingLogs struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingLogs) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingLogs) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestRuntimeLogStreamCancelsTheScanOnClientDisconnect(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")
	logs := &blockingLogs{closed: make(chan struct{})}
	e.runtime.follow = logs
	shortPing(t)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.http.URL+"/apps/"+itoa(app.ID)+"/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("headers did not arrive while the container was quiet: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readSSEUntil(t, br, ": ping\n\n")

	cancel()
	select {
	case <-logs.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not close the log stream after the client went away")
	}
	rest, _ := io.ReadAll(br)
	if strings.Contains(string(rest), "event: done") {
		t.Errorf("stream ended with done after the client went away:\n%s", rest)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server/ -run 'RuntimeLogStreamPings|DatabaseLogStreamPings|EndsWhenStreamsClose|CancelsTheScan'`

Expected: FAIL — `getWithin` returns a deadline error because no headers or lines are flushed while the pipe is quiet.

- [ ] **Step 3: Implement**

In `streamContainerLogs`, after `defer logs.Close()` and before the scanner:

```go
flusher.Flush() // the page's open fires now, even while the container is quiet
```

Replace the `sc.Scan()` loop with one goroutine and one select loop:

```go
lines := make(chan string)
go func() {
	defer close(lines)
	for sc.Scan() {
		select {
		case lines <- sc.Text():
		case <-ctx.Done():
			return
		}
	}
}()

ticker := time.NewTicker(ssePingEvery)
defer ticker.Stop()
for {
	select {
	case <-ctx.Done():
		return
	case <-ticker.C:
		writeSSEComment(w, "ping")
		flusher.Flush()
	case line, open := <-lines:
		if !open {
			if ctx.Err() != nil {
				return
			}
			writeSSE(w, "done", "")
			flusher.Flush()
			return
		}
		writeSSE(w, "log", line)
		flusher.Flush()
	}
}
```

Keep the existing watcher goroutine that closes `logs` on `ctx.Done()`. The scanner goroutine's send gives up on the same context, so it cannot outlive the handler.

- [ ] **Step 4: Run all the server tests**

Run: `go test ./internal/server/`

Expected: PASS, including the existing `TestRuntimeLogsSSE` and `TestDatabaseLogsSSEStreams`.

- [ ] **Step 5: Commit**

```bash
git add internal/server/sse.go internal/server/sse_ping_test.go
git commit -m "feat(server): runtime log streams flush at once, ping while quiet, and scan without blocking" -m "A quiet container's log page stayed on connecting and never opened. Flush at once, ping, and scan without blocking the handler."
```

---

## Task 3: The pages clear and reconnect

**Files:**
- Create: `internal/server/static/logstream.js`, `internal/server/logstream_page_test.go`
- Modify: `internal/server/templates/base.tmpl`, `deployment.tmpl`, `app.tmpl`, `database.tmpl`
- Test: `internal/server/logstream_page_test.go`

**Interfaces:**
- Produces:

```js
// window.outhaulLogStream(opts) -> { reopen: function }
// opts = {
//   log: "id of the <pre>",
//   indicator: "id of the status span",
//   url: function () { return "the EventSource URL"; },
//   doneText: "stopped",           // optional; default "complete"
//   onDone: function (received) {} // optional; received counts log events since open
// }
```

- [ ] **Step 1: Write the failing page tests**

Create `internal/server/logstream_page_test.go`:

```go
func TestLogStreamScriptIsServed(t *testing.T) {
	e := newTestEnv(t)
	page := body(t, e.get(t, "/static/logstream.js"))
	if !strings.Contains(page, "outhaulLogStream") {
		t.Error("logstream.js does not define outhaulLogStream")
	}
}

func TestLogPagesLoadTheSharedStreamScript(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")

	check := func(name, path string) {
		t.Helper()
		page := body(t, e.get(t, path))
		if !strings.Contains(page, "outhaulLogStream(") {
			t.Errorf("%s page does not use outhaulLogStream", name)
		}
		if strings.Contains(page, "new EventSource(") {
			t.Errorf("%s page still opens its own EventSource", name)
		}
	}
	check("app", "/apps/"+itoa(app.ID))
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	check("database", "/databases/"+itoa(d.ID))
	dep, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("deployment", "/deployments/"+itoa(dep.ID))
}
```

The second assertion is what makes this test fail on the old templates: `base.tmpl` alone would satisfy a check for `/static/logstream.js`, but only a rewritten page contains `outhaulLogStream(` and none contains `new EventSource(`.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/server/ -run 'LogStreamScript|LogPagesLoad'`

Expected: FAIL — `/static/logstream.js` is 404, and the old pages contain `new EventSource(` and not `outhaulLogStream(`.

- [ ] **Step 3: Write the helper**

Create `internal/server/static/logstream.js`. It must behave exactly as follows:

- `outhaulLogStream(opts)` looks up the two elements by ID. It calls `open()` once at the start.
- `open()` (the initial call and a manual `reopen`): clear the box (`textContent = ""`), reset `received = 0`, reset the five-minute window and the retry delay, cancel any retry timer, close any previous `EventSource`, set the indicator to `connecting…`, then `connect()`.
- `connect()`: create the `EventSource` and register the handlers; it does **not** clear the box or change the indicator. Automatic retries call `connect()`, never `open()`.
- `onopen`: clear the box and count again, reset the retry delay to 2000ms and the five-minute window, and set the indicator to `streaming…`. (The browser can reconnect the same `EventSource` by itself; this is the only place that clears for that case, and for a retry that has just succeeded.)
- `log` event: append `e.data + '\n'`; scroll to the bottom only if the box was already at the bottom; `received++`.
- `err` event: close the source, put `e.data` in the box, set `unavailable`; never retry.
- `done` event: close the source, set the indicator to `opts.doneText` (default `complete`), call `opts.onDone(received)`; never retry.
- `error`:
  - if `es.readyState === EventSource.CONNECTING`, the browser is retrying: set `reconnecting…` and return. Leave the box alone.
  - otherwise (CLOSED, e.g. a 502 or the sign-in page): start a five-minute window on the first fatal error. If it has passed, set `disconnected. Reload the page to try again.` and leave the box as it was. Otherwise set `reconnecting…`, leave the box as it was, and schedule `connect()` after 2000ms, doubling to 30000ms.
- Return `{reopen: open}`; a tail or service change calls `reopen`, which clears the box and starts a fresh stream.

Exact strings: `connecting…`, `streaming…`, `reconnecting…`, `disconnected. Reload the page to try again.`

The box is cleared on `open()` and `onopen`, never at the start of a retry. While Outhaul is down or the session has ended, a page that already showed a log keeps it, under the `reconnecting…` label, until a retry opens or the five-minute window closes.

- [ ] **Step 4: Load the helper and rewrite the three page scripts**

In `base.tmpl`, add a synchronous script before the deferred htmx script:

```html
<script src="/static/logstream.js"></script>
```

(No `defer`: the page's inline script runs during parsing and needs the function.)

In `deployment.tmpl`, keep the existing `(function () { ... })();` wrapper and replace its contents with:

```js
var el = document.getElementById('log');
var indicator = document.getElementById('stream-indicator');
var id = el.getAttribute('data-id');
var live = el.getAttribute('data-live') === 'true';

outhaulLogStream({
  log: 'log',
  indicator: 'stream-indicator',
  url: function () { return '/deployments/' + id + '/logs'; },
  onDone: function (received) {
    if (live) { window.location.reload(); return; }
    if (received === 0) {
      el.classList.add('log-empty');
      el.textContent = 'Build logs for this deployment are no longer available. Outhaul keeps them in memory only for the most recent deployments, and clears them when the service restarts.';
      indicator.textContent = 'no logs';
    }
  }
});
```

In `app.tmpl`, keep the existing `(function () { ... })();` wrapper and replace its contents with:

```js
var id = document.getElementById('runtime-log').getAttribute('data-id');
var svcSel = document.getElementById('log-service');
var tailSel = document.getElementById('log-tail');
var stream = outhaulLogStream({
  log: 'runtime-log',
  indicator: 'runtime-indicator',
  doneText: 'stopped',
  url: function () {
    var qs = 'tail=' + tailSel.value;
    if (svcSel) { qs += '&service=' + encodeURIComponent(svcSel.value); }
    return '/apps/' + id + '/logs?' + qs;
  }
});
if (svcSel) { svcSel.addEventListener('change', function () { stream.reopen(); }); }
tailSel.addEventListener('change', function () { stream.reopen(); });
```

In `database.tmpl`, keep the existing `(function () { ... })();` wrapper and replace its contents with:

```js
var id = document.getElementById('runtime-log').getAttribute('data-id');
var tailSel = document.getElementById('log-tail');
var stream = outhaulLogStream({
  log: 'runtime-log',
  indicator: 'runtime-indicator',
  doneText: 'stopped',
  url: function () { return '/databases/' + id + '/logs?tail=' + tailSel.value; }
});
tailSel.addEventListener('change', function () { stream.reopen(); });
```

Keep `{{define "content"}}`/`{{end}}` and the surrounding markup unchanged.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/server/ -run 'LogStreamScript|LogPagesLoad'`

Expected: PASS.

Run: `go test ./internal/server/`

Expected: PASS.

- [ ] **Step 6 (a person, or an agent with a browser): Check the pages by hand**

Do not tick this step if no terminal and browser are available; the real-host list in the docs carries it for the staging host.

`deploy/dev/run.sh` runs `docker run -it`, so it needs a terminal, and it uses this machine's Docker daemon.

Open a running build's log page, an app's runtime log, and a database's runtime log. Then:

1. Stop Outhaul (Ctrl-C in `run.sh`, or `docker stop outhaul-dev`). The browser sees a refused connection and auto-retries by itself; all three pages say `reconnecting…`, and a page that showed a log keeps it.
2. Start Outhaul again. Each page says `streaming…` and shows its log once, with no line twice and no missing line.
3. Sign out in a second tab, which ends the session. Restart Outhaul. The log page's next reconnect gets the sign-in page, which is not a stream, so the page's own 2s→30s retry runs; the box keeps its last log and the label says `reconnecting…`. Sign in again; the next retry opens, clears the box and replays. If it is left for five minutes, it ends with `disconnected. Reload the page to try again.`
4. Let a build finish while its page is open. The page reloads and shows the final status; it does not reconnect.
5. Change an app's tail or compose service, and a database's tail. The box clears, then shows the newly requested tail.

Expected: the strings above and no duplicated or missing lines. The spec accepts this as the check for the page script; no automated test runs it.

- [ ] **Step 7: Commit**

```bash
git add internal/server/static/logstream.js internal/server/templates internal/server/logstream_page_test.go
git commit -m "fix(server): a reconnecting log page clears and replays instead of showing lines twice" -m "A reconnect duplicated the whole log, and a non-stream reply left the page disconnected forever. Clear and replay on open, and retry with backoff."
```

---

## Task 4: Docs

**Files:**
- Modify: `docs/BEHIND-A-PROXY.md`, `AGENTS.md`

- [ ] **Step 1: Write the guide**

In `docs/BEHIND-A-PROXY.md` if it is present in this tree, add "Streams". The proxy-ingress branch already has that file, so **do not create it from scratch here**: if it is absent, put the same material in a new `docs/STREAMS.md` and add a one-line pointer to it from `AGENTS.md`. When the proxy-ingress branch merges, move the sections into `BEHIND-A-PROXY.md` and append the real-host items to its list.

- All three log views are SSE; every stream sends `: ping` and flushes every 20 seconds, so a quiet build or container keeps the connection through a CDN.
- A reconnect clears the page and replays: build logs replay what the broker holds (at most 20,000 lines), runtime logs replay the chosen tail (100 to 5,000 lines). No line is shown twice; no event ids exist.
- A page whose stream breaks retries for five minutes with a 2-second to 30-second backoff, then says `disconnected. Reload the page to try again.` A `done` or `err` event stops it.
- A build log older than the broker's memory (or from before a restart) gets an immediate `done`; the page says the log is no longer kept.

Add "WebSockets": Outhaul's own pages use none; an app's WebSockets pass through Traefik, and a CDN that forwards WebSockets needs nothing from Outhaul.

Add these as items 4–6 of the real-host list: in `BEHIND-A-PROXY.md` under its existing heading, or in `STREAMS.md` under a new "What only the real host can show". Items 1–3 belong to the proxy-ingress document. The three checks: a build step that prints nothing for four minutes keeps one `/logs` request and stays `streaming…`; restarting Outhaul with a build log and a runtime log open shows `reconnecting…`, then the log once; an app that uses a WebSocket works through the CDN.

- [ ] **Step 2: Write the kept decision**

In `AGENTS.md`, add a "Decisions to keep" bullet:

```
- **Streams.** Every SSE log stream sends `: ping` every 20 seconds. A
  reconnect clears the page and replays from the start, so no line appears
  twice and no event ids are needed. The pages retry a broken stream for five
  minutes, then ask for a reload. Build logs stay in memory only.
```

- [ ] **Step 3: Verify**

Run: `grep -n "ping\|WebSocket" docs/BEHIND-A-PROXY.md docs/STREAMS.md AGENTS.md 2>/dev/null`

Expected: the streams section (in one of the two docs) and the kept decision are present.

Run: `go build ./... && go vet ./... && go test ./...`

Expected: PASS.

Run: `go test -race -timeout 30m ./...`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add docs AGENTS.md
git commit -m "docs(streams): pings, reconnect and replay, and WebSockets through a CDN" -m "Stream pings, reconnect and WebSockets were undocumented. Add the guide and the kept decision."
```
