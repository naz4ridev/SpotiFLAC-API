#!/usr/bin/env bash
#
# Refresh the API's C2 endpoints from a new SpotiFLAC-Next build.
#
# Workflow when a new SpotiFLAC-Next is released:
#   1. Download / locate the new binary (or .app bundle).
#   2. Run this script: it extracts a fresh manifest, diffs it against the
#      committed reference, and (with --apply) pushes it to a running API via
#      POST /admin/c2/import.
#
# Usage:
#   update-c2-from-binary.sh <binary-or-.app> [--apply] [--api URL] [--ref FILE]
#
# Options:
#   --apply       POST the new manifest to the API config store.
#   --api URL     API base URL (default: $API_BASE_URL or http://127.0.0.1:8080).
#   --ref FILE    Reference manifest to diff against and update
#                 (default: scripts/c2-manifest.json next to this script).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXTRACTOR="$SCRIPT_DIR/extract-spotiflac-next.py"
REF_MANIFEST="$SCRIPT_DIR/c2-manifest.json"
API_BASE_URL="${API_BASE_URL:-http://127.0.0.1:8080}"
APPLY=0

BINARY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --api) API_BASE_URL="$2"; shift 2 ;;
    --ref) REF_MANIFEST="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) BINARY="$1"; shift ;;
  esac
done

if [[ -z "$BINARY" ]]; then
  echo "error: missing binary/.app path" >&2
  exit 2
fi

NEW_MANIFEST="$(mktemp -t c2-manifest.XXXXXX.json)"
trap 'rm -f "$NEW_MANIFEST"' EXIT

echo ">> Extracting C2 endpoints from: $BINARY"
python3 "$EXTRACTOR" "$BINARY" -o "$NEW_MANIFEST"

if [[ -f "$REF_MANIFEST" ]]; then
  echo ">> Diff against reference ($REF_MANIFEST):"
  python3 "$EXTRACTOR" "$BINARY" --diff "$REF_MANIFEST" || true
else
  echo ">> No reference manifest yet at $REF_MANIFEST (first run)."
fi

if [[ "$APPLY" == "1" ]]; then
  echo ">> Importing into API at $API_BASE_URL/admin/c2/import"
  curl -fsS -X POST "$API_BASE_URL/admin/c2/import" \
    -H 'Content-Type: application/json' \
    --data-binary "@$NEW_MANIFEST" && echo " ...done"
fi

echo ">> Updating reference manifest: $REF_MANIFEST"
cp "$NEW_MANIFEST" "$REF_MANIFEST"
echo ">> Review and commit $REF_MANIFEST if the changes look correct."
