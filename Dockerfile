# Stage 1: Build the Go application
FROM golang:1.26-bookworm AS builder
WORKDIR /app

# Download dependencies first (leverages Docker cache layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o main .

# Stage 2: Clean and lightweight runtime container
FROM debian:bookworm-slim

# Install ca-certificates (HTTPS requests), curl (healthchecks), and ffmpeg (downloader dependency)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/main .

# Expose internal port
EXPOSE 8080

# Environment configurations
ENV PORT=8080
ENV BIND_ADDR=0.0.0.0
ENV FFMPEG_AUTO_INSTALL=false

# Run the API binary
CMD ["./main"]
