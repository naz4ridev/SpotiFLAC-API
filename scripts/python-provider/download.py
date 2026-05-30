#!/usr/bin/env python3
import sys
import os
import json
import argparse
import logging

# Pin the target search paths for SpotiFLAC modules.
sys.path.insert(0, "/opt/python-spotiflac-src")
sys.path.insert(0, "/opt/python-spotiflac-src/backend")

# Save a reference to real stdout and redirect sys.stdout to sys.stderr to avoid pollution.
real_stdout = sys.stdout
sys.stdout = sys.stderr

def print_result(ok, file_path=None, error=None, metadata=None):
    res = {"ok": ok}
    if file_path:
        res["file_path"] = file_path
    if error:
        res["error"] = error
    if metadata:
        res["metadata"] = metadata
    
    # Write to the actual stdout
    real_stdout.write(json.dumps(res) + "\n")
    real_stdout.flush()

try:
    from SpotiFLAC import SpotiFLAC
    from SpotiFLAC.downloader import SpotiflacDownloader, DownloadOptions, download_one
    from SpotiFLAC.core.models import TrackMetadata
except ImportError as e:
    print_result(False, error=f"Failed to import SpotiFLAC: {e}")
    sys.exit(0)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()

    # Mute root logging to prevent third-party packages from writing to stdout/stderr unnecessarily
    logging.basicConfig(level=logging.WARNING, stream=sys.stderr)

    try:
        opts = DownloadOptions(
            output_dir=args.output_dir,
            services=["tidal", "qobuz", "amazon"],
            filename_format="{title} - {artist}",
            quality="LOSSLESS",
            embed_lyrics=True,
            enrich_metadata=True,
        )

        downloader = SpotiflacDownloader(opts)
        
        # 1. Resolve metadata
        collection_name, tracks, info = downloader._resolve_metadata(args.url)
        if not tracks:
            print_result(False, error="No tracks found for URL")
            return

        track = tracks[0] # Take first track for single download

        # 2. Resolve ISRC
        tracks = downloader._resolve_isrc_bulk([track])
        track = tracks[0]

        # 3. Build providers and run download
        from SpotiFLAC.downloader import _build_provider
        providers = []
        for service_name in opts.services:
            p = _build_provider(service_name, opts)
            if p:
                providers.append(p)
        if not providers:
            print_result(False, error=f"No valid providers found in: {opts.services}")
            return

        os.makedirs(args.output_dir, exist_ok=True)
        
        result = download_one(track, args.output_dir, providers, opts)
        
        if result.success:
            if result.file_path and os.path.exists(result.file_path):
                metadata = {
                    "title": track.title,
                    "artists": track.artists,
                    "album": track.album,
                    "isrc": track.isrc,
                }
                print_result(True, file_path=result.file_path, metadata=metadata)
            else:
                print_result(False, error="Download succeeded but output file is missing")
        else:
            print_result(False, error=result.error or "Download failed with unknown error")

    except Exception as e:
        print_result(False, error=f"Python provider error: {str(e)}")

if __name__ == "__main__":
    main()
