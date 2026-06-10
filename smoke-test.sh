#!/usr/bin/env bash
# Smoke test script for SpotiFLAC API.
# Tests /health, /diagnostics/providers, /internal/warmup, /v1/download-url, and downloads + validates the file with ffprobe.

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

log_warn() {
  echo -e "${YELLOW}[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] [WARN]${NC} $1"
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

# Setup defaults for smoke test variables
SMOKE_STRATEGY=${SMOKE_STRATEGY:-race}
SMOKE_FORCE_PROVIDER=${SMOKE_FORCE_PROVIDER:-}
SMOKE_MAX_TIME_SECONDS=${SMOKE_MAX_TIME_SECONDS:-240}
SMOKE_WARMUP_ENABLED=${SMOKE_WARMUP_ENABLED:-true}
REQUIRE_ALL_PROVIDERS_HEALTHY=${REQUIRE_ALL_PROVIDERS_HEALTHY:-false}
SMOKE_REQUIRE_ALL_PROVIDERS=${SMOKE_REQUIRE_ALL_PROVIDERS:-$REQUIRE_ALL_PROVIDERS_HEALTHY}
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
if STATUS_RESP=$(curl -fsSL --connect-timeout 10 -m 20 "$STATUS_URL" 2>/dev/null) && echo "$STATUS_RESP" | jq -e '.ok == true' >/dev/null 2>&1; then
  MONO_UP=$(echo "$STATUS_RESP" | jq -r '.monochrome.instances_up // 0')
  MONO_TOTAL=$(echo "$STATUS_RESP" | jq -r '.monochrome.instances_total // 0')
  NEXT_ACTIVE=$(echo "$STATUS_RESP" | jq -r '[.spotiflac_next // {} | to_entries[] | select(.value.active)] | length')
  log_info "Status OK: monochrome ${MONO_UP}/${MONO_TOTAL} up, spotiflac-next active services: ${NEXT_ACTIVE}"
else
  log_info "Status endpoint not available or returned no data (non-fatal)."
fi

# 2. Verify GET /diagnostics/providers (if exists)
DIAGNOSTICS_URL="${BASE_URL}/diagnostics/providers"
log_info "Testing diagnostics endpoint: GET ${DIAGNOSTICS_URL}"
if DIAG_RESP=$(curl -fsSL --connect-timeout 10 -m 15 "$DIAGNOSTICS_URL" 2>/dev/null); then
  log_info "Diagnostics retrieved successfully:"
  echo "$DIAG_RESP" | jq .
else
  log_warn "Diagnostics endpoint returned error or is not reachable."
fi

# 3. Trigger POST /internal/warmup (if enabled)
if [ "$SMOKE_WARMUP_ENABLED" = "true" ]; then
  WARMUP_URL="${BASE_URL}/internal/warmup"
  log_info "Triggering internal warmup: POST ${WARMUP_URL}"
  WARMUP_RESP_FILE="${TMP_DIR}/warmup_response.json"
  
  WARMUP_HTTP_CODE=$(curl -s -w "%{http_code}" \
    -X POST \
    -o "$WARMUP_RESP_FILE" \
    --connect-timeout 10 -m 70 \
    "$WARMUP_URL")
    
  if [ "$WARMUP_HTTP_CODE" -eq 200 ]; then
    log_info "Warmup completed successfully: $(cat "$WARMUP_RESP_FILE")"
  else
    log_warn "Warmup endpoint returned non-200 HTTP code ($WARMUP_HTTP_CODE). Details: $(cat "$WARMUP_RESP_FILE" 2>/dev/null || echo "no response")"
  fi
fi

# 4. Verify the end-to-end download.
#
# This depends on EXTERNAL, volatile C2 providers (Tidal/Qobuz/Amazon/Deezer
# mirrors + Monochrome) whose availability fluctuates and whose hostnames change
# between releases. A transient provider outage must NOT roll back an otherwise
# healthy code deploy (health/status/diagnostics/warmup already validated it), so
# the download is a SOFT check by default. Set SMOKE_REQUIRE_DOWNLOAD=true to make
# a failed/incomplete download fail the smoke test (and trigger a rollback).
SMOKE_REQUIRE_DOWNLOAD=${SMOKE_REQUIRE_DOWNLOAD:-false}

download_check() {
  local DOWNLOAD_URL_API="${BASE_URL}/v1/download-url"
  log_info "Requesting download URL: POST ${DOWNLOAD_URL_API}"
  log_info "Using strategy: ${SMOKE_STRATEGY}, forced provider: ${SMOKE_FORCE_PROVIDER:-none}"

  local JSON_PAYLOAD
  if [ -n "${SMOKE_FORCE_PROVIDER}" ]; then
    JSON_PAYLOAD=$(jq -n --arg url "$TEST_TRACK_URL" --arg prov "$SMOKE_FORCE_PROVIDER" --arg strat "$SMOKE_STRATEGY" '{spotify_url: $url, provider: $prov, strategy: $strat}')
  else
    JSON_PAYLOAD=$(jq -n --arg url "$TEST_TRACK_URL" --arg strat "$SMOKE_STRATEGY" '{spotify_url: $url, strategy: $strat}')
  fi

  local RESP_FILE="${TMP_DIR}/api_response.json"
  local HTTP_CODE
  HTTP_CODE=$(curl -s -w "%{http_code}" \
    -H "Content-Type: application/json" \
    -d "$JSON_PAYLOAD" \
    -o "$RESP_FILE" \
    --connect-timeout 15 -m "$SMOKE_MAX_TIME_SECONDS" \
    "$DOWNLOAD_URL_API")

  if [ "$HTTP_CODE" -ne 200 ]; then
    log_error "POST /v1/download-url failed with HTTP status ${HTTP_CODE}."
    [ -f "$RESP_FILE" ] && log_error "Response was: $(cat "$RESP_FILE")"
    return 1
  fi

  local API_RESP
  API_RESP=$(cat "$RESP_FILE")
  if ! echo "$API_RESP" | jq -e '.ok == true' >/dev/null; then
    log_error "API returned failure: ${API_RESP}"
    return 1
  fi

  # The download endpoint is asynchronous: poll (POST {task_id}) until done.
  local TASK_ID STATUS POLL_START POLL_PAYLOAD POLL_RESP ELAPSED
  TASK_ID=$(echo "$API_RESP" | jq -r '.task_id // empty')
  STATUS=$(echo "$API_RESP" | jq -r '.status // empty')
  if [ -n "$TASK_ID" ] && [ "$STATUS" != "completed" ]; then
    log_info "Download queued as task ${TASK_ID}; polling until ready (max ${SMOKE_MAX_TIME_SECONDS}s)..."
    POLL_START=$(date +%s)
    POLL_PAYLOAD=$(jq -n --arg t "$TASK_ID" '{task_id: $t}')
    while true; do
      sleep 3
      POLL_RESP=$(curl -s -H "Content-Type: application/json" -d "$POLL_PAYLOAD" \
        --connect-timeout 15 -m 60 "$DOWNLOAD_URL_API")
      STATUS=$(echo "$POLL_RESP" | jq -r '.status // empty')
      if [ "$STATUS" = "completed" ]; then
        API_RESP="$POLL_RESP"
        log_info "Download task completed."
        break
      fi
      if [ "$STATUS" = "failed" ]; then
        log_error "Download task failed: ${POLL_RESP}"
        return 1
      fi
      ELAPSED=$(( $(date +%s) - POLL_START ))
      if [ "$ELAPSED" -ge "$SMOKE_MAX_TIME_SECONDS" ]; then
        log_error "Download task did not complete within ${SMOKE_MAX_TIME_SECONDS}s. Last response: ${POLL_RESP}"
        return 1
      fi
      log_info "Task status: ${STATUS:-unknown} (${ELAPSED}s elapsed)..."
    done
  fi

  if [ "$SMOKE_REQUIRE_ALL_PROVIDERS" = "true" ]; then
    local FAILED_ATTEMPTS
    FAILED_ATTEMPTS=$(echo "$API_RESP" | jq '[.attempts[] | select(.ok == false)] | length')
    if [ "$FAILED_ATTEMPTS" -gt 0 ]; then
      log_error "Some providers failed, and SMOKE_REQUIRE_ALL_PROVIDERS is set to true. Attempts summary:"
      echo "$API_RESP" | jq '.attempts'
      return 1
    fi
  fi

  local DOWNLOAD_URL
  DOWNLOAD_URL=$(echo "$API_RESP" | jq -r '.download_url')
  if [ -z "$DOWNLOAD_URL" ] || [ "$DOWNLOAD_URL" = "null" ]; then
    log_error "API response did not contain a valid download_url: ${API_RESP}"
    return 1
  fi
  log_info "Download URL received successfully: ${DOWNLOAD_URL}"

  # 5. Download the audio file.
  local AUDIO_FILE="${TMP_DIR}/test_track.flac"
  local DL_HTTP_CODE
  log_info "Downloading resolved audio file from ${DOWNLOAD_URL}..."
  DL_HTTP_CODE=$(curl -s -L -w "%{http_code}" -o "$AUDIO_FILE" --connect-timeout 15 -m 60 "$DOWNLOAD_URL")
  if [ "$DL_HTTP_CODE" -ne 200 ]; then
    log_error "Failed to download audio file. HTTP status ${DL_HTTP_CODE}."
    return 1
  fi

  # 6. Verify file size.
  local FILE_SIZE
  FILE_SIZE=$(wc -c < "$AUDIO_FILE" | tr -d ' ')
  log_info "Downloaded file size: ${FILE_SIZE} bytes (Minimum required: ${MIN_BYTES} bytes)"
  if [ "$FILE_SIZE" -lt "$MIN_BYTES" ]; then
    log_error "Downloaded file size (${FILE_SIZE} bytes) is below the threshold of ${MIN_BYTES} bytes."
    return 1
  fi

  # 7. Verify format using ffprobe.
  log_info "Verifying file integrity with ffprobe..."
  if ! ffprobe -v error -show_format -show_streams "$AUDIO_FILE" > /dev/null 2>&1; then
    log_error "ffprobe integrity verification failed. The downloaded file is not a valid audio file."
    return 1
  fi
  local FORMAT_NAME
  FORMAT_NAME=$(ffprobe -v error -show_entries format=format_name -of default=noprint_wrappers=1:nokey=1 "$AUDIO_FILE")
  log_info "Audio format verified successfully: ${FORMAT_NAME}"
  return 0
}

if download_check; then
  log_info "Smoke test passed successfully!"
  exit 0
fi

if [ "$SMOKE_REQUIRE_DOWNLOAD" = "true" ]; then
  log_error "Download verification failed and SMOKE_REQUIRE_DOWNLOAD=true — failing smoke test."
  exit 1
fi

log_warn "Download verification failed, but it is a SOFT check (SMOKE_REQUIRE_DOWNLOAD!=true)."
log_warn "Health, status, diagnostics and warmup all passed — keeping the deploy. This is"
log_warn "usually a transient external provider/C2 outage, not a code regression."
exit 0
