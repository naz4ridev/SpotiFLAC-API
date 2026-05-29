#!/usr/bin/env bash
# Main SpotiFLAC update orchestration script.
# Designed to run in /opt/spotiflac-updater on the server.
# Keeps the core API in the main branch updated, verified, and safely deployed via Coolify.

set -euo pipefail

# Find script directory and load .env configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "${SCRIPT_DIR}/.env" ]; then
  set -a
  # shellcheck source=/dev/null
  source "${SCRIPT_DIR}/.env"
  set +a
else
  echo "[ERROR] .env file not found in ${SCRIPT_DIR}. Please configure it." >&2
  exit 1
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

# 1. Acquire process lock to avoid parallel checks
LOCK_FILE="${SCRIPT_DIR}/.update.lock"
exec 200>"$LOCK_FILE"
if ! flock -n 200; then
  log_error "Another update process is currently active. Exiting."
  exit 1
fi

# Set defaults for optional vars
SPOTIFLAC_REPO=${SPOTIFLAC_REPO:-spotbye/SpotiFLAC}
GO_MODULE_PATH=${GO_MODULE_PATH:-github.com/afkarxyz/SpotiFLAC}
UPDATE_MODE=${UPDATE_MODE:-release}
APP_BRANCH=${APP_BRANCH:-main}
WORKDIR=${WORKDIR:-/tmp/spotiflacapi-update-workdir}
STATE_DIR=${STATE_DIR:-${SCRIPT_DIR}/state}
COOLIFY_DEPLOY_TIMEOUT_SECONDS=${COOLIFY_DEPLOY_TIMEOUT_SECONDS:-600}
GIT_AUTHOR_NAME=${GIT_AUTHOR_NAME:-spotiflac-updater}
GIT_AUTHOR_EMAIL=${GIT_AUTHOR_EMAIL:-spotiflac-updater@naz4rimusic.com}

push_uptime_kuma() {
  local status="$1"
  local msg="$2"
  
  if [ -z "${UPTIME_KUMA_UPDATE_PUSH_URL:-}" ]; then
    log_info "UPTIME_KUMA_UPDATE_PUSH_URL is not set. Skipping push."
    return 0
  fi
  
  local encoded_msg
  encoded_msg=$(echo -n "$msg" | jq -sRr @uri)
  
  local url="${UPTIME_KUMA_UPDATE_PUSH_URL}"
  if [[ "$url" == *\?* ]]; then
    url="${url}&status=${status}&msg=${encoded_msg}"
  else
    url="${url}?status=${status}&msg=${encoded_msg}"
  fi
  
  log_info "Pushing status '${status}' to Uptime Kuma..."
  if curl -fsSL -m 10 "$url" >/dev/null 2>&1; then
    log_info "Successfully notified Uptime Kuma."
  else
    log_warn "Failed to notify Uptime Kuma."
  fi
}

resolve_tag_to_sha() {
  local tag="$1"
  local remote_url="https://github.com/${SPOTIFLAC_REPO}.git"
  local output
  output=$(git ls-remote --tags "$remote_url" "$tag")
  
  local sha
  sha=$(echo "$output" | grep "${tag}\^{}" | awk '{print $1}')
  if [ -z "$sha" ]; then
    sha=$(echo "$output" | grep "${tag}$" | awk '{print $1}')
  fi
  echo "$sha"
}

# 2. Get latest remote version info
REMOTE_SHA=""
REMOTE_VERSION=""

log_info "Monitoring upstream repo: https://github.com/${SPOTIFLAC_REPO} (Mode: ${UPDATE_MODE})"

if [ "$UPDATE_MODE" = "release" ]; then
  RELEASE_JSON=$(curl -fsSL -m 15 "https://api.github.com/repos/${SPOTIFLAC_REPO}/releases/latest" 2>/dev/null || echo "")
  if [ -n "$RELEASE_JSON" ] && echo "$RELEASE_JSON" | jq -e '.tag_name' >/dev/null 2>&1; then
    REMOTE_VERSION=$(echo "$RELEASE_JSON" | jq -r '.tag_name')
  else
    log_warn "GitHub API release retrieval failed. Falling back to git ls-remote tags..."
    REMOTE_VERSION=$(git ls-remote --tags "https://github.com/${SPOTIFLAC_REPO}.git" | awk '{print $2}' | grep -v "\^{}" | sed 's|refs/tags/||' | sort -V | tail -n1)
  fi

  if [ -z "$REMOTE_VERSION" ]; then
    log_error "Failed to resolve latest remote release tag."
    push_uptime_kuma "down" "Error: Failed to resolve latest remote release tag."
    exit 1
  fi
  
  log_info "Latest remote release tag: ${REMOTE_VERSION}"
  REMOTE_SHA=$(resolve_tag_to_sha "$REMOTE_VERSION")
  
  if [ -z "$REMOTE_SHA" ]; then
    log_error "Could not resolve release tag ${REMOTE_VERSION} to a commit SHA."
    push_uptime_kuma "down" "Error: Could not resolve tag ${REMOTE_VERSION} to commit SHA."
    exit 1
  fi
else
  # UPDATE_MODE=commit
  REMOTE_SHA=$(git ls-remote "https://github.com/${SPOTIFLAC_REPO}.git" HEAD | awk '{print $1}')
  REMOTE_VERSION="commit-${REMOTE_SHA:0:12}"
  
  if [ -z "$REMOTE_SHA" ]; then
    log_error "Could not resolve HEAD commit of remote repository."
    push_uptime_kuma "down" "Error: Could not resolve remote HEAD commit."
    exit 1
  fi
fi

REMOTE_SHORT_SHA="${REMOTE_SHA:0:12}"
log_info "Latest remote version commit SHA: ${REMOTE_SHORT_SHA}"

# 3. Get currently recorded version from state
LAST_VERSION_FILE="${STATE_DIR}/last_version"
LOCAL_SHA=""
if [ -f "$LAST_VERSION_FILE" ]; then
  LOCAL_SHA=$(cat "$LAST_VERSION_FILE" | tr -d ' \n')
fi

# 4. Check if update is required
if [ "$LOCAL_SHA" = "$REMOTE_SHORT_SHA" ]; then
  log_info "No update required. Production is already running ${LOCAL_SHA}."
  
  # Check if production API is currently healthy
  log_info "Verifying health of production service..."
  BASE_URL="${BASE_URL%/}"
  if curl -fsSL --connect-timeout 5 -m 10 "${BASE_URL}/health" | jq -e '.ok == true' >/dev/null 2>&1; then
    log_info "Production service is healthy."
    push_uptime_kuma "up" "Production is healthy and up-to-date (SHA: ${LOCAL_SHA})."
    exit 0
  else
    log_warn "Production service at ${BASE_URL}/health is unresponsive!"
    push_uptime_kuma "down" "Production is up-to-date (SHA: ${LOCAL_SHA}) but API healthcheck failed."
    exit 1
  fi
fi

log_info "New upstream version detected: updating from ${LOCAL_SHA:-unknown} to ${REMOTE_SHORT_SHA}..."

# 5. Clone main branch temporarily to WORKDIR
log_info "Cleaning up old workdir and cloning main branch..."
rm -rf "$WORKDIR"
mkdir -p "$(dirname "$WORKDIR")"

if ! git clone --branch "${APP_BRANCH}" --single-branch "${APP_REPO_URL}" "$WORKDIR" 2>&1; then
  log_error "Failed to clone repository ${APP_REPO_URL} (${APP_BRANCH}) into ${WORKDIR}."
  push_uptime_kuma "down" "Error: Failed to clone repository ${APP_REPO_URL}."
  exit 1
fi

# 6. Apply update and perform syntax/tests/docker builds checks
# Move into WORKDIR
cd "$WORKDIR"

# Set Git Author configs for the commit
git config user.name "${GIT_AUTHOR_NAME}"
git config user.email "${GIT_AUTHOR_EMAIL}"

log_info "Updating Go dependency to ${REMOTE_SHA}..."
if ! go get "${GO_MODULE_PATH}@${REMOTE_SHA}" 2>&1; then
  log_error "go get failed."
  push_uptime_kuma "down" "Build check failed: go get ${GO_MODULE_PATH}@${REMOTE_SHORT_SHA} failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Running 'go mod tidy'..."
if ! go mod tidy 2>&1; then
  log_error "go mod tidy failed."
  push_uptime_kuma "down" "Build check failed: go mod tidy failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Running syntax compile checks (go build ./...)..."
if ! go build ./... 2>&1; then
  log_error "Go compile check failed."
  push_uptime_kuma "down" "Build check failed: go build ./... failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Running unit tests (go test ./...)..."
if ! go test ./... 2>&1; then
  log_error "Go tests failed."
  push_uptime_kuma "down" "Build check failed: unit tests failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Verifying Docker Compose configuration..."
if ! docker compose config >/dev/null 2>&1; then
  log_error "Docker compose config validation failed."
  push_uptime_kuma "down" "Build check failed: docker compose config syntax failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Building Docker image locally to check Dockerfile compilation..."
if ! docker compose build --no-cache 2>&1; then
  log_error "Docker image build failed."
  push_uptime_kuma "down" "Build check failed: docker compose build failed."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "All build, test, and container compilation checks passed! Preparing commit..."

# 7. Commit changes and push to remote main branch
git add go.mod go.sum
COMMIT_MSG="Auto-update ${GO_MODULE_PATH} to ${REMOTE_VERSION} (${REMOTE_SHORT_SHA})"
git commit -m "$COMMIT_MSG"

log_info "Pushing commit to remote branch '${APP_BRANCH}'..."
if ! git push origin "${APP_BRANCH}" 2>&1; then
  log_error "Failed to push updates to remote Git repository."
  push_uptime_kuma "down" "Error: Failed to push Go updates to remote Git branch."
  rm -rf "$WORKDIR"
  exit 1
fi

# 8. Trigger Coolify deployment
if [ -z "${COOLIFY_REDEPLOY_URL:-}" ]; then
  log_error "COOLIFY_REDEPLOY_URL is not set. Deployment cannot proceed."
  push_uptime_kuma "down" "Error: COOLIFY_REDEPLOY_URL is missing in configuration."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Triggering Coolify redeployment..."
if ! curl -fsSL -m 30 -X GET "${COOLIFY_REDEPLOY_URL}" >/dev/null 2>&1; then
  log_warn "Coolify webhook call returned non-200 or timed out. Checking deployment health anyway..."
fi

# 9. Poll /health to verify deployment
log_info "Waiting for service to become healthy (Timeout: ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
BASE_URL="${BASE_URL%/}"
HEALTH_URL="${BASE_URL}/health"
SERVICE_UP=false
START_TIME=$(date +%s)

while true; do
  CURRENT_TIME=$(date +%s)
  ELAPSED=$((CURRENT_TIME - START_TIME))
  
  log_info "Polling health endpoint (${ELAPSED}s / ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
  if curl -fsSL --connect-timeout 2 -m 4 "$HEALTH_URL" | jq -e '.ok == true' >/dev/null 2>&1; then
    SERVICE_UP=true
    log_info "Service is up and responding /health."
    break
  fi
  
  if [ "$ELAPSED" -ge "$COOLIFY_DEPLOY_TIMEOUT_SECONDS" ]; then
    break
  fi
  sleep 4
done

# 10. Run smoke test on deployed API
SMOKE_PASSED=false
if [ "$SERVICE_UP" = true ]; then
  log_info "Running post-deploy smoke test..."
  if "${SCRIPT_DIR}/smoke-test.sh"; then
    SMOKE_PASSED=true
    log_info "Smoke test passed successfully!"
  else
    log_error "Post-deploy smoke test failed."
  fi
fi

# 11. Handle deployment failures (Rollback via Git Revert)
if [ "$SMOKE_PASSED" = false ]; then
  log_warn "Deployment or verification failed! Initiating Git revert rollback..."
  
  # Perform revert in workdir
  cd "$WORKDIR"
  log_info "Creating revert commit..."
  if git revert HEAD --no-edit 2>&1; then
    log_info "Revert commit created. Pushing to remote '${APP_BRANCH}'..."
    if git push origin "${APP_BRANCH}" 2>&1; then
      log_info "Push successful. Re-triggering Coolify deployment for rollback..."
      curl -fsSL -m 30 -X GET "${COOLIFY_REDEPLOY_URL}" >/dev/null 2>&1 || true
      
      # Wait for previous healthy state to recover
      log_info "Waiting for restored service to recover..."
      for i in {1..30}; do
        if curl -fsSL --connect-timeout 2 -m 4 "$HEALTH_URL" | jq -e '.ok == true' >/dev/null 2>&1; then
          log_info "Restored service recovered successfully."
          break
        fi
        sleep 2
      done
    else
      log_error "Failed to push revert commit to remote repository."
    fi
  else
    log_error "Failed to create git revert commit locally."
  fi
  
  push_uptime_kuma "down" "Deploy failed for ${REMOTE_VERSION} (${REMOTE_SHORT_SHA}). Rollback initiated."
  rm -rf "$WORKDIR"
  exit 1
fi

# 12. Complete update successfully
mkdir -p "$STATE_DIR"
echo "$REMOTE_SHORT_SHA" > "$LAST_VERSION_FILE"
log_info "Saved new version state: ${REMOTE_SHORT_SHA}"

push_uptime_kuma "up" "Successfully updated and deployed SpotiFLAC to ${REMOTE_VERSION} (${REMOTE_SHORT_SHA})."
rm -rf "$WORKDIR"
exit 0
EOF
