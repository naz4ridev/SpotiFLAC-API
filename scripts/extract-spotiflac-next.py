#!/usr/bin/env python3
"""
Static C2 / endpoint extractor for SpotiFLAC-Next builds.

SpotiFLAC-Next is a compiled Go application. The backend talks to a set of
private "C2" endpoints (``*.spotbye.qzz.io``, ``flacdownloader.com``, ...) plus
public lyric/metadata providers. Those addresses change between releases.

This script statically pulls everything the API needs to stay in sync from the
shipped binary, so we never have to decompile by hand again. Point it at a new
build, get a ``c2-manifest.json`` back, diff it against the previous one, and
import the result into the API config store (see scripts/update-c2-from-binary.sh).

Usage:
    extract-spotiflac-next.py <path-to-binary-or-.app> [options]

Options:
    -o, --output FILE      Write the manifest JSON here (default: stdout).
    --emit-sql             Also print SQL upserts for the API config store.
    --diff OLD_MANIFEST    Print a human-readable diff against a previous manifest.
    --raw-urls             Dump every unique URL found (debugging aid).

No third-party dependencies; standard library only.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import hashlib
import json
import os
import plistlib
import re
import sys
from typing import Iterable

# --- printable-string extraction -------------------------------------------

# Go stores string literals back-to-back without NUL terminators, so a single
# printable run often glues many unrelated tokens together (e.g.
# "tdl.spotbye.qzz.iolcd.spotbye.qzz.ioamz.spotbye.qzz.ioflacdownloader.comTidal").
# We extract long printable runs, then pull non-overlapping matches out of them
# with anchored regexes. That recovers each address regardless of its neighbours.

_PRINTABLE = re.compile(rb"[\x20-\x7e]{6,}")


def printable_runs(data: bytes) -> Iterable[str]:
    for m in _PRINTABLE.finditer(data):
        yield m.group().decode("ascii", "replace")


# --- patterns ---------------------------------------------------------------

# A hostname/URL stops at the first character that cannot be part of one. Go
# string concatenation means the char right after a host is usually an uppercase
# letter or '{' (a format verb), so we deliberately keep the host charset tight.
_HOST_CHARS = r"[a-z0-9.-]+"

# Full URLs (scheme + host + optional path/query up to a non-URL char).
_URL = re.compile(r"https?://[a-zA-Z0-9./_%:?=&{}#@~+-]+")

# Bare C2 / fallback hosts wherever they appear. SpotiFLAC-Next keeps several
# host families per service (the a/b/c/d/e/x "variants" from the status payload):
# spotbye.qzz.io, anandserver.cfd, squid.wtf, and flacdownloader.com. Only the
# currently-provisioned ones resolve, so we capture them all and let the engine
# fall back across candidates.
_C2_HOST = re.compile(
    r"(?:[a-z0-9-]+\.)?(?:spotbye\.qzz\.io|anandserver\.cfd|squid\.wtf)|flacdownloader\.com",
)

# Subdomain (of anandserver.cfd / squid.wtf) -> (service, role).
_OTHER_FAMILY_MAP = {
    "deezer.anandserver.cfd": ("deezer", "download"),
    "tidal.anandserver.cfd": ("tidal", "keys"),
    "amz.squid.wtf": ("amazon", "download"),
}

# Status gist: gist.githubusercontent.com/<user>/<32 hex>/raw
_GIST = re.compile(
    r"gist\.githubusercontent\.com/[A-Za-z0-9_-]+/[0-9a-f]{20,40}/raw[A-Za-z0-9._/-]*"
)

# Subdomain -> (service, role) for the spotbye family and known fallbacks.
_SPOTBYE_MAP = {
    "tdl": ("tidal", "download"),
    "tdlalt": ("tidal", "download_alt"),
    "qbz": ("qobuz", "download"),
    "qbzalt": ("qobuz", "download_alt"),
    "amz": ("amazon", "download"),
    "amznalt": ("amazon", "download_alt"),
    "dzr": ("deezer", "download"),
    "jmdl": ("jiosaavn", "download"),
    "am": ("apple", "download"),
    "mb": ("musicbrainz", "metadata"),
    "lcd": ("lyrics", "aux"),
    "status": ("_status", "status"),
}

# Known public providers we care about, matched as URL prefixes.
_KNOWN_ENDPOINT_PREFIXES = {
    "lyrics.lrclib_get": "https://lrclib.net/api/get",
    "lyrics.lrclib_search": "https://lrclib.net/api/search",
    "lyrics.musixmatch_token": "https://apic.musixmatch.com/ws/1.1/token.get",
    "lyrics.musixmatch_ws": "https://apic.musixmatch.com/ws/1.1/",
    "lyrics.spotify_color": "https://spclient.wg.spotify.com/color-lyrics/v2/track/",
    "metadata.spotify_token": "https://open.spotify.com/api/token",
    "metadata.spotify_partner": "https://api-partner.spotify.com/pathfinder/v2/query",
    "metadata.musicbrainz_genre": "https://mb.spotbye.qzz.io/api/genre",
}

# Loose, opt-in constant probes for manual review (values that drift per build).
_CONST_PROBES = {
    "default_user_agent": re.compile(
        r"Mozilla/5\.0 \([^)]*\)[^\"]*?(?:Safari/[0-9.]+|Firefox/[0-9.]+)"
    ),
    "tidal_client_id": re.compile(r"\b[A-Za-z0-9]{16}\b"),  # heuristic; review
}


# Known spotbye subdomains, longest first so suffix matching is greedy-correct.
_KNOWN_SUBS = sorted(_SPOTBYE_MAP, key=len, reverse=True)

# Trailing tokens Go glues onto a string literal (the next literal in .rodata).
# We trim a single trailing capitalised word or known noise so example URLs are
# usable. This is best-effort: the host + path template are what matter.
_GLUE_TAIL = re.compile(
    r"(?:[A-Z][a-zA-Z]{2,}|failed|received|default|deezer|tidal|reflect:?|Warning:.*|up|no)$"
)


def host_of(url: str) -> str:
    m = re.match(r"https?://([a-z0-9.-]+)", url)
    return m.group(1) if m else url


def trim_glue(url: str) -> str:
    """Cut whitespace, a glued second URL, and a trailing literal off a URL."""
    url = url.split()[0] if url.split() else url
    # A second "http(s)://" means another literal was glued on; cut it.
    second = url.find("http", 5)
    if second != -1:
        url = url[:second].rstrip("?&=/")
    # Drop trailing Go noise (error-wrap %w, stray colon/dot) but keep %s/%d
    # which are legitimate path/query template verbs.
    url = re.sub(r"(%w|:|\.)+$", "", url)
    for _ in range(3):  # peel a couple of stacked glue tokens
        new = _GLUE_TAIL.sub("", url)
        if new == url:
            break
        url = new
    return url.rstrip("?&")


def classify_host(host: str) -> tuple[str, str] | None:
    """Resolve a (possibly prefix-glued) host to (service, role).

    Go concatenates literals, so "nameam.spotbye.qzz.io" is really
    "name" + "am.spotbye.qzz.io". We therefore match the leading label against
    the *suffix* of a known subdomain. Hosts we cannot confidently resolve return
    None (they still surface in all_c2_hosts for manual review)."""
    if host.endswith(".spotbye.qzz.io"):
        label = host[: -len(".spotbye.qzz.io")]
        if label in _SPOTBYE_MAP:
            return _SPOTBYE_MAP[label]
        for sub in _KNOWN_SUBS:
            if label.endswith(sub):
                return _SPOTBYE_MAP[sub]
        return None
    if host == "flacdownloader.com":
        return ("tidal", "download_fallback")
    if host in _OTHER_FAMILY_MAP:
        return _OTHER_FAMILY_MAP[host]
    if host.endswith(".anandserver.cfd"):
        return ("tidal", "keys")
    if host.endswith(".squid.wtf"):
        return ("amazon", "download")
    return None


# Host families currently provisioned (resolve in DNS) rank above the older
# spotbye.qzz.io download subdomains, many of which are NXDOMAIN in newer builds.
def host_family_priority(host: str) -> int:
    if host.endswith(".anandserver.cfd") or host.endswith(".squid.wtf") or host == "flacdownloader.com":
        return 2
    return 1


def canonical_host(host: str) -> str:
    """Strip a glued prefix label down to the known spotbye subdomain."""
    if host.endswith(".spotbye.qzz.io"):
        label = host[: -len(".spotbye.qzz.io")]
        if label in _SPOTBYE_MAP:
            return host
        for sub in _KNOWN_SUBS:
            if label.endswith(sub):
                return f"{sub}.spotbye.qzz.io"
    return host


# --- extraction core ---------------------------------------------------------


def extract(data: bytes) -> dict:
    urls: set[str] = set()
    c2_hosts: set[str] = set()
    gists: set[str] = set()

    for run in printable_runs(data):
        for m in _URL.finditer(run):
            urls.add(m.group())
        for m in _C2_HOST.finditer(run):
            c2_hosts.add(m.group())
        for m in _GIST.finditer(run):
            # Trim any trailing junk glued on after "/raw".
            g = m.group()
            idx = g.find("/raw")
            gists.add(g[: idx + 4])

    # Group download/metadata endpoints by (service, role), keeping the richest
    # (longest, i.e. most path/params) URL we saw per host.
    endpoints: dict[str, dict] = {}
    for url in sorted(urls):  # deterministic ordering for stable diffs
        host = host_of(url)
        cls = classify_host(host)
        if not cls:
            continue
        service, role = cls
        # Skip embedded-frontend JS artifacts that share a backend host.
        if "window." in url or "__" in url:
            continue
        chost = canonical_host(host)
        # Re-anchor the URL onto the canonical host and trim glued tail.
        clean_url = trim_glue(url.replace(host, chost, 1))
        key = f"{service}.{role}"
        prev = endpoints.get(key)
        # On collision, prefer the currently-provisioned host family, then the
        # URL that carries a path/query (longest after trimming).
        cand = (host_family_priority(chost), len(clean_url))
        if prev is None or cand > (host_family_priority(prev["host"]), len(prev["example_url"])):
            endpoints[key] = {
                "service": service,
                "role": role,
                "host": chost,
                "example_url": clean_url,
            }

    # Also register hosts seen only bare (no scheme), so nothing is missed.
    for host in c2_hosts:
        cls = classify_host(host)
        if not cls:
            continue
        service, role = cls
        chost = canonical_host(host)
        key = f"{service}.{role}"
        endpoints.setdefault(
            key,
            {"service": service, "role": role, "host": chost, "example_url": f"https://{chost}"},
        )

    # Public lyric / metadata providers.
    public: dict[str, str] = {}
    for name, prefix in _KNOWN_ENDPOINT_PREFIXES.items():
        match = next((u for u in sorted(urls, key=len, reverse=True) if u.startswith(prefix)), None)
        if match:
            public[name] = trim_glue(match)

    # Status source.
    status_gist = sorted(gists)

    # Loose constants for manual review.
    constants: dict[str, list[str]] = {}
    text = data.decode("latin-1", "replace")
    ua = _CONST_PROBES["default_user_agent"].findall(text)
    if ua:
        constants["user_agent"] = sorted(set(ua))[:5]

    return {
        "endpoints": dict(sorted(endpoints.items())),
        "public_providers": dict(sorted(public.items())),
        "status_gists": status_gist,
        "constants": constants,
        "all_c2_hosts": sorted(c2_hosts),
    }


def build_manifest(binary_path: str, app_version: str | None) -> dict:
    with open(binary_path, "rb") as fh:
        data = fh.read()
    sha = hashlib.sha256(data).hexdigest()
    result = extract(data)
    result_meta = {
        "extracted_at": _dt.datetime.now(_dt.timezone.utc).isoformat(),
        "binary_path": os.path.abspath(binary_path),
        "binary_sha256": sha,
        "binary_size": len(data),
        "app_version": app_version,
        "extractor_version": 1,
    }
    return {"meta": result_meta, **result}


# --- input resolution ---------------------------------------------------------


def resolve_binary(path: str) -> tuple[str, str | None]:
    """Accept a raw binary or a macOS .app bundle. Returns (binary, app_version)."""
    app_version = None
    if path.endswith(".app") or os.path.isdir(path):
        info = os.path.join(path, "Contents", "Info.plist")
        if os.path.exists(info):
            try:
                with open(info, "rb") as fh:
                    raw = fh.read()
                try:
                    pl = plistlib.loads(raw)
                except Exception:
                    # Some bundles ship an XML plist that opens with <!DOCTYPE
                    # (no <?xml> prolog), defeating plistlib autodetection.
                    pl = plistlib.loads(raw, fmt=plistlib.FMT_XML)
                app_version = pl.get("CFBundleShortVersionString") or pl.get("CFBundleVersion")
                exe = pl.get("CFBundleExecutable")
            except Exception:
                exe = None
        else:
            exe = None
        macos = os.path.join(path, "Contents", "MacOS")
        if exe and os.path.exists(os.path.join(macos, exe)):
            return os.path.join(macos, exe), app_version
        # Fall back to the first regular file in MacOS/.
        if os.path.isdir(macos):
            for name in sorted(os.listdir(macos)):
                full = os.path.join(macos, name)
                if os.path.isfile(full) and "." not in name:
                    return full, app_version
        raise SystemExit(f"could not locate executable inside {path}")
    return path, app_version


# --- SQL emission -------------------------------------------------------------


def emit_sql(manifest: dict) -> str:
    """Upserts matching the API config store schema (see internal/config)."""
    lines = ["BEGIN;"]
    for key, ep in manifest["endpoints"].items():
        service = ep["service"].replace("'", "''")
        role = ep["role"].replace("'", "''")
        url = ep["example_url"].replace("'", "''")
        lines.append(
            "INSERT INTO c2_endpoints(service, role, url, variant, enabled, priority) "
            f"VALUES('{service}','{role}','{url}','', 1, 0) "
            "ON CONFLICT(service, role, variant) DO UPDATE SET url=excluded.url;"
        )
    for name, url in manifest["public_providers"].items():
        k = name.replace("'", "''")
        v = url.replace("'", "''")
        lines.append(
            f"INSERT INTO settings(key, value) VALUES('endpoint.{k}','{v}') "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value;"
        )
    # Primary status source is the status host; the gists are listed in the
    # manifest as fallback candidates (which one is the downloader-status gist
    # cannot be told apart statically, so we don't guess here).
    status_ep = manifest["endpoints"].get("_status.status")
    if status_ep:
        s = status_ep["example_url"].replace("'", "''")
        lines.append(
            f"INSERT INTO settings(key, value) VALUES('status.source_url','{s}') "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value;"
        )
    lines.append("COMMIT;")
    return "\n".join(lines)


# --- diff ---------------------------------------------------------------------


def diff_manifests(old: dict, new: dict) -> str:
    out: list[str] = []

    def flat(m: dict) -> dict[str, str]:
        f = {}
        for k, ep in m.get("endpoints", {}).items():
            f[f"endpoint:{k}"] = ep["example_url"]
        for k, v in m.get("public_providers", {}).items():
            f[f"public:{k}"] = v
        for i, g in enumerate(m.get("status_gists", [])):
            f[f"status_gist:{i}"] = g
        return f

    of, nf = flat(old), flat(new)
    for k in sorted(set(of) | set(nf)):
        ov, nv = of.get(k), nf.get(k)
        if ov == nv:
            continue
        if ov is None:
            out.append(f"+ ADDED   {k}: {nv}")
        elif nv is None:
            out.append(f"- REMOVED {k}: {ov}")
        else:
            out.append(f"~ CHANGED {k}:\n    old: {ov}\n    new: {nv}")
    if not out:
        return "No C2/endpoint changes between manifests."
    return "\n".join(out)


# --- cli ----------------------------------------------------------------------


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="Extract SpotiFLAC-Next C2 endpoints statically.")
    ap.add_argument("binary", help="Path to the SpotiFLAC-Next binary or .app bundle")
    ap.add_argument("-o", "--output", help="Write manifest JSON to this file")
    ap.add_argument("--emit-sql", action="store_true", help="Print SQL upserts for the config store")
    ap.add_argument("--diff", metavar="OLD_MANIFEST", help="Diff against a previous manifest JSON")
    ap.add_argument("--raw-urls", action="store_true", help="Dump every unique URL found")
    args = ap.parse_args(argv)

    binary, app_version = resolve_binary(args.binary)
    manifest = build_manifest(binary, app_version)

    if args.raw_urls:
        with open(binary, "rb") as fh:
            data = fh.read()
        urls = set()
        for run in printable_runs(data):
            urls.update(_URL.findall(run))
        print("\n".join(sorted(urls)), file=sys.stderr)

    if args.diff:
        with open(args.diff) as fh:
            old = json.load(fh)
        print(diff_manifests(old, manifest))
        return 0

    payload = json.dumps(manifest, indent=2, ensure_ascii=False)
    if args.output:
        with open(args.output, "w") as fh:
            fh.write(payload + "\n")
        print(f"Wrote manifest to {args.output}", file=sys.stderr)
    else:
        print(payload)

    if args.emit_sql:
        print("\n-- SQL upserts --", file=sys.stderr)
        print(emit_sql(manifest))

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
