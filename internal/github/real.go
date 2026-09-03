package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// previewCommentMarker is the hidden HTML comment that identifies Outhaul's
// single sticky preview comment on a PR.
const previewCommentMarker = "<!-- outhaul-preview -->"

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

// doJSON is like do but marshals reqBody to a JSON request body.
func (c *HTTPClient) doJSON(ctx context.Context, method, url, auth string, reqBody, out any) error {
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
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
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repositories"`
	}
	url := fmt.Sprintf("%s/installation/repositories?per_page=100", c.BaseURL)
	if err := c.do(ctx, http.MethodGet, url, "token "+token, &r); err != nil {
		return nil, err
	}
	repos := make([]Repo, 0, len(r.Repositories))
	for _, x := range r.Repositories {
		repos = append(repos, Repo{FullName: x.FullName, DefaultBranch: x.DefaultBranch})
	}
	return repos, nil
}

func (c *HTTPClient) Installation(ctx context.Context, appJWT string, installationID int64) (Installation, error) {
	var r struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	url := fmt.Sprintf("%s/app/installations/%d", c.BaseURL, installationID)
	if err := c.do(ctx, http.MethodGet, url, "Bearer "+appJWT, &r); err != nil {
		return Installation{}, err
	}
	return Installation{ID: r.ID, AccountLogin: r.Account.Login, AccountType: r.Account.Type}, nil
}

func (c *HTTPClient) UpsertPRComment(ctx context.Context, token, repo string, pr int, body string) error {
	full := previewCommentMarker + "\n" + body
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	list := fmt.Sprintf("%s/repos/%s/issues/%d/comments?per_page=100", c.BaseURL, repo, pr)
	if err := c.do(ctx, http.MethodGet, list, "token "+token, &comments); err != nil {
		return err
	}
	for _, cm := range comments {
		if strings.Contains(cm.Body, previewCommentMarker) {
			edit := fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.BaseURL, repo, cm.ID)
			return c.doJSON(ctx, http.MethodPatch, edit, "token "+token, map[string]string{"body": full}, nil)
		}
	}
	create := fmt.Sprintf("%s/repos/%s/issues/%d/comments", c.BaseURL, repo, pr)
	return c.doJSON(ctx, http.MethodPost, create, "token "+token, map[string]string{"body": full}, nil)
}
