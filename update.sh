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

# Pinned settings for Go and Python upstreams
GO_SPOTIFLAC_REPO=${GO_SPOTIFLAC_REPO:-spotbye/SpotiFLAC}
GO_MODULE_PATH=${GO_MODULE_PATH:-github.com/afkarxyz/SpotiFLAC}
GO_UPDATE_MODE=${GO_UPDATE_MODE:-release}

PYTHON_SPOTIFLAC_REPO=${PYTHON_SPOTIFLAC_REPO:-https://github.com/ShuShuzinhuu/SpotiFLAC-Module-Version.git}
PYTHON_UPDATE_MODE=${PYTHON_UPDATE_MODE:-commit}
PYTHON_REF_FILE=${PYTHON_REF_FILE:-.python-spotiflac-ref}

# Backwards compatibility check
if [ -n "${SPOTIFLAC_REPO:-}" ] && [ "${SPOTIFLAC_REPO}" != "spotbye/SpotiFLAC" ]; then
  GO_SPOTIFLAC_REPO="${SPOTIFLAC_REPO}"
fi
if [ -n "${UPDATE_MODE:-}" ] && [ "${UPDATE_MODE}" != "release" ]; then
  GO_UPDATE_MODE="${UPDATE_MODE}"
fi

APP_BRANCH=${APP_BRANCH:-main}
WORKDIR=${WORKDIR:-/tmp/spotiflacapi-update-workdir}
STATE_DIR=${STATE_DIR:-${SCRIPT_DIR}/state}
COOLIFY_DEPLOY_TIMEOUT_SECONDS=${COOLIFY_DEPLOY_TIMEOUT_SECONDS:-600}
GIT_AUTHOR_NAME=${GIT_AUTHOR_NAME:-spotiflac-updater}
GIT_AUTHOR_EMAIL=${GIT_AUTHOR_EMAIL:-spotiflac-updater@naz4rimusic.com}
SMOKE_STRATEGY=${SMOKE_STRATEGY:-race}

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
  local repo="$1"
  local tag="$2"
  local remote_url="https://github.com/${repo}.git"
  local output
  output=$(git ls-remote --tags "$remote_url" "$tag")
  
  local sha
  sha=$(echo "$output" | grep "${tag}\^{}" | awk '{print $1}')
  if [ -z "$sha" ]; then
    sha=$(echo "$output" | grep "${tag}$" | awk '{print $1}')
  fi
  echo "$sha"
}

# 2. Get latest remote Go version info
GO_REMOTE_SHA=""
GO_REMOTE_VERSION=""
log_info "Monitoring Go upstream: https://github.com/${GO_SPOTIFLAC_REPO} (Mode: ${GO_UPDATE_MODE})"

if [ "$GO_UPDATE_MODE" = "release" ]; then
  RELEASE_JSON=$(curl -fsSL -m 15 "https://api.github.com/repos/${GO_SPOTIFLAC_REPO}/releases/latest" 2>/dev/null || echo "")
  if [ -n "$RELEASE_JSON" ] && echo "$RELEASE_JSON" | jq -e '.tag_name' >/dev/null 2>&1; then
    GO_REMOTE_VERSION=$(echo "$RELEASE_JSON" | jq -r '.tag_name')
  else
    log_warn "GitHub API release retrieval failed for Go. Falling back to git ls-remote tags..."
    GO_REMOTE_VERSION=$(git ls-remote --tags "https://github.com/${GO_SPOTIFLAC_REPO}.git" | awk '{print $2}' | grep -v "\^{}" | sed 's|refs/tags/||' | sort -V | tail -n1)
  fi

  if [ -z "$GO_REMOTE_VERSION" ]; then
    log_error "Failed to resolve latest remote Go release tag."
    push_uptime_kuma "down" "Error: Failed to resolve latest remote Go release tag."
    exit 1
  fi
  
  log_info "Latest Go remote release tag: ${GO_REMOTE_VERSION}"
  GO_REMOTE_SHA=$(resolve_tag_to_sha "${GO_SPOTIFLAC_REPO}" "${GO_REMOTE_VERSION}")
  
  if [ -z "$GO_REMOTE_SHA" ]; then
    log_error "Could not resolve Go release tag ${GO_REMOTE_VERSION} to a commit SHA."
    push_uptime_kuma "down" "Error: Could not resolve Go tag ${GO_REMOTE_VERSION} to commit SHA."
    exit 1
  fi
else
  # GO_UPDATE_MODE=commit
  GO_REMOTE_SHA=$(git ls-remote "https://github.com/${GO_SPOTIFLAC_REPO}.git" HEAD | awk '{print $1}')
  GO_REMOTE_VERSION="commit-${GO_REMOTE_SHA:0:12}"
  
  if [ -z "$GO_REMOTE_SHA" ]; then
    log_error "Could not resolve HEAD commit of Go remote repository."
    push_uptime_kuma "down" "Error: Could not resolve Go remote HEAD commit."
    exit 1
  fi
fi

GO_REMOTE_SHORT_SHA="${GO_REMOTE_SHA:0:12}"
log_info "Latest Go remote version commit SHA: ${GO_REMOTE_SHORT_SHA}"

# 3. Get latest remote Python version info
PYTHON_REMOTE_SHA=""
PYTHON_REMOTE_VERSION=""
log_info "Monitoring Python upstream: ${PYTHON_SPOTIFLAC_REPO} (Mode: ${PYTHON_UPDATE_MODE})"

if [ "$PYTHON_UPDATE_MODE" = "commit" ]; then
  PYTHON_REMOTE_SHA=$(git ls-remote "${PYTHON_SPOTIFLAC_REPO}" HEAD | awk '{print $1}')
  PYTHON_REMOTE_VERSION="commit-${PYTHON_REMOTE_SHA:0:12}"
  
  if [ -z "$PYTHON_REMOTE_SHA" ]; then
    log_error "Could not resolve HEAD commit of Python remote repository."
    push_uptime_kuma "down" "Error: Could not resolve Python remote HEAD commit."
    exit 1
  fi
else
  log_error "Unsupported Python update mode: ${PYTHON_UPDATE_MODE}"
  exit 1
fi

PYTHON_REMOTE_SHORT_SHA="${PYTHON_REMOTE_SHA:0:12}"
log_info "Latest Python remote version commit SHA: ${PYTHON_REMOTE_SHORT_SHA}"

# 4. Get currently recorded versions from state
LAST_GO_VERSION_FILE="${STATE_DIR}/last_go_version"
LAST_PYTHON_VERSION_FILE="${STATE_DIR}/last_python_version"

LOCAL_GO_SHA=""
if [ -f "$LAST_GO_VERSION_FILE" ]; then
  LOCAL_GO_SHA=$(cat "$LAST_GO_VERSION_FILE" | tr -d ' \n')
fi

# Support backward compatibility with old state layout
if [ -z "$LOCAL_GO_SHA" ] && [ -f "${STATE_DIR}/last_version" ]; then
  LOCAL_GO_SHA=$(cat "${STATE_DIR}/last_version" | tr -d ' \n')
fi

LOCAL_PYTHON_SHA=""
if [ -f "$LAST_PYTHON_VERSION_FILE" ]; then
  LOCAL_PYTHON_SHA=$(cat "$LAST_PYTHON_VERSION_FILE" | tr -d ' \n')
fi

# 5. Check if update is required
GO_CHANGED=false
PYTHON_CHANGED=false

if [ "$LOCAL_GO_SHA" != "$GO_REMOTE_SHORT_SHA" ]; then
  GO_CHANGED=true
fi

if [ "$LOCAL_PYTHON_SHA" != "$PYTHON_REMOTE_SHORT_SHA" ]; then
  PYTHON_CHANGED=true
fi

if [ "$GO_CHANGED" = "false" ] && [ "$PYTHON_CHANGED" = "false" ]; then
  log_info "No update required. Production is already running Go (${LOCAL_GO_SHA:-unknown}) and Python (${LOCAL_PYTHON_SHA:-unknown})."
  
  # Check if production API is currently healthy
  log_info "Verifying health of production service..."
  BASE_URL="${BASE_URL%/}"
  if curl -fsSL --connect-timeout 5 -m 10 "${BASE_URL}/health" | jq -e '.ok == true' >/dev/null 2>&1; then
    log_info "Production service is healthy."
    push_uptime_kuma "up" "Production is healthy and up-to-date (Go: ${LOCAL_GO_SHA}, Python: ${LOCAL_PYTHON_SHA})."
    exit 0
  else
    log_warn "Production service at ${BASE_URL}/health is unresponsive!"
    push_uptime_kuma "down" "Production is up-to-date but API healthcheck failed."
    exit 1
  fi
fi

log_info "New upstream version detected: updating (Go changed=${GO_CHANGED}, Python changed=${PYTHON_CHANGED})..."

# 6. Clone main branch temporarily to WORKDIR
log_info "Cleaning up old workdir and cloning main branch..."
rm -rf "$WORKDIR"
mkdir -p "$(dirname "$WORKDIR")"

if ! git clone --branch "${APP_BRANCH}" --single-branch "${APP_REPO_URL}" "$WORKDIR" 2>&1; then
  log_error "Failed to clone repository ${APP_REPO_URL} (${APP_BRANCH}) into ${WORKDIR}."
  push_uptime_kuma "down" "Error: Failed to clone repository ${APP_REPO_URL}."
  exit 1
fi

# 7. Apply updates
cd "$WORKDIR"

git config user.name "${GIT_AUTHOR_NAME}"
git config user.email "${GIT_AUTHOR_EMAIL}"

COMMIT_PARTS=()

if [ "$GO_CHANGED" = "true" ]; then
  log_info "Updating Go dependency to ${GO_REMOTE_SHA}..."
  if ! go get "${GO_MODULE_PATH}@${GO_REMOTE_SHA}" 2>&1; then
    log_error "go get failed."
    push_uptime_kuma "down" "Build check failed: go get ${GO_MODULE_PATH}@${GO_REMOTE_SHORT_SHA} failed."
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
  
  git add go.mod go.sum
  COMMIT_PARTS+=("go ${GO_REMOTE_VERSION}")
fi

if [ "$PYTHON_CHANGED" = "true" ]; then
  log_info "Updating Python reference inside master branch to ${PYTHON_REMOTE_SHA}..."
  echo "${PYTHON_REMOTE_SHA}" > "${PYTHON_REF_FILE}"
  git add "${PYTHON_REF_FILE}"
  COMMIT_PARTS+=("python ${PYTHON_REMOTE_VERSION}")
fi

# Create a minimal .env file for local compose building without secrets
cat <<EOF > .env
COMPOSE_PROJECT_NAME=spotiflacapi-test
HOST_BIND_IP=127.0.0.1
HOST_PORT=18150
CONTAINER_PORT=8080
FFMPEG_AUTO_INSTALL=false
DOWNLOAD_PROVIDERS=go,python
DEFAULT_DOWNLOAD_PROVIDER_ORDER=go,python
DOWNLOAD_STRATEGY=race
PYTHON_PROVIDER_ENABLED=true
PYTHON_PROVIDER_TIMEOUT_SECONDS=180
EOF

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

# Clean up minimal test .env
rm -f .env

log_info "All build, test, and container compilation checks passed! Preparing commit..."

# 8. Commit changes and push to remote main branch
COMMIT_MSG="Auto-update providers: $(IFS=' / '; echo "${COMMIT_PARTS[*]}")"
git commit -m "$COMMIT_MSG"

log_info "Pushing commit to remote branch '${APP_BRANCH}'..."
if ! git push origin "${APP_BRANCH}" 2>&1; then
  log_error "Failed to push updates to remote Git repository."
  push_uptime_kuma "down" "Error: Failed to push updates to remote Git branch."
  rm -rf "$WORKDIR"
  exit 1
fi

# 9. Trigger Coolify deployment
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

# 10. Poll /health to verify deployment
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

# 11. Run smoke test on deployed API
SMOKE_PASSED=false
if [ "$SERVICE_UP" = true ]; then
  log_info "Running post-deploy smoke test..."
  if SMOKE_STRATEGY="${SMOKE_STRATEGY}" "${SCRIPT_DIR}/smoke-test.sh"; then
    SMOKE_PASSED=true
    log_info "Smoke test passed successfully!"
  else
    log_error "Post-deploy smoke test failed."
  fi
fi

# 12. Handle deployment failures (Rollback via Git Revert)
if [ "$SMOKE_PASSED" = false ]; then
  log_warn "Deployment or verification failed! Initiating Git revert rollback..."
  
  cd "$WORKDIR"
  log_info "Creating revert commit..."
  if git revert HEAD --no-edit 2>&1; then
    log_info "Revert commit created. Pushing to remote '${APP_BRANCH}'..."
    if git push origin "${APP_BRANCH}" 2>&1; then
      log_info "Push successful. Re-triggering Coolify deployment for rollback..."
      curl -fsSL -m 30 -X GET "${COOLIFY_REDEPLOY_URL}" >/dev/null 2>&1 || true
      
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
  
  push_uptime_kuma "down" "Deploy failed for update: $(IFS=' / '; echo "${COMMIT_PARTS[*]}") - Rollback initiated."
  rm -rf "$WORKDIR"
  exit 1
fi

# 13. Complete update successfully
mkdir -p "$STATE_DIR"
if [ "$GO_CHANGED" = "true" ]; then
  echo "$GO_REMOTE_SHORT_SHA" > "$LAST_GO_VERSION_FILE"
  log_info "Saved new Go version state: ${GO_REMOTE_SHORT_SHA}"
fi
if [ "$PYTHON_CHANGED" = "true" ]; then
  echo "$PYTHON_REMOTE_SHORT_SHA" > "$LAST_PYTHON_VERSION_FILE"
  log_info "Saved new Python version state: ${PYTHON_REMOTE_SHORT_SHA}"
fi

push_uptime_kuma "up" "Successfully updated and deployed SpotiFLAC providers: $(IFS=' / '; echo "${COMMIT_PARTS[*]}")"
rm -rf "$WORKDIR"
exit 0
