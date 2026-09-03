#!/bin/sh
# Build and run outhaul in a dev container against the host Docker daemon.
# Wipe state with: rm -rf "$DATA_DIR"
set -eu

REPO_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
DATA_DIR="${OUTHAUL_DEV_DATA_DIR:-$HOME/.outhaul-dev-data}"

docker build -t outhaul-dev -f "$REPO_DIR/deploy/dev/Dockerfile" "$REPO_DIR"

# Same path inside and out: outhaul hands host paths under its data dir to the
# host daemon (traefik config, dbaas volumes), so the paths must match.
mkdir -p "$DATA_DIR"
exec docker run --rm -it \
    --name outhaul-dev \
    --network host \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$DATA_DIR:$DATA_DIR" \
    -e OUTHAUL_DATA_DIR="$DATA_DIR" \
    "$@" \
    outhaul-dev
