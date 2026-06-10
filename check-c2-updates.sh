#!/usr/bin/env bash
#
# Keep the API's dynamic config current and detect new SpotiFLAC-Next releases.
#
# What it does (lightweight; no binary download):
#   1. Refresh the Monochrome instance list from the canonical INSTANCES.md
#      (--apply pushes it to the running API: monochrome.* settings).
#   2. Detect the latest SpotiFLAC-Next version (scrape the Drive folder; no gdown,
#      no download) and notify on change.
#
# It no longer imports the binary's C2: download endpoints are the live per-variant
# pool ({prefix}-{variant}.spotbye.qzz.io/api/dl) that the API resolves AT RUNTIME
# from the status payload + the spotbye.base_domain setting. (For manual inspection
# use update-c2-from-binary.sh; pool base domain/path are editable at /admin.)
#
# Intended to run on the host (same systemd timer as update.sh). Requires: python3.
#
# Env / flags:
#   API_BASE_URL          API base for monochrome settings (default http://127.0.0.1:8080)
#   STATE_DIR             where the last seen version is recorded (default ./state)
#   SPOTIFLAC_NEXT_GIST   gist id (default b2f7b815b1560d7a58d7dd847f073f00)
#   --apply               push refreshed Monochrome instances to the API
#   --version vX.Y.Z      force a specific version instead of latest
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Locate the Go API helper scripts. In a full master checkout they live in
# ../scripts; on the flat spotiflac-updater branch they sit next to this file.
# Override with API_SCRIPTS_DIR if needed.
if [[ -z "${API_SCRIPTS_DIR:-}" ]]; then
  if [[ -f "$SCRIPT_DIR/../scripts/extract-spotiflac-next.py" ]]; then
    API_SCRIPTS_DIR="$SCRIPT_DIR/../scripts"
  else
    API_SCRIPTS_DIR="$SCRIPT_DIR"
  fi
fi
EXTRACTOR="$API_SCRIPTS_DIR/extract-spotiflac-next.py"
FETCHER="$API_SCRIPTS_DIR/fetch-latest-next.py"
REF_MANIFEST="${REF_MANIFEST:-$API_SCRIPTS_DIR/c2-manifest.json}"

API_BASE_URL="${API_BASE_URL:-http://127.0.0.1:8080}"
STATE_DIR="${STATE_DIR:-$SCRIPT_DIR/state}"
GIST_ID="${SPOTIFLAC_NEXT_GIST:-b2f7b815b1560d7a58d7dd847f073f00}"
APPLY=0
FORCE_VERSION=""

NO_NOTIFY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --version) FORCE_VERSION="$2"; shift 2 ;;
    --no-notify) NO_NOTIFY=1; shift ;;  # caller (update.sh) sends a consolidated notification
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# write_summary records the detected versions so update.sh can include them in a
# single consolidated Telegram notification.
SUMMARY_FILE="${STATE_DIR}/refresh-summary.env"
write_summary() {
  {
    echo "NEXT_VERSION=${NEXT_VERSION:-unknown}"
    echo "NEXT_PREV_VERSION=${LAST_VERSION:-}"
    echo "NEXT_CHANGED=${NEXT_CHANGED:-false}"
    echo "MONO_API_COUNT=${MONO_API_COUNT:-0}"
    echo "MONO_STREAM_COUNT=${MONO_STREAM_COUNT:-0}"
    echo "MONO_CHANGED=${MONO_CHANGED:-false}"
  } > "$SUMMARY_FILE"
}

# Telegram notifications (no-op unless TELEGRAM_NOTIFY_TOKEN is set).
if [[ -f "$SCRIPT_DIR/.env" ]]; then set -a; source "$SCRIPT_DIR/.env"; set +a; fi
if [[ -f "$SCRIPT_DIR/notify.sh" ]]; then source "$SCRIPT_DIR/notify.sh"; else notify_telegram() { :; }; fi

mkdir -p "$STATE_DIR"
LAST_VERSION_FILE="$STATE_DIR/last_next_version"
LAST_VERSION="$(cat "$LAST_VERSION_FILE" 2>/dev/null || echo "")"

# Refresh Monochrome instances from the canonical INSTANCES.md (independent of
# the SpotiFLAC-Next release cycle; community instances change on their own).
MONO_API_COUNT=0
MONO_STREAM_COUNT=0
MONO_CHANGED=false
MONO_FETCHER="$API_SCRIPTS_DIR/fetch-monochrome-instances.py"
if [[ -f "$MONO_FETCHER" ]]; then
  log "Refreshing Monochrome instances from upstream INSTANCES.md..."
  # Parse the current list once (to count + detect change), then apply it. The
  # API stores it as a single CSV setting, so applying fully REPLACES the list —
  # only the latest instances are kept, nothing accumulates.
  MONO_JSON="$(python3 "$MONO_FETCHER" --json 2>/dev/null || echo '{}')"
  MONO_API_COUNT=$(echo "$MONO_JSON" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("api_instances",[])))' 2>/dev/null || echo 0)
  MONO_STREAM_COUNT=$(echo "$MONO_JSON" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("streaming_instances",[])))' 2>/dev/null || echo 0)
  # Detect whether the instance set changed since last run (stable signature).
  MONO_SIG=$(echo "$MONO_JSON" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(",".join(sorted(d.get("api_instances",[])+d.get("streaming_instances",[]))))' 2>/dev/null || echo "")
  MONO_SIG_FILE="$STATE_DIR/last_monochrome_sig"
  if [[ -n "$MONO_SIG" && "$MONO_SIG" != "$(cat "$MONO_SIG_FILE" 2>/dev/null || echo)" ]]; then
    MONO_CHANGED=true
  fi
  if [[ "$APPLY" == "1" ]]; then
    if python3 "$MONO_FETCHER" --apply --api "$API_BASE_URL"; then
      [[ -n "$MONO_SIG" ]] && echo "$MONO_SIG" > "$MONO_SIG_FILE"
      log "Monochrome instances applied (${MONO_API_COUNT} API, ${MONO_STREAM_COUNT} streaming; replaced in full)."
    else
      log "monochrome refresh failed (non-fatal)"
    fi
  else
    log "monochrome list parsed: ${MONO_API_COUNT} API instances (dry-run; use --apply to push)"
  fi
fi

# Monochrome is done; persist what we know so far so the consolidated
# notification has the Monochrome counts even if the (heavier) SpotiFLAC-Next
# step below can't run on this host.
write_summary

# 1) Detect the latest SpotiFLAC-Next version CHEAPLY — scrape the Drive folder
#    (no gdown, no large download). This always works, so the notification gets
#    the real version even on hosts where the heavy download can't run.
if [[ -n "$FORCE_VERSION" ]]; then
  NEXT_VERSION="$FORCE_VERSION"
elif VCHECK_JSON="$(python3 "$FETCHER" --gist-id "$GIST_ID" --check-version 2>/dev/null)"; then
  NEXT_VERSION="$(echo "$VCHECK_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])' 2>/dev/null || echo unknown)"
else
  NEXT_VERSION="unknown (version check failed)"
fi
log "Latest SpotiFLAC-Next version: ${NEXT_VERSION} (current: ${LAST_VERSION:-none})"

if [[ "$NEXT_VERSION" == "$LAST_VERSION" || "$NEXT_VERSION" == unknown* ]]; then
  NEXT_CHANGED=false
  write_summary
  [[ "$NEXT_VERSION" == "$LAST_VERSION" ]] && log "No new SpotiFLAC-Next version." || log "Version undetermined; skipping download."
  exit 0
fi
NEXT_CHANGED=true
write_summary
log "New SpotiFLAC-Next version detected: ${LAST_VERSION:-none} -> ${NEXT_VERSION}"

# NOTE: the API no longer needs the binary's C2 imported. The download endpoints
# are the live per-variant pool ({prefix}-{variant}.${SPOTBYE_BASE:-spotbye.qzz.io}
# /api/dl) which the app resolves AT RUNTIME from the status payload + the
# spotbye.base_domain setting. So a new version normally needs nothing imported.
# (For deep inspection you can still diff a build manually with
#  update-c2-from-binary.sh; the pool base domain/path are editable at /admin.)
if [[ "$NO_NOTIFY" != "1" ]]; then
  notify_telegram "🆕 SpotiFLAC-Next ${NEXT_VERSION} detectado" "Versión anterior: ${LAST_VERSION:-ninguna}
Monochrome: ${MONO_API_COUNT} API instances$([ "$MONO_CHANGED" = true ] && echo ' (cambió)')
Endpoints de descarga: pool dinámico (status + spotbye.base_domain), no requiere import.
Si las descargas fallan tras esta versión, revisa el esquema del pool en /admin."
fi

echo "$NEXT_VERSION" > "$LAST_VERSION_FILE"
log "Recorded SpotiFLAC-Next version ${NEXT_VERSION}."
