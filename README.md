# SpotiFLACAPI

REST API wrapper for [SpotiFLAC](https://github.com/afkarxyz/SpotiFLAC) focused on a simple use case:

1. Receive a Spotify track URL/ID.
2. Resolve and download audio using SpotiFLAC providers.
3. Return a browser-friendly download URL.

## Features

- Spotify track input (`https://open.spotify.com/track/...`, `spotify:track:...`, or raw track ID).
- Multi-provider support (Go and Python implementations of SpotiFLAC).
- Dynamic download strategies:
  - `race`: runs providers in parallel and returns the fastest successful result.
  - `fallback`: tries providers sequentially in order.
  - `single`: runs only the specified provider.
- Provider fallback chain (default order: `tidal -> qobuz -> amazon`).
- Download engine selector: `auto`, `spotiflac`, `monochrome`, or `spotbye` (for the Go provider).
  - `auto` is a combo that prefers **spotbye** (SpotiFLAC-Next C2) whenever its services are live per the status gating, then falls back to `spotiflac` (upstream) and finally `monochrome`.
- Temporary tokenized download URLs (`GET /v1/download/{token}`).
- In-memory token store with TTL-based cleanup.
- CORS enabled (`*`) for frontend/API integrations.
- Automatic `ffmpeg/ffprobe` bootstrap on first download request (enabled by default).
- Warmup endpoint (`POST /internal/warmup`) for preparing environments.
- Diagnostics endpoint (`GET /diagnostics/providers`) for inspecting provider states.
- SpotiFLAC-Next integration: `spotbye` C2 download engine, aggregated method status (`GET /v1/status`), lyrics (`GET /v1/lyrics`), a runtime-editable C2 config store (`/admin/`), and tooling to keep C2/monochrome endpoints fresh. See [docs/spotiflac-next-integration.md](docs/spotiflac-next-integration.md).

## Architecture

- `POST /v1/download-url`
  - Validates input, requested strategy, and download provider.
  - Ensures `ffmpeg/ffprobe` are available.
  - Resolves download via `ProviderManager` executing `race`, `fallback`, or `single` mode.
  - Stores file path + token + expiry in memory.
  - Returns a public download URL.
- `GET /v1/download/{token}`
  - Validates token and expiry.
  - Streams file with `Content-Disposition: attachment`.
- `GET /diagnostics/providers`
  - Returns JSON with provider availability, paths, versions, and pinned commit info.
- `POST /internal/warmup`
  - Bootstraps binaries, verifies Python imports, and optionally does a full test download if `FULL_WARMUP_DOWNLOAD=true`.
- Background cleaner
  - Removes expired tokens and associated temp files.

## Requirements

- Go `1.25+`
- Python `3.9+` (for Python provider)
- Network access to upstream services used by SpotiFLAC.

## Run

Default local run:

```bash
go run .
```

Run on `127.0.0.1:9000`:

```bash
BIND_ADDR=127.0.0.1 PORT=9000 BASE_URL=http://127.0.0.1:9000 go run .
```

## Configuration

- `BIND_ADDR`: bind address for the HTTP server (default: `127.0.0.1`)
- `PORT`: HTTP port (default: `8080`)
- `DOWNLOAD_TTL`: token TTL as Go duration (default: `2h`, example: `30m`)
- `BASE_URL`: optional public base URL used when building `download_url`
- `HTTP_CLIENT_TIMEOUT`: timeout for outbound HTTP calls from this API (default: `20s`)
- `FFMPEG_AUTO_INSTALL`: auto-install `ffmpeg/ffprobe` when missing (default: `true`)
- `DOWNLOAD_PROVIDERS`: enabled providers (default: `go,python`)
- `DEFAULT_DOWNLOAD_PROVIDER_ORDER`: fallback/race priority order (default: `go,python`)
- `DOWNLOAD_STRATEGY`: default strategy (default: `race`, valid: `race`, `fallback`, `single`)
- `PYTHON_PROVIDER_ENABLED`: enable/disable python provider (default: `true`)
- `PYTHON_PROVIDER_TIMEOUT_SECONDS`: subprocess timeout for Python provider (default: `180`)
- `DOWNLOAD_GLOBAL_TIMEOUT_SECONDS`: global timeout for the API request (default: `240`)
- `WARMUP_TIMEOUT_SECONDS`: timeout for the warmup endpoint (default: `60`)

There is also a ready-to-edit example file in `.env.example`.

## FFmpeg behavior

SpotiFLAC backend uses `ffmpeg` and/or `ffprobe` in several download and metadata paths.

In this API:

- `ffmpeg/ffprobe` are checked before processing download requests.
- If missing and `FFMPEG_AUTO_INSTALL=true`, the API downloads and installs them automatically.
- If auto-install is disabled and binaries are missing, requests fail with a clear error.

## API

### `GET /health`

Returns service status and current UTC timestamp.

### `GET /diagnostics/providers`

Returns the current providers state and system dependency statuses.

### `GET /v1/status`

Aggregated availability of all worlds, mirroring SpotiFLAC-Next's "which methods
are up" check: `spotiflac_next` (per service + variant from the status payload),
`monochrome` (live instance reachability), and `spotiflac_upstream`. Downloads
are gated on this (only active services are attempted; toggle with
`ENFORCE_ACTIVE_METHODS`).

### `GET /v1/lyrics`

Fetches lyrics using the same multi-source chain as SpotiFLAC-Next (LRCLib →
Musixmatch → Spotify color-lyrics). Query: `spotify_url` or `spotify_id`;
optional `format=json|lrc|text`. Returns synced (LRC) and plain lyrics plus the
source used.

### `GET /admin/` · `GET|POST|PUT|DELETE /admin/c2` · `POST /admin/c2/import`

Web UI and CRUD API for the C2 config store (SQLite). Lets you view and edit the
spotbye/monochrome endpoints, lyric/metadata providers, and credentials at
runtime, and bulk-import a `c2-manifest.json` produced by
`scripts/extract-spotiflac-next.py`. The admin panel resolves its API calls
relative to its own path, so it works at `/admin/` or behind a reverse-proxy
subpath. Protect it at your reverse proxy (e.g. HTTP basic auth).

### `POST /internal/warmup`

Performs warmup preparation (ffmpeg check, python import check, and optional test track download).

### `POST /v1/download-url`

Creates a downloadable resource from a Spotify track.

Request body:

```json
{
  "spotify_url": "https://open.spotify.com/track/3n3Ppam7vgaVa1iaRUc9Lp",
  "services": ["tidal", "qobuz", "amazon"],
  "ttl_seconds": 3600,
  "engine": "auto",
  "provider": "python",
  "strategy": "single"
}
```

Notes:

- `spotify_url` is required.
- `services` is optional; default order is `["tidal", "qobuz", "amazon"]`.
- `ttl_seconds` is optional and capped server-side.
- `engine` is optional; valid values are `auto`, `spotiflac`, `monochrome`, and `spotbye`.
- `provider` is optional; valid values are `go` and `python`.
- `strategy` is optional; valid values are `race`, `fallback`, and `single`.
  - If `provider` is defined and `strategy` is not, strategy defaults to `single`.
  - If `strategy` is not defined, it defaults to the configured `DOWNLOAD_STRATEGY` (default: `race`).
- `method` is accepted as an alias of `engine` in the JSON body.
- `?engine=...` and `?method=...` in the request URL override the JSON body.

Success response:

```json
{
  "ok": true,
  "spotify_id": "3n3Ppam7vgaVa1iaRUc9Lp",
  "service": "tidal",
  "method": "spotiflac",
  "filename": "Track - Artist.flac",
  "download_url": "http://127.0.0.1:9000/v1/download/<token>",
  "expires_at": "2026-02-21T12:00:00Z",
  "provider": "go",
  "duration_ms": 4200,
  "attempts": [
    {
      "provider": "go",
      "ok": true,
      "duration_ms": 4180,
      "service_attempts": [
        {
          "service": "tidal"
        }
      ]
    }
  ]
}
```

Failure response:

```json
{
  "ok": false,
  "error": "failed in all services: tidal -> qobuz -> amazon",
  "attempts": [
    {
      "service": "tidal",
      "error": "..."
    },
    {
      "service": "qobuz",
      "error": "..."
    },
    {
      "service": "amazon",
      "error": "..."
    }
  ]
}
```

### `GET /v1/download/{token}`

Returns the audio file as an attachment if token is valid and not expired.

## Usage example

```bash
curl -s -X POST http://127.0.0.1:9000/v1/download-url \
  -H 'Content-Type: application/json' \
  -d '{"spotify_url":"https://open.spotify.com/track/3n3Ppam7vgaVa1iaRUc9Lp"}'
```

Force Monochrome for testing:

```bash
curl -s -X POST 'http://127.0.0.1:9000/v1/download-url?engine=monochrome' \
  -H 'Content-Type: application/json' \
  -d '{"spotify_url":"https://open.spotify.com/track/3n3Ppam7vgaVa1iaRUc9Lp"}'
```

Force custom Monochrome settings from environment:

```bash
MONOCHROME_DISCOVERY_URLS=https://example-worker-1.dev,https://example-worker-2.dev \
MONOCHROME_API_INSTANCES=https://api-a.example,https://api-b.example \
MONOCHROME_STREAMING_INSTANCES=https://stream-a.example,https://stream-b.example \
go run .
```

Then open the returned `download_url` in a browser or fetch it with:

```bash
curl -L -o track.flac "http://127.0.0.1:9000/v1/download/<token>"
```

## Dependency management (SpotiFLAC upstream)

SpotiFLAC now uses `module github.com/afkarxyz/SpotiFLAC`, so this project pins upstream directly as a normal module requirement in `go.mod`.

Current pin:

- Commit: `7346730be98c`
- Pseudo-version: `v0.0.0-20260414003641-7346730be98c`

Update to latest upstream commit:

```bash
./scripts/pin-upstream.sh latest
```

Or pin a specific pseudo-version:

```bash
./scripts/pin-upstream.sh v0.0.0-20260414003641-7346730be98c
```

After changing upstream:

```bash
go build ./...
go test ./...
```

## Automated SpotiFLAC updates

To keep the production environment clean, the automated updater and monitoring scripts reside on a dedicated branch named `spotiflac-updater` in this same repository. 

- **`main` Branch**: Contains only the core Go API codebase, `Dockerfile`, and `docker-compose.yml`. This is the branch that Coolify pulls, builds, and deploys to production.
- **`spotiflac-updater` Branch**: Contains only the update script (`update.sh`), daily verification script (`daily-smoke.sh`), systemd timer configuration, and documentation. This is cloned on the host server at `/opt/spotiflac-updater` and schedules updates via local cron timers.

For detailed instructions on how to create the updater branch, clone it onto your server, configure `.env`, and register automation timers, see the **[updater/README.md](file:///home/mariano_palomo/dev/personal/proyectos_personales/SpotiFLAC-API/updater/README.md)** file.

---

## Nginx Subpath Configuration

The API container binds to port `18150` on localhost inside the server. To publish it under your subpath `https://office.naz4rimusic.com/tools/spotiflacapi`, configure Nginx to proxy requests to `http://127.0.0.1:18150/`:

```nginx
# Proxy block for SpotiFLAC API served under a subpath
location /tools/spotiflacapi/ {
    proxy_pass http://127.0.0.1:18150/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    
    # Increase timeouts because track conversion and downloading can take time
    proxy_connect_timeout 120s;
    proxy_send_timeout    120s;
    proxy_read_timeout    120s;
}
```

> [!IMPORTANT]
> The trailing slash in `proxy_pass http://127.0.0.1:18150/;` is critical. It replaces the `/tools/spotiflacapi/` subpath portion of the request URI so that the API container receives clean endpoints like `/health` and `/v1/download-url` instead of `/tools/spotiflacapi/health`.

---

## Security & Firewall Verification

### 1. Local port binding
The API service runs in a Docker container that publishes port `18150` bound exclusively to the loopback interface (`127.0.0.1`). This prevents public access bypassing Nginx. You can verify this binding on your host by running:
```bash
ss -tulpn | grep 18150
```
Expected output:
```text
tcp   LISTEN 0      4096       127.0.0.1:18150      0.0.0.0:*
```

### 2. UFW status
To ensure no external access is permitted directly to the container, keep UFW active and do not open port `18150` to public interfaces:
```bash
sudo ufw status
```
Only ports `80` (HTTP) and `443` (HTTPS) should be allowed from public IPs.



