// Package tunnel manages the cloudflared connector container that exposes
// Outhaul through a Cloudflare Tunnel. The connector dials outbound to
// Cloudflare's edge and forwards traffic to the managed Traefik proxy over the
// shared Docker network, so no inbound ports need be published on the host.
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/outhaul-dev/outhaul/internal/docker"
)

// ContainerName is the fixed name of the managed cloudflared container.
const ContainerName = "outhaul-cloudflared"

// connectorStopTimeout bounds how long a drifted connector may take to stop.
const connectorStopTimeout = 10 * time.Second

// ConnectorConfig parameterises the cloudflared container.
type ConnectorConfig struct {
	Image   string // e.g. "cloudflare/cloudflared:2025.7.0"
	Network string // shared network the connector and Traefik both join
	Token   string // Cloudflare connector token (passed via TUNNEL_TOKEN env)
}

// EnsureConnector makes the cloudflared connector present and running. It is
// idempotent: it adopts an existing connector whose config hash matches,
// starts it if stopped, or pulls the image and (re)creates it otherwise. A
// changed token yields a new hash and triggers a recreate.
func EnsureConnector(ctx context.Context, dc docker.Client, cc ConnectorConfig, logOut io.Writer) error {
	if err := dc.EnsureNetwork(ctx, cc.Network); err != nil {
		return fmt.Errorf("ensure network %q: %w", cc.Network, err)
	}
	spec := connectorSpec(cc)

	existing, err := dc.FindContainer(ctx, ContainerName)
	if err != nil {
		return fmt.Errorf("find cloudflared container: %w", err)
	}
	if existing != nil && existing.Labels["outhaul.config-hash"] == spec.Labels["outhaul.config-hash"] {
		if existing.Running() {
			return nil
		}
		return dc.StartContainer(ctx, existing.ID)
	}

	// Pull FIRST so a pull failure never tears down a working connector.
	if err := dc.PullImage(ctx, cc.Image, logOut); err != nil {
		return fmt.Errorf("pull cloudflared image: %w", err)
	}
	if existing != nil {
		if existing.Running() {
			_ = dc.StopContainer(ctx, existing.ID, connectorStopTimeout)
		}
		if err := dc.RemoveContainer(ctx, existing.ID, true); err != nil {
			return fmt.Errorf("remove drifted cloudflared: %w", err)
		}
	}
	id, err := dc.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create cloudflared: %w", err)
	}
	return dc.StartContainer(ctx, id)
}

// RemoveConnector stops and removes the connector. It is idempotent (a no-op
// when no connector exists), so disabling the tunnel can be safely retried.
func RemoveConnector(ctx context.Context, dc docker.Client) error {
	existing, err := dc.FindContainer(ctx, ContainerName)
	if err != nil {
		return fmt.Errorf("find cloudflared container: %w", err)
	}
	if existing == nil {
		return nil
	}
	if existing.Running() {
		_ = dc.StopContainer(ctx, existing.ID, connectorStopTimeout)
	}
	return dc.RemoveContainer(ctx, existing.ID, true)
}

// connectorSpec builds the desired cloudflared container spec.
func connectorSpec(cc ConnectorConfig) docker.ContainerSpec {
	return docker.ContainerSpec{
		Name:          ContainerName,
		Image:         cc.Image,
		Cmd:           []string{"tunnel", "--no-autoupdate", "run"},
		Env:           []string{"TUNNEL_TOKEN=" + cc.Token},
		Networks:      []string{cc.Network},
		RestartPolicy: "unless-stopped",
		Labels: map[string]string{
			"outhaul.managed":     "true",
			"outhaul.role":        "tunnel",
			"outhaul.config-hash": hashConfig(cc),
		},
	}
}

// hashConfig fingerprints the connector. It hashes a SHA-256 digest of the
// token, never the raw token, so the token never lands in a Docker label while
// a rotation still changes the hash and forces a recreate.
func hashConfig(cc ConnectorConfig) string {
	tok := sha256.Sum256([]byte(cc.Token))
	sum := sha256.Sum256([]byte(cc.Image + "\n" + cc.Network + "\n" + hex.EncodeToString(tok[:])))
	return hex.EncodeToString(sum[:])[:12]
}
