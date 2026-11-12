// Command forwardedconfig prints the file-provider YAML Outhaul mounts while
// it trusts a proxy, so integration_gate.sh checks the real content.
package main

import (
	"fmt"

	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func main() { fmt.Print(traefik.ForwardedDynamicConfig()) }
