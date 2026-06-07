# SpotiFLAC API Branch-Based Updater

This directory contains the automated monitoring and update system for the SpotiFLAC API. 

To keep the production environment clean, the updater lives in a dedicated branch (`spotiflac-updater`) separate from the Go API source code (`main`). It works by cloning the `main` branch temporarily, verifying upstream updates, committing and pushing Go module bumps to the remote `main` branch, and triggering Coolify to deploy.

## Architecture & Workflow

1. **Isolation**: Coolify only deploys the `main` branch. The `spotiflac-updater` branch is never deployed to production.
2. **Periodic Checks**: The systemd timer executes `update.sh` every 6 hours on the host.
3. **Validation**: If a new version is detected in `spotbye/SpotiFLAC`, the updater clones `main` to `/tmp/spotiflacapi-update-workdir` and tests the dependency change:
   - `go mod tidy`
   - `go build ./...` (compilation verification)
   - `go test ./...` (unit tests validation)
   - `docker compose build` (verifies Docker image compiles successfully)
4. **Zero-Staging Deployment**: If checks pass, it pushes the updated `go.mod`/`go.sum` back to the remote `main` branch and triggers a Coolify redeploy webhook.
5. **Post-Deploy Smoke Test**: It polls `/health` and runs `smoke-test.sh` to request a real Spotify track download, verifying size and file structure with `ffprobe`.
6. **Automatic Git Revert**: If the deployment fails or the smoke test is unsuccessful, the updater runs `git revert HEAD` in the temporary folder, pushes the revert back to `main`, and redeploys the previous version in Coolify.

---

## C2 endpoint maintenance (SpotiFLAC-Next + Monochrome)

The API keeps its C2 endpoints and settings in a SQLite store (`C2_DB_PATH`,
default `/app/data/c2.db`, persisted on the `c2-data` Docker volume). The
branch-based updater only bumps `go.mod`/`go.sum` and redeploys, so the volume —
and all operator C2 edits — persists across updates automatically. View/edit at
`/admin/` (web) or via the `/admin/c2` API.

This branch also carries the C2 maintenance tooling (kept at the branch root so
it is self-contained on the host):

- **`check-c2-updates.sh`** — the orchestrator. On each run it refreshes the
  Monochrome instance list from the canonical
  [INSTANCES.md](https://github.com/monochrome-music/monochrome/blob/main/INSTANCES.md),
  detects a new SpotiFLAC-Next release (gist → Google Drive → macOS `.dmg` →
  `.app`), extracts its C2 (`extract-spotiflac-next.py`), diffs against the
  committed `c2-manifest.json`, and with `--apply` imports the changes into the
  running API (`POST /admin/c2/import`). It records the last seen version in
  `STATE_DIR`. Requires `python3` + `7z`/`hdiutil`; `gdown` is auto-installed in
  a venv.

  ```bash
  API_BASE_URL="$BASE_URL" ./check-c2-updates.sh --apply
  ```

- **`fetch-latest-next.py`** — gist → Drive → dmg → `.app` binary resolver.
- **`fetch-monochrome-instances.py`** — parse INSTANCES.md → `monochrome.*` settings.
- **`extract-spotiflac-next.py`** / **`update-c2-from-binary.sh`** — static C2
  extraction + diff/import from a binary you already have.

Run `check-c2-updates.sh` from the same systemd timer as `update.sh` (see below).

---

## Initial Setup on GitHub

Follow these steps to create the `spotiflac-updater` branch using this directory's files:

```bash
# 1. Start from main branch
git checkout main
git pull

# 2. Create an orphan branch (empty history)
git checkout --orphan spotiflac-updater
git reset

# 3. Add only the updater files to the root of the new branch
git add updater/*
mv updater/* .
rmdir updater

# 4. Remove all other files from git tracking in this branch
git clean -fdx

# 5. Commit and push to GitHub
git commit -m "Initial SpotiFLAC updater branch setup"
git push origin spotiflac-updater

# 6. Revert your local copy back to main branch
git checkout main
```

---

## Server Installation

Clone the `spotiflac-updater` branch directly to `/opt/spotiflac-updater` on the server:

```bash
# Clone only the updater branch
sudo git clone --branch spotiflac-updater --single-branch git@github.com:OWNER/REPO.git /opt/spotiflac-updater

# Fix directory ownership so your server user can write and run scripts without root
sudo chown -R $USER:$USER /opt/spotiflac-updater
```

---

## Configuration (`/opt/spotiflac-updater/.env`)

Copy the configuration template and configure it:

```bash
cd /opt/spotiflac-updater
cp .env.example .env
nano .env
```

### Environment Variables

| Variable | Description |
| :--- | :--- |
| `APP_REPO_URL` | SSH URL to your repository (e.g. `git@github.com:user/repo.git`). *Note: Ensure your server user's SSH key has write access to push dependency commits.* |
| `APP_BRANCH` | Branch deployed by Coolify (`main`). |
| `WORKDIR` | Temp directory for compilation checking (`/tmp/spotiflacapi-update-workdir`). |
| `STATE_DIR` | Directory to save the last successfully deployed commits (`/opt/spotiflac-updater/state`). State files: `last_go_version`, `last_python_version`. |
| `BASE_URL` | Public URL of your API (e.g. `https://office.naz4rimusic.com/tools/spotiflacapi`). |
| `TEST_TRACK_URL` | Spotify track URL used for smoke test validation. |
| `MIN_AUDIO_BYTES` | Minimum expected file size (default: `100000` bytes). |
| `GO_SPOTIFLAC_REPO` | Upstream repository for Go provider (default: `spotbye/SpotiFLAC`). |
| `GO_MODULE_PATH` | Go module import path (default: `github.com/afkarxyz/SpotiFLAC`). |
| `GO_UPDATE_MODE` | Upstream Go update mode: `release` or `commit`. |
| `PYTHON_SPOTIFLAC_REPO` | Upstream repository for Python provider (default: `https://github.com/ShuShuzinhuu/SpotiFLAC-Module-Version.git`). |
| `PYTHON_UPDATE_MODE` | Upstream Python update mode: `commit`. |
| `PYTHON_REF_FILE` | Pinned ref file inside master branch (default: `.python-spotiflac-ref`). |
| `SMOKE_STRATEGY` | Default strategy for smoke test (default: `race`). |
| `COOLIFY_REDEPLOY_URL` | Coolify webhook URL to trigger a deploy. Obtain from the application settings page in Coolify. |
| `UPTIME_KUMA_UPDATE_PUSH_URL` | Optional Uptime Kuma Push Monitor URL for updates (notifies OK/DOWN). |
| `UPTIME_KUMA_DAILY_SMOKE_PUSH_URL` | Optional Uptime Kuma Push Monitor URL for the daily smoke test. |

---

## Registering Systemd Timers

Execute the timer installer script on the host to schedule updater and daily health checks:

```bash
./install-systemd-timers.sh
```

This installs:
- `spotiflac-update.timer`: Runs `update.sh` every 6 hours.
- `spotiflac-daily-smoke.timer`: Runs `daily-smoke.sh` daily at midnight.

---

## Operational Guide

### Check Log Executions
All script logs are logged to systemd's journal. Inspect logs in real-time with:

```bash
# View update logs
journalctl -u spotiflac-update.service -f

# View daily smoke test logs
journalctl -u spotiflac-daily-smoke.service -f
```

### Manual Trigger Commands
You can trigger these scripts manually at any time:

```bash
# Force check and update cycle
/opt/spotiflac-updater/update.sh

# Run manual smoke test
/opt/spotiflac-updater/smoke-test.sh

# Run manual daily smoke test
/opt/spotiflac-updater/daily-smoke.sh
```

### Manual Rollback
If a deployed release experiences runtime issues not caught by the smoke test, you can roll back manually:
1. Clone the `main` branch to your local workspace or temporary folder.
2. Find the auto-update commit you want to undo and revert it:
   ```bash
   git revert <commit-sha> --no-edit
   git push origin main
   ```
3. Trigger Coolify redeployment (either via Coolify dashboard or by curling the `COOLIFY_REDEPLOY_URL` webhook).
4. Update the saved state on the server so the updater doesn't re-apply the update:
   ```bash
   # Revert the saved version hashes to the previous versions
   echo "<previous-go-short-sha>" > /opt/spotiflac-updater/state/last_go_version
   echo "<previous-python-short-sha>" > /opt/spotiflac-updater/state/last_python_version
   ```
