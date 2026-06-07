# SpotiFLAC-Next integration

This document describes how the API integrates the three download backends
(**spotiflac** upstream, **spotbye** = SpotiFLAC-Next private C2, and
**monochrome**), the runtime C2 config store, status/gating, lyrics, and the
tooling that keeps everything up to date when a new SpotiFLAC-Next is released.

## Backends and the `auto` combo

| Engine | What it is | Hosts |
| --- | --- | --- |
| `spotiflac` | The upstream `github.com/afkarxyz/SpotiFLAC` backend | `*.afkarxyz.qzz.io` |
| `spotbye` | SpotiFLAC-Next private C2 (newer) | `*.spotbye.qzz.io`, `*.anandserver.cfd`, `amz.squid.wtf`, `flacdownloader.com` |
| `monochrome` | Community Hi-Fi API instances | `monochrome.tf`, `*.qqdl.site`, `hifi.geeked.wtf`, … |

`POST /v1/download-url` with `engine=auto` (default) runs a combo that **prefers
spotbye** (when its services are live per status gating), then `spotiflac`, then
`monochrome`. You can force any single engine with `engine=spotbye|spotiflac|monochrome`.

Service-id resolution for spotbye reuses odesli (`backend.SongLinkClient`):
Tidal/Amazon URLs + ISRC, and the Deezer id via the public Deezer ISRC lookup.
The response parser tolerates either a direct FLAC stream or a JSON pointer
(`url`/`downloadUrl`/`streamUrl`/`stream`/`link`/`file`/`path`, possibly nested
under `data`).

## C2 config store (SQLite) + admin

C2 endpoints and tunable settings live in a SQLite DB (`C2_DB_PATH`, default
`/app/data/c2.db`, persisted on the `c2-data` Docker volume). The DB is the
source of truth; `.env` only seeds an empty DB on first boot. Manage it at:

- `GET /admin/` — web UI (editable table). Protect it at your reverse proxy
  (e.g. HTTP basic auth); the panel works at `/admin/` or behind a subpath.
- `GET/POST /admin/c2`, `PUT/DELETE /admin/c2/{id}`, `PUT /admin/settings/{key}`
- `POST /admin/c2/import` — bulk-import a `c2-manifest.json`
- `POST /admin/c2/reload` — reload the cache

## Status and gating

`GET /v1/status` aggregates availability of all three worlds:

- `spotiflac_next`: the downloader-status payload (a gist of `tidal_a..x`,
  `qobuz_*`, `apple`… = up/down), normalized to `service -> {variant: up}`.
- `monochrome`: live reachability of the configured instances.
- `spotiflac_upstream`: provider availability + a Spotify reachability probe.

Downloads are **gated** on this: only services with an active variant are
attempted (toggle with `ENFORCE_ACTIVE_METHODS`; cache TTL `STATUS_CACHE_TTL`).

## Lyrics

`GET /v1/lyrics?spotify_url=…&format=json|lrc|text` uses the same multi-source
chain as SpotiFLAC-Next (LRCLib → Musixmatch → Spotify color-lyrics), provided
by the upstream `backend.LyricsClient.FetchLyricsAllSources`.

## Keeping C2 up to date (scripts + updater)

The C2 addresses change between SpotiFLAC-Next builds, so nothing is hard-coded
permanently. Tooling:

- **`scripts/extract-spotiflac-next.py`** — statically extracts a
  `c2-manifest.json` (hosts, endpoints, lyric/metadata providers, status source,
  UA, version) from a binary/`.app`. `--diff` for change review, `--emit-sql`.
- **`scripts/update-c2-from-binary.sh`** — extract → diff → (with `--apply`)
  `POST /admin/c2/import`.
- **`scripts/fetch-latest-next.py`** — resolves the supporter gist
  (`b2f7b815…`) → its Google Drive folder → downloads the latest macOS `.dmg`
  (gdown) → extracts the `.app` (hdiutil/7z) → returns `{version, binary}`.
- **`scripts/fetch-monochrome-instances.py`** — parses the canonical
  [monochrome INSTANCES.md](https://github.com/monochrome-music/monochrome/blob/main/INSTANCES.md)
  and writes `monochrome.api_instances` / `streaming_instances` /
  `discovery_urls` settings (no longer hard-coded in `.env`).
- **`updater/check-c2-updates.sh`** — the orchestrator (run from the same host
  timer as `update.sh`): refreshes monochrome instances every run, detects a new
  SpotiFLAC-Next version, extracts + diffs its C2, and with `--apply` imports the
  changes into the running API. Records the last seen version in `STATE_DIR`.

In production (Coolify + docker compose) the updater runs on the host and POSTs
to the API; the SQLite store persists on the `c2-data` volume across redeploys.

## DNS reality of the spotbye hosts (verified 2026-06)

SpotiFLAC-Next ships **multiple host families per service** (the `a..e/x`
variants); only the currently-provisioned ones resolve, and the status payload
says which are up. As of this writing (checked against 1.1.1.1 / 8.8.8.8):

| Service | Status | Live host + contract | Key |
| --- | --- | --- | --- |
| **qobuz** | ✅ **working end-to-end** (verified download) | search `qbzmt.spotbye.qzz.io/api/search?q=` → match ISRC/hires → `qbz.spotbye.qzz.io/api/download-music?track_id={id}` → `{data:{url}}` | none |
| deezer | host+path confirmed, needs key | `deezer.anandserver.cfd/api/track/{id}` (401 without key) | `X-API-Key` (supporter) |
| tidal | needs key | `tidal.anandserver.cfd` + `flacdownloader.com/flac/download` | `X-API-Key` (supporter) |
| amazon | needs key | `amz.squid.wtf` | `X-API-Key` (supporter) |
| apple | id resolution TODO | `am.spotbye.qzz.io` | — |

The `dzr/amz/tdl*/qbzalt/jmdl/lcd/status` `spotbye.qzz.io` subdomains are
NXDOMAIN in current builds; the live backends moved to `anandserver.cfd` /
`squid.wtf`. `spotbye.go` defaults are live-host-first.

**Supporter API keys.** Self-serve key generation was retired
(`tidal.anandserver.cfd/api/keys/generate` → HTTP 410, "Supporter keys are now
issued directly by the developer; see antra.anandserver.cfd"). The deezer / tidal
/ amazon C2 require an `X-API-Key`. These are **not** in the binary — set the key
you receive as a supporter in the config store and the engine sends it:

- per-service: `spotbye.deezer_api_key`, `spotbye.tidal_api_key`, `spotbye.amazon_api_key`
- or a shared `spotbye.api_key`

Set them via `/admin/` (web) or `PUT /admin/settings/spotbye.deezer_api_key`.

### Remaining work

- Supply the supporter `X-API-Key`(s) to enable deezer/tidal/amazon.
- apple service-id resolution (odesli doesn't provide it).
- Confirm the exact tidal/amazon request paths once a key is available (deezer
  path `/api/track/{id}` is confirmed).
