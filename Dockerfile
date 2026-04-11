# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_wefax Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_wefax ./...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false wefax

COPY --from=go-builder /out/ubersdr_wefax /usr/local/bin/ubersdr_wefax

# Copy entrypoint script (translates env vars to ubersdr_wefax flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default output directory and ensure the wefax user owns it.
# Users can volume-mount a host directory over /data to persist images on the host.
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data \
    && chown wefax:wefax /data

USER wefax

VOLUME ["/data"]

# Expose the web gallery port (default; override with WEB_PORT env var)
EXPOSE 6094

# Verify the binary can print help
HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/ubersdr_wefax", "-help"] || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
