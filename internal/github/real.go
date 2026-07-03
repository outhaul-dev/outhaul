package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient is the real GitHub API client.
type HTTPClient struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns an HTTPClient pointed at api.github.com.
func New() *HTTPClient {
	return &HTTPClient{
		BaseURL: "https://api.github.com",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *HTTPClient) do(ctx context.Context, method, url, auth string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github %s %s: status %d: %s", method, url, resp.StatusCode, body)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *HTTPClient) ExchangeManifest(ctx context.Context, code string) (ManifestResult, error) {
	var r struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
	}
	url := fmt.Sprintf("%s/app-manifests/%s/conversions", c.BaseURL, code)
	if err := c.do(ctx, http.MethodPost, url, "", &r); err != nil {
		return ManifestResult{}, err
	}
	return ManifestResult{
		AppID: r.ID, Slug: r.Slug, PEM: r.PEM, WebhookSecret: r.WebhookSecret,
		ClientID: r.ClientID, ClientSecret: r.ClientSecret,
	}, nil
}

func (c *HTTPClient) InstallationToken(ctx context.Context, appJWT string, installationID int64) (string, error) {
	var r struct {
		Token string `json:"token"`
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.BaseURL, installationID)
	if err := c.do(ctx, http.MethodPost, url, "Bearer "+appJWT, &r); err != nil {
		return "", err
	}
	return r.Token, nil
}

func (c *HTTPClient) ListRepos(ctx context.Context, token string) ([]Repo, error) {
	var r struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	url := fmt.Sprintf("%s/installation/repositories?per_page=100", c.BaseURL)
	if err := c.do(ctx, http.MethodGet, url, "token "+token, &r); err != nil {
		return nil, err
	}
	repos := make([]Repo, 0, len(r.Repositories))
	for _, x := range r.Repositories {
		repos = append(repos, Repo{FullName: x.FullName})
	}
	return repos, nil
}
