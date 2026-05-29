#!/usr/bin/env bash
# SpotiFLAC API Daily Smoke Test Runner.
# Runs the smoke test script and pings the daily-smoke Uptime Kuma monitor.

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
NC='\e[0m'

log_info() {
  echo -e "${GREEN}[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] [INFO]${NC} $1"
}

log_error() {
  echo -e "${RED}[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] [ERROR]${NC} $1" >&2
}

push_uptime_kuma() {
  local status="$1"
  local msg="$2"
  
  if [ -z "${UPTIME_KUMA_DAILY_SMOKE_PUSH_URL:-}" ]; then
    log_info "UPTIME_KUMA_DAILY_SMOKE_PUSH_URL is not set. Skipping push."
    return 0
  fi
  
  local encoded_msg
  encoded_msg=$(echo -n "$msg" | jq -sRr @uri)
  
  local url="${UPTIME_KUMA_DAILY_SMOKE_PUSH_URL}"
  if [[ "$url" == *\?* ]]; then
    url="${url}&status=${status}&msg=${encoded_msg}"
  else
    url="${url}?status=${status}&msg=${encoded_msg}"
  fi
  
  log_info "Pushing daily status '${status}' to Uptime Kuma..."
  if curl -fsSL -m 10 "$url" >/dev/null 2>&1; then
    log_info "Successfully notified Uptime Kuma."
  else
    log_error "Failed to notify Uptime Kuma."
  fi
}

log_info "Executing daily API smoke test check..."

if "${SCRIPT_DIR}/smoke-test.sh"; then
  log_info "Daily smoke test passed successfully."
  push_uptime_kuma "up" "Daily API smoke test passed successfully."
  exit 0
else
  log_error "Daily smoke test failed!"
  push_uptime_kuma "down" "Daily API smoke test failed."
  exit 1
fi
