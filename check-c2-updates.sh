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
# Requires: python3, gdown (auto-installed into a venv), and 7z or hdiutil.
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

# Ensure gdown is available to download the SpotiFLAC-Next build. Prefer the
# system python3 (no venv needed); fall back to a --user install, then a venv.
# If none works (e.g. python3-venv/pip missing), skip the SpotiFLAC-Next step
# WITHOUT failing — Monochrome was already refreshed.
PYBIN="python3"
if ! python3 -c 'import gdown' 2>/dev/null; then
  log "gdown not found; attempting to install..."
  if python3 -m pip install --user --quiet --upgrade gdown 2>/dev/null && python3 -c 'import gdown' 2>/dev/null; then
    log "gdown installed via pip --user."
  elif python3 -m venv "$STATE_DIR/venv" 2>/dev/null && "$STATE_DIR/venv/bin/pip" install --quiet --upgrade gdown 2>/dev/null; then
    PYBIN="$STATE_DIR/venv/bin/python3"
    log "gdown installed in venv."
  else
    log "WARNING: could not install gdown. Install on the host: sudo apt install python3-pip python3-venv"
    log "Skipping SpotiFLAC-Next version check (Monochrome was refreshed)."
    NEXT_VERSION="unknown (gdown unavailable)"
    NEXT_CHANGED=false
    write_summary
    exit 0
  fi
fi

WORKDIR="$(mktemp -d -t spotiflac-next-check.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT

log "Fetching latest SpotiFLAC-Next (gist $GIST_ID)..."
FETCH_ARGS=(--gist-id "$GIST_ID" --workdir "$WORKDIR")
[[ -n "$FORCE_VERSION" ]] && FETCH_ARGS+=(--version "$FORCE_VERSION")
if ! FETCH_JSON="$("$PYBIN" "$FETCHER" "${FETCH_ARGS[@]}")"; then
  log "WARNING: failed to fetch the latest SpotiFLAC-Next build; skipping (Monochrome refreshed)."
  NEXT_VERSION="unknown (fetch failed)"
  NEXT_CHANGED=false
  write_summary
  exit 0
fi

VERSION="$(echo "$FETCH_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')"
BINARY="$(echo "$FETCH_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["binary"])')"
log "Latest version: $VERSION (binary: $BINARY)"
NEXT_VERSION="$VERSION"

if [[ "$VERSION" == "$LAST_VERSION" ]]; then
  log "No new SpotiFLAC-Next version (current: $LAST_VERSION)."
  NEXT_CHANGED=false
  write_summary
  exit 0
fi
log "New version detected: $LAST_VERSION -> $VERSION"
NEXT_CHANGED=true

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
