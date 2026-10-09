# Build stage
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

WORKDIR /app

# Copy dependency files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application: the package, not main.go alone, so every file of
# package main is in the binary (queue_close_restored.go among them).
RUN CGO_ENABLED=0 GOOS=linux go build -o skymail-backend .

# Final stage
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/skymail-backend .
# Copy migrations
COPY --from=builder /app/db/migrations ./db/migrations

# Expose the application port
EXPOSE 3000

# Set environment variables
ENV PORT=3000

# The container's health check: this task is not shutting down and its
# database answers (GET /ready?gate=skip on APP_PORT, 3000 unset). No curl or
# wget needed. A Swarm service uses it unless its own Health Check replaces
# it; the values and why are in docs/health-and-shutdown.md.
HEALTHCHECK --interval=10s --timeout=5s --start-period=60s --start-interval=2s --retries=6 \
  CMD ["/app/skymail-backend", "healthcheck"]

# Run the application. On SIGTERM it drains and stops within 25 s: give the
# service a stop grace period of 30 s (docs/health-and-shutdown.md).
CMD ["./skymail-backend"]
