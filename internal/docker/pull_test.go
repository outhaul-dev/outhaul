package docker

import (
	"bytes"
	"strings"
	"testing"
)

// The daemon answers a pull with HTTP 200 and reports failures inside the
// JSON progress stream. Treating a drained stream as success let EnsureProxy
// remove a working Traefik and then fail to create the new one ("No such
// image") — a full ingress outage on a full disk or a flaky registry.
func TestDrainPullReturnsInStreamError(t *testing.T) {
	stream := `{"status":"Pulling from library/traefik","id":"v3.7.6"}
{"status":"Downloading","progressDetail":{"current":1,"total":2},"id":"abc"}
{"errorDetail":{"message":"write /var/lib/docker/tmp: no space left on device"},"error":"write /var/lib/docker/tmp: no space left on device"}
`
	var out bytes.Buffer
	err := drainPull(strings.NewReader(stream), &out)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("drainPull error = %v, want the in-stream error", err)
	}
	if !strings.Contains(out.String(), "Pulling from library/traefik") {
		t.Errorf("progress was not passed through to the log: %q", out.String())
	}
}

func TestDrainPullSucceedsOnCleanStream(t *testing.T) {
	stream := `{"status":"Pulling from library/traefik","id":"v3.7.6"}
{"status":"Status: Downloaded newer image for traefik:v3.7.6"}
`
	if err := drainPull(strings.NewReader(stream), &bytes.Buffer{}); err != nil {
		t.Fatalf("drainPull on a clean stream: %v", err)
	}
}
