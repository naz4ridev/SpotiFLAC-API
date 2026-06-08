#!/usr/bin/env bash
#
# Shared Telegram notification helper for the updater scripts.
#
# Sends a message via the Alfred Telegram bot (curl with a bearer token).
# Configure in .env:
#   TELEGRAM_NOTIFY_TOKEN   bearer token (REQUIRED to enable; empty = disabled)
#   TELEGRAM_NOTIFY_URL     endpoint (default https://alfred-telegram.naz4ri.music/)
#
# Usage: notify_telegram "subject" "body"
# Safe to call unconditionally: it no-ops (and never fails the caller) when the
# token is unset.

notify_telegram() {
  local subject="$1" body="$2"
  local url="${TELEGRAM_NOTIFY_URL:-https://alfred-telegram.naz4ri.music/}"
  local token="${TELEGRAM_NOTIFY_TOKEN:-}"

  if [ -z "$token" ]; then
    return 0
  fi

  if curl -fsS -m 15 "$url" \
      -H "Authorization: Bearer ${token}" \
      -F "subject=${subject}" \
      -F "body=${body}" >/dev/null 2>&1; then
    echo "[notify] Telegram sent: ${subject}"
  else
    echo "[notify] Telegram notification failed: ${subject}" >&2
  fi
  return 0
}
