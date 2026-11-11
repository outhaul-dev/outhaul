package blobstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubTokens is a TokenSource whose value the test controls.
type stubTokens struct {
	mu        sync.Mutex
	token     string
	refreshTo string
	refreshes int
	err       error
}

func (s *stubTokens) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.err
}

func (s *stubTokens) Refresh(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	if s.refreshTo != "" {
		s.token = s.refreshTo
	}
	return s.token, s.err
}

type recReq struct {
	method   string
	path     string
	rawQuery string
	header   http.Header
}

// fakeAzure is an httptest-backed Azure Blob endpoint. Storage behaviour is the
// default; a test can install respond to answer (and read the body) itself.
type fakeAzure struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	objects map[string][]byte
	blocks  map[string]map[string][]byte
	commits int
	body    []byte
	reqs    []recReq

	respond func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeAzure(t *testing.T) *fakeAzure {
	t.Helper()
	f := &fakeAzure{t: t, objects: map[string][]byte{}, blocks: map[string]map[string][]byte{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAzure) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, recReq{method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, header: r.Header.Clone()})
	f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("x-ms-version") != azureVersion || r.Header.Get("x-ms-date") == "" {
		f.t.Errorf("missing version/date: %q %q", r.Header.Get("x-ms-version"), r.Header.Get("x-ms-date"))
	}
	if f.respond != nil && f.respond(w, r) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	f.mu.Lock()
	f.body = body
	f.mu.Unlock()
	f.serveStorage(w, r, body)
}

func (f *fakeAzure) serveStorage(w http.ResponseWriter, r *http.Request, body []byte) {
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/outhaul/")
	if r.URL.Path == "/outhaul" {
		key = ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/outhaul" && q.Get("comp") == "list":
		var names []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("<EnumerationResults><Blobs>")
		for _, n := range names {
			fmt.Fprintf(&b, "<Blob><Name>%s</Name><Properties><Content-Length>%d</Content-Length></Properties></Blob>",
				xmlEscape(n), len(f.objects[n]))
		}
		b.WriteString("</Blobs><NextMarker></NextMarker></EnumerationResults>")
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, b.String())
	case r.Method == http.MethodPut && q.Get("comp") == "block":
		if f.blocks[key] == nil {
			f.blocks[key] = map[string][]byte{}
		}
		f.blocks[key][q.Get("blockid")] = append([]byte(nil), body...)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && q.Get("comp") == "blocklist":
		var list struct {
			Latest []string `xml:"Latest"`
		}
		if err := xml.Unmarshal(body, &list); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var out []byte
		for _, id := range list.Latest {
			out = append(out, f.blocks[key][id]...)
		}
		f.objects[key] = out
		delete(f.blocks, key)
		f.commits++
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut:
		f.objects[key] = append([]byte(nil), body...)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.Header().Set("x-ms-error-code", "BlobNotFound")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(b)
	case r.Method == http.MethodDelete:
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeAzure) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func (f *fakeAzure) requests() []recReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recReq(nil), f.reqs...)
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// newTestAzure builds a client against f, with retry sleeps removed.
func newTestAzure(t *testing.T, f *fakeAzure, tokens TokenSource) *azure {
	t.Helper()
	c, err := NewAzureClient(f.srv.URL, "outhaul", tokens)
	if err != nil {
		t.Fatal(err)
	}
	a := c.(*azure)
	a.retryDelay = 0
	a.stall = 50 * time.Millisecond
	return a
}

func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%010d", i)))
}

func TestAzurePutBoundaries(t *testing.T) {
	ctx := context.Background()
	f := newFakeAzure(t)
	var blockIDs []string
	var small, commits int
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		q := r.URL.Query()
		io.Copy(io.Discard, r.Body)
		switch q.Get("comp") {
		case "block":
			blockIDs = append(blockIDs, q.Get("blockid"))
			w.WriteHeader(201)
			return true
		case "blocklist":
			commits++
			w.WriteHeader(201)
			return true
		default:
			small++
			if got := r.Header.Get("x-ms-blob-type"); got != "BlockBlob" {
				t.Errorf("x-ms-blob-type = %q, want BlockBlob", got)
			}
			w.WriteHeader(201)
			return true
		}
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})

	exact := int64(8 << 20)
	if err := c.Put(ctx, "one", bytes.NewReader(make([]byte, exact)), exact); err != nil {
		t.Fatalf("8 MiB Put: %v", err)
	}
	if small != 1 || len(blockIDs) != 0 || commits != 0 {
		t.Fatalf("8 MiB: small=%d blocks=%d commits=%d, want 1/0/0", small, len(blockIDs), commits)
	}

	over := exact + 1
	if err := c.Put(ctx, "two", bytes.NewReader(make([]byte, over)), over); err != nil {
		t.Fatalf("8 MiB + 1 Put: %v", err)
	}
	if len(blockIDs) != 2 || commits != 1 {
		t.Fatalf("8 MiB + 1: blocks=%d commits=%d, want 2/1", len(blockIDs), commits)
	}
	if blockIDs[0] != blockID(0) || blockIDs[1] != blockID(1) {
		t.Errorf("block IDs = %q, want %q and %q", blockIDs, blockID(0), blockID(1))
	}

	requestsBefore := len(f.requests())
	huge := int64(azureMaxBlocks)*azureBlockSize + 1
	if err := c.Put(ctx, "big", strings.NewReader(""), huge); err == nil || !strings.Contains(err.Error(), "390 GiB") {
		t.Fatalf("oversize Put = %v, want an error naming the 390 GiB limit", err)
	}
	if got := len(f.requests()); got != requestsBefore {
		t.Errorf("oversize Put made %d requests, want none", got-requestsBefore)
	}
}

func TestAzureBlockListBody(t *testing.T) {
	ctx := context.Background()
	f := newFakeAzure(t)
	var listBody []byte
	var listType string
	sawUncommitted := false
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		if r.Method != http.MethodPut {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		switch q.Get("comp") {
		case "block":
			if _, ok := f.object("wide"); ok {
				t.Error("blob visible before its block list committed")
			}
			sawUncommitted = true
			f.mu.Lock()
			if f.blocks["wide"] == nil {
				f.blocks["wide"] = map[string][]byte{}
			}
			f.blocks["wide"][q.Get("blockid")] = body
			f.mu.Unlock()
			w.WriteHeader(201)
			return true
		case "blocklist":
			listBody = body
			listType = r.Header.Get("Content-Type")
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(body, &list); err != nil {
				t.Errorf("block list body: %v", err)
			}
			if len(list.Latest) != 2 || list.Latest[0] != blockID(0) || list.Latest[1] != blockID(1) {
				t.Errorf("Latest = %q", list.Latest)
			}
			f.mu.Lock()
			var assembled []byte
			for _, id := range list.Latest {
				assembled = append(assembled, f.blocks["wide"][id]...)
			}
			f.objects["wide"] = assembled
			f.mu.Unlock()
			w.WriteHeader(201)
			return true
		}
		return false
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	payload := bytes.Repeat([]byte("x"), 8<<20+10)
	if err := c.Put(ctx, "wide", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if !sawUncommitted {
		t.Error("never saw a block upload")
	}
	if !strings.HasPrefix(string(listBody), "<BlockList>") || !strings.Contains(string(listBody), "<Latest>"+blockID(0)+"</Latest>") {
		t.Errorf("block list body = %q", listBody)
	}
	if listType != "application/xml" {
		t.Errorf("Content-Type = %q, want application/xml", listType)
	}
	got, _ := f.object("wide")
	if !bytes.Equal(got, payload) {
		t.Errorf("stored %d bytes, want %d", len(got), len(payload))
	}
}

func TestAzureListPaginatesSortsAndDecodes(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("marker") == "" {
			io.WriteString(w, `<EnumerationResults><Blobs>
				<Blob><Name>b</Name><Properties><Content-Length>3</Content-Length></Properties></Blob>
				<Blob><Name Encoded="true">a%20b</Name><Properties><Content-Length>5</Content-Length></Properties></Blob>
			</Blobs><NextMarker>m2</NextMarker></EnumerationResults>`)
			return true
		}
		io.WriteString(w, `<EnumerationResults><Blobs>
			<Blob><Name>a</Name><Properties><Content-Length>1</Content-Length></Properties></Blob>
		</Blobs><NextMarker></NextMarker></EnumerationResults>`)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	got, err := c.List(context.Background(), "p/")
	if err != nil {
		t.Fatal(err)
	}
	want := []Object{{Key: "a", Size: 1}, {Key: "a b", Size: 5}, {Key: "b", Size: 3}}
	if len(got) != len(want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAzureGetMissingIsAnError(t *testing.T) {
	c := newTestAzure(t, newFakeAzure(t), &stubTokens{token: "tok"})
	if _, err := c.Get(context.Background(), "nope"); err == nil {
		t.Fatal("Get of a missing blob succeeded, want an error")
	}
}

func TestAzureDelete404CountsAsDone(t *testing.T) {
	c := newTestAzure(t, newFakeAzure(t), &stubTokens{token: "tok"})
	if err := c.Delete(context.Background(), "nope"); err != nil {
		t.Fatalf("Delete of a missing blob = %v, want nil", err)
	}
}

func TestAzure401RefreshesOnce(t *testing.T) {
	f := newFakeAzure(t)
	var calls int
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		calls++
		io.Copy(io.Discard, r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		w.WriteHeader(http.StatusCreated)
		return true
	}
	tokens := &stubTokens{token: "tok-1", refreshTo: "tok-2"}
	c := newTestAzure(t, f, tokens)
	if err := c.Put(context.Background(), "k", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer tok-1" {
		t.Errorf("first bearer = %q", got)
	}
	if got := reqs[1].header.Get("Authorization"); got != "Bearer tok-2" {
		t.Errorf("second bearer = %q, want the refreshed token", got)
	}
	if tokens.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", tokens.refreshes)
	}
}

func TestAzureSecond401NamesTheRefusal(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "the storage account refused the token") {
		t.Fatalf("error = %v, want the refusal text", err)
	}
	if got := len(f.requests()); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestAzureRetries408429And5xx(t *testing.T) {
	for _, status := range []int{408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFakeAzure(t)
			var calls int
			f.respond = func(w http.ResponseWriter, r *http.Request) bool {
				calls++
				io.Copy(io.Discard, r.Body)
				if calls < 3 {
					w.WriteHeader(status)
					return true
				}
				w.WriteHeader(http.StatusCreated)
				return true
			}
			c := newTestAzure(t, f, &stubTokens{token: "tok"})
			if err := c.Put(context.Background(), "k", strings.NewReader("x"), 1); err != nil {
				t.Fatalf("Put after two %d answers = %v, want success", status, err)
			}
			if calls != 3 {
				t.Errorf("calls = %d, want 3", calls)
			}
		})
	}
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	if err := c.Put(context.Background(), "k", strings.NewReader("x"), 1); err == nil {
		t.Fatal("Put with three 500s succeeded, want an error")
	}
	if got := len(f.requests()); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
}

func TestAzureBlockRetryAnd401(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xAB}, 8<<20+1024)

	t.Run("retry", func(t *testing.T) {
		f := newFakeAzure(t)
		var ids []string
		failed := false
		f.respond = func(w http.ResponseWriter, r *http.Request) bool {
			q := r.URL.Query()
			if r.Method != http.MethodPut {
				return false
			}
			body, _ := io.ReadAll(r.Body)
			switch q.Get("comp") {
			case "block":
				ids = append(ids, q.Get("blockid"))
				if q.Get("blockid") == blockID(1) && !failed {
					failed = true
					w.WriteHeader(500)
					return true
				}
				f.mu.Lock()
				if f.blocks["k"] == nil {
					f.blocks["k"] = map[string][]byte{}
				}
				f.blocks["k"][q.Get("blockid")] = body
				f.mu.Unlock()
				w.WriteHeader(201)
				return true
			case "blocklist":
				var list struct {
					Latest []string `xml:"Latest"`
				}
				if err := xml.Unmarshal(body, &list); err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				var out []byte
				for _, id := range list.Latest {
					out = append(out, f.blocks["k"][id]...)
				}
				f.objects["k"] = out
				f.commits++
				f.mu.Unlock()
				w.WriteHeader(201)
				return true
			}
			return false
		}
		c := newTestAzure(t, f, &stubTokens{token: "tok"})
		if err := c.Put(ctx, "k", bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		want := []string{blockID(0), blockID(1), blockID(1)}
		if len(ids) != len(want) {
			t.Fatalf("block requests = %q, want %q", ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Errorf("block request %d = %q, want %q", i, ids[i], want[i])
			}
		}
		if f.commits != 1 {
			t.Errorf("commits = %d, want 1", f.commits)
		}
		if got, _ := f.object("k"); !bytes.Equal(got, payload) {
			t.Errorf("stored bytes differ from input")
		}
	})

	t.Run("401", func(t *testing.T) {
		f := newFakeAzure(t)
		var ids []string
		failed := false
		f.respond = func(w http.ResponseWriter, r *http.Request) bool {
			q := r.URL.Query()
			if r.Method != http.MethodPut {
				return false
			}
			body, _ := io.ReadAll(r.Body)
			switch q.Get("comp") {
			case "block":
				ids = append(ids, q.Get("blockid"))
				if q.Get("blockid") == blockID(0) && !failed {
					failed = true
					w.WriteHeader(http.StatusUnauthorized)
					return true
				}
				f.mu.Lock()
				if f.blocks["k"] == nil {
					f.blocks["k"] = map[string][]byte{}
				}
				f.blocks["k"][q.Get("blockid")] = body
				f.mu.Unlock()
				w.WriteHeader(201)
				return true
			case "blocklist":
				var list struct {
					Latest []string `xml:"Latest"`
				}
				if err := xml.Unmarshal(body, &list); err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				var out []byte
				for _, id := range list.Latest {
					out = append(out, f.blocks["k"][id]...)
				}
				f.objects["k"] = out
				f.commits++
				f.mu.Unlock()
				w.WriteHeader(201)
				return true
			}
			return false
		}
		tokens := &stubTokens{token: "tok-1", refreshTo: "tok-2"}
		c := newTestAzure(t, f, tokens)
		if err := c.Put(ctx, "k", bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		want := []string{blockID(0), blockID(0), blockID(1)}
		if len(ids) != len(want) {
			t.Fatalf("block requests = %q, want %q", ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Errorf("block request %d = %q, want %q", i, ids[i], want[i])
			}
		}
		if tokens.refreshes != 1 {
			t.Errorf("refreshes = %d, want 1", tokens.refreshes)
		}
		reqs := f.requests()
		if got := reqs[1].header.Get("Authorization"); got != "Bearer tok-2" {
			t.Errorf("second block bearer = %q, want tok-2", got)
		}
		if f.commits != 1 {
			t.Errorf("commits = %d, want 1", f.commits)
		}
		if got, _ := f.object("k"); !bytes.Equal(got, payload) {
			t.Errorf("stored bytes differ from input")
		}
	})
}

func TestAzure403NamesTheRoleAndFirewall(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AuthorizationPermissionMismatch</Code></Error>`)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "Storage Blob Data Contributor") || !strings.Contains(err.Error(), "firewall") {
		t.Fatalf("error = %v, want the role and firewall", err)
	}
}

func TestAzure404ContainerNotFound(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("x-ms-error-code", "ContainerNotFound")
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil || !strings.Contains(err.Error(), "container doesn't exist") {
		t.Fatalf("error = %v, want the container message", err)
	}
}

func TestAzureStalledDownloadEnds(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	rc, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get headers: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a stalled body read succeeded, want a read error")
	}
}

func TestAzureGetHeaderStallEnds(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	c.stall = 30 * time.Millisecond
	start := time.Now()
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Fatal("Get with no response headers succeeded, want an error")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("header stall took %v, want about the stall duration", elapsed)
	}
}

func TestAzureSlowUploadIsNotCutOff(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		if r.Method != http.MethodPut {
			return false
		}
		if q.Get("comp") == "blocklist" {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(201)
			return true
		}
		if q.Get("comp") != "block" {
			return false
		}
		buf := make([]byte, 1<<20)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				time.Sleep(20 * time.Millisecond)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read block: %v", err)
				break
			}
		}
		w.WriteHeader(201)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	payload := bytes.Repeat([]byte("z"), 8<<20+1)
	if err := c.Put(context.Background(), "k", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("slow upload = %v, want success", err)
	}
}

func TestAzureNoTokenInErrors(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
		io.WriteString(w, `<Error><Code>super-secret-token</Code></Error>`)
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "super-secret-token"})
	err := c.Put(context.Background(), "k", strings.NewReader("x"), 1)
	if err == nil {
		t.Fatal("Put succeeded, want an error")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestAzureRequestShape(t *testing.T) {
	f := newFakeAzure(t)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		io.Copy(io.Discard, r.Body)
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			io.WriteString(w, "<EnumerationResults></EnumerationResults>")
		}
		return true
	}
	c := newTestAzure(t, f, &stubTokens{token: "tok"})
	ctx := context.Background()

	if err := c.Put(ctx, "dir/a b+%?.txt", strings.NewReader("hi"), 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "dir/a b+%?.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx, "p/q "); err != nil {
		t.Fatal(err)
	}

	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	if got, want := reqs[0].path, "/outhaul/dir/a%20b%2B%25%3F.txt"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	h := reqs[0].header
	if h.Get("Authorization") != "Bearer tok" || h.Get("x-ms-version") != azureVersion || h.Get("x-ms-blob-type") != "BlockBlob" {
		t.Errorf("headers = %v", h)
	}
	if _, err := http.ParseTime(h.Get("x-ms-date")); err != nil {
		t.Errorf("x-ms-date = %q: %v", h.Get("x-ms-date"), err)
	}
	vals, err := url.ParseQuery(reqs[2].rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("restype") != "container" || vals.Get("comp") != "list" || vals.Get("prefix") != "p/q " {
		t.Errorf("list query = %v", vals)
	}
	if reqs[1].method != http.MethodDelete || reqs[1].path != "/outhaul/dir/a%20b%2B%25%3F.txt" {
		t.Errorf("delete request = %s %s", reqs[1].method, reqs[1].path)
	}
}
