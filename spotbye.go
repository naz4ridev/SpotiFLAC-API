package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// spotbye.go implements the "spotbye" download engine: the private SpotiFLAC-Next
// C2 endpoints (*.spotbye.qzz.io, flacdownloader.com, ...). Unlike the upstream
// SpotiFLAC backend (which targets the older afkarxyz.qzz.io hosts), these
// addresses and their API shapes change between Next releases, so every endpoint
// URL is read from the C2 config store (seeded/refreshed via the extraction
// script) and the active variant is chosen using the status tracker.
//
// Per-service track-ID resolution reuses odesli (backend.SongLinkClient), which
// maps a Spotify track to its Tidal/Amazon URLs and ISRC; the Deezer id comes
// from the public Deezer ISRC lookup.
//
// Endpoint contracts (reverse-engineered from SpotiFLAC-Next v1.3.x):
//   deezer : dzr.spotbye.qzz.io/api/track/{deezer_id}?f=flac
//   qobuz  : qbz.spotbye.qzz.io/api/download-music?track_id={qobuz_id}
//            qbzalt.spotbye.qzz.io/{id}?quality={q}
//   amazon : amz / amznalt .spotbye.qzz.io
//   tidal  : tdl / tdlalt .spotbye.qzz.io  -> flacdownloader.com/flac/download[-token]
//   apple  : am.spotbye.qzz.io
// Responses are either a direct FLAC stream or JSON pointing at the media URL
// (fields url / downloadUrl / streamUrl / stream / link / file / path, possibly
// nested under "data"); spotbyeFetchMedia tolerates all of these.

var (
	tidalIDRe  = regexp.MustCompile(`(?:track/)(\d+)`)
	amazonIDRe = regexp.MustCompile(`(?:tracks/)([A-Za-z0-9]+)`)
)

// resolveWithSpotbye tries each requested service via its spotbye C2 endpoint,
// returning the path to the downloaded FLAC and the service used.
func (s *apiServer) resolveWithSpotbye(ctx context.Context, meta trackMetadata, serviceOrder []string, outputDir string) (string, string, []attempt, error) {
	attempts := make([]attempt, 0, len(serviceOrder))
	if s.cfg == nil {
		return "", "", attempts, fmt.Errorf("spotbye engine requires the C2 config store")
	}

	for _, service := range serviceOrder {
		if s.status != nil && !s.status.ServiceActive(ctx, service) {
			attempts = append(attempts, attempt{Service: service, Error: "skipped: no active variant in status"})
			continue
		}

		mediaURL, err := s.spotbyeResolveMedia(ctx, service, meta)
		if err != nil {
			attempts = append(attempts, attempt{Service: service, Error: err.Error()})
			continue
		}

		outputPath := filepath.Join(outputDir, buildMonochromeFilename(meta))
		if err := downloadMonochromeTrack(ctx, mediaURL, outputPath, meta); err != nil {
			attempts = append(attempts, attempt{Service: service, Error: "download failed: " + err.Error()})
			continue
		}
		if !isValidDownloadedFile(outputPath) {
			attempts = append(attempts, attempt{Service: service, Error: "downloaded file invalid"})
			continue
		}
		attempts = append(attempts, attempt{Service: service})
		return outputPath, service, attempts, nil
	}

	return "", "", attempts, fmt.Errorf("spotbye failed in all services: %s", strings.Join(serviceOrder, " -> "))
}

// spotbyeResolveMedia resolves an ffmpeg-ingestible media URL for a track on the
// given service by resolving its id and calling the configured C2 endpoints in
// order (primary -> alt -> fallback), returning the first that yields media.
func (s *apiServer) spotbyeResolveMedia(ctx context.Context, service string, meta trackMetadata) (string, error) {
	service = strings.ToLower(service)

	id, idErr := s.spotbyeServiceID(ctx, service, meta)
	if idErr != nil {
		return "", idErr
	}

	endpoints := s.spotbyeEndpoints(service)
	if len(endpoints) == 0 {
		return "", fmt.Errorf("spotbye %s: no endpoint configured", service)
	}

	var lastErr error
	for _, tmpl := range endpoints {
		endpoint := applyIDTemplate(tmpl, id)
		media, err := s.spotbyeFetchMedia(ctx, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		return media, nil
	}
	return "", fmt.Errorf("spotbye %s: all endpoints failed: %v", service, lastErr)
}

// spotbyeServiceID resolves the service-specific track identifier for a track.
func (s *apiServer) spotbyeServiceID(ctx context.Context, service string, meta trackMetadata) (string, error) {
	switch service {
	case "deezer":
		isrc := strings.TrimSpace(meta.ISRC)
		if isrc == "" {
			return "", fmt.Errorf("deezer: missing ISRC")
		}
		dz, err := s.resolveDeezerID(ctx, isrc)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d", dz), nil

	case "tidal":
		urls := s.resolveSongLinkURLs(meta.SpotifyID)
		if urls != nil {
			if m := tidalIDRe.FindStringSubmatch(urls.TidalURL); len(m) == 2 {
				return m[1], nil
			}
		}
		return "", fmt.Errorf("tidal: could not resolve tidal id via odesli")

	case "amazon":
		urls := s.resolveSongLinkURLs(meta.SpotifyID)
		if urls != nil {
			if m := amazonIDRe.FindStringSubmatch(urls.AmazonURL); len(m) == 2 {
				return m[1], nil
			}
		}
		return "", fmt.Errorf("amazon: could not resolve amazon id via odesli")

	case "qobuz", "apple":
		// Qobuz needs its numeric id and Apple its track id; neither is provided
		// by odesli. Qobuz is still covered by the upstream spotiflac engine in
		// the auto chain. Tracked as remaining work.
		return "", fmt.Errorf("spotbye %s: id resolution not implemented yet", service)

	default:
		return "", fmt.Errorf("unsupported spotbye service: %s", service)
	}
}

// spotbyeEndpoints returns the ordered C2 endpoint templates for a service:
// store-configured primary, then alt, then fallback, with sensible defaults.
func (s *apiServer) spotbyeEndpoints(service string) []string {
	var out []string
	add := func(urls []string) {
		for _, u := range urls {
			if strings.TrimSpace(u) != "" {
				out = append(out, u)
			}
		}
	}
	add(s.cfg.EnabledURLs(service, "download"))
	add(s.cfg.EnabledURLs(service, "download_alt"))
	add(s.cfg.EnabledURLs(service, "download_fallback"))

	if len(out) == 0 {
		// Defaults if the store has no rows yet. Ordered live-host-first based on
		// DNS reality (verified 2026-06): several spotbye.qzz.io download
		// subdomains are NXDOMAIN in current builds, while the service backends
		// actually resolve under anandserver.cfd / squid.wtf. qobuz is confirmed
		// live (qbz.spotbye.qzz.io/api/download-music returns HTTP 400 on a bad
		// id). The exact request paths for the anandserver/squid hosts are not
		// statically recoverable and should be confirmed/overridden in prod via
		// the /admin CRUD (where these hosts resolve); the spotbye templates are
		// kept as fallbacks. The tolerant response parser handles either a direct
		// stream or a JSON media-URL pointer regardless of host.
		switch service {
		case "qobuz":
			out = []string{"https://qbz.spotbye.qzz.io/api/download-music?track_id=", "https://qbzalt.spotbye.qzz.io/%s?quality=27"}
		case "deezer":
			out = []string{"https://deezer.anandserver.cfd/%s", "https://dzr.spotbye.qzz.io/api/track/%d?f=flac"}
		case "amazon":
			out = []string{"https://amz.squid.wtf/%s", "https://amz.spotbye.qzz.io/api/track/%s"}
		case "tidal":
			out = []string{"https://flacdownloader.com/flac/download?id=%s", "https://tdl.spotbye.qzz.io/api/track/%s"}
		case "apple":
			out = []string{"https://am.spotbye.qzz.io/api/track/%s"}
		}
	}
	return out
}

// resolveSongLinkURLs fetches the odesli URL map for a Spotify track (best-effort).
func (s *apiServer) resolveSongLinkURLs(spotifyID string) *backend.SongLinkURLs {
	if strings.TrimSpace(spotifyID) == "" {
		return nil
	}
	client := backend.NewSongLinkClient()
	urls, err := client.GetAllURLsFromSpotify(spotifyID, "US")
	if err != nil {
		return nil
	}
	return urls
}

// resolveDeezerID looks up a Deezer numeric track id from an ISRC.
func (s *apiServer) resolveDeezerID(ctx context.Context, isrc string) (int64, error) {
	api := fmt.Sprintf("https://api.deezer.com/track/isrc:%s", url.PathEscape(isrc))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("deezer isrc lookup: %w", err)
	}
	defer resp.Body.Close()
	var payload struct {
		ID    int64 `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("deezer isrc decode: %w", err)
	}
	if payload.ID == 0 {
		msg := "not found on deezer"
		if payload.Error != nil && payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		return 0, fmt.Errorf("deezer isrc %s: %s", isrc, msg)
	}
	return payload.ID, nil
}

// spotbyeFetchMedia calls a C2 endpoint and returns a media URL ffmpeg can read.
// The C2 may stream the FLAC directly (the endpoint URL is returned as-is for
// ffmpeg to fetch) or return JSON pointing at the media URL under one of several
// known field names, possibly nested under "data".
func (s *apiServer) spotbyeFetchMedia(ctx context.Context, endpoint string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("c2 request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("c2 returned %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "audio") || strings.Contains(ctype, "flac") || strings.Contains(ctype, "octet-stream") {
		return endpoint, nil // direct stream; ffmpeg fetches it
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("c2 read: %w", err)
	}
	if url := extractMediaURL(body); url != "" {
		return url, nil
	}
	if len(body) > 4 && string(body[:4]) == "fLaC" {
		return endpoint, nil
	}
	return "", fmt.Errorf("c2 response had no media url")
}

// mediaURLFields are the JSON keys SpotiFLAC-Next uses for a downloadable URL.
var mediaURLFields = []string{"url", "downloadUrl", "download_url", "streamUrl", "stream", "link", "file", "path"}

// extractMediaURL pulls a media URL out of a C2 JSON response, checking the
// known field names at the top level and nested under "data".
func extractMediaURL(body []byte) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return ""
	}
	if u := pickURL(top); u != "" {
		return u
	}
	if data, ok := top["data"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(data, &nested) == nil {
			if u := pickURL(nested); u != "" {
				return u
			}
		}
	}
	return ""
}

func pickURL(m map[string]json.RawMessage) string {
	for _, f := range mediaURLFields {
		raw, ok := m[f]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && strings.HasPrefix(v, "http") {
			return v
		}
	}
	return ""
}

// applyIDTemplate substitutes the track id into an endpoint template. Supports
// printf-style %d/%s and a trailing "track_id="/"id="/"/" suffix.
func applyIDTemplate(endpoint, id string) string {
	if strings.Contains(endpoint, "%d") || strings.Contains(endpoint, "%s") {
		endpoint = strings.ReplaceAll(endpoint, "%d", id)
		endpoint = strings.ReplaceAll(endpoint, "%s", id)
		return endpoint
	}
	if strings.HasSuffix(endpoint, "track_id=") || strings.HasSuffix(endpoint, "id=") || strings.HasSuffix(endpoint, "/") {
		return endpoint + id
	}
	return endpoint
}

// firstEnabledURL returns the first URL from the list, or the fallback if empty.
func firstEnabledURL(urls []string, fallback string) string {
	for _, u := range urls {
		if strings.TrimSpace(u) != "" {
			return u
		}
	}
	return fallback
}
