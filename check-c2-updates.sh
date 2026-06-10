#!/usr/bin/env bash
#
# Detect a new SpotiFLAC-Next release and refresh the API's C2 endpoints.
#
# Pipeline:
#   1. fetch-latest-next.py : gist -> Google Drive -> macOS .dmg -> .app binary
#   2. extract-spotiflac-next.py : binary -> c2-manifest.json
#   3. diff against the committed reference manifest
#   4. if changed and --apply: POST /admin/c2/import to the running API
#
# Intended to run on the host (e.g. from the same systemd timer as update.sh).
# Requires: python3, unzip, and 7z (Linux) or hdiutil (macOS). No gdown needed.
#
# Env / flags:
#   API_BASE_URL          API base for /admin/c2/import (default http://127.0.0.1:8080)
#   STATE_DIR             where the last seen version is recorded (default ./state)
#   SPOTIFLAC_NEXT_GIST   gist id (default b2f7b815b1560d7a58d7dd847f073f00)
#   --apply               push the new manifest to the API
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
write_summary   # record the new version now, in case the download below can't run
log "New SpotiFLAC-Next version: ${LAST_VERSION:-none} -> ${NEXT_VERSION}. Downloading to extract C2..."

# 2) A new version exists -> download ONLY its macOS zip (~18 MB, direct from
#    Drive; no gdown) and extract its C2. Needs `unzip` + (`7z` on Linux /
#    `hdiutil` on macOS). Non-fatal: on failure keep the detected version but
#    leave endpoints unchanged and do NOT advance last-seen (retry next run).
WORKDIR="$(mktemp -d -t spotiflac-next-check.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT
log "Downloading SpotiFLAC-Next ${NEXT_VERSION} (macOS build)..."
if ! FETCH_JSON="$(python3 "$FETCHER" --gist-id "$GIST_ID" --version "$NEXT_VERSION" --workdir "$WORKDIR")"; then
  log "WARNING: failed to download/extract the build (need unzip + 7z/hdiutil); endpoints NOT updated (will retry next run)."
  exit 0
fi
VERSION="$NEXT_VERSION"
BINARY="$(echo "$FETCH_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["binary"])')"
log "Downloaded binary: $BINARY"

NEW_MANIFEST="$WORKDIR/c2-manifest.json"
python3 "$EXTRACTOR" "$BINARY" -o "$NEW_MANIFEST"

C2_DIFF="(no reference manifest to diff against)"
if [[ -f "$REF_MANIFEST" ]]; then
  log "C2 changes vs reference:"
  C2_DIFF="$(python3 "$EXTRACTOR" "$BINARY" --diff "$REF_MANIFEST" 2>/dev/null || true)"
  echo "$C2_DIFF"
fi

APPLIED_NOTE="(dry-run; not applied)"
if [[ "$APPLY" == "1" ]]; then
  log "Importing new C2 into API at $API_BASE_URL"
  if curl -fsS -X POST "$API_BASE_URL/admin/c2/import" \
      -H 'Content-Type: application/json' --data-binary "@$NEW_MANIFEST"; then
    echo
    APPLIED_NOTE="Imported into the running API."
  else
    APPLIED_NOTE="Import to API FAILED."
  fi
fi

write_summary

# Notify about the new SpotiFLAC-Next version and its C2 changes — unless the
# caller (update.sh) will send a single consolidated notification.
if [[ "$NO_NOTIFY" != "1" ]]; then
  notify_telegram "🆕 SpotiFLAC-Next ${VERSION} detectado" "Versión anterior: ${LAST_VERSION:-ninguna}
${APPLIED_NOTE}
Monochrome: ${MONO_API_COUNT} API instances$([ "$MONO_CHANGED" = true ] && echo ' (cambió)')

Cambios de C2/endpoints:
${C2_DIFF}"
fi

# Persist the reference manifest and the seen version.
cp "$NEW_MANIFEST" "$REF_MANIFEST"
echo "$VERSION" > "$LAST_VERSION_FILE"
log "Updated reference manifest and recorded version $VERSION."
log "Review/commit $REF_MANIFEST if running from a git checkout."
