# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1 — Build the Go binary
# ---------------------------------------------------------------------------
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Cache dependency downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully-static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o ai-proxy ./main.go

# ---------------------------------------------------------------------------
# Stage 2 — Minimal runtime image
# ---------------------------------------------------------------------------
FROM alpine:3.20

LABEL maintainer="CODENEX-AI-PROXY-API Team"
LABEL description="CODENEX-AI-PROXY-API – Go/Gin edition"

# ca-certificates is required for TLS calls to Google / OpenAI / Anthropic.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# Copy the binary and the static web UI.
COPY --from=builder /build/ai-proxy ./ai-proxy
COPY --from=builder /build/static   ./static
COPY --from=builder /build/configs  ./configs

# Data directories.
RUN mkdir -p /app/logs /app/data

# Non-root user for security.
RUN addgroup -S proxy && adduser -S proxy -G proxy
RUN chown -R proxy:proxy /app
USER proxy

# Main API & Web UI
EXPOSE 3000
# Gemini CLI OAuth callback
EXPOSE 8085
# Antigravity OAuth callback
EXPOSE 8086

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:3000/health || exit 1

ENTRYPOINT ["./ai-proxy"]
