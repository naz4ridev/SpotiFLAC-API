#!/usr/bin/env python3
import sys
import json
import argparse
import logging

# Ensure target modules can be loaded from standard paths.
sys.path.insert(0, "/opt/python-spotiflac-src")
sys.path.insert(0, "/opt/python-spotiflac-src/backend")

# Save a reference to real stdout and redirect sys.stdout to sys.stderr to avoid pollution.
real_stdout = sys.stdout
sys.stdout = sys.stderr

def print_result(ok, data=None, error=None):
    res = {"ok": ok}
    if data:
        res.update(data)
    if error:
        res["error"] = error
    real_stdout.write(json.dumps(res) + "\n")
    real_stdout.flush()

try:
    from SpotiFLAC.providers.spotify_metadata import SpotifyMetadataClient, parse_spotify_url
except ImportError as e:
    print_result(False, error=f"Failed to import SpotiFLAC: {e}")
    sys.exit(0)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    args = parser.parse_args()

    # Mute root logging to prevent third-party packages from writing to stdout/stderr unnecessarily
    logging.basicConfig(level=logging.WARNING, stream=sys.stderr)

    try:
        info = parse_spotify_url(args.url)
        if info["type"] != "track":
            print_result(False, error=f"Unsupported URL type: {info['type']}. Only track URLs are supported.")
            return

        client = SpotifyMetadataClient()
        meta = client.get_track(info["id"])
        
        print_result(True, data={
            "spotify_id": meta.id,
            "title": meta.title,
            "artist": meta.artists,
            "album": meta.album,
            "duration_ms": meta.duration_ms,
        })
    except Exception as e:
        print_result(False, error=str(e))

if __name__ == "__main__":
    main()
