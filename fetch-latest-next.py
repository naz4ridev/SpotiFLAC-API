#!/usr/bin/env python3
"""
Fetch the latest SpotiFLAC-Next macOS app and expose its binary for C2 extraction.

The latest build is published in a Google Drive folder linked from a public gist
(https://gist.github.com/afkarxyz/b2f7b815b1560d7a58d7dd847f073f00). The Drive
folder holds one sub-folder per version (e.g. v1.3.3); inside each is a zip per
platform. The macOS artifact is a .dmg containing SpotiFLAC-Next.app, whose
Mach-O binary is what scripts/extract-spotiflac-next.py reads.

This script:
  1. reads the gist to find the Drive folder id,
  2. uses gdown to download the macOS .dmg (the latest version, or --version),
  3. extracts the .app (hdiutil on macOS, 7z on Linux),
  4. prints JSON: {"version": "...", "binary": "/path/to/Mach-O"}.

Requires: gdown (pip install gdown), and either hdiutil (macOS) or 7z (Linux).

Usage:
    fetch-latest-next.py [--gist-id ID] [--version vX.Y.Z] [--workdir DIR] [--keep]
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request

DEFAULT_GIST_ID = "b2f7b815b1560d7a58d7dd847f073f00"
VERSION_RE = re.compile(r"v?\d+\.\d+\.\d+")


def log(msg: str) -> None:
    print(msg, file=sys.stderr)


def gist_drive_folder_id(gist_id: str) -> str:
    url = f"https://api.github.com/gists/{gist_id}"
    req = urllib.request.Request(url, headers={"User-Agent": "spotiflac-updater"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.load(resp)
    content = " ".join(f.get("content", "") for f in data.get("files", {}).values())
    m = re.search(r"drive\.google\.com/drive/folders/([A-Za-z0-9_-]+)", content)
    if not m:
        raise SystemExit("could not find a Drive folder link in the gist")
    return m.group(1)


def require_gdown():
    try:
        import gdown  # noqa: F401
        return gdown
    except ImportError:
        raise SystemExit("gdown is required: pip install gdown")


def download_folder(folder_id: str, dest: str) -> str:
    """Download the Drive folder tree into dest, returning the local root path."""
    gdown = require_gdown()
    url = f"https://drive.google.com/drive/folders/{folder_id}"
    log(f">> Downloading Drive folder {folder_id} (this may be large)...")
    paths = gdown.download_folder(url, output=dest, quiet=False, use_cookies=False, remaining_ok=True)
    if not paths:
        raise SystemExit("gdown downloaded nothing from the folder")
    return dest


def pick_version_dir(root: str, want: str | None) -> tuple[str, str]:
    """Choose the version sub-folder: the requested one, else the highest semver."""
    candidates = []
    for name in os.listdir(root):
        full = os.path.join(root, name)
        if os.path.isdir(full) and VERSION_RE.search(name):
            candidates.append((name, full))
    if not candidates:
        # The folder may itself be a single version (no sub-dirs).
        return os.path.basename(root.rstrip("/")), root
    if want:
        for name, full in candidates:
            if want.lstrip("v") in name:
                return name, full
        raise SystemExit(f"version {want} not found; have: {[c[0] for c in candidates]}")

    def semver_key(name: str):
        m = VERSION_RE.search(name)
        return tuple(int(x) for x in m.group().lstrip("v").split("."))

    candidates.sort(key=lambda c: semver_key(c[0]))
    return candidates[-1]


def find_macos_dmg(version_dir: str) -> str:
    dmgs = []
    for pat in ("**/*[Mm]ac*.dmg", "**/*[Dd]arwin*.dmg", "**/*.dmg"):
        dmgs = glob.glob(os.path.join(version_dir, pat), recursive=True)
        if dmgs:
            break
    if not dmgs:
        raise SystemExit(f"no macOS .dmg found under {version_dir}")
    return dmgs[0]


def extract_app_binary(dmg: str, workdir: str) -> str:
    """Extract SpotiFLAC-Next.app from a dmg and return the Mach-O binary path."""
    out = os.path.join(workdir, "dmg-extracted")
    os.makedirs(out, exist_ok=True)

    if sys.platform == "darwin" and shutil.which("hdiutil"):
        log(">> Mounting dmg with hdiutil...")
        res = subprocess.run(
            ["hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", os.path.join(out, "mnt"), dmg],
            check=True, capture_output=True, text=True,
        )
        mnt = os.path.join(out, "mnt")
        try:
            binary = locate_binary(mnt)
            staged = os.path.join(workdir, "SpotiFLAC-Next.app")
            app_root = binary.split("/Contents/")[0]
            shutil.copytree(app_root, staged, dirs_exist_ok=True)
        finally:
            subprocess.run(["hdiutil", "detach", mnt], capture_output=True)
        return locate_binary(staged)

    if shutil.which("7z"):
        log(">> Extracting dmg with 7z...")
        subprocess.run(["7z", "x", "-y", f"-o{out}", dmg], check=True, capture_output=True)
        return locate_binary(out)

    raise SystemExit("need hdiutil (macOS) or 7z (Linux) to extract the dmg")


def locate_binary(root: str) -> str:
    """Find the SpotiFLAC-Next Mach-O executable under an extracted/mounted tree."""
    for path in glob.glob(os.path.join(root, "**/Contents/MacOS/*"), recursive=True):
        if os.path.isfile(path) and "." not in os.path.basename(path):
            return path
    raise SystemExit(f"could not locate the app binary under {root}")


def _semver_key(v: str):
    m = VERSION_RE.search(v)
    return tuple(int(x) for x in m.group().lstrip("v").split(".")) if m else (0, 0, 0)


def latest_version_from_folder(folder_id: str) -> str:
    """Return the highest version (folder name) in the public Drive folder by
    scraping its page — no gdown and no large download needed. Used for cheap
    version detection on every run."""
    url = f"https://drive.google.com/drive/folders/{folder_id}"
    req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        html = resp.read().decode("utf-8", "replace")
    versions = set(VERSION_RE.findall(html))
    if not versions:
        raise SystemExit("could not find any version in the Drive folder page")
    return max(versions, key=_semver_key)


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--gist-id", default=DEFAULT_GIST_ID)
    ap.add_argument("--version", help="specific version to fetch (e.g. v1.3.3); default: latest")
    ap.add_argument("--check-version", action="store_true",
                    help="only detect the latest version (scrape the Drive folder; no gdown/download)")
    ap.add_argument("--workdir", help="working directory (default: a temp dir)")
    ap.add_argument("--keep", action="store_true", help="keep the workdir on exit")
    args = ap.parse_args(argv)

    if args.check_version:
        folder_id = gist_drive_folder_id(args.gist_id)
        print(json.dumps({"version": latest_version_from_folder(folder_id)}))
        return 0

    workdir = args.workdir or tempfile.mkdtemp(prefix="spotiflac-next-")
    os.makedirs(workdir, exist_ok=True)
    try:
        folder_id = gist_drive_folder_id(args.gist_id)
        root = download_folder(folder_id, os.path.join(workdir, "drive"))
        version, vdir = pick_version_dir(root, args.version)
        dmg = find_macos_dmg(vdir)
        binary = extract_app_binary(dmg, workdir)
        print(json.dumps({"version": version, "binary": binary, "dmg": dmg}))
        return 0
    finally:
        if not args.keep and not args.workdir:
            shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
