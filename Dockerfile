# MAIL-SHADOW-MCP — Structured email access for AI agents.
#
# Copyright (c) 2026 Benjamin Kaiser.
# SPDX-License-Identifier: Apache-2.0
# https://github.com/dryas/mail-shadow-mcp
#
# Multi-stage build:
#   Stage 1 (builder) — compiles the binary with ldflags version injection.
#   Stage 2 (runtime) — minimal alpine image with TLS root certificates.
#
# Required volumes:
#   /config  — must contain config.yaml  (set transport: http for Docker use)
#   /data    — persistent storage for the SQLite database and attachments
#
# Example run:
#   docker run -d \
#     -v ./config.yaml:/config/config.yaml:ro \
#     -v mail-shadow-data:/data \
#     -p 8080:8080 \
#     ghcr.io/dryas/mail-shadow-mcp:latest

# ─── Stage 1: builder ────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

ARG VERSION=dev

WORKDIR /src

# Cache dependencies separately from source code.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/mail-shadow-mcp \
    ./cmd/mail-shadow-mcp

# ─── Stage 2: runtime ────────────────────────────────────────────────────────
FROM alpine:3.21

# ca-certificates is required for IMAPS TLS connections.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/mail-shadow-mcp /usr/local/bin/mail-shadow-mcp
COPY config.example.yaml /etc/mail-shadow-mcp/config.example.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# /config  — mount your config.yaml here (read-only recommended)
# /data    — persistent storage: SQLite DB + downloaded attachments
VOLUME ["/config", "/data"]

EXPOSE 8080

ENV CONFIG_PATH=/config/config.yaml

ENTRYPOINT ["docker-entrypoint.sh"]
