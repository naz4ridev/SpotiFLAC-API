# Stage 1: Build the Go application
FROM golang:1.26-bookworm AS builder
WORKDIR /app

# Download dependencies first (leverages Docker cache layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o main .

# Stage 2: Build the Python virtual environment and source code
FROM debian:bookworm-slim AS python-builder
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    python3 \
    python3-venv \
    python3-pip \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY .python-spotiflac-ref .
RUN python3 -m venv /opt/python-spotiflac-venv && \
    /opt/python-spotiflac-venv/bin/pip install --upgrade pip && \
    git clone https://github.com/ShuShuzinhuu/SpotiFLAC-Module-Version.git /opt/python-spotiflac-src && \
    cd /opt/python-spotiflac-src && \
    git checkout $(cat /app/.python-spotiflac-ref) && \
    /opt/python-spotiflac-venv/bin/pip install -r requirements.txt && \
    /opt/python-spotiflac-venv/bin/pip install .

# Stage 3: Clean and lightweight runtime container
FROM debian:bookworm-slim

# Install ca-certificates (HTTPS requests), curl (healthchecks), ffmpeg, python3, and python3-venv
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    python3 \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# Data dir for the SQLite C2 config store (mount a volume here in production).
RUN mkdir -p /app/data
COPY --from=builder /app/main .
COPY scripts/ /app/scripts/
COPY .python-spotiflac-ref /app/.python-spotiflac-ref
COPY --from=python-builder /opt/python-spotiflac-venv /opt/python-spotiflac-venv
COPY --from=python-builder /opt/python-spotiflac-src /opt/python-spotiflac-src

# Expose internal port
EXPOSE 8080

# Environment configurations
ENV PORT=8080
ENV BIND_ADDR=0.0.0.0
ENV FFMPEG_AUTO_INSTALL=false
ENV C2_DB_PATH=/app/data/c2.db
ENV PYTHON_SPOTIFLAC_REPO=https://github.com/ShuShuzinhuu/SpotiFLAC-Module-Version.git
ENV PYTHON_PROVIDER_ENABLED=true
ENV PYTHON_PROVIDER_TIMEOUT_SECONDS=180
ENV PYTHON_PROVIDER_OUTPUT_DIR=/tmp/spotiflac-python
ENV FFMPEG_PATH=/usr/bin/ffmpeg
ENV FFPROBE_PATH=/usr/bin/ffprobe

# Run the API binary
CMD ["./main"]
