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

# Telegram notifications (no-op unless TELEGRAM_NOTIFY_TOKEN is set in .env).
if [ -f "${SCRIPT_DIR}/notify.sh" ]; then
  source "${SCRIPT_DIR}/notify.sh"
else
  notify_telegram() { :; }
fi

# refresh_endpoints brings the running API's C2 config up to date BEFORE the
# strict smoke test: it refreshes the Monochrome instance list (INSTANCES.md)
# and detects/imports any new SpotiFLAC-Next version's endpoints. This is what
# makes the app resilient to endpoint changes between releases — the updater
# updates them dynamically instead of relying on hard-coded values. The
# SpotiFLAC (Go upstream) endpoints are refreshed by deploying the new module
# (it resolves them dynamically at runtime).
# Detected versions, populated by refresh_endpoints (for notifications).
NEXT_VERSION="unknown"; NEXT_CHANGED="false"; MONO_API_COUNT="?"; MONO_CHANGED="false"
refresh_endpoints() {
  if [ ! -x "${SCRIPT_DIR}/check-c2-updates.sh" ]; then
    log_warn "check-c2-updates.sh not found; skipping dynamic endpoint refresh."
    return 0
  fi
  log_info "Refreshing dynamic endpoints (Monochrome + SpotiFLAC-Next C2)..."
  # --no-notify: update.sh sends a single consolidated notification with all versions.
  if API_BASE_URL="${BASE_URL%/}" "${SCRIPT_DIR}/check-c2-updates.sh" --apply --no-notify; then
    log_info "Endpoint refresh completed."
  else
    log_warn "Endpoint refresh reported issues (continuing)."
  fi
  # Load the versions check-c2-updates.sh detected (SpotiFLAC-Next + Monochrome).
  if [ -f "${STATE_DIR}/refresh-summary.env" ]; then
    # shellcheck source=/dev/null
    source "${STATE_DIR}/refresh-summary.env" || true
  fi
}

# versions_block prints the new detected version of each component for notifications.
versions_block() {
  printf '• spotiflac: %s (%s)\n• python: %s\n• spotiflac-next: %s%s\n• monochrome: %s API instances%s' \
    "${GO_REMOTE_VERSION:-?}" "${GO_REMOTE_SHORT_SHA:-?}" \
    "${PYTHON_REMOTE_SHORT_SHA:-?}" \
    "${NEXT_VERSION:-?}" "$([ "${NEXT_CHANGED}" = true ] && echo ' (nueva)')" \
    "${MONO_API_COUNT:-?}" "$([ "${MONO_CHANGED}" = true ] && echo ' (cambió)')"
}

# trigger_coolify_deploy hits the Coolify redeploy URL, sending the API bearer
# token (COOLIFY_API_TOKEN) when set, and logs the HTTP status + response body so
# failures are diagnosable instead of a silent "non-200". Coolify's API deploy
# endpoint (/api/v1/deploy?uuid=...) REQUIRES the token. Returns 0 only on 2xx.
trigger_coolify_deploy() {
  local hdr=()
  [ -n "${COOLIFY_API_TOKEN:-}" ] && hdr=(-H "Authorization: Bearer ${COOLIFY_API_TOKEN}")
  local out code body
  out=$(curl -sS -m 30 -X GET "${hdr[@]}" -w $'\n%{http_code}' "${COOLIFY_REDEPLOY_URL}" 2>&1) || true
  code=$(printf '%s' "$out" | tail -n1)
  body=$(printf '%s' "$out" | sed '$d' | head -c 500)
  log_info "Coolify redeploy -> HTTP ${code}"
  [ -n "$body" ] && log_info "Coolify response: ${body}"
  case "$code" in
    2*) return 0 ;;
    401|403) log_warn "Coolify auth failed (HTTP ${code}). Set COOLIFY_API_TOKEN in .env (Coolify API token)."; return 1 ;;
    *) log_warn "Coolify redeploy returned HTTP ${code} (expected 2xx)."; return 1 ;;
  esac
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
  log_info "No Go/Python update required. Production is already running Go (${LOCAL_GO_SHA:-unknown}) and Python (${LOCAL_PYTHON_SHA:-unknown})."

  # Even without a code change, refresh endpoints: SpotiFLAC-Next and Monochrome
  # rotate independently of the Go module, and the running app must follow them.
  refresh_endpoints

  # If SpotiFLAC-Next or Monochrome changed (no Go/Python change here), notify.
  if [ "${NEXT_CHANGED}" = "true" ] || [ "${MONO_CHANGED}" = "true" ]; then
    notify_telegram "🔄 SpotiFLAC: endpoints actualizados (sin cambio de código)" "SpotiFLAC-Next/Monochrome cambiaron; endpoints refrescados en la API.

Versiones detectadas:
$(versions_block)"
  fi

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

# Refresh endpoints now so the notification and the post-deploy smoke use the
# current providers (writes to the SQLite volume the new container will read).
refresh_endpoints

notify_telegram "SpotiFLAC: nueva versión detectada" "Iniciando build/test/deploy. Versiones detectadas:
$(versions_block)"

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

# 8. Commit changes and push to remote main branch.
# If nothing is staged, ${APP_BRANCH} is already at the target version (e.g. a
# previous run pushed the bump but never finished deploying) — skip the commit
# (a bare `git commit` would exit non-zero and abort under `set -e`) and proceed
# straight to (re)deploy + smoke so the already-committed version gets validated.
COMMIT_MSG="Auto-update providers: $(IFS=' / '; echo "${COMMIT_PARTS[*]}")"
if git diff --cached --quiet; then
  log_warn "Nothing to commit — '${APP_BRANCH}' is already at the target version. Proceeding to (re)deploy and validate it."
else
  git commit -m "$COMMIT_MSG"
  log_info "Pushing commit to remote branch '${APP_BRANCH}'..."
  if ! git push origin "${APP_BRANCH}" 2>&1; then
    log_error "Failed to push updates to remote Git repository."
    push_uptime_kuma "down" "Error: Failed to push updates to remote Git branch."
    rm -rf "$WORKDIR"
    exit 1
  fi
fi

# 9. Trigger Coolify deployment
if [ -z "${COOLIFY_REDEPLOY_URL:-}" ]; then
  log_error "COOLIFY_REDEPLOY_URL is not set. Deployment cannot proceed."
  push_uptime_kuma "down" "Error: COOLIFY_REDEPLOY_URL is missing in configuration."
  rm -rf "$WORKDIR"
  exit 1
fi

log_info "Triggering Coolify redeployment..."
if ! trigger_coolify_deploy; then
  log_warn "Coolify redeploy did not return 2xx (see HTTP code above). Checking deployment health anyway..."
fi

# 10. Poll /health to verify deployment.
#
# Crucially, when the Go module changed we wait until /health reports the NEW
# build (its spotiflac_version contains the expected commit SHA) — otherwise the
# old container answers /health immediately and the smoke test would validate the
# stale build, causing a misleading rollback. If the new build never appears
# within the timeout, Coolify did not deploy it (e.g. the redeploy webhook
# failed) and we say so explicitly.
log_info "Waiting for service to become healthy (Timeout: ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
BASE_URL="${BASE_URL%/}"
HEALTH_URL="${BASE_URL}/health"
SERVICE_UP=false
START_TIME=$(date +%s)

EXPECT_SHA=""
[ "$GO_CHANGED" = "true" ] && EXPECT_SHA="$GO_REMOTE_SHORT_SHA"

while true; do
  ELAPSED=$(( $(date +%s) - START_TIME ))
  HEALTH_JSON=$(curl -fsSL --connect-timeout 2 -m 4 "$HEALTH_URL" 2>/dev/null || echo "")

  if echo "$HEALTH_JSON" | jq -e '.ok == true' >/dev/null 2>&1; then
    if [ -z "$EXPECT_SHA" ]; then
      SERVICE_UP=true
      log_info "Service is up and responding /health."
      break
    fi
    DEPLOYED_VER=$(echo "$HEALTH_JSON" | jq -r '.spotiflac_version // ""')
    if echo "$DEPLOYED_VER" | grep -q "$EXPECT_SHA"; then
      SERVICE_UP=true
      log_info "New build is live (spotiflac ${DEPLOYED_VER}) after ${ELAPSED}s."
      break
    fi
    log_info "Service up but still the old build (${DEPLOYED_VER:-?}); waiting for ${EXPECT_SHA} (${ELAPSED}s / ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
  else
    log_info "Polling health endpoint (${ELAPSED}s / ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s)..."
  fi

  if [ "$ELAPSED" -ge "$COOLIFY_DEPLOY_TIMEOUT_SECONDS" ]; then
    break
  fi
  sleep 4
done

if [ "$SERVICE_UP" != true ] && [ -n "$EXPECT_SHA" ]; then
  log_error "The new build (spotiflac ${EXPECT_SHA}) never went live within ${COOLIFY_DEPLOY_TIMEOUT_SECONDS}s."
  log_error "Coolify did not deploy the pushed commit — check COOLIFY_REDEPLOY_URL / Coolify auto-deploy."
fi

# 11. Run smoke test on deployed API
SMOKE_PASSED=false
if [ "$SERVICE_UP" = true ]; then
  # Endpoints were already refreshed pre-deploy (into the persistent SQLite
  # volume the new container reads), so the smoke validates current providers.
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
      trigger_coolify_deploy || true
      
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
  notify_telegram "⚠️ SpotiFLAC: update FALLÓ (rollback)" "El deploy/smoke falló y se hizo git revert en ${APP_BRANCH}.

Versiones detectadas:
$(versions_block)"
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
notify_telegram "✅ SpotiFLAC: actualizado y desplegado" "Push a ${APP_BRANCH} + redeploy Coolify OK, smoke test pasado.

Versiones nuevas:
$(versions_block)"
rm -rf "$WORKDIR"
exit 0
