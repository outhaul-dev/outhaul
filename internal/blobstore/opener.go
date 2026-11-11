package blobstore

import (
	"errors"
	"fmt"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Opener picks the client for a destination's kind. It holds one
// managed-identity token source, shared by every Azure client in the process,
// so a token fetched for one run serves the next.
type Opener struct {
	tokens TokenSource
}

// NewOpener returns an opener whose Azure clients use the host's managed
// identity. azureClientID is AZURE_CLIENT_ID; "" asks for the VM's only one.
func NewOpener(azureClientID string) *Opener {
	return &Opener{tokens: NewTokenSource(azureClientID)}
}

// Open returns the client a destination's kind calls for.
func (o *Opener) Open(d core.Destination) (Client, error) {
	switch d.Kind {
	case "", core.DestinationS3:
		return OpenS3(d)
	case core.DestinationAzure:
		if d.Endpoint == "" {
			return nil, errors.New("OUTHAUL_BACKUP_AZURE_ACCOUNT is not set")
		}
		return NewAzureClient(d.Endpoint, d.Bucket, o.tokens)
	default:
		return nil, fmt.Errorf("blobstore: unknown destination kind %q", d.Kind)
	}
}
