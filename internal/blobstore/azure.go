package blobstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Azure Blob limits and the API version every deployed account accepts.
const (
	azureBlockSize = 8 << 20
	azureMaxBlocks = 50000
	azureVersion   = "2023-11-03"

	azureListTimeout   = 30 * time.Second
	azureDeleteTimeout = 30 * time.Second
	azureCommitTimeout = 60 * time.Second
	azurePutTimeout    = 5 * time.Minute
	azureMaxRetries    = 2
)

// azure is an Azure Blob Storage client. It holds one token source for the
// process; every request asks it for a token, so a long run picks up a
// refreshed one between blocks.
type azure struct {
	endpoint   string
	container  string
	tokens     TokenSource
	http       *http.Client
	now        func() time.Time
	stall      time.Duration
	retryDelay time.Duration
}

// NewAzureClient returns a Client for endpoint (scheme://host[:port][/account])
// and container. It sets no http.Client.Timeout; each call has its own deadline.
func NewAzureClient(endpoint, container string, tokens TokenSource) (Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("blobstore: endpoint %q is not an http(s) URL", endpoint)
	}
	if container == "" {
		return nil, errors.New("blobstore: Azure container is empty")
	}
	if tokens == nil {
		return nil, errors.New("blobstore: Azure client needs a token source")
	}
	return &azure{
		endpoint:  strings.TrimSuffix(u.String(), "/"),
		container: container,
		tokens:    tokens,
		http: &http.Client{Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		}},
		now:        time.Now,
		stall:      60 * time.Second,
		retryDelay: 100 * time.Millisecond,
	}, nil
}

func (a *azure) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size > int64(azureMaxBlocks)*azureBlockSize {
		return fmt.Errorf("put %s: %d bytes is more than the 390 GiB the block list can hold", key, size)
	}
	if size <= azureBlockSize {
		buf, err := io.ReadAll(io.LimitReader(r, size+1))
		if err != nil {
			return fmt.Errorf("put %s: read: %w", key, err)
		}
		if int64(len(buf)) != size {
			return fmt.Errorf("put %s: body has %d bytes, want %d", key, len(buf), size)
		}
		resp, cancel, err := a.do(ctx, http.MethodPut, key, nil, buf, "", azurePutTimeout,
			map[string]string{"x-ms-blob-type": "BlockBlob"})
		if err != nil {
			return err
		}
		defer cancel()
		if resp.StatusCode != http.StatusCreated {
			return a.statusError("put "+key, resp)
		}
		resp.Body.Close()
		return nil
	}

	blocks := (size + azureBlockSize - 1) / azureBlockSize
	buf := make([]byte, azureBlockSize)
	ids := make([]string, 0, blocks)
	for i := int64(0); i < blocks; i++ {
		n := int64(azureBlockSize)
		if remaining := size - i*azureBlockSize; remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return fmt.Errorf("put %s: block %d: %w", key, i, err)
		}
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%010d", i)))
		ids = append(ids, id)
		q := url.Values{"comp": {"block"}, "blockid": {id}}
		resp, cancel, err := a.do(ctx, http.MethodPut, key, q, buf[:n], "", azurePutTimeout, nil)
		if err != nil {
			return fmt.Errorf("put %s: block %d: %w", key, i, err)
		}
		if resp.StatusCode != http.StatusCreated {
			err := a.statusError(fmt.Sprintf("put %s block %d", key, i), resp)
			cancel()
			return err
		}
		resp.Body.Close()
		cancel()
	}

	var list strings.Builder
	list.WriteString("<BlockList>")
	for _, id := range ids {
		list.WriteString("<Latest>")
		list.WriteString(id)
		list.WriteString("</Latest>")
	}
	list.WriteString("</BlockList>")
	q := url.Values{"comp": {"blocklist"}}
	resp, cancel, err := a.do(ctx, http.MethodPut, key, q, []byte(list.String()), "application/xml", azureCommitTimeout, nil)
	if err != nil {
		return err
	}
	defer cancel()
	if resp.StatusCode != http.StatusCreated {
		return a.statusError("put "+key+" block list", resp)
	}
	resp.Body.Close()
	return nil
}

func (a *azure) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	reqCtx, cancelAll := context.WithCancel(ctx)
	timer := time.AfterFunc(a.stall, cancelAll)
	resp, cancel, err := a.do(reqCtx, http.MethodGet, key, nil, nil, "", 0, nil)
	timer.Stop()
	if err != nil {
		cancelAll()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if reqCtx.Err() == context.Canceled {
			return nil, fmt.Errorf("get %s: no answer from the storage account for %s", key, a.stall)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		err := a.statusError("get "+key, resp)
		cancel()
		cancelAll()
		return nil, err
	}
	return &azureBody{
		r:      &stallReader{r: resp.Body, stall: a.stall},
		c:      resp.Body,
		cancel: func() { cancel(); cancelAll() },
	}, nil
}

func (a *azure) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	marker := ""
	for {
		q := url.Values{"restype": {"container"}, "comp": {"list"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, cancel, err := a.do(ctx, http.MethodGet, "", q, nil, "", azureListTimeout, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := a.statusError("list "+prefix, resp)
			cancel()
			return nil, err
		}
		var page struct {
			Blobs []struct {
				Name struct {
					Value   string `xml:",chardata"`
					Encoded string `xml:"Encoded,attr"`
				} `xml:"Name"`
				Encoded string `xml:"Encoded,attr"`
				Props   struct {
					ContentLength int64 `xml:"Content-Length"`
				} `xml:"Properties"`
			} `xml:"Blobs>Blob"`
			NextMarker string `xml:"NextMarker"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("list %s: decode: %w", prefix, err)
		}
		for _, b := range page.Blobs {
			name := b.Name.Value
			if b.Name.Encoded == "true" || b.Encoded == "true" {
				if decoded, err := url.PathUnescape(name); err == nil {
					name = decoded
				}
			}
			out = append(out, Object{Key: name, Size: b.Props.ContentLength})
		}
		if page.NextMarker == "" {
			break
		}
		marker = page.NextMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (a *azure) Delete(ctx context.Context, key string) error {
	resp, cancel, err := a.do(ctx, http.MethodDelete, key, nil, nil, "", azureDeleteTimeout, nil)
	if err != nil {
		return err
	}
	defer cancel()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil
	}
	return a.statusError("delete "+key, resp)
}

// do sends one logical request, retrying transient failures twice and a 401
// once after refreshing the token. The caller must call cancel after reading
// or closing the response.
func (a *azure) do(ctx context.Context, method, key string, query url.Values, body []byte,
	contentType string, timeout time.Duration, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	token, err := a.tokens.Token(ctx)
	if err != nil {
		return nil, nil, err
	}
	attempts := 0
	refreshed := false
	for {
		reqCtx := ctx
		cancel := context.CancelFunc(func() {})
		if timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, a.urlFor(key, query), bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("x-ms-version", azureVersion)
		req.Header.Set("x-ms-date", a.now().UTC().Format(http.TimeFormat))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}

		resp, err := a.http.Do(req)
		if err != nil {
			cancel()
			if attempts < azureMaxRetries && ctx.Err() == nil {
				attempts++
				if !a.sleep(ctx, attempts) {
					return nil, nil, ctx.Err()
				}
				continue
			}
			return nil, nil, fmt.Errorf("%s %s: %w", method, key, err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drainAndClose(resp)
			cancel()
			if refreshed {
				return nil, nil, errors.New("the storage account refused the token")
			}
			refreshed = true
			token, err = a.tokens.Refresh(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("refresh the storage token: %w", err)
			}
			continue
		}
		if retryableStatus(resp.StatusCode) && attempts < azureMaxRetries && ctx.Err() == nil {
			attempts++
			drainAndClose(resp)
			cancel()
			if !a.sleep(ctx, attempts) {
				return nil, nil, ctx.Err()
			}
			continue
		}
		return resp, cancel, nil
	}
}

func (a *azure) urlFor(key string, query url.Values) string {
	u := a.endpoint + "/" + a.container
	if key != "" {
		u += "/" + uriEncode(key, true)
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (a *azure) sleep(ctx context.Context, attempt int) bool {
	delay := a.retryDelay * time.Duration(1<<uint(attempt-1))
	if delay <= 0 {
		return true
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// validErrorCode accepts the CamelCase identifiers Azure sends, so free text
// from a response body never reaches an error or a log.
func validErrorCode(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func drainAndClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// statusError maps a failed response to the sentence a run or Test shows.
func (a *azure) statusError(prefix string, resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	code := resp.Header.Get("x-ms-error-code")
	if code == "" {
		var e struct {
			Code string `xml:"Code"`
		}
		if xml.Unmarshal(body, &e) == nil {
			code = e.Code
		}
	}
	// Error codes are identifiers. Anything else is free text from the
	// service's body, which must not reach a log: it can echo a token.
	if !validErrorCode(code) {
		code = ""
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%s: the storage account refused the token", prefix)
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s: the identity may not write to this container; it needs the \"Storage Blob Data Contributor\" role on it, and the storage account's firewall must admit this host", prefix)
	case resp.StatusCode == http.StatusNotFound && code == "ContainerNotFound":
		return fmt.Errorf("%s: the container doesn't exist in this account", prefix)
	}
	if code != "" {
		return fmt.Errorf("%s: %s (%s)", prefix, resp.Status, code)
	}
	return fmt.Errorf("%s: %s", prefix, resp.Status)
}

// stallReader ends a read that stops making progress. The body has no overall
// deadline; a whole download is bounded only by the run's context.
type stallReader struct {
	r     io.Reader
	stall time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := s.r.Read(p)
		ch <- result{n, err}
	}()
	t := time.NewTimer(s.stall)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-t.C:
		return 0, fmt.Errorf("no data from the storage account for %s", s.stall)
	}
}

type azureBody struct {
	r      io.Reader
	c      io.Closer
	cancel func()
}

func (b *azureBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *azureBody) Close() error {
	b.cancel()
	return b.c.Close()
}
