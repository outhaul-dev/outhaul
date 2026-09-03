# Dev-only image: run outhaul in a container against the host's Docker daemon.
# See deploy/dev/run.sh for the required socket/data-dir mounts.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /outhaul .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git docker.io \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /usr/local/lib/docker/cli-plugins \
    && curl -fsSL "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-$(uname -m)" \
        -o /usr/local/lib/docker/cli-plugins/docker-compose \
    && chmod +x /usr/local/lib/docker/cli-plugins/docker-compose \
    && curl -fsSL https://nixpacks.com/install.sh | bash
COPY --from=build /outhaul /usr/local/bin/outhaul
ENTRYPOINT ["outhaul", "serve"]
