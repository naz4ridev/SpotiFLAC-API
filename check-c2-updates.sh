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

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --version) FORCE_VERSION="$2"; shift 2 ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

mkdir -p "$STATE_DIR"
LAST_VERSION_FILE="$STATE_DIR/last_next_version"
LAST_VERSION="$(cat "$LAST_VERSION_FILE" 2>/dev/null || echo "")"

# Refresh Monochrome instances from the canonical INSTANCES.md (independent of
# the SpotiFLAC-Next release cycle; community instances change on their own).
MONO_FETCHER="$API_SCRIPTS_DIR/fetch-monochrome-instances.py"
if [[ -f "$MONO_FETCHER" ]]; then
  log "Refreshing Monochrome instances from upstream INSTANCES.md..."
  if [[ "$APPLY" == "1" ]]; then
    python3 "$MONO_FETCHER" --apply --api "$API_BASE_URL" || log "monochrome refresh failed (non-fatal)"
  else
    python3 "$MONO_FETCHER" --json >/dev/null && log "monochrome list parsed OK (dry-run; use --apply to push)" || log "monochrome parse failed (non-fatal)"
  fi
fi

# Ensure gdown is available in a local venv.
VENV="$STATE_DIR/venv"
if [[ ! -x "$VENV/bin/python3" ]]; then
  log "Creating venv for gdown at $VENV"
  python3 -m venv "$VENV"
fi
"$VENV/bin/pip" install --quiet --upgrade gdown >/dev/null

WORKDIR="$(mktemp -d -t spotiflac-next-check.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT

log "Fetching latest SpotiFLAC-Next (gist $GIST_ID)..."
FETCH_ARGS=(--gist-id "$GIST_ID" --workdir "$WORKDIR")
[[ -n "$FORCE_VERSION" ]] && FETCH_ARGS+=(--version "$FORCE_VERSION")
FETCH_JSON="$("$VENV/bin/python3" "$FETCHER" "${FETCH_ARGS[@]}")"

VERSION="$(echo "$FETCH_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')"
BINARY="$(echo "$FETCH_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["binary"])')"
log "Latest version: $VERSION (binary: $BINARY)"

if [[ "$VERSION" == "$LAST_VERSION" ]]; then
  log "No new version (current: $LAST_VERSION). Nothing to do."
  exit 0
fi
log "New version detected: $LAST_VERSION -> $VERSION"

NEW_MANIFEST="$WORKDIR/c2-manifest.json"
python3 "$EXTRACTOR" "$BINARY" -o "$NEW_MANIFEST"

if [[ -f "$REF_MANIFEST" ]]; then
  log "C2 changes vs reference:"
  python3 "$EXTRACTOR" "$BINARY" --diff "$REF_MANIFEST" || true
fi

if [[ "$APPLY" == "1" ]]; then
  log "Importing new C2 into API at $API_BASE_URL"
  curl -fsS -X POST "$API_BASE_URL/admin/c2/import" \
    -H 'Content-Type: application/json' --data-binary "@$NEW_MANIFEST" && echo
fi

# Persist the reference manifest and the seen version.
cp "$NEW_MANIFEST" "$REF_MANIFEST"
echo "$VERSION" > "$LAST_VERSION_FILE"
log "Updated reference manifest and recorded version $VERSION."
log "Review/commit $REF_MANIFEST if running from a git checkout."
