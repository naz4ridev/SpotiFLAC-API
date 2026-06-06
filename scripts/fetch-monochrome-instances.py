#!/usr/bin/env python3
"""
Fetch the current Monochrome (Hi-Fi API) instance list and push it to the API.

Monochrome instances are community-hosted and change over time. The canonical,
maintained list lives in the project's INSTANCES.md
(https://github.com/monochrome-music/monochrome/blob/main/INSTANCES.md), under an
"API Instances" section, plus a live status tracker URL.

Instead of hard-coding these in .env, this script parses that file and writes the
results into the API config store (settings: monochrome.api_instances,
monochrome.streaming_instances, monochrome.discovery_urls) so the running combo
always uses an up-to-date set. The app then probes reachability via /v1/status
and discovers more at runtime from the tracker.

Usage:
    fetch-monochrome-instances.py [--source URL] [--apply --api BASE_URL] [--json]

No third-party dependencies (urllib only).
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import urllib.request

DEFAULT_SOURCE = "https://raw.githubusercontent.com/monochrome-music/monochrome/main/INSTANCES.md"

# Hosts in INSTANCES.md that are not usable API instances.
_EXCLUDE = ("github.com", "rentry.co", "/limitedtidalaccs")
# Status/uptime trackers (used for discovery, not as direct instances).
_TRACKER_HINT = ("uptime", "status")


def fetch(url: str) -> str:
    req = urllib.request.Request(url, headers={"User-Agent": "spotiflac-updater"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.read().decode("utf-8", "replace")


def section(md: str, heading: str) -> str:
    """Return the markdown between a `## heading` and the next `## ` heading."""
    lines = md.splitlines()
    out, capturing = [], False
    for ln in lines:
        if ln.strip().lower().startswith("## "):
            if capturing:
                break
            capturing = heading.lower() in ln.lower()
            continue
        if capturing:
            out.append(ln)
    return "\n".join(out)


def parse_instances(md: str) -> dict:
    api_section = section(md, "API Instances") or md
    urls = re.findall(r"https?://[a-zA-Z0-9._/-]+", api_section)

    api, trackers = [], []
    seen = set()
    for u in urls:
        u = u.rstrip("/")
        if any(x in u for x in _EXCLUDE):
            continue
        if u in seen:
            continue
        seen.add(u)
        if any(h in u for h in _TRACKER_HINT):
            trackers.append(u)
        else:
            api.append(u)

    # Streaming instances are the subset that historically serve manifests
    # (geeked + the qqdl Lucida mirrors). Fall back to the full API list.
    streaming = [u for u in api if "geeked.wtf" in u or "qqdl.site" in u]
    if not streaming:
        streaming = list(api)

    return {
        "api_instances": api,
        "streaming_instances": streaming,
        "discovery_urls": trackers,
    }


def put_setting(api_base: str, key: str, value: str) -> None:
    url = f"{api_base.rstrip('/')}/admin/settings/{key}"
    body = json.dumps({"value": value}).encode()
    req = urllib.request.Request(url, data=body, method="PUT",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        resp.read()


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="Fetch Monochrome instances into the API config store.")
    ap.add_argument("--source", default=DEFAULT_SOURCE, help="INSTANCES.md raw URL")
    ap.add_argument("--api", help="API base URL for --apply (e.g. http://127.0.0.1:8080)")
    ap.add_argument("--apply", action="store_true", help="PUT the lists into the API config store")
    ap.add_argument("--json", action="store_true", help="print parsed instances as JSON")
    args = ap.parse_args(argv)

    md = fetch(args.source)
    parsed = parse_instances(md)

    if not parsed["api_instances"]:
        print("error: no API instances parsed from source", file=sys.stderr)
        return 1

    if args.json or not args.apply:
        print(json.dumps(parsed, indent=2))

    if args.apply:
        if not args.api:
            print("error: --apply requires --api BASE_URL", file=sys.stderr)
            return 2
        put_setting(args.api, "monochrome.api_instances", ",".join(parsed["api_instances"]))
        put_setting(args.api, "monochrome.streaming_instances", ",".join(parsed["streaming_instances"]))
        if parsed["discovery_urls"]:
            put_setting(args.api, "monochrome.discovery_urls", ",".join(parsed["discovery_urls"]))
        print(f"Applied {len(parsed['api_instances'])} API instances to {args.api}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
