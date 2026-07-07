package blobstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// TestSigV4TestVector checks the signer against the worked example in AWS's
// "Authenticating Requests (AWS Signature Version 4)" documentation: a GET of
// /test.txt from examplebucket with a Range header.
func TestSigV4TestVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	at := time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC)

	sign(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", emptyPayloadHash, at)

	const want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"simple-key_1.tar.gz", false, "simple-key_1.tar.gz"},
		{"a b", false, "a%20b"},
		{"a/b", false, "a%2Fb"},
		{"/bucket/a/b", true, "/bucket/a/b"},
		{"pre fix/x", true, "pre%20fix/x"},
		{"tilde~ok", false, "tilde~ok"},
		{"plus+amp&", false, "plus%2Bamp%26"},
	}
	for _, tc := range cases {
		if got := uriEncode(tc.in, tc.keepSlash); got != tc.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tc.in, tc.keepSlash, got, tc.want)
		}
	}
}

// fakeS3 is an httptest-backed S3 endpoint storing objects in memory.
type fakeS3 struct {
	t       *testing.T
	objects map[string][]byte // key (without bucket) -> content
}

func (f *fakeS3) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKID/") ||
			!strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") ||
			!strings.Contains(auth, "Signature=") {
			f.t.Errorf("bad Authorization header: %q", auth)
			http.Error(w, "bad auth", http.StatusForbidden)
			return
		}
		if r.Header.Get("x-amz-date") == "" || r.Header.Get("x-amz-content-sha256") == "" {
			f.t.Error("missing x-amz-date / x-amz-content-sha256")
		}
		key, ok := strings.CutPrefix(r.URL.Path, "/testbucket")
		if !ok {
			http.Error(w, "wrong bucket path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		key = strings.TrimPrefix(key, "/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			f.objects[key] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				f.list(w, r)
				return
			}
			b, ok := f.objects[key]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
				return
			}
			w.Write(b)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}
}

// list implements just enough ListObjectsV2: prefix filter and one-object
// pages so pagination is exercised.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("list-type") != "2" {
		http.Error(w, "want list-type=2", http.StatusBadRequest)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// Deterministic order for paging.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	start := 0
	if tok := r.URL.Query().Get("continuation-token"); tok != "" {
		fmt.Sscanf(tok, "%d", &start)
	}
	type content struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	res := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}{}
	if start < len(keys) {
		res.Contents = []content{{Key: keys[start], Size: len(f.objects[keys[start]])}}
	}
	if start+1 < len(keys) {
		res.IsTruncated = true
		res.NextContinuationToken = fmt.Sprintf("%d", start+1)
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(res)
}

func newTestClient(t *testing.T) (Client, *fakeS3) {
	t.Helper()
	f := &fakeS3{t: t, objects: map[string][]byte{}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	c, err := Open(core.Destination{
		Name: "test", Endpoint: ts.URL, Bucket: "testbucket",
		AccessKey: "AKID", SecretKey: "SECRET",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c, f
}

func TestPutListDelete(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()

	for _, key := range []string{"pre/db/2.gz", "pre/db/1.gz", "pre/db/3.gz", "other/x.gz"} {
		if err := c.Put(ctx, key, strings.NewReader("data-"+key), int64(len("data-"+key))); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	if string(f.objects["pre/db/1.gz"]) != "data-pre/db/1.gz" {
		t.Errorf("stored content = %q", f.objects["pre/db/1.gz"])
	}

	got, err := c.List(ctx, "pre/db/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 || got[0].Key != "pre/db/1.gz" || got[2].Key != "pre/db/3.gz" {
		t.Fatalf("List = %+v, want the three pre/db objects in order", got)
	}
	if got[0].Size != int64(len("data-pre/db/1.gz")) {
		t.Errorf("size = %d", got[0].Size)
	}

	if err := c.Delete(ctx, "pre/db/1.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := f.objects["pre/db/1.gz"]; ok {
		t.Error("object survived Delete")
	}
}

func TestGetStreamsObject(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	f.objects["pre/db/1.gz"] = []byte("dump-bytes")

	rc, err := c.Get(ctx, "pre/db/1.gz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "dump-bytes" {
		t.Errorf("Get body = %q", got)
	}

	if _, err := c.Get(ctx, "does/not/exist"); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("missing key: err = %v, want the S3 NoSuchKey error surfaced", err)
	}
}

func TestProbeWritesAndCleansUp(t *testing.T) {
	c, f := newTestClient(t)
	if err := Probe(context.Background(), c); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(f.objects) != 0 {
		t.Errorf("probe left objects behind: %v", f.objects)
	}
}

func TestOpenRejectsBadEndpoints(t *testing.T) {
	bad := []core.Destination{
		{Endpoint: "not-a-url", Bucket: "b"},
		{Endpoint: "ftp://host/", Bucket: "b"},
		{Endpoint: "https://host", Bucket: ""},
	}
	for _, d := range bad {
		if _, err := Open(d); err == nil {
			t.Errorf("Open(%+v) accepted, want error", d)
		}
	}
}

func TestErrorsCarryS3Body(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
	}))
	defer ts.Close()
	c, err := Open(core.Destination{Endpoint: ts.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Delete(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want the S3 error body surfaced", err)
	}
}
