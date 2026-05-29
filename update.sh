#!/usr/bin/env bash
# Main SpotiFLAC update orchestration script.
# Designed to run in /opt/spotiflac-updater on the server.
# Keeps the core API branch updated, verified, and safely deployed via Coolify.

set -euo pipefail

# -----------------------------------------------------------------------------
# Load configuration
# -----------------------------------------------------------------------------

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

# -----------------------------------------------------------------------------
# Logging
# -----------------------------------------------------------------------------

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

# -----------------------------------------------------------------------------
# Defaults
# -----------------------------------------------------------------------------

SPOTIFLAC_REPO="${SPOTIFLAC_REPO:-spotbye/SpotiFLAC}"
GO_MODULE_PATH="${GO_MODULE_PATH:-github.com/afkarxyz/SpotiFLAC}"
UPDATE_MODE="${UPDATE_MODE:-release}"

# Your production branch is master, not main.
APP_BRANCH="${APP_BRANCH:-master}"

WORKDIR="${WORKDIR:-/tmp/spotiflacapi-update-workdir}"
STATE_DIR="${STATE_DIR:-${SCRIPT_DIR}/state}"

BASE_URL="${BASE_URL:-http://127.0.0.1:18150}"
BASE_URL="${BASE_URL%/}"

PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-$BASE_URL}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"

COOLIFY_DEPLOY_TIMEOUT_SECONDS="${COOLIFY_DEPLOY_TIMEOUT_SECONDS:-600}"

GIT_AUTHOR_NAME="${GIT_AUTHOR_NAME:-spotiflac-updater}"
GIT_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-spotiflac-updater@naz4rimusic.com}"

COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-spotiflacapi}"
HOST_BIND_IP="${HOST_BIND_IP:-127.0.0.1}"
HOST_PORT="${HOST_PORT:-18150}"
CONTAINER_PORT="${CONTAINER_PORT:-8080}"
FFMPEG_AUTO_INSTALL="${FFMPEG_AUTO_INSTALL:-false}"

# -----------------------------------------------------------------------------
# Lock
# -----------------------------------------------------------------------------

LOCK_FILE="${SCRIPT_DIR}/.update.lock"
exec 200>"$LOCK_FILE"

if ! flock -n 200; then
  log_error "Another update process is currently active. Exiting."
  exit 1
fi

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------

push_uptime_kuma() {
  local status="$1"
  local msg="$2"

  if [ -z "${UPTIME_KUMA_UPDATE_PUSH_URL:-}" ]; then
    log_info "UPTIME_KUMA_UPDATE_PUSH_URL is not set. Skipping push."
    return 0
  fi

  local encoded_msg
  encoded_msg="$(echo -n "$msg" | jq -sRr @uri)"

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

trigger_coolify_deploy() {
  if [ -z "${COOLIFY_REDEPLOY_URL:-}" ]; then
    log_error "COOLIFY_REDEPLOY_URL is not set. Deployment cannot proceed."
    return 1
  fi

  if [ -z "${COOLIFY_API_TOKEN:-}" ]; then
    log_error "COOLIFY_API_TOKEN is not set. Deployment cannot proceed."
    return 1
  fi

  log_info "Triggering Coolify redeployment..."

  local response_file="/tmp/spotiflac-coolify-deploy-response.txt"
  local error_file="/tmp/spotiflac-coolify-deploy-error.txt"

  : > "$response_file"
  : > "$error_file"

  if curl -fsSL \
    --connect-timeout 10 \
    --max-time 60 \
    -X POST "${COOLIFY_REDEPLOY_URL}" \
    -H "Authorization: Bearer ${COOLIFY_API_TOKEN}" \
    -H "Content-Type: application/json" \
    >"$response_file" 2>"$error_file"; then
    log_info "Coolify redeployment triggered successfully."
    return 0
  fi

  log_error "Coolify redeployment request failed."

  if [ -s "$error_file" ]; then
    log_error "Coolify curl error: $(cat "$error_file")"
  fi

  if [ -s "$response_file" ]; then
    log_error "Coolify response: $(cat "$response_file")"
  fi

  return 1
}

resolve_tag_to_sha() {
  local tag="$1"
  local remote_url="https://github.com/${SPOTIFLAC_REPO}.git"

  local output
  output="$(git ls-remote --tags "$remote_url" "$tag")"

  local sha
  sha="$(echo "$output" | grep "${tag}\^{}" | awk '{print $1}' || true)"

  if [ -z "$sha" ]; then
    sha="$(echo "$output" | grep "${tag}$" | awk '{print $1}' || true)"
  fi

  echo "$sha"
}

wait_for_public_health() {
  local health_url="${PUBLIC_BASE_URL}/health"
  local service_up=false
  local start_time
  start_time="$(date +%s)"

  log_info "Waiting for service to become healthy (Timeout: ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
  log_info "Public health URL: ${health_url}"

  while true; do
    local current_time
    local elapsed

    current_time="$(date +%s)"
    elapsed=$((current_time - start_time))

    log_info "Polling health endpoint (${elapsed}s / ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."

    if curl -fsSL --connect-timeout 2 -m 4 "$health_url" | jq -e '.ok == true' >/dev/null 2>&1; then
      service_up=true
      log_info "Service is up and responding /health."
      break
    fi

    if [ "$elapsed" -ge "$COOLIFY_DEPLOY_TIMEOUT_SECONDS" ]; then
      break
    fi

    sleep 4
  done

  if [ "$service_up" = true ]; then
    return 0
  fi

  return 1
}

create_minimal_compose_env() {
  local target_env="${WORKDIR}/.env"

  log_info "Creating minimal Docker Compose .env for temporary validation..."

  # Do NOT copy the updater .env. It contains Coolify/GitHub secrets.
  cat > "$target_env" <<EOF
COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME}
HOST_BIND_IP=${HOST_BIND_IP}
HOST_PORT=${HOST_PORT}
CONTAINER_PORT=${CONTAINER_PORT}
FFMPEG_AUTO_INSTALL=${FFMPEG_AUTO_INSTALL}
EOF
}

cleanup_workdir() {
  rm -rf "$WORKDIR"
}

# -----------------------------------------------------------------------------
# Resolve upstream version
# -----------------------------------------------------------------------------

REMOTE_SHA=""
REMOTE_VERSION=""

log_info "Monitoring upstream repo: https://github.com/${SPOTIFLAC_REPO} (Mode: ${UPDATE_MODE})"

if [ "$UPDATE_MODE" = "release" ]; then
  RELEASE_JSON="$(curl -fsSL -m 15 "https://api.github.com/repos/${SPOTIFLAC_REPO}/releases/latest" 2>/dev/null || echo "")"

  if [ -n "$RELEASE_JSON" ] && echo "$RELEASE_JSON" | jq -e '.tag_name' >/dev/null 2>&1; then
    REMOTE_VERSION="$(echo "$RELEASE_JSON" | jq -r '.tag_name')"
  else
    log_warn "GitHub API release retrieval failed. Falling back to git ls-remote tags..."
    REMOTE_VERSION="$(
      git ls-remote --tags "https://github.com/${SPOTIFLAC_REPO}.git" \
        | awk '{print $2}' \
        | grep -v "\^{}" \
        | sed 's|refs/tags/||' \
        | sort -V \
        | tail -n1
    )"
  fi

  if [ -z "$REMOTE_VERSION" ]; then
    log_error "Failed to resolve latest remote release tag."
    push_uptime_kuma "down" "Error: Failed to resolve latest remote release tag."
    exit 1
  fi

  log_info "Latest remote release tag: ${REMOTE_VERSION}"

  REMOTE_SHA="$(resolve_tag_to_sha "$REMOTE_VERSION")"

  if [ -z "$REMOTE_SHA" ]; then
    log_error "Could not resolve release tag ${REMOTE_VERSION} to a commit SHA."
    push_uptime_kuma "down" "Error: Could not resolve tag ${REMOTE_VERSION} to commit SHA."
    exit 1
  fi
else
  REMOTE_SHA="$(git ls-remote "https://github.com/${SPOTIFLAC_REPO}.git" HEAD | awk '{print $1}')"
  REMOTE_VERSION="commit-${REMOTE_SHA:0:12}"

  if [ -z "$REMOTE_SHA" ]; then
    log_error "Could not resolve HEAD commit of remote repository."
    push_uptime_kuma "down" "Error: Could not resolve remote HEAD commit."
    exit 1
  fi
fi

REMOTE_SHORT_SHA="${REMOTE_SHA:0:12}"
log_info "Latest remote version commit SHA: ${REMOTE_SHORT_SHA}"

# -----------------------------------------------------------------------------
# Check current state
# -----------------------------------------------------------------------------

mkdir -p "$STATE_DIR"

LAST_VERSION_FILE="${STATE_DIR}/last_version"
LOCAL_SHA=""

if [ -f "$LAST_VERSION_FILE" ]; then
  LOCAL_SHA="$(cat "$LAST_VERSION_FILE" | tr -d ' \n')"
fi

if [ "$LOCAL_SHA" = "$REMOTE_SHORT_SHA" ]; then
  log_info "No update required. Production is already running ${LOCAL_SHA}."
  log_info "Verifying public health of production service..."

  if wait_for_public_health; then
    push_uptime_kuma "up" "Production is healthy and up-to-date (SHA: ${LOCAL_SHA})."
    exit 0
  fi

  log_warn "Production public healthcheck failed at ${PUBLIC_BASE_URL}/health."
  push_uptime_kuma "down" "Production is up-to-date (SHA: ${LOCAL_SHA}) but public healthcheck failed."
  exit 1
fi

log_info "New upstream version detected: updating from ${LOCAL_SHA:-unknown} to ${REMOTE_SHORT_SHA}..."
log_info "App branch: ${APP_BRANCH}"
log_info "Public base URL: ${PUBLIC_BASE_URL}"
log_info "Smoke test base URL: ${BASE_URL}"
log_info "Compose host binding: ${HOST_BIND_IP}:${HOST_PORT}->${CONTAINER_PORT}"

# -----------------------------------------------------------------------------
# Clone app branch temporarily
# -----------------------------------------------------------------------------

log_info "Cleaning up old workdir and cloning app branch..."
cleanup_workdir
mkdir -p "$(dirname "$WORKDIR")"

if ! git clone --branch "${APP_BRANCH}" --single-branch "${APP_REPO_URL}" "$WORKDIR" 2>&1; then
  log_error "Failed to clone repository ${APP_REPO_URL} (${APP_BRANCH}) into ${WORKDIR}."
  push_uptime_kuma "down" "Error: Failed to clone repository ${APP_REPO_URL}."
  cleanup_workdir
  exit 1
fi

cd "$WORKDIR"

create_minimal_compose_env

git config user.name "${GIT_AUTHOR_NAME}"
git config user.email "${GIT_AUTHOR_EMAIL}"

# -----------------------------------------------------------------------------
# Update dependency and validate build
# -----------------------------------------------------------------------------

log_info "Updating Go dependency to ${REMOTE_SHA}..."

if ! go get "${GO_MODULE_PATH}@${REMOTE_SHA}" 2>&1; then
  log_error "go get failed."
  push_uptime_kuma "down" "Build check failed: go get ${GO_MODULE_PATH}@${REMOTE_SHORT_SHA} failed."
  cleanup_workdir
  exit 1
fi

log_info "Running 'go mod tidy'..."

if ! go mod tidy 2>&1; then
  log_error "go mod tidy failed."
  push_uptime_kuma "down" "Build check failed: go mod tidy failed."
  cleanup_workdir
  exit 1
fi

log_info "Running syntax compile checks (go build ./...)..."

if ! go build ./... 2>&1; then
  log_error "Go compile check failed."
  push_uptime_kuma "down" "Build check failed: go build ./... failed."
  cleanup_workdir
  exit 1
fi

log_info "Running unit tests (go test ./...)..."

if ! go test ./... 2>&1; then
  log_error "Go tests failed."
  push_uptime_kuma "down" "Build check failed: unit tests failed."
  cleanup_workdir
  exit 1
fi

log_info "Verifying Docker Compose configuration..."

if ! docker compose config >/dev/null 2>&1; then
  log_error "Docker compose config validation failed."
  push_uptime_kuma "down" "Build check failed: docker compose config syntax failed."
  cleanup_workdir
  exit 1
fi

log_info "Building Docker image locally to check Dockerfile compilation..."

if ! docker compose build --no-cache 2>&1; then
  log_error "Docker image build failed."
  push_uptime_kuma "down" "Build check failed: docker compose build failed."
  cleanup_workdir
  exit 1
fi

log_info "All build, test, and container compilation checks passed."

# -----------------------------------------------------------------------------
# Commit and push update
# -----------------------------------------------------------------------------

if git diff --quiet go.mod go.sum; then
  log_warn "No go.mod/go.sum changes were produced even though upstream SHA differs."
  log_warn "Saving state to avoid repeated attempts for the same upstream SHA."
  echo "$REMOTE_SHORT_SHA" > "$LAST_VERSION_FILE"
  push_uptime_kuma "up" "No dependency changes needed for ${REMOTE_VERSION} (${REMOTE_SHORT_SHA}). State updated."
  cleanup_workdir
  exit 0
fi

git add go.mod go.sum

COMMIT_MSG="Auto-update ${GO_MODULE_PATH} to ${REMOTE_VERSION} (${REMOTE_SHORT_SHA})"

git commit -m "$COMMIT_MSG"

log_info "Pushing commit to remote branch '${APP_BRANCH}'..."

if ! git push origin "${APP_BRANCH}" 2>&1; then
  log_error "Failed to push updates to remote Git repository."
  push_uptime_kuma "down" "Error: Failed to push Go updates to remote Git branch."
  cleanup_workdir
  exit 1
fi

# -----------------------------------------------------------------------------
# Deploy through Coolify
# -----------------------------------------------------------------------------

if ! trigger_coolify_deploy; then
  push_uptime_kuma "down" "Error: Coolify redeploy request failed."
  cleanup_workdir
  exit 1
fi

# -----------------------------------------------------------------------------
# Post-deploy verification
# -----------------------------------------------------------------------------

SERVICE_UP=false

if wait_for_public_health; then
  SERVICE_UP=true
fi

SMOKE_PASSED=false

if [ "$SERVICE_UP" = true ]; then
  log_info "Running post-deploy smoke test..."
  log_info "Smoke test base URL: ${BASE_URL}"

  if "${SCRIPT_DIR}/smoke-test.sh"; then
    SMOKE_PASSED=true
    log_info "Smoke test passed successfully."
  else
    log_error "Post-deploy smoke test failed."
  fi
else
  log_error "Service did not become healthy after deployment."
fi

# -----------------------------------------------------------------------------
# Rollback via Git revert + Coolify redeploy
# -----------------------------------------------------------------------------

if [ "$SMOKE_PASSED" = false ]; then
  log_warn "Deployment or verification failed. Initiating Git revert rollback..."

  cd "$WORKDIR"

  log_info "Creating revert commit..."

  if git revert HEAD --no-edit 2>&1; then
    log_info "Revert commit created. Pushing to remote '${APP_BRANCH}'..."

    if git push origin "${APP_BRANCH}" 2>&1; then
      log_info "Push successful. Re-triggering Coolify deployment for rollback..."

      if ! trigger_coolify_deploy; then
        log_error "Failed to trigger Coolify deployment for rollback."
      fi

      log_info "Waiting for restored service to recover..."

      if wait_for_public_health; then
        log_info "Restored service recovered successfully."
      else
        log_error "Rollback was triggered, but public health did not recover within timeout."
      fi
    else
      log_error "Failed to push revert commit to remote repository."
    fi
  else
    log_error "Failed to create git revert commit locally."
  fi

  push_uptime_kuma "down" "Deploy failed for ${REMOTE_VERSION} (${REMOTE_SHORT_SHA}). Rollback initiated."
  cleanup_workdir
  exit 1
fi

# -----------------------------------------------------------------------------
# Success
# -----------------------------------------------------------------------------

echo "$REMOTE_SHORT_SHA" > "$LAST_VERSION_FILE"
log_info "Saved new version state: ${REMOTE_SHORT_SHA}"

push_uptime_kuma "up" "Successfully updated and deployed SpotiFLAC to ${REMOTE_VERSION} (${REMOTE_SHORT_SHA})."

cleanup_workdir
exit 0
