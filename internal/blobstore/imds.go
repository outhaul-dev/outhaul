package blobstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Azure Storage accepts a bearer token from the VM's managed identity. The
// instance metadata service (IMDS) issues it; nothing is requested until a
// backup or a Test asks for one.
const (
	imdsTokenURL       = "http://169.254.169.254/metadata/identity/oauth2/token"
	tokenResource      = "https://storage.azure.com/"
	tokenAPIVersion    = "2018-02-01"
	tokenRefreshBefore = 5 * time.Minute
)

// TokenSource supplies and refreshes the bearer token Azure Storage accepts.
type TokenSource interface {
	Token(ctx context.Context) (string, error)   // cached until five minutes before expiry
	Refresh(ctx context.Context) (string, error) // drop the cached token, fetch one
}

// NewTokenSource returns the IMDS-backed source for the host's managed
// identity. clientID is AZURE_CLIENT_ID; "" asks for the VM's only identity.
func NewTokenSource(clientID string) TokenSource {
	return &azureTokenSource{
		clientID: clientID,
		url:      imdsTokenURL,
		http: &http.Client{Transport: &http.Transport{
			// IMDS refuses proxied requests; connect to it directly.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			TLSHandshakeTimeout: 2 * time.Second,
		}},
		now:     time.Now,
		backoff: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 5 * time.Second},
	}
}

type azureTokenSource struct {
	clientID string
	url      string // tests point this at a local server
	http     *http.Client
	now      func() time.Time
	backoff  []time.Duration

	fetchMu sync.Mutex
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (s *azureTokenSource) Token(ctx context.Context) (string, error) {
	if tok, ok := s.cached(); ok {
		return tok, nil
	}
	return s.fresh(ctx)
}

func (s *azureTokenSource) Refresh(ctx context.Context) (string, error) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	s.mu.Lock()
	s.token = ""
	s.expires = time.Time{}
	s.mu.Unlock()
	return s.fetchLocked(ctx)
}

func (s *azureTokenSource) cached() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expires.Add(-tokenRefreshBefore)) {
		return s.token, true
	}
	return "", false
}

func (s *azureTokenSource) fresh(ctx context.Context) (string, error) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if tok, ok := s.cached(); ok {
		return tok, nil
	}
	tok, err := s.fetchLocked(ctx)
	if err != nil {
		// A failed refresh does not throw away a token that still works.
		s.mu.Lock()
		old := s.token
		valid := old != "" && s.now().Before(s.expires)
		s.mu.Unlock()
		if valid {
			return old, nil
		}
		return "", err
	}
	return tok, nil
}

func (s *azureTokenSource) fetchLocked(ctx context.Context) (string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		tok, expires, retry, err := s.fetchOnce(ctx)
		if err == nil {
			s.mu.Lock()
			s.token, s.expires = tok, expires
			s.mu.Unlock()
			return tok, nil
		}
		lastErr = err
		if !retry || attempt >= len(s.backoff) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.backoff[attempt]):
		}
	}
}

type tokenAnswer struct {
	AccessToken string `json:"access_token"`
	ExpiresOn   string `json:"expires_on"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// fetchOnce makes one token request. retry is true for the statuses the
// service documents as transient.
func (s *azureTokenSource) fetchOnce(ctx context.Context) (tok string, expires time.Time, retry bool, err error) {
	q := url.Values{}
	q.Set("api-version", tokenAPIVersion)
	q.Set("resource", tokenResource)
	if s.clientID != "" {
		q.Set("client_id", s.clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"?"+q.Encode(), nil)
	if err != nil {
		return "", time.Time{}, false, err
	}
	req.Header.Set("Metadata", "true")

	resp, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, false, ctx.Err()
		}
		return "", time.Time{}, false, fmt.Errorf(
			"can't reach the instance metadata service at 169.254.169.254; a managed identity works only on an Azure VM that has one assigned: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("read the metadata service answer: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var a tokenAnswer
		_ = json.Unmarshal(body, &a)
		if resp.StatusCode == http.StatusBadRequest {
			return "", time.Time{}, false, fmt.Errorf("the metadata service refused the token request: %s (%s); check AZURE_CLIENT_ID",
				a.Description, a.Error)
		}
		retry = resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
			resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", time.Time{}, retry, fmt.Errorf("the metadata service answered %d", resp.StatusCode)
	}

	var a tokenAnswer
	if err := json.Unmarshal(body, &a); err != nil || a.AccessToken == "" {
		return "", time.Time{}, false, errors.New("unexpected answer from the metadata service")
	}
	secs, err := strconv.ParseInt(a.ExpiresOn, 10, 64)
	if err != nil {
		return "", time.Time{}, false, errors.New("unexpected answer from the metadata service")
	}
	return a.AccessToken, time.Unix(secs, 0), false, nil
}
