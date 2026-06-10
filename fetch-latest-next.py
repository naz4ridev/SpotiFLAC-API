#!/usr/bin/env python3
"""
Fetch the latest SpotiFLAC-Next macOS build and expose its binary for C2 extraction.

The latest build is published in a public Google Drive folder linked from a gist
(https://gist.github.com/afkarxyz/b2f7b815b1560d7a58d7dd847f073f00). Layout:

    <Drive folder>/
        v1.3.5/                       (one sub-folder per version)
            macos-portable.zip        -> contains SpotiFLAC-Next.dmg
            windows-portable.zip
            linux-portable.zip
            ...

So the macOS artifact is a ZIP that contains a .dmg, which contains the .app,
whose Mach-O binary is what scripts/extract-spotiflac-next.py reads.

This script needs NO gdown: it scrapes the public folder pages to resolve the
version sub-folder and the macOS zip's file id, then downloads ONLY that ~18 MB
zip directly (drive.usercontent.google.com), unzips it, extracts the .dmg
(hdiutil on macOS, 7z on Linux), and returns the binary path.

Usage:
    fetch-latest-next.py [--gist-id ID] [--version vX.Y.Z] [--workdir DIR] [--keep]
    fetch-latest-next.py --check-version        # just print the latest version (no download)

Requires: python3, unzip, and either hdiutil (macOS) or 7z (Linux).
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
import zipfile

DEFAULT_GIST_ID = "b2f7b815b1560d7a58d7dd847f073f00"
VERSION_RE = re.compile(r"v?\d+\.\d+\.\d+")
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

# Drive folder pages render each item with: aria-label="<name> <type...>" and an
# ssk attribute carrying the item id: ssk='N:hash:<ITEM_ID>-a-b'.
_ITEM_RE = re.compile(
    r'aria-label="([^"]+?)"[^>]*?ssk=\'[^:\']*:[^:\']*:([0-9A-Za-z_-]{20,50})-\d+-\d+\''
)


def log(msg: str) -> None:
    print(msg, file=sys.stderr)


def _get(url: str, timeout: int = 60) -> str:
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read().decode("utf-8", "replace")


def gist_drive_folder_id(gist_id: str) -> str:
    data = json.loads(_get(f"https://api.github.com/gists/{gist_id}", timeout=30))
    content = " ".join(f.get("content", "") for f in data.get("files", {}).values())
    m = re.search(r"drive\.google\.com/drive/folders/([A-Za-z0-9_-]+)", content)
    if not m:
        raise SystemExit("could not find a Drive folder link in the gist")
    return m.group(1)


def scrape_folder_items(folder_id: str) -> list[tuple[str, str]]:
    """Return [(aria_label, item_id)] for a public Drive folder."""
    html = _get(f"https://drive.google.com/drive/folders/{folder_id}")
    return _ITEM_RE.findall(html)


def _semver_key(v: str):
    m = VERSION_RE.search(v)
    return tuple(int(x) for x in m.group().lstrip("v").split(".")) if m else (0, 0, 0)


def version_subfolders(folder_id: str) -> dict[str, str]:
    """Map version string -> sub-folder id (folders have 'folder' in the label)."""
    out: dict[str, str] = {}
    for label, fid in scrape_folder_items(folder_id):
        m = VERSION_RE.search(label)
        if m and "folder" in label.lower():
            out[m.group()] = fid
    return out


def latest_version_from_folder(folder_id: str) -> str:
    vers = version_subfolders(folder_id)
    if not vers:
        raise SystemExit("could not find any version sub-folder in the Drive folder")
    return max(vers, key=_semver_key)


def pick_version(folder_id: str, want: str | None) -> tuple[str, str]:
    vers = version_subfolders(folder_id)
    if not vers:
        raise SystemExit("no version sub-folders found")
    if want:
        w = want.lstrip("v")
        for name, fid in vers.items():
            if name.lstrip("v") == w:
                return name, fid
        raise SystemExit(f"version {want} not found; have: {sorted(vers)}")
    best = max(vers, key=_semver_key)
    return best, vers[best]


def find_macos_artifact(version_folder_id: str) -> tuple[str, str]:
    """Return (filename, file_id) of the macOS artifact (a .zip, or a bare .dmg)."""
    items = scrape_folder_items(version_folder_id)
    for want_ext in (".zip", ".dmg"):
        for label, fid in items:
            low = label.lower()
            if "mac" in low and want_ext in low:
                return label.split(" ")[0], fid
    raise SystemExit(f"no macOS artifact in version folder {version_folder_id}; items: {[i[0] for i in items]}")


def download_drive_file(file_id: str, dest: str) -> None:
    """Download a public Drive file by id (no gdown). Handles the large-file
    confirmation interstitial if it appears."""
    url = f"https://drive.usercontent.google.com/download?id={file_id}&export=download&confirm=t"
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=300) as resp:
        if "text/html" in resp.headers.get("Content-Type", ""):
            html = resp.read().decode("utf-8", "replace")
            params = dict(re.findall(r'name="([^"]+)"\s+value="([^"]*)"', html))
            if not params:
                raise SystemExit("Drive returned an HTML page instead of the file (download blocked?)")
            from urllib.parse import urlencode
            retry = "https://drive.usercontent.google.com/download?" + urlencode({
                "id": file_id, "export": "download",
                "confirm": params.get("confirm", "t"), "uuid": params.get("uuid", ""),
            })
            with urllib.request.urlopen(urllib.request.Request(retry, headers={"User-Agent": UA}), timeout=300) as r2, open(dest, "wb") as f:
                shutil.copyfileobj(r2, f)
            return
        with open(dest, "wb") as f:
            shutil.copyfileobj(resp, f)


def locate_binary(root: str) -> str | None:
    """Find the SpotiFLAC-Next Mach-O executable under an extracted tree."""
    for path in glob.glob(os.path.join(root, "**/Contents/MacOS/*"), recursive=True):
        if os.path.isfile(path) and "." not in os.path.basename(path):
            return path
    return None


def extract_dmg(dmg: str, out: str) -> str:
    """Extract a .dmg and return the contained Mach-O binary path."""
    os.makedirs(out, exist_ok=True)
    if sys.platform == "darwin" and shutil.which("hdiutil"):
        mnt = os.path.join(out, "mnt")
        subprocess.run(["hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mnt, dmg],
                       check=True, capture_output=True, text=True)
        try:
            binary = locate_binary(mnt)
            if not binary:
                raise SystemExit(f"no binary found in mounted dmg {dmg}")
            app_root = binary.split("/Contents/")[0]
            shutil.copytree(app_root, os.path.join(out, "app", os.path.basename(app_root)), dirs_exist_ok=True)
        finally:
            subprocess.run(["hdiutil", "detach", mnt], capture_output=True)
        b = locate_binary(out)
        if not b:
            raise SystemExit("failed to stage binary from dmg")
        return b
    if shutil.which("7z"):
        subprocess.run(["7z", "x", "-y", f"-o{out}", dmg], check=True, capture_output=True)
        b = locate_binary(out)
        if not b:
            raise SystemExit(f"no binary found after 7z-extracting {dmg}")
        return b
    raise SystemExit("need hdiutil (macOS) or 7z (Linux) to extract the dmg")


def fetch_binary(folder_id: str, want: str | None, workdir: str) -> tuple[str, str]:
    version, vfolder = pick_version(folder_id, want)
    name, file_id = find_macos_artifact(vfolder)
    log(f">> {version}: downloading {name} ({file_id})...")
    artifact = os.path.join(workdir, name)
    download_drive_file(file_id, artifact)

    extract_dir = os.path.join(workdir, "extract")
    os.makedirs(extract_dir, exist_ok=True)

    if name.lower().endswith(".zip"):
        with zipfile.ZipFile(artifact) as z:
            z.extractall(extract_dir)
        binary = locate_binary(extract_dir)
        if binary:
            return version, binary
        dmgs = glob.glob(os.path.join(extract_dir, "**/*.dmg"), recursive=True)
        if not dmgs:
            raise SystemExit("zip contained neither an .app nor a .dmg")
        return version, extract_dmg(dmgs[0], os.path.join(workdir, "dmg"))
    if name.lower().endswith(".dmg"):
        return version, extract_dmg(artifact, os.path.join(workdir, "dmg"))
    raise SystemExit(f"unsupported macOS artifact: {name}")


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--gist-id", default=DEFAULT_GIST_ID)
    ap.add_argument("--version", help="specific version to fetch (e.g. v1.3.5); default: latest")
    ap.add_argument("--check-version", action="store_true",
                    help="only print the latest version (scrape; no download)")
    ap.add_argument("--workdir", help="working directory (default: a temp dir)")
    ap.add_argument("--keep", action="store_true", help="keep the workdir on exit")
    args = ap.parse_args(argv)

    folder_id = gist_drive_folder_id(args.gist_id)

    if args.check_version:
        print(json.dumps({"version": latest_version_from_folder(folder_id)}))
        return 0

    workdir = args.workdir or tempfile.mkdtemp(prefix="spotiflac-next-")
    os.makedirs(workdir, exist_ok=True)
    try:
        version, binary = fetch_binary(folder_id, args.version, workdir)
        print(json.dumps({"version": version, "binary": binary}))
        return 0
    finally:
        if not args.keep and not args.workdir:
            shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
