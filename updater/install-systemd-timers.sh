#!/usr/bin/env bash
# Installer script for SpotiFLAC API auto-updater and daily-smoke timers.
# Registers systemd timer units on the host machine.

set -euo pipefail

# Find script directory (assumed to be /opt/spotiflac-updater)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Dynamic parameters
CURRENT_USER=$(whoami)
UPDATE_SCRIPT="${SCRIPT_DIR}/update.sh"
DAILY_SMOKE_SCRIPT="${SCRIPT_DIR}/daily-smoke.sh"

echo "=================================================="
echo "Preparing systemd units for SpotiFLAC API Updater..."
echo "  - User: ${CURRENT_USER}"
echo "  - WorkingDirectory: ${SCRIPT_DIR}"
echo "  - Update script: ${UPDATE_SCRIPT}"
echo "  - Daily-smoke script: ${DAILY_SMOKE_SCRIPT}"
echo "=================================================="

# Temp file paths
UPDATE_SERVICE_TMP=$(mktemp)
UPDATE_TIMER_TMP=$(mktemp)
SMOKE_SERVICE_TMP=$(mktemp)
SMOKE_TIMER_TMP=$(mktemp)

cleanup() {
  rm -f "$UPDATE_SERVICE_TMP" "$UPDATE_TIMER_TMP" "$SMOKE_SERVICE_TMP" "$SMOKE_TIMER_TMP"
}
trap cleanup EXIT

# 1. Generate spotiflac-update units
cat <<EOF > "$UPDATE_SERVICE_TMP"
[Unit]
Description=SpotiFLAC API Auto-Updater (Git Branch Updater)
After=network.target

[Service]
Type=oneshot
User=${CURRENT_USER}
WorkingDirectory=${SCRIPT_DIR}
ExecStart=${UPDATE_SCRIPT}
StandardOutput=journal
StandardError=journal
EOF

cat <<EOF > "$UPDATE_TIMER_TMP"
[Unit]
Description=Run SpotiFLAC API Auto-Updater every 6 hours

[Timer]
OnBootSec=10min
OnUnitActiveSec=6h
Persistent=true

[Install]
WantedBy=timers.target
EOF

# 2. Generate spotiflac-daily-smoke units
cat <<EOF > "$SMOKE_SERVICE_TMP"
[Unit]
Description=SpotiFLAC API Daily Smoke Test
After=network.target

[Service]
Type=oneshot
User=${CURRENT_USER}
WorkingDirectory=${SCRIPT_DIR}
ExecStart=${DAILY_SMOKE_SCRIPT}
StandardOutput=journal
StandardError=journal
EOF

cat <<EOF > "$SMOKE_TIMER_TMP"
[Unit]
Description=Run SpotiFLAC API Daily Smoke Test Daily

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
EOF

# 3. Copy to /etc/systemd/system/
UPDATE_SERVICE_PATH="/etc/systemd/system/spotiflac-update.service"
UPDATE_TIMER_PATH="/etc/systemd/system/spotiflac-update.timer"
SMOKE_SERVICE_PATH="/etc/systemd/system/spotiflac-daily-smoke.service"
SMOKE_TIMER_PATH="/etc/systemd/system/spotiflac-daily-smoke.timer"

echo "Copying systemd unit files to /etc/systemd/system/ (requires sudo privileges)..."
sudo cp "$UPDATE_SERVICE_TMP" "$UPDATE_SERVICE_PATH"
sudo cp "$UPDATE_TIMER_TMP" "$UPDATE_TIMER_PATH"
sudo cp "$SMOKE_SERVICE_TMP" "$SMOKE_SERVICE_PATH"
sudo cp "$SMOKE_TIMER_TMP" "$SMOKE_TIMER_PATH"

# Set proper permissions
sudo chmod 644 "$UPDATE_SERVICE_PATH" "$UPDATE_TIMER_PATH" "$SMOKE_SERVICE_PATH" "$SMOKE_TIMER_PATH"

# 4. Reload and enable service
echo "Reloading systemd daemon..."
sudo systemctl daemon-reload

echo "Enabling and starting timers..."
sudo systemctl enable spotiflac-update.timer
sudo systemctl start spotiflac-update.timer
sudo systemctl enable spotiflac-daily-smoke.timer
sudo systemctl start spotiflac-daily-smoke.timer

echo "=================================================="
echo "Timers installed and started successfully!"
echo "--------------------------------------------------"
echo "Check active timers:"
echo "  systemctl list-timers | grep spotiflac"
echo ""
echo "Check timer statuses:"
echo "  systemctl status spotiflac-update.timer"
echo "  systemctl status spotiflac-daily-smoke.timer"
echo ""
echo "View execution logs:"
echo "  journalctl -u spotiflac-update.service -f"
echo "  journalctl -u spotiflac-daily-smoke.service -f"
echo "=================================================="
exit 0
EOF
