package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// TestPruneImagesOnlyOuthaulBuilt checks the dangling-image prune is
// filtered to images Outhaul built, so a shared daemon's other dangling
// images are left alone.
func TestPruneImagesOnlyOuthaulBuilt(t *testing.T) {
	var filters map[string]map[string]bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/images/prune") {
			http.NotFound(w, r)
			return
		}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Errorf("filters query: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ImagesDeleted":[],"SpaceReclaimed":0}`))
	}))
	defer srv.Close()

	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&real{cli: cli}).PruneImages(context.Background()); err != nil {
		t.Fatalf("PruneImages: %v", err)
	}
	if !filters["dangling"]["true"] {
		t.Errorf("filters = %v, want dangling=true", filters)
	}
	if !filters["label"][ImageLabel] {
		t.Errorf("filters = %v, want label=%s", filters, ImageLabel)
	}
}
