# Single image for nft-forward; the container's command decides the role.
# Build stage produces a static binary; runtime stage installs the
# minimal userland the daemon and panel need.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nft-forward ./cmd/nft-forward

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends nftables iproute2 ca-certificates curl python3 sqlite3 procps \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/nft-forward /usr/local/sbin/nft-forward
COPY install.sh /usr/local/sbin/nft-forward-install
RUN chmod +x /usr/local/sbin/nft-forward-install
# No ENTRYPOINT — docker-compose.yml supplies the per-service command so the
# same image serves daemon, server, or agent roles.
