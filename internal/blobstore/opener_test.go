package blobstore

import (
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestOpenerChoosesTheClientByKind(t *testing.T) {
	o := NewOpener("")

	for _, kind := range []string{"", core.DestinationS3} {
		c, err := o.Open(core.Destination{
			Kind: kind, Name: "minio", Endpoint: "https://s3.example.com", Bucket: "b",
			AccessKey: "AK", SecretKey: "SK",
		})
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if _, ok := c.(*s3); !ok {
			t.Errorf("kind %q opened %T, want *s3", kind, c)
		}
	}

	c, err := o.Open(core.Destination{
		Kind: core.DestinationAzure, Name: "Azure Blob",
		Endpoint: "https://examplebackups.blob.core.windows.net", Bucket: "outhaul",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(*azure); !ok {
		t.Errorf("azure opened %T, want *azure", c)
	}

	if _, err := o.Open(core.Destination{Kind: core.DestinationAzure}); err == nil ||
		!strings.Contains(err.Error(), "OUTHAUL_BACKUP_AZURE_ACCOUNT is not set") {
		t.Fatalf("azure without an endpoint = %v, want the variable's name", err)
	}

	if _, err := o.Open(core.Destination{Kind: "gcs"}); err == nil || !strings.Contains(err.Error(), "gcs") {
		t.Fatalf("unknown kind = %v, want an error naming it", err)
	}
}
