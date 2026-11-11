package blobstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// azuriteJWT is the unsigned token Azurite's --oauth basic accepts. It is not
// a credential: Azurite checks the shape of the claims, not a signature.
func azuriteJWT() string {
	now := time.Now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"aud":"https://storage.azure.com","iss":"https://sts.windows.net/example-tenant/","iat":%d,"nbf":%d,"exp":%d}`,
		now, now, now+3600)))
	return header + "." + payload + "."
}

// TestAzureAzurite drives the real client against Azurite. Run it through
// deploy/test/integration_azure.sh, which starts the container with a
// throwaway certificate and creates the container; it is skipped otherwise.
func TestAzureAzurite(t *testing.T) {
	endpoint := os.Getenv("OUTHAUL_AZURE_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OUTHAUL_AZURE_TEST_ENDPOINT to an Azurite account URL")
	}
	c, err := NewAzureClient(endpoint, "outhaul", &stubTokens{token: azuriteJWT()})
	if err != nil {
		t.Fatal(err)
	}
	// The script's certificate is throwaway and issued for 127.0.0.1.
	az := c.(*azure)
	tr, ok := az.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", az.http.Transport)
	}
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec

	ctx := context.Background()
	prefix := fmt.Sprintf("test-%d", time.Now().UnixNano())
	small := prefix + "/small.txt"
	large := prefix + "/large.bin"
	smallBody := []byte("outhaul azurite check")
	largeBody := bytes.Repeat([]byte("azure-block-"), (9<<20)/12+1)[:9<<20]

	if err := c.Put(ctx, small, bytes.NewReader(smallBody), int64(len(smallBody))); err != nil {
		t.Fatalf("put small: %v", err)
	}
	if err := c.Put(ctx, large, bytes.NewReader(largeBody), int64(len(largeBody))); err != nil {
		t.Fatalf("put large: %v", err)
	}

	got, err := c.Get(ctx, small)
	if err != nil {
		t.Fatalf("get small: %v", err)
	}
	b, err := io.ReadAll(got)
	got.Close()
	if err != nil || !bytes.Equal(b, smallBody) {
		t.Fatalf("small round-trip = %q, %v", b, err)
	}
	got, err = c.Get(ctx, large)
	if err != nil {
		t.Fatalf("get large: %v", err)
	}
	b, err = io.ReadAll(got)
	got.Close()
	if err != nil || !bytes.Equal(b, largeBody) {
		t.Fatalf("large round-trip: %d bytes, %v", len(b), err)
	}

	objects, err := c.List(ctx, prefix+"/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	if len(objects) != 2 || keys[0] != large || keys[1] != small {
		t.Fatalf("list = %v, want [%s %s]", keys, large, small)
	}

	if err := c.Delete(ctx, small); err != nil {
		t.Fatalf("delete small: %v", err)
	}
	objects, err = c.List(ctx, prefix+"/")
	if err != nil || len(objects) != 1 || objects[0].Key != large {
		t.Fatalf("after delete, list = %v (err %v)", objects, err)
	}
	if _, err := c.Get(ctx, small); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("get deleted = %v, want an error naming 404", err)
	}
	if err := c.Delete(ctx, large); err != nil {
		t.Fatalf("delete large: %v", err)
	}
}
