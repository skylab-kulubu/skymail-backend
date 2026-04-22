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

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o skymail-backend main.go

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

# Run the application
CMD ["./skymail-backend"]
