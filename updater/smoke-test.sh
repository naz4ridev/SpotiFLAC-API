#!/usr/bin/env bash
# Smoke test script for SpotiFLAC API.
# Tests /health, /v1/download-url, and downloads + validates the file with ffprobe.

set -euo pipefail

# Find script directory and load .env if present
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/.env" ]; then
  set -a
  # shellcheck source=/dev/null
  source "${SCRIPT_DIR}/.env"
  set +a
fi

# Terminal colors
RED='\e[31m'
GREEN='\e[32m'
YELLOW='\e[33m'
NC='\e[0m'

log_info() {
  echo -e "${GREEN}[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] [INFO]${NC} $1"
}

log_error() {
  echo -e "${RED}[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] [ERROR]${NC} $1" >&2
}

log_info "Initializing smoke test..."

# Normalize and validate BASE_URL
if [ -z "${BASE_URL:-}" ]; then
  log_error "BASE_URL environment variable is not set."
  exit 1
fi
BASE_URL="${BASE_URL%/}"

# Validate TEST_TRACK_URL
if [ -z "${TEST_TRACK_URL:-}" ]; then
  log_error "TEST_TRACK_URL environment variable is not set."
  exit 1
fi

# Set default minimum audio size (100KB)
MIN_BYTES=${MIN_AUDIO_BYTES:-100000}

# Setup temp files cleanup
TMP_DIR=$(mktemp -d)
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# 1. Verify GET /health
HEALTH_URL="${BASE_URL}/health"
log_info "Testing health endpoint: GET ${HEALTH_URL}"
HEALTH_RESP=$(curl -fsSL --connect-timeout 10 -m 15 "$HEALTH_URL")

if ! echo "$HEALTH_RESP" | jq -e '.ok == true' >/dev/null; then
  log_error "Health check failed. Response was: ${HEALTH_RESP}"
  exit 1
fi
log_info "Health check passed!"

# 1b. Aggregated method status (informational; does not fail the smoke test).
STATUS_URL="${BASE_URL}/v1/status"
log_info "Checking aggregated status: GET ${STATUS_URL}"
STATUS_RESP=$(curl -fsSL --connect-timeout 10 -m 20 "$STATUS_URL" 2>/dev/null || true)
if echo "$STATUS_RESP" | jq -e '.ok == true' >/dev/null 2>&1; then
  MONO_UP=$(echo "$STATUS_RESP" | jq -r '.monochrome.instances_up // 0')
  MONO_TOTAL=$(echo "$STATUS_RESP" | jq -r '.monochrome.instances_total // 0')
  NEXT_ACTIVE=$(echo "$STATUS_RESP" | jq -r '[.spotiflac_next // {} | to_entries[] | select(.value.active)] | length')
  log_info "Status OK: monochrome ${MONO_UP}/${MONO_TOTAL} up, spotiflac-next active services: ${NEXT_ACTIVE}"
else
  log_info "Status endpoint not available or returned no data (non-fatal)."
fi

# 2. Verify POST /v1/download-url
DOWNLOAD_URL_API="${BASE_URL}/v1/download-url"
log_info "Requesting download URL: POST ${DOWNLOAD_URL_API}"

# Prepare payload
JSON_PAYLOAD=$(jq -n --arg url "$TEST_TRACK_URL" '{spotify_url: $url, engine: "auto"}')

# Call the API. resolution can take time on the backend, so connect timeout is 15s and execution 90s.
RESP_FILE="${TMP_DIR}/api_response.json"
HTTP_CODE=$(curl -s -w "%{http_code}" \
  -H "Content-Type: application/json" \
  -d "$JSON_PAYLOAD" \
  -o "$RESP_FILE" \
  --connect-timeout 15 -m 90 \
  "$DOWNLOAD_URL_API")

if [ "$HTTP_CODE" -ne 200 ]; then
  log_error "POST /v1/download-url failed with HTTP status ${HTTP_CODE}."
  if [ -f "$RESP_FILE" ]; then
    log_error "Response was: $(cat "$RESP_FILE")"
  fi
  exit 1
fi

API_RESP=$(cat "$RESP_FILE")
if ! echo "$API_RESP" | jq -e '.ok == true' >/dev/null; then
  log_error "API returned failure: ${API_RESP}"
  exit 1
fi

DOWNLOAD_URL=$(echo "$API_RESP" | jq -r '.download_url')
if [ -z "$DOWNLOAD_URL" ] || [ "$DOWNLOAD_URL" = "null" ]; then
  log_error "API response did not contain a valid download_url: ${API_RESP}"
  exit 1
fi

log_info "Download URL received successfully: ${DOWNLOAD_URL}"

# 3. Download the audio file
AUDIO_FILE="${TMP_DIR}/test_track.flac"
log_info "Downloading resolved audio file from ${DOWNLOAD_URL}..."

DL_HTTP_CODE=$(curl -s -L -w "%{http_code}" \
  -o "$AUDIO_FILE" \
  --connect-timeout 15 -m 60 \
  "$DOWNLOAD_URL")

if [ "$DL_HTTP_CODE" -ne 200 ]; then
  log_error "Failed to download audio file. HTTP status ${DL_HTTP_CODE}."
  exit 1
fi

# 4. Verify file size
FILE_SIZE=$(wc -c < "$AUDIO_FILE" | tr -d ' ')
log_info "Downloaded file size: ${FILE_SIZE} bytes (Minimum required: ${MIN_BYTES} bytes)"

if [ "$FILE_SIZE" -lt "$MIN_BYTES" ]; then
  log_error "Downloaded file size (${FILE_SIZE} bytes) is below the threshold of ${MIN_BYTES} bytes."
  exit 1
fi

# 5. Verify format using ffprobe
log_info "Verifying file integrity with ffprobe..."
if ! ffprobe -v error -show_format -show_streams "$AUDIO_FILE" > /dev/null 2>&1; then
  log_error "ffprobe integrity verification failed. The downloaded file is not a valid audio file."
  exit 1
fi

FORMAT_NAME=$(ffprobe -v error -show_entries format=format_name -of default=noprint_wrappers=1:nokey=1 "$AUDIO_FILE")
log_info "Audio format verified successfully: ${FORMAT_NAME}"

log_info "Smoke test passed successfully!"
exit 0
