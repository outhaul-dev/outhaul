// Package blobstore talks to S3-compatible object storage (AWS S3, MinIO,
// Cloudflare R2, Backblaze B2, Wasabi, …) with exactly the surface backups
// and restores need: put, get, list, delete. Requests are SigV4-signed with
// the standard library — no SDK — the same house call as the hand-rolled
// GitHub App JWT.
// Addressing is always path-style (endpoint/bucket/key), which every
// S3-compatible accepts. Uploads are a single PUT, so one object is capped at
// S3's 5 GB single-PUT limit; multipart is a deliberate seam.
package blobstore

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Object is a stored object's key and size.
type Object struct {
	Key  string
	Size int64
}

// Client is the object-storage surface backups depend on.
type Client interface {
	// Put uploads size bytes from r as key.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get streams key's content; the caller must close the reader. A missing
	// key is an error (unlike Delete, there is nothing sensible to do without
	// the bytes).
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns all objects under prefix (paginated internally), sorted by
	// key ascending.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Delete removes key (no error if it does not exist, per S3 semantics).
	Delete(ctx context.Context, key string) error
}

// Open returns a Client for an S3-compatible destination.
func Open(d core.Destination) (Client, error) {
	u, err := url.Parse(d.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("blobstore: endpoint %q is not an http(s) URL", d.Endpoint)
	}
	if d.Bucket == "" {
		return nil, fmt.Errorf("blobstore: destination %q has no bucket", d.Name)
	}
	region := d.Region
	if region == "" {
		region = "us-east-1" // SigV4 needs a region string; this is the S3-compatible default
	}
	return &s3{
		endpoint:  strings.TrimSuffix(u.String(), "/"),
		host:      u.Host,
		bucket:    d.Bucket,
		region:    region,
		accessKey: d.AccessKey,
		secretKey: d.SecretKey,
		http:      &http.Client{Timeout: 15 * time.Minute}, // large uploads take a while
		now:       time.Now,
	}, nil
}

// Probe verifies a destination is writable: put a small object, delete it.
func Probe(ctx context.Context, c Client) error {
	const key = ".outhaul-probe"
	body := "outhaul destination test"
	if err := c.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
		return err
	}
	return c.Delete(ctx, key)
}

type s3 struct {
	endpoint  string // scheme://host[:port][/base] without trailing slash
	host      string
	bucket    string
	region    string
	accessKey string
	secretKey string
	http      *http.Client
	now       func() time.Time
}

func (s *s3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	req, err := s.newRequest(ctx, http.MethodPut, key, nil, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	resp.Body.Close()
	return nil
}

func (s *s3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	return resp.Body, nil
}

func (s *s3) Delete(ctx context.Context, key string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	resp.Body.Close()
	return nil
}

// listResult is the ListObjectsV2 response envelope.
type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (s *s3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.do(req)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		var page listResult
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: decode: %w", prefix, err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// newRequest builds a signed path-style request for a key ("" = the bucket).
func (s *s3) newRequest(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	path := "/" + s.bucket
	if key != "" {
		path += "/" + key
	}
	u := s.endpoint + uriEncode(path, true)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	// Streaming uploads are signed UNSIGNED-PAYLOAD (standard for non-chunked
	// SigV4 over TLS) so the body never needs hashing in advance.
	payloadHash := emptyPayloadHash
	if body != nil {
		payloadHash = unsignedPayload
	}
	sign(req, s.accessKey, s.secretKey, s.region, payloadHash, s.now().UTC())
	return req, nil
}

// do executes the request and turns non-2xx responses into errors carrying
// the S3 error body.
func (s *s3) do(req *http.Request) (*http.Response, error) {
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet)))
}
