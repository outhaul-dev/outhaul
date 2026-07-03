package deploy

import (
	"context"
	"net/http"
	"time"
)

// HealthChecker reports whether the app at url becomes reachable within timeout.
// "Reachable" means any HTTP response (even 4xx/5xx) — we verify the process is
// listening, not that routes are correct.
type HealthChecker func(ctx context.Context, url string, timeout time.Duration) bool

// httpPoll is the default HealthChecker: it GETs url every second until it gets
// any response or the deadline passes.
func httpPoll(ctx context.Context, url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(1 * time.Second):
		}
	}
	return false
}
